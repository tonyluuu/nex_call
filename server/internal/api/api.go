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
	mux.HandleFunc("GET /api/calls/{id}", s.requireUser(s.getCall))
	mux.HandleFunc("POST /api/admin/kick", s.requireAdmin(s.kick))
	mux.HandleFunc("GET /api/admin/users", s.requireAdmin(s.listUsers))
	mux.HandleFunc("POST /api/admin/users", s.requireAdmin(s.createUser))
	mux.HandleFunc("DELETE /api/admin/users/{username}", s.requireAdmin(s.deleteUser))
	mux.HandleFunc("POST /api/admin/devices/{id}/volume", s.requireAdmin(s.setVolume))
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
	if u := userRecord(r); u != nil {
		return u.Username
	}
	return ""
}

func userRecord(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxKey{}).(*store.User)
	return u
}

// authenticate 先验签，再核对数据库里的会话号。重新登录后旧令牌不再有效。
func (s *Server) authenticate(ctx context.Context, token string) (*store.User, error) {
	user, sid, err := s.Sessions.Verify(token)
	if err != nil {
		return nil, err
	}
	u, err := s.Store.GetUser(ctx, user)
	if err != nil || sid == "" || u.SessionID != sid {
		return nil, auth.ErrInvalidToken
	}
	return u, nil
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
		u, err := s.authenticate(r.Context(), bearer(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if userRecord(r).Role != signal.RoleAdmin {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	})
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
	sid, err := auth.NewSessionID()
	if err != nil {
		slog.Error("session id", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.Store.SetSession(r.Context(), u.Username, sid); err != nil {
		slog.Error("set session", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Hub.EndUserSessions(u.Username, signal.ReasonReplaced)
	writeJSON(w, http.StatusOK, map[string]string{
		"token":      s.Sessions.Issue(u.Username, sid),
		"username":   u.Username,
		"role":       u.Role,
		"livekitUrl": s.LiveKitURL,
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.GetUser(r.Context(), userFrom(r))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username, "role": u.Role, "livekitUrl": s.LiveKitURL})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Hub.Snapshot())
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	party := "web:" + userFrom(r)
	if u, err := s.Store.GetUser(r.Context(), userFrom(r)); err == nil && u.Role == signal.RoleAdmin {
		party = ""
	}
	calls, err := s.Store.ListCalls(r.Context(), party, limit)
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

func (s *Server) getCall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	call, err := s.Store.GetCall(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	u, err := s.Store.GetUser(r.Context(), userFrom(r))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	parts, err := s.Store.ListParticipants(r.Context(), id)
	if err != nil {
		slog.Error("list participants", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if u.Role != signal.RoleAdmin && !callInvolves(call, parts, "web:"+u.Username) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	if parts == nil {
		parts = []*store.Participant{}
	}
	writeJSON(w, http.StatusOK, struct {
		*store.CallRecord
		Participants []*store.Participant `json:"participants"`
	}{call, parts})
}

func callInvolves(c *store.CallRecord, parts []*store.Participant, addr string) bool {
	if c.Caller == addr || c.Callee == addr {
		return true
	}
	for _, p := range parts {
		if p.Identity == addr {
			return true
		}
	}
	return false
}

func (s *Server) kick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Room     string `json:"room"`
		Identity string `json:"identity"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Room == "" || req.Identity == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.Hub.Kick(req.Room, req.Identity, "web:"+userFrom(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

type userView struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

func toUserView(u store.User) userView {
	return userView{Username: u.Username, Role: u.Role, CreatedAt: u.CreatedAt}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		slog.Error("list users", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, toUserView(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func validUsername(name string) bool {
	if name == "" || len([]rune(name)) > 32 || strings.TrimSpace(name) != name {
		return false
	}
	return !strings.ContainsAny(name, ": \t\r\n")
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !validUsername(req.Username) {
		writeErr(w, http.StatusBadRequest, "invalid username")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.Store.CreateUser(r.Context(), req.Username, hash, signal.RoleUser); err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, "user exists")
			return
		}
		slog.Error("create user", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	u, err := s.Store.GetUser(r.Context(), req.Username)
	if err != nil {
		slog.Error("get created user", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toUserView(*u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("username")
	if name == "" || name == userFrom(r) {
		writeErr(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	u, err := s.Store.GetUser(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if u.Role == signal.RoleAdmin {
		writeErr(w, http.StatusForbidden, "cannot delete admin")
		return
	}
	if err := s.Store.DeleteUser(r.Context(), name); err != nil {
		slog.Error("delete user", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Hub.EndUserSessions(name, signal.ReasonDeleted)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

const maxVolume = 200

func volumeOK(v *int) bool {
	return v == nil || (*v >= 0 && *v <= maxVolume)
}

func (s *Server) setVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Speaker *int `json:"speaker"`
		Mic     *int `json:"mic"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil ||
		(req.Speaker == nil && req.Mic == nil) || !volumeOK(req.Speaker) || !volumeOK(req.Mic) {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.Hub.SetDeviceVolume(r.PathValue("id"), req.Speaker, req.Mic); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// wsWeb 浏览器的 WebSocket 不能自定义请求头，会话令牌放在查询参数里。
func (s *Server) wsWeb(w http.ResponseWriter, r *http.Request) {
	u, err := s.authenticate(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	peer := signal.NewPeer(signal.KindWeb, u.Username, u.Username)
	peer.Role = u.Role
	s.Hub.Serve(w, r, peer, s.OriginPatterns)
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
	mu    sync.Mutex
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
