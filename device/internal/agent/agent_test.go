package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nexcall/device/internal/gpio"
	"nexcall/device/internal/signal"
)

type fakeSig struct {
	mu   sync.Mutex
	sent []signal.Message
}

func (f *fakeSig) Send(m signal.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSig) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		out = append(out, m.Type)
	}
	return out
}

type fakeMedia struct {
	peerLeft, done chan struct{}
	closed         chan struct{}
	once           sync.Once
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{peerLeft: make(chan struct{}), done: make(chan struct{}), closed: make(chan struct{})}
}
func (m *fakeMedia) PeerLeft() <-chan struct{} { return m.peerLeft }
func (m *fakeMedia) Done() <-chan struct{}     { return m.done }
func (m *fakeMedia) Close()                    { m.once.Do(func() { close(m.closed) }) }

type fakeRinger struct {
	mu      sync.Mutex
	ringing bool
}

func (r *fakeRinger) Start(bool) { r.mu.Lock(); r.ringing = true; r.mu.Unlock() }
func (r *fakeRinger) Stop()      { r.mu.Lock(); r.ringing = false; r.mu.Unlock() }
func (r *fakeRinger) on() bool   { r.mu.Lock(); defer r.mu.Unlock(); return r.ringing }

type nopInd struct{}

func (nopInd) SetPattern(gpio.Pattern) {}

type harness struct {
	t       *testing.T
	sig     *fakeSig
	ringer  *fakeRinger
	msgs    chan signal.Message
	presses chan gpio.Press
	states  chan State
	media   *fakeMedia
	joinErr error
	cancel  context.CancelFunc
}

func start(t *testing.T, cfg Config) *harness {
	h := &harness{
		t:       t,
		sig:     &fakeSig{},
		ringer:  &fakeRinger{},
		msgs:    make(chan signal.Message),
		presses: make(chan gpio.Press),
		states:  make(chan State, 32),
		media:   newFakeMedia(),
	}
	join := func(url, token string) (Media, error) {
		if h.joinErr != nil {
			return nil, h.joinErr
		}
		return h.media, nil
	}
	a := New(cfg, h.sig, join, h.ringer, nopInd{})
	a.OnState = func(s State) { h.states <- s }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)
	go a.Run(ctx, h.msgs, h.presses)
	h.msgs <- signal.Message{Type: signal.EventConnected}
	return h
}

func (h *harness) wait(want State) {
	h.t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case s := <-h.states:
			if s == want {
				return
			}
		case <-timeout:
			h.t.Fatalf("timeout waiting for state %s", want)
		}
	}
}

func (h *harness) lastSent() signal.Message {
	h.sig.mu.Lock()
	defer h.sig.mu.Unlock()
	return h.sig.sent[len(h.sig.sent)-1]
}

func TestIncomingAcceptAndRemoteHangup(t *testing.T) {
	h := start(t, Config{})
	h.msgs <- signal.Message{Type: "invite", CallID: "c1", From: "web:alice"}
	h.wait(Incoming)
	if !h.ringer.on() {
		t.Fatal("should ring")
	}
	h.presses <- gpio.Short
	h.wait(Connecting)
	if h.ringer.on() {
		t.Fatal("ringer should stop after accept")
	}
	h.msgs <- signal.Message{Type: "accepted", CallID: "c1", URL: "wss://x", Token: "t"}
	h.wait(Active)
	h.msgs <- signal.Message{Type: "hangup", CallID: "c1", Reason: "normal"}
	h.wait(Idle)
	<-h.media.closed
	got := h.sig.types()
	want := []string{"ringing", "accept"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sent %v want %v", got, want)
	}
}

func TestAutoAnswer(t *testing.T) {
	h := start(t, Config{AutoAnswer: true})
	h.msgs <- signal.Message{Type: "invite", CallID: "c1"}
	h.wait(Connecting)
	if h.lastSent().Type != "accept" {
		t.Fatalf("last sent %+v", h.lastSent())
	}
}

func TestLongPressRejects(t *testing.T) {
	h := start(t, Config{})
	h.msgs <- signal.Message{Type: "invite", CallID: "c1"}
	h.wait(Incoming)
	h.presses <- gpio.Long
	h.wait(Idle)
	if m := h.lastSent(); m.Type != "reject" || m.CallID != "c1" {
		t.Fatalf("last sent %+v", m)
	}
}

func TestOutgoingCallPeerLeft(t *testing.T) {
	h := start(t, Config{CallTarget: "alice"})
	h.presses <- gpio.Short
	h.wait(Dialing)
	if m := h.lastSent(); m.Type != "call" || m.To != "web:alice" {
		t.Fatalf("sent %+v", m)
	}
	h.msgs <- signal.Message{Type: "outgoing", CallID: "c2"}
	h.wait(Calling)
	h.msgs <- signal.Message{Type: "accepted", CallID: "c2", URL: "wss://x", Token: "t"}
	h.wait(Active)
	close(h.media.peerLeft)
	h.wait(Idle)
	if m := h.lastSent(); m.Type != "hangup" || m.CallID != "c2" {
		t.Fatalf("sent %+v", m)
	}
}

func TestOfflineTargetReturnsIdle(t *testing.T) {
	h := start(t, Config{})
	h.presses <- gpio.Short
	h.wait(Dialing)
	h.msgs <- signal.Message{Type: "hangup", CallID: "cx", Reason: "offline"}
	h.wait(Idle)
}

func TestBusyRejectsSecondInvite(t *testing.T) {
	h := start(t, Config{})
	h.msgs <- signal.Message{Type: "invite", CallID: "c1"}
	h.wait(Incoming)
	h.msgs <- signal.Message{Type: "invite", CallID: "c2"}
	time.Sleep(20 * time.Millisecond)
	if m := h.lastSent(); m.Type != "reject" || m.CallID != "c2" {
		t.Fatalf("sent %+v", m)
	}
}

func TestJoinFailureHangsUp(t *testing.T) {
	h := start(t, Config{AutoAnswer: true})
	h.joinErr = errors.New("boom")
	h.msgs <- signal.Message{Type: "invite", CallID: "c1"}
	h.wait(Connecting)
	h.msgs <- signal.Message{Type: "accepted", CallID: "c1", URL: "u", Token: "t"}
	h.wait(Idle)
	if m := h.lastSent(); m.Type != "hangup" {
		t.Fatalf("sent %+v", m)
	}
}

func TestSignalDisconnectEndsCall(t *testing.T) {
	h := start(t, Config{AutoAnswer: true})
	h.msgs <- signal.Message{Type: "invite", CallID: "c1"}
	h.msgs <- signal.Message{Type: "accepted", CallID: "c1", URL: "u", Token: "t"}
	h.wait(Active)
	h.msgs <- signal.Message{Type: signal.EventDisconnected}
	h.wait(Idle)
	<-h.media.closed
}

func TestPressIgnoredWhenOffline(t *testing.T) {
	h := start(t, Config{})
	h.msgs <- signal.Message{Type: signal.EventDisconnected}
	h.presses <- gpio.Short
	time.Sleep(20 * time.Millisecond)
	if len(h.sig.types()) != 0 {
		t.Fatalf("sent %v", h.sig.types())
	}
}
