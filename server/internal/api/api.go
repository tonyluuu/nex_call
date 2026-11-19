// Package api 提供 HTTP 接口：登录、设备列表、通话记录和信令 WebSocket 入口。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nexcall/server/internal/auth"
	"nexcall/server/internal/signal"
	"nexcall/server/internal/store"
)

type Server struct {
	Store          *store.Store
	Hub            *signal.Hub
	Sessions       *auth.Sessions
	LiveKitURL     string
	OriginPatterns []string
	WebDir         string

	limiter loginLimiter
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/me", s.requireUser(s.me))
	mux.HandleFunc("GET /api/devices", s.requireUser(s.listDevices))
	mux.HandleFunc("GET /api/calls", s.requireUser(s.listCalls))
	mux.HandleFunc("GET /ws/web", s.wsWeb)
	mux.HandleFunc("GET /ws/device", s.wsDevice)
	if s.WebDir != "" {
		mux.Handle("GET /", spaHandler(s.WebDir))
	}
	return logRequests(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type ctxKey struct{}

func userFrom(r *http.Request) string {
	u, _ := r.Context().Value(ctxKey{}).(string)
	return u
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return t
	}
	return ""
}

func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.Sessions.Verify(bearer(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try later")
		return
	}
	u, err := s.Store.GetUser(r.Context(), req.Username)
	if err != nil || !auth.CheckPassword(u.PasswordHash, req.Password) {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("get user", "err", err)
		}
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.limiter.reset(ip)
	writeJSON(w, http.StatusOK, map[string]string{
		"token":      s.Sessions.Issue(u.Username),
		"username":   u.Username,
		"livekitUrl": s.LiveKitURL,
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": userFrom(r), "livekitUrl": s.LiveKitURL})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Hub.Snapshot())
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	calls, err := s.Store.ListCalls(r.Context(), "", limit)
	if err != nil {
		slog.Error("list calls", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if calls == nil {
		calls = []*store.CallRecord{}
	}
	writeJSON(w, http.StatusOK, calls)
}

// wsWeb 浏览器的 WebSocket 不能自定义请求头，会话令牌放在查询参数里。
func (s *Server) wsWeb(w http.ResponseWriter, r *http.Request) {
	user, err := s.Sessions.Verify(r.URL.Query().Get("token"))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.Hub.Serve(w, r, signal.NewPeer(signal.KindWeb, user, user), s.OriginPatterns)
}

// wsDevice 设备用请求头 Authorization: Bearer <deviceId>:<secret> 鉴权。
func (s *Server) wsDevice(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := strings.Cut(bearer(r), ":")
	if !ok || id == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try later")
		return
	}
	d, err := s.Store.GetDevice(r.Context(), id)
	if err != nil || !auth.CheckDeviceSecret(d.SecretHash, secret) {
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.limiter.reset(ip)
	s.Hub.Serve(w, r, signal.NewPeer(signal.KindDevice, d.ID, d.Name), nil)
}

// loginLimiter 每个 IP 连续失败 10 次后锁定 5 分钟。
type loginLimiter struct {
	mu   sync.Mutex
	fails map[string]*failure
}

type failure struct {
	count int
	until time.Time
}

const (
	maxFails    = 10
	lockoutTime = 5 * time.Minute
)

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[ip]
	return f == nil || f.count < maxFails || time.Now().After(f.until)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*failure{}
	}
	f := l.fails[ip]
	if f == nil || (f.count >= maxFails && time.Now().After(f.until)) {
		f = &failure{}
		l.fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.until = time.Now().Add(lockoutTime)
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".") {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, dir+"/index.html")
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap 让 websocket.Accept 能拿到底层的 http.Hijacker。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/ws/") || strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
		}
	})
}
