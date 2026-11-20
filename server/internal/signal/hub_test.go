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
	mu     sync.Mutex
	opened int
	closed map[string]string
	joined []string
	left   []string
}

func (r *fakeRecorder) RoomOpened(context.Context, string, string, string, time.Time) error {
	r.mu.Lock()
	r.opened++
	r.mu.Unlock()
	return nil
}
func (r *fakeRecorder) ParticipantJoined(_ context.Context, _, identity string, _ time.Time) error {
	r.mu.Lock()
	r.joined = append(r.joined, identity)
	r.mu.Unlock()
	return nil
}
func (r *fakeRecorder) ParticipantLeft(_ context.Context, _, identity, reason string, _ time.Time) error {
	r.mu.Lock()
	r.left = append(r.left, identity+":"+reason)
	r.mu.Unlock()
	return nil
}
func (r *fakeRecorder) RoomClosed(_ context.Context, id, reason string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed == nil {
		r.closed = map[string]string{}
	}
	r.closed[id] = reason
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

type fakeRemove struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (f *fakeRemove) RemoveParticipant(_ context.Context, room, identity string) error {
	f.mu.Lock()
	f.got = append(f.got, room+" "+identity)
	f.mu.Unlock()
	return f.fail
}

func newTestHub() (*Hub, *fakeRecorder) {
	rec := &fakeRecorder{}
	return NewHub(fakeTokens{}, rec, fakeDir{ids: []string{"pi1", "pi2"}}, "wss://test", time.Minute), rec
}

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
	if kind == KindDevice {
		j := expect(t, p, TypeJoined)
		if j.Room != DeviceRoom(id) || j.Token == "" {
			t.Fatalf("device join %+v", j)
		}
	}
	return p
}

func TestDeviceRoomStaysAndWebsComeAndGo(t *testing.T) {
	h, rec := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	aj := expect(t, alice, TypeJoined)
	if aj.Room != DeviceRoom("pi1") || aj.Token != "tok:"+DeviceRoom("pi1")+":web:alice" {
		t.Fatalf("alice joined %+v", aj)
	}
	expectNone(t, dev)

	h.Handle(bob, Message{Type: TypeJoin, Room: DeviceRoom("pi1")})
	expect(t, bob, TypeJoined)

	if s := h.Snapshot(); !s[0].Online || !s[0].Busy {
		t.Fatalf("snapshot %+v", s)
	}

	h.Handle(alice, Message{Type: TypeLeave})
	expect(t, alice, TypeLeft)
	expectNone(t, bob)

	h.Handle(bob, Message{Type: TypeLeave})
	expect(t, bob, TypeLeft)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.opened != 1 || len(rec.closed) != 1 {
		t.Fatalf("records opened=%d closed=%v", rec.opened, rec.closed)
	}
}

func TestWebCallAutoInviteAndThirdJoin(t *testing.T) {
	h, rec := newTestHub()
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")
	cara := register(t, h, KindWeb, "cara")

	h.Handle(alice, Message{Type: TypeCall, To: "web:bob"})
	joined := expect(t, alice, TypeJoined)
	inv := expect(t, bob, TypeInvite)
	if inv.Room != joined.Room || inv.From != "web:alice" {
		t.Fatalf("invite %+v joined %+v", inv, joined)
	}
	expectNone(t, cara)

	h.Handle(bob, Message{Type: TypeJoin, Room: inv.Room})
	if bj := expect(t, bob, TypeJoined); bj.Room != inv.Room {
		t.Fatalf("bob %+v", bj)
	}

	h.Handle(cara, Message{Type: TypeJoin, Room: inv.Room})
	expect(t, cara, TypeJoined)

	h.Handle(bob, Message{Type: TypeLeave})
	expect(t, bob, TypeLeft)
	expectNone(t, alice)
	expectNone(t, cara)

	h.Handle(cara, Message{Type: TypeLeave})
	expect(t, cara, TypeLeft)
	if m := expect(t, alice, TypeLeft); m.Reason != ReasonPeerLeft {
		t.Fatalf("alice left %+v", m)
	}

	h.Handle(cara, Message{Type: TypeJoin, Room: inv.Room})
	if m := expect(t, cara, TypeError); m.Reason != "no such room" {
		t.Fatalf("rejoin %+v", m)
	}

	rec.mu.Lock()
	if rec.opened != 1 || len(rec.closed) != 1 {
		t.Fatalf("records %+v", rec.closed)
	}
	rec.mu.Unlock()
}

func TestCallBusyAndOffline(t *testing.T) {
	h, _ := newTestHub()
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")
	h.Handle(bob, Message{Type: TypeJoin, Room: "device:pi1"})
	expect(t, bob, TypeJoined)

	h.Handle(alice, Message{Type: TypeCall, To: "web:bob"})
	if m := expect(t, alice, TypeError); m.Reason != ReasonBusy {
		t.Fatalf("got %+v", m)
	}
	h.Handle(alice, Message{Type: TypeCall, To: "web:nobody"})
	if m := expect(t, alice, TypeError); m.Reason != ReasonOffline {
		t.Fatalf("got %+v", m)
	}
}

func TestKickRemovesWebNotDevice(t *testing.T) {
	h, _ := newTestHub()
	rem := &fakeRemove{}
	h.SetRemover(rem)
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	admin := register(t, h, KindWeb, "root")
	admin.Role = RoleAdmin

	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	room := expect(t, alice, TypeJoined).Room

	h.Handle(alice, Message{Type: TypeKick, Room: room, Peer: "web:alice"})
	if m := expect(t, alice, TypeError); m.Reason != "forbidden" {
		t.Fatalf("non-admin %+v", m)
	}

	h.Handle(admin, Message{Type: TypeKick, Room: room, Peer: "device:pi1"})
	if m := expect(t, admin, TypeError); m.Reason != "cannot kick device" {
		t.Fatalf("kick device %+v", m)
	}
	expectNone(t, dev)

	if err := h.Kick(room, "web:alice", "web:root"); err != nil {
		t.Fatal(err)
	}
	k := expect(t, alice, TypeKicked)
	if k.Room != room || k.Reason != ReasonKicked {
		t.Fatalf("kicked %+v", k)
	}
	if len(rem.got) != 1 || rem.got[0] != room+" web:alice" {
		t.Fatalf("remover %+v", rem.got)
	}
}

func TestDisconnectClearsMembership(t *testing.T) {
	h, rec := newTestHub()
	alice := register(t, h, KindWeb, "alice")
	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	expect(t, alice, TypeJoined)
	h.Unregister(alice)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.closed) != 1 {
		t.Fatalf("closed %+v", rec.closed)
	}
	for _, reason := range rec.closed {
		if reason != ReasonDisconnected {
			t.Fatalf("reason %s", reason)
		}
	}
}

func TestNotifyTargets(t *testing.T) {
	h, _ := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(dev, Message{Type: TypeNotify, To: "web:alice"})
	n := expect(t, alice, TypeNotify)
	if n.From != "device:pi1" || n.Room != DeviceRoom("pi1") {
		t.Fatalf("notify %+v", n)
	}
	expectNone(t, bob)

	h.Handle(dev, Message{Type: TypeNotify})
	expect(t, alice, TypeNotify)
	expect(t, bob, TypeNotify)
}

func TestPeerLeaveEndsCallForTheOther(t *testing.T) {
	h, rec := newTestHub()
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(alice, Message{Type: TypeCall, To: "web:bob"})
	room := expect(t, alice, TypeJoined).Room
	expect(t, bob, TypeInvite)
	h.Handle(bob, Message{Type: TypeJoin, Room: room})
	expect(t, bob, TypeJoined)

	h.Handle(alice, Message{Type: TypeLeave})
	if m := expect(t, alice, TypeLeft); m.Reason != ReasonNormal {
		t.Fatalf("alice %+v", m)
	}
	if m := expect(t, bob, TypeLeft); m.Reason != ReasonPeerLeft || m.Room != room {
		t.Fatalf("bob %+v", m)
	}
	for _, r := range h.presence().Rooms {
		if r.ID == room {
			t.Fatalf("room still listed %+v", r)
		}
	}
	h.Handle(bob, Message{Type: TypeJoin, Room: room})
	if m := expect(t, bob, TypeError); m.Reason != "no such room" {
		t.Fatalf("rejoin %+v", m)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.opened != 1 || len(rec.closed) != 1 {
		t.Fatalf("records opened=%d closed=%v", rec.opened, rec.closed)
	}
	for _, reason := range rec.closed {
		if reason != ReasonPeerLeft {
			t.Fatalf("close reason %s", reason)
		}
	}
}

func TestThreePartyLeaveKeepsTheRest(t *testing.T) {
	h, _ := newTestHub()
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")
	cara := register(t, h, KindWeb, "cara")

	h.Handle(alice, Message{Type: TypeCall, To: "web:bob"})
	room := expect(t, alice, TypeJoined).Room
	expect(t, bob, TypeInvite)
	h.Handle(bob, Message{Type: TypeJoin, Room: room})
	expect(t, bob, TypeJoined)
	h.Handle(cara, Message{Type: TypeJoin, Room: room})
	expect(t, cara, TypeJoined)

	h.Handle(alice, Message{Type: TypeLeave})
	expect(t, alice, TypeLeft)
	expectNone(t, bob)
	expectNone(t, cara)

	h.Handle(bob, Message{Type: TypeLeave})
	expect(t, bob, TypeLeft)
	if m := expect(t, cara, TypeLeft); m.Reason != ReasonPeerLeft {
		t.Fatalf("cara %+v", m)
	}
}

func TestWebLeaveKeepsDeviceInRoom(t *testing.T) {
	h, _ := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")

	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	expect(t, alice, TypeJoined)
	h.Handle(alice, Message{Type: TypeLeave})
	if m := expect(t, alice, TypeLeft); m.Reason != ReasonNormal {
		t.Fatalf("alice %+v", m)
	}
	expectNone(t, dev)
	if h.peerRoom[dev] != DeviceRoom("pi1") {
		t.Fatal("device left its room")
	}
	for _, r := range h.presence().Rooms {
		for _, m := range r.Members {
			if m.Kind == KindWeb {
				t.Fatalf("web still listed %+v", r)
			}
		}
	}
	if s := h.Snapshot(); s[0].ID == "pi1" && s[0].Busy {
		t.Fatal("device still busy")
	}
}

func TestDeviceDisconnectEndsWebCall(t *testing.T) {
	h, _ := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	expect(t, alice, TypeJoined)

	h.Unregister(dev)
	if m := expect(t, alice, TypeLeft); m.Reason != ReasonPeerLeft {
		t.Fatalf("alice %+v", m)
	}
}

func TestWaitingTimeout(t *testing.T) {
	rec := &fakeRecorder{}
	h := NewHub(fakeTokens{}, rec, fakeDir{ids: []string{"pi1"}}, "wss://test", 40*time.Millisecond)
	alice := register(t, h, KindWeb, "alice")
	bob := register(t, h, KindWeb, "bob")

	h.Handle(alice, Message{Type: TypeCall, To: "web:bob"})
	room := expect(t, alice, TypeJoined).Room
	expect(t, bob, TypeInvite)

	if m := expect(t, alice, TypeLeft); m.Reason != ReasonTimeout || m.Room != room {
		t.Fatalf("timeout %+v", m)
	}
	h.Handle(bob, Message{Type: TypeJoin, Room: room})
	if m := expect(t, bob, TypeError); m.Reason != "no such room" {
		t.Fatalf("join after timeout %+v", m)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, reason := range rec.closed {
		if reason != ReasonTimeout {
			t.Fatalf("close reason %s", reason)
		}
	}
}

func TestEndUserSessionLeavesRoom(t *testing.T) {
	h, rec := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	h.Handle(alice, Message{Type: TypeJoin, Room: "device:pi1"})
	expect(t, alice, TypeJoined)

	h.EndUserSessions("alice", ReasonReplaced)
	if m := expect(t, alice, TypeSessionEnded); m.Reason != ReasonReplaced {
		t.Fatalf("ended %+v", m)
	}
	if m := expect(t, alice, TypeLeft); m.Reason != ReasonReplaced {
		t.Fatalf("left %+v", m)
	}
	select {
	case <-alice.Done():
	case <-time.After(time.Second):
		t.Fatal("connection not closed")
	}
	expectNone(t, dev)
	if h.peerRoom[dev] != DeviceRoom("pi1") {
		t.Fatal("device left its room")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.closed) != 1 {
		t.Fatalf("closed %+v", rec.closed)
	}
	for _, reason := range rec.closed {
		if reason != ReasonReplaced {
			t.Fatalf("close reason %s", reason)
		}
	}
}

func TestDeviceVolumeInPresence(t *testing.T) {
	h, _ := newTestHub()
	dev := register(t, h, KindDevice, "pi1")
	alice := register(t, h, KindWeb, "alice")
	speaker, mic := 80, 150
	h.Handle(dev, Message{Type: TypeVolume, Speaker: &speaker, Mic: &mic})

	deadline := time.After(time.Second)
	for {
		select {
		case m := <-alice.send:
			if m.Type != TypePresence {
				continue
			}
			if len(m.Devices) == 0 || m.Devices[0].Speaker == nil || m.Devices[0].Mic == nil {
				continue
			}
			if *m.Devices[0].Speaker != 80 || *m.Devices[0].Mic != 150 || !m.Devices[0].Online {
				t.Fatalf("presence %+v", m.Devices[0])
			}
			return
		case <-deadline:
			t.Fatal("volume did not show up in presence")
		}
	}
}
