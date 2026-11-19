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

func setup(t *testing.T) (*httptest.Server, string) {
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
	return ts, secret
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
	ts, secret := setup(t)
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

	_ = wsjson.Write(ctx, web, signal.Message{Type: signal.TypeCall, To: "device:pi1"})
	out := read(t, web, signal.TypeOutgoing)
	inv := read(t, dev, signal.TypeInvite)
	if inv.Token == "" || inv.CallID != out.CallID {
		t.Fatalf("invite %+v", inv)
	}
	_ = wsjson.Write(ctx, dev, signal.Message{Type: signal.TypeAccept, CallID: inv.CallID})
	acc := read(t, web, signal.TypeAccepted)
	if acc.Token == "" || acc.URL != "wss://example" {
		t.Fatalf("accepted %+v", acc)
	}
	read(t, dev, signal.TypeAccepted)

	_ = wsjson.Write(ctx, dev, signal.Message{Type: signal.TypeHangup, CallID: inv.CallID})
	read(t, web, signal.TypeHangup)

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

func TestUnauthorized(t *testing.T) {
	ts, _ := setup(t)
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
