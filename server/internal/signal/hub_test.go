package signal

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeTokens struct{}

func (fakeTokens) RoomToken(room, identity, name string) (string, error) {
	return "tok:" + room + ":" + identity, nil
}

type fakeRecorder struct {
	mu    sync.Mutex
	ended map[string]string
}

func (r *fakeRecorder) CallStarted(context.Context, string, string, string, time.Time) error {
	return nil
}
func (r *fakeRecorder) CallAnswered(context.Context, string, string, time.Time) error { return nil }
func (r *fakeRecorder) CallEnded(_ context.Context, id, reason string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended == nil {
		r.ended = map[string]string{}
	}
	r.ended[id] = reason
	return nil
}

type fakeDir struct{ ids []string }

func (d fakeDir) Devices(context.Context) ([]DeviceStatus, error) {
	var out []DeviceStatus
	for _, id := range d.ids {
		out = append(out, DeviceStatus{ID: id, Name: id})
	}
	return out, nil
}
func (fakeDir) Touch(context.Context, string, time.Time) {}

func newTestHub(ring time.Duration) (*Hub, *fakeRecorder) {
	rec := &fakeRecorder{}
	return NewHub(fakeTokens{}, rec, fakeDir{ids: []string{"pi1", "pi2"}}, "wss://test", ring), rec
}

// expect 读取下一条非 presence 消息。
func expect(t *testing.T, p *Peer, typ string) Message {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case m := <-p.send:
			if m.Type == TypePresence {
				continue
			}
			if m.Type != typ {
				t.Fatalf("%s: got %+v, want type %s", p.Address(), m, typ)
			}
			return m
		case <-timeout:
			t.Fatalf("%s: timeout waiting for %s", p.Address(), typ)
		}
	}
}

func expectNone(t *testing.T, p *Peer) {
	t.Helper()
	for {
		select {
		case m := <-p.send:
			if m.Type == TypePresence {
				continue
			}
			t.Fatalf("%s: unexpected %+v", p.Address(), m)
		default:
			return
		}
	}
}

func register(t *testing.T, h *Hub, kind, id string) *Peer {
	t.Helper()
	p := NewPeer(kind, id, id)
	h.Register(p)
	expect(t, p, TypeRegistered)
	return p
}

func TestWebCallsDeviceAcceptAndHangup(t *testing.T) {
	h, rec := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	web := register(t, h, KindWeb, "alice")

	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	out := expect(t, web, TypeOutgoing)
	inv := expect(t, dev, TypeInvite)
	if inv.CallID != out.CallID || inv.Token != "tok:"+inv.Room+":device:pi1" || inv.From != "web:alice" || inv.URL != "wss://test" {
		t.Fatalf("bad invite %+v", inv)
	}

	h.Handle(dev, Message{Type: TypeRinging, CallID: inv.CallID})
	expect(t, web, TypeRinging)

	h.Handle(dev, Message{Type: TypeAccept, CallID: inv.CallID})
	acc := expect(t, web, TypeAccepted)
	if acc.Token != "tok:"+inv.Room+":web:alice" || acc.Peer != "device:pi1" {
		t.Fatalf("bad accepted %+v", acc)
	}
	expect(t, dev, TypeAccepted)

	if s := h.Snapshot(); !s[0].Online || !s[0].Busy || s[1].Online {
		t.Fatalf("snapshot %+v", s)
	}

	h.Handle(web, Message{Type: TypeHangup, CallID: inv.CallID})
	if m := expect(t, dev, TypeHangup); m.Reason != ReasonNormal {
		t.Fatalf("reason %q", m.Reason)
	}
	expectNone(t, web)
	if rec.ended[inv.CallID] != ReasonNormal {
		t.Fatalf("recorded %q", rec.ended[inv.CallID])
	}
	if len(h.calls) != 0 || len(h.peerCalls) != 0 {
		t.Fatalf("leaked state: calls=%d peerCalls=%d", len(h.calls), len(h.peerCalls))
	}
}

func TestDeviceCallsAllWebAnsweredElsewhere(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	tab1 := register(t, h, KindWeb, "alice")
	tab2 := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(dev, Message{Type: TypeCall})
	out := expect(t, dev, TypeOutgoing)
	if out.To != AllWeb {
		t.Fatalf("to = %q", out.To)
	}
	for _, p := range []*Peer{tab1, tab2, bob} {
		expect(t, p, TypeInvite)
	}

	h.Handle(tab2, Message{Type: TypeAccept, CallID: out.CallID})
	if m := expect(t, dev, TypeAccepted); m.Peer != "web:alice" {
		t.Fatalf("peer %q", m.Peer)
	}
	expect(t, tab2, TypeAccepted)
	for _, p := range []*Peer{tab1, bob} {
		if m := expect(t, p, TypeHangup); m.Reason != ReasonElsewhere {
			t.Fatalf("reason %q", m.Reason)
		}
	}

	h.Handle(dev, Message{Type: TypeHangup, CallID: out.CallID})
	expect(t, tab2, TypeHangup)
	expectNone(t, tab1)
}

func TestRingTimeout(t *testing.T) {
	h, rec := newTestHub(50 * time.Millisecond)
	dev := register(t, h, KindDevice, "pi1")
	web := register(t, h, KindWeb, "alice")

	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	out := expect(t, web, TypeOutgoing)
	expect(t, dev, TypeInvite)
	if m := expect(t, web, TypeHangup); m.Reason != ReasonTimeout {
		t.Fatalf("reason %q", m.Reason)
	}
	if m := expect(t, dev, TypeHangup); m.Reason != ReasonTimeout {
		t.Fatalf("reason %q", m.Reason)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.ended[out.CallID] != ReasonTimeout {
		t.Fatalf("recorded %q", rec.ended[out.CallID])
	}
}

func TestRejectAndCancel(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	web := register(t, h, KindWeb, "alice")

	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	expect(t, web, TypeOutgoing)
	inv := expect(t, dev, TypeInvite)
	h.Handle(dev, Message{Type: TypeReject, CallID: inv.CallID})
	if m := expect(t, web, TypeHangup); m.Reason != ReasonRejected {
		t.Fatalf("reason %q", m.Reason)
	}

	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	out := expect(t, web, TypeOutgoing)
	expect(t, dev, TypeInvite)
	h.Handle(web, Message{Type: TypeHangup, CallID: out.CallID})
	if m := expect(t, dev, TypeHangup); m.Reason != ReasonCancelled {
		t.Fatalf("reason %q", m.Reason)
	}
}

func TestOfflineAndBusy(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(alice, Message{Type: TypeCall, To: "device:pi2"})
	if m := expect(t, alice, TypeHangup); m.Reason != ReasonOffline {
		t.Fatalf("reason %q", m.Reason)
	}

	dev := register(t, h, KindDevice, "pi1")
	h.Handle(alice, Message{Type: TypeCall, To: "device:pi1"})
	expect(t, alice, TypeOutgoing)
	expect(t, dev, TypeInvite)

	h.Handle(bob, Message{Type: TypeCall, To: "device:pi1"})
	if m := expect(t, bob, TypeHangup); m.Reason != ReasonBusy {
		t.Fatalf("reason %q", m.Reason)
	}

	h.Handle(alice, Message{Type: TypeCall, To: "device:pi1"})
	expect(t, alice, TypeError)
}

func TestDisconnectDuringCall(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	web := register(t, h, KindWeb, "alice")

	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	expect(t, web, TypeOutgoing)
	inv := expect(t, dev, TypeInvite)
	h.Handle(dev, Message{Type: TypeAccept, CallID: inv.CallID})
	expect(t, web, TypeAccepted)
	expect(t, dev, TypeAccepted)

	h.Unregister(dev)
	if m := expect(t, web, TypeHangup); m.Reason != ReasonDisconnected {
		t.Fatalf("reason %q", m.Reason)
	}
	if len(h.peerCalls) != 0 {
		t.Fatalf("leaked peerCalls %d", len(h.peerCalls))
	}
}

func TestInvitedTabClosingKeepsCallUntilLast(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	tab1 := register(t, h, KindWeb, "alice")
	tab2 := register(t, h, KindWeb, "alice")

	h.Handle(dev, Message{Type: TypeCall, To: "web:alice"})
	expect(t, dev, TypeOutgoing)
	expect(t, tab1, TypeInvite)
	expect(t, tab2, TypeInvite)

	h.Unregister(tab1)
	expectNone(t, dev)
	h.Unregister(tab2)
	if m := expect(t, dev, TypeHangup); m.Reason != ReasonDisconnected {
		t.Fatalf("reason %q", m.Reason)
	}
}

func TestDeviceReconnectReplacesOld(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	old := register(t, h, KindDevice, "pi1")
	web := register(t, h, KindWeb, "alice")
	h.Handle(web, Message{Type: TypeCall, To: "device:pi1"})
	expect(t, web, TypeOutgoing)
	expect(t, old, TypeInvite)

	register(t, h, KindDevice, "pi1")
	select {
	case <-old.Done():
	default:
		t.Fatal("old connection not closed")
	}
	if m := expect(t, web, TypeHangup); m.Reason != ReasonDisconnected {
		t.Fatalf("reason %q", m.Reason)
	}
	if !h.Snapshot()[0].Online {
		t.Fatal("device should still be online via new connection")
	}
}

func TestStrangerCannotTouchCall(t *testing.T) {
	h, _ := newTestHub(time.Minute)
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(alice, Message{Type: TypeCall, To: "device:pi1"})
	out := expect(t, alice, TypeOutgoing)
	expect(t, dev, TypeInvite)

	h.Handle(bob, Message{Type: TypeAccept, CallID: out.CallID})
	expect(t, bob, TypeError)
	h.Handle(bob, Message{Type: TypeHangup, CallID: out.CallID})
	expect(t, bob, TypeError)
	expectNone(t, alice)
}
