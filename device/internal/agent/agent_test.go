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

func (f *fakeSig) last() signal.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

type fakeMedia struct {
	occ    chan int
	done   chan struct{}
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	muted  bool
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{occ: make(chan int, 4), done: make(chan struct{}), closed: make(chan struct{})}
}
func (m *fakeMedia) Occupancy() <-chan int { return m.occ }
func (m *fakeMedia) Done() <-chan struct{} { return m.done }
func (m *fakeMedia) SetMuted(v bool)       { m.mu.Lock(); m.muted = v; m.mu.Unlock() }
func (m *fakeMedia) isMuted() bool         { m.mu.Lock(); defer m.mu.Unlock(); return m.muted }
func (m *fakeMedia) Close()                { m.once.Do(func() { close(m.closed) }) }

type nopInd struct{}

func (nopInd) SetPattern(gpio.Pattern) {}

type harness struct {
	t       *testing.T
	sig     *fakeSig
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
		msgs:    make(chan signal.Message, 4),
		presses: make(chan gpio.Press, 4),
		states:  make(chan State, 32),
		media:   newFakeMedia(),
	}
	join := func(url, token string) (Media, error) {
		if h.joinErr != nil {
			return nil, h.joinErr
		}
		return h.media, nil
	}
	a := New(cfg, h.sig, join, nopInd{})
	a.OnState = func(s State) { h.states <- s }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)
	go a.Run(ctx, h.msgs, h.presses)
	h.msgs <- signal.Message{Type: signal.EventConnected}
	h.wait(Idle)
	return h
}

func (h *harness) wait(want State) {
	h.t.Helper()
	timeout := time.After(2 * time.Second)
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

func (h *harness) joinRoom() {
	h.t.Helper()
	h.msgs <- signal.Message{Type: "joined", Room: "device-pi1", URL: "wss://x", Token: "t"}
	time.Sleep(30 * time.Millisecond)
}

func TestJoinsRoomMutedUntilSomeoneArrives(t *testing.T) {
	h := start(t, Config{DeviceID: "pi1"})
	h.joinRoom()
	h.media.occ <- 0
	time.Sleep(30 * time.Millisecond)
	if !h.media.isMuted() {
		t.Fatal("should stay muted with nobody in the room")
	}
	h.media.occ <- 2
	h.wait(Active)
	if h.media.isMuted() {
		t.Fatal("should unmute when web users are present")
	}
	h.media.occ <- 0
	h.wait(Idle)
	if !h.media.isMuted() {
		t.Fatal("should mute again when the room is empty of web users")
	}
}

func TestShortPressNotifiesOrMutes(t *testing.T) {
	h := start(t, Config{DeviceID: "pi1", CallTarget: "alice"})
	h.presses <- gpio.Short
	time.Sleep(20 * time.Millisecond)
	if m := h.sig.last(); m.Type != "notify" || m.To != "web:alice" {
		t.Fatalf("notify %+v", m)
	}
	h.joinRoom()
	h.media.occ <- 1
	h.wait(Active)
	h.presses <- gpio.Short
	time.Sleep(20 * time.Millisecond)
	if !h.media.isMuted() {
		t.Fatal("short press during a call should mute")
	}
	h.presses <- gpio.Short
	time.Sleep(20 * time.Millisecond)
	if h.media.isMuted() {
		t.Fatal("second short press should unmute")
	}
}

func TestPressIgnoredWhenOffline(t *testing.T) {
	h := start(t, Config{DeviceID: "pi1"})
	h.msgs <- signal.Message{Type: signal.EventDisconnected}
	h.wait(Offline)
	h.presses <- gpio.Short
	time.Sleep(20 * time.Millisecond)
	if len(h.sig.types()) != 0 {
		t.Fatalf("sent %v", h.sig.types())
	}
}

func TestMediaEndAsksForNewToken(t *testing.T) {
	h := start(t, Config{DeviceID: "pi1"})
	h.joinRoom()
	close(h.media.done)
	time.Sleep(40 * time.Millisecond)
	if m := h.sig.last(); m.Type != "join" || m.Room != "device:pi1" {
		t.Fatalf("sent %+v", m)
	}
	select {
	case <-h.media.closed:
	case <-time.After(time.Second):
		t.Fatal("media not closed")
	}
}

func TestJoinFailureRequestsFreshToken(t *testing.T) {
	h := start(t, Config{DeviceID: "pi1"})
	h.joinErr = errors.New("boom")
	h.msgs <- signal.Message{Type: "joined", Room: "device-pi1", URL: "wss://x", Token: "t"}
	deadline := time.After(12 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("sent %v", h.sig.types())
		default:
		}
		h.sig.mu.Lock()
		n := len(h.sig.sent)
		var last signal.Message
		if n > 0 {
			last = h.sig.sent[n-1]
		}
		h.sig.mu.Unlock()
		if last.Type == "join" && last.Room == "device:pi1" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
