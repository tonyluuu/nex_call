package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"nexcall/server/internal/auth"
	"nexcall/server/internal/signal"
	"nexcall/server/internal/store"
)

type dir struct{ st *store.Store }

func (d dir) Devices(ctx context.Context) ([]signal.DeviceStatus, error) {
	list, err := d.st.ListDevices(ctx)
	var out []signal.DeviceStatus
	for _, x := range list {
		out = append(out, signal.DeviceStatus{ID: x.ID, Name: x.Name})
	}
	return out, err
}
func (d dir) Touch(ctx context.Context, id string, t time.Time) { _ = d.st.TouchDevice(ctx, id, t) }

func setup(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	hash, _ := auth.HashPassword("password123")
	if err := st.UpsertUser(context.Background(), "alice", hash); err != nil {
		t.Fatal(err)
	}
	secret, shash, _ := auth.NewDeviceSecret()
	if err := st.UpsertDevice(context.Background(), "pi1", "客厅", shash); err != nil {
		t.Fatal(err)
	}

	lk := &auth.LiveKit{URL: "wss://example", APIKey: "APIkey", APISecret: "secretsecretsecretsecretsecretsecret"}
	srv := &Server{
		Store:      st,
		Hub:        signal.NewHub(lk, st, dir{st}, lk.URL, time.Minute),
		Sessions:   auth.NewSessions("test-session-secret", time.Hour),
		LiveKitURL: lk.URL,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, secret
}

func login(t *testing.T, base, user, pw string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	resp, err := http.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out["token"]
}

func read(t *testing.T, c *websocket.Conn, typ string) signal.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		var m signal.Message
		if err := wsjson.Read(ctx, c, &m); err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		if m.Type == signal.TypePresence && typ != signal.TypePresence {
			continue
		}
		if m.Type != typ {
			t.Fatalf("got %+v want %s", m, typ)
		}
		return m
	}
}

func TestLoginAndCallFlow(t *testing.T) {
	ts, _, secret := setup(t)
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx := context.Background()

	if code, _ := login(t, ts.URL, "alice", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", code)
	}
	code, token := login(t, ts.URL, "alice", "password123")
	if code != http.StatusOK || token == "" {
		t.Fatalf("login: %d", code)
	}

	if _, resp, err := websocket.Dial(ctx, wsBase+"/ws/device", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer pi1:wrong"}},
	}); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("device with bad secret should be rejected, err=%v", err)
	}

	dev, _, err := websocket.Dial(ctx, wsBase+"/ws/device", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer pi1:" + secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.CloseNow()
	if m := read(t, dev, signal.TypeRegistered); m.Self != "device:pi1" {
		t.Fatalf("self %q", m.Self)
	}

	web, _, err := websocket.Dial(ctx, wsBase+"/ws/web?token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer web.CloseNow()
	read(t, web, signal.TypeRegistered)
	p := read(t, web, signal.TypePresence)
	if len(p.Devices) != 1 || !p.Devices[0].Online || p.Devices[0].Name != "客厅" {
		t.Fatalf("presence %+v", p)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("devices: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()

	read(t, dev, signal.TypeJoined)

	_ = wsjson.Write(ctx, web, signal.Message{Type: signal.TypeJoin, Room: "device:pi1"})
	joined := read(t, web, signal.TypeJoined)
	if joined.Token == "" || joined.URL != "wss://example" || joined.Room != signal.DeviceRoom("pi1") {
		t.Fatalf("joined %+v", joined)
	}
	_ = wsjson.Write(ctx, web, signal.Message{Type: signal.TypeLeave})
	if m := read(t, web, signal.TypeLeft); m.Reason != signal.ReasonNormal {
		t.Fatalf("left %+v", m)
	}

	time.Sleep(50 * time.Millisecond)
	req, _ = http.NewRequest("GET", ts.URL+"/api/calls", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var calls []store.CallRecord
	_ = json.NewDecoder(resp.Body).Decode(&calls)
	if len(calls) != 1 || calls[0].AnsweredAt == nil || calls[0].EndReason != signal.ReasonNormal {
		t.Fatalf("calls %+v", calls)
	}
}

func TestAdminKickAndCallVisibility(t *testing.T) {
	ts, st, secret := setup(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("password123")
	if err := st.UpsertUser(ctx, "root", hash); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRole(ctx, "root", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(ctx, "bob", hash); err != nil {
		t.Fatal(err)
	}

	_, aliceToken := login(t, ts.URL, "alice", "password123")
	_, rootToken := login(t, ts.URL, "root", "password123")
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	dev, _, err := websocket.Dial(ctx, wsBase+"/ws/device", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer pi1:" + secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.CloseNow()
	read(t, dev, signal.TypeRegistered)
	read(t, dev, signal.TypeJoined)

	alice, _, err := websocket.Dial(ctx, wsBase+"/ws/web?token="+aliceToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.CloseNow()
	read(t, alice, signal.TypeRegistered)

	body, _ := json.Marshal(map[string]string{"room": signal.DeviceRoom("pi1"), "identity": "web:alice"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/kick", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user kick status %d", resp.StatusCode)
	}

	_ = wsjson.Write(ctx, alice, signal.Message{Type: signal.TypeJoin, Room: "device:pi1"})
	read(t, alice, signal.TypeJoined)

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/admin/kick", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rootToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin kick status %d", resp.StatusCode)
	}
	if m := read(t, alice, signal.TypeKicked); m.Reason != signal.ReasonKicked {
		t.Fatalf("kicked %+v", m)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/api/calls", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var aliceCalls []store.CallRecord
	_ = json.NewDecoder(resp.Body).Decode(&aliceCalls)
	resp.Body.Close()

	_, bobToken := login(t, ts.URL, "bob", "password123")
	req, _ = http.NewRequest("GET", ts.URL+"/api/calls", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var bobCalls []store.CallRecord
	_ = json.NewDecoder(resp.Body).Decode(&bobCalls)
	resp.Body.Close()
	if len(aliceCalls) != 1 || len(bobCalls) != 0 {
		t.Fatalf("alice %d bob %d", len(aliceCalls), len(bobCalls))
	}
}

func authed(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginReplacesPreviousSession(t *testing.T) {
	ts, _, _ := setup(t)
	_, token1 := login(t, ts.URL, "alice", "password123")
	ctx := context.Background()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	web, _, err := websocket.Dial(ctx, wsBase+"/ws/web?token="+token1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer web.CloseNow()
	read(t, web, signal.TypeRegistered)

	_, token2 := login(t, ts.URL, "alice", "password123")
	if token2 == "" || token2 == token1 {
		t.Fatal("expected a new token")
	}
	if m := read(t, web, signal.TypeSessionEnded); m.Reason != signal.ReasonReplaced {
		t.Fatalf("session ended %+v", m)
	}

	resp := authed(t, http.MethodGet, ts.URL+"/api/me", token1, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token status %d", resp.StatusCode)
	}
	resp = authed(t, http.MethodGet, ts.URL+"/api/me", token2, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new token status %d", resp.StatusCode)
	}
}

func TestAdminUsersAndVolume(t *testing.T) {
	ts, st, secret := setup(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("password123")
	if err := st.UpsertUser(ctx, "root", hash); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRole(ctx, "root", "admin"); err != nil {
		t.Fatal(err)
	}
	_, alice := login(t, ts.URL, "alice", "password123")
	_, root := login(t, ts.URL, "root", "password123")

	resp := authed(t, http.MethodPost, ts.URL+"/api/admin/users", alice, `{"username":"cara","password":"password123"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user create status %d", resp.StatusCode)
	}

	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/users", root, `{"username":"cara","password":"short"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password status %d", resp.StatusCode)
	}
	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/users", root, `{"username":"bad name","password":"password123"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad username status %d", resp.StatusCode)
	}

	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/users", root, `{"username":"cara","password":"password123"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/users", root, `{"username":"cara","password":"password123"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status %d", resp.StatusCode)
	}

	resp = authed(t, http.MethodGet, ts.URL+"/api/admin/users", root, "")
	var users []struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, u := range users {
		if u.Username == "cara" && u.Role == "user" {
			found = true
		}
		if strings.Contains(u.Username, "hash") {
			t.Fatal("password hash leaked")
		}
	}
	if !found {
		t.Fatalf("users %+v", users)
	}

	_, cara := login(t, ts.URL, "cara", "password123")
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	caraWS, _, err := websocket.Dial(ctx, wsBase+"/ws/web?token="+cara, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer caraWS.CloseNow()
	read(t, caraWS, signal.TypeRegistered)

	resp = authed(t, http.MethodDelete, ts.URL+"/api/admin/users/root", root, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete self status %d", resp.StatusCode)
	}
	resp = authed(t, http.MethodDelete, ts.URL+"/api/admin/users/root", alice, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete admin by user status %d", resp.StatusCode)
	}
	if err := st.UpsertUser(ctx, "boss", hash); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRole(ctx, "boss", "admin"); err != nil {
		t.Fatal(err)
	}
	resp = authed(t, http.MethodDelete, ts.URL+"/api/admin/users/boss", root, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete other admin status %d", resp.StatusCode)
	}

	resp = authed(t, http.MethodDelete, ts.URL+"/api/admin/users/cara", root, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	if m := read(t, caraWS, signal.TypeSessionEnded); m.Reason != signal.ReasonDeleted {
		t.Fatalf("deleted session %+v", m)
	}
	resp = authed(t, http.MethodGet, ts.URL+"/api/me", cara, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deleted token status %d", resp.StatusCode)
	}

	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/devices/pi1/volume", root, `{"speaker":250}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("volume range status %d", resp.StatusCode)
	}
	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/devices/pi1/volume", root, `{"speaker":80}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("offline volume status %d", resp.StatusCode)
	}

	dev, _, err := websocket.Dial(ctx, wsBase+"/ws/device", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer pi1:" + secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.CloseNow()
	read(t, dev, signal.TypeRegistered)
	read(t, dev, signal.TypeJoined)
	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/devices/pi1/volume", root, `{"speaker":0,"mic":200}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volume status %d", resp.StatusCode)
	}
	m := read(t, dev, signal.TypeSetVolume)
	if m.Speaker == nil || m.Mic == nil || *m.Speaker != 0 || *m.Mic != 200 {
		t.Fatalf("set_volume %+v", m)
	}
	resp = authed(t, http.MethodPost, ts.URL+"/api/admin/devices/pi1/volume", alice, `{"speaker":50}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user volume status %d", resp.StatusCode)
	}
}

func TestUnauthorized(t *testing.T) {
	ts, _, _ := setup(t)
	resp, err := http.Get(ts.URL + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/ws/web?token=forged.token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws status %d", resp.StatusCode)
	}
}
