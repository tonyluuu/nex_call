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
	held   bool
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{occ: make(chan int, 4), done: make(chan struct{}), closed: make(chan struct{})}
}
func (m *fakeMedia) Occupancy() <-chan int { return m.occ }
func (m *fakeMedia) Done() <-chan struct{} { return m.done }
func (m *fakeMedia) SetMuted(v bool)       { m.mu.Lock(); m.muted = v; m.mu.Unlock() }
func (m *fakeMedia) SetHeld(v bool)        { m.mu.Lock(); m.held = v; m.mu.Unlock() }
func (m *fakeMedia) isMuted() bool         { m.mu.Lock(); defer m.mu.Unlock(); return m.muted }
func (m *fakeMedia) isHeld() bool          { m.mu.Lock(); defer m.mu.Unlock(); return m.held }
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
	mix     Mixer
	joinErr error
	cancel  context.CancelFunc
}

func start(t *testing.T, cfg Config) *harness {
	return startWith(t, cfg, nil)
}

func startWith(t *testing.T, cfg Config, mix Mixer) *harness {
	h := &harness{
		t:       t,
		sig:     &fakeSig{},
		msgs:    make(chan signal.Message, 4),
		presses: make(chan gpio.Press, 4),
		states:  make(chan State, 32),
		media:   newFakeMedia(),
		mix:     mix,
	}
	join := func(url, token string) (Media, error) {
		if h.joinErr != nil {
			return nil, h.joinErr
		}
		return h.media, nil
	}
	a := New(cfg, h.sig, join, nopInd{}, h.mix)
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

type fakeMix struct {
	mu           sync.Mutex
	speaker, mic int
}

func (f *fakeMix) Get() (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaker, f.mic, nil
}

func (f *fakeMix) Set(speaker, mic *int) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if speaker != nil {
		f.speaker = *speaker
	}
	if mic != nil {
		f.mic = *mic
	}
	return f.speaker, f.mic, nil
}

func (f *fakeMix) levels() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaker, f.mic
}

func (h *harness) waitSent(typ string) signal.Message {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.sig.mu.Lock()
		for i, m := range h.sig.sent {
			if m.Type == typ {
				h.sig.sent = append(h.sig.sent[:i], h.sig.sent[i+1:]...)
				h.sig.mu.Unlock()
				return m
			}
		}
		h.sig.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timeout waiting for %s, sent %v", typ, h.sig.types())
	return signal.Message{}
}

func TestVolumeReportedAndApplied(t *testing.T) {
	mix := &fakeMix{speaker: 40, mic: 55}
	h := startWith(t, Config{DeviceID: "pi1"}, mix)
	h.msgs <- signal.Message{Type: "registered"}
	got := h.waitSent("volume")
	if got.Speaker == nil || got.Mic == nil || *got.Speaker != 40 || *got.Mic != 55 {
		t.Fatalf("reported %+v %+v", got.Speaker, got.Mic)
	}

	speaker := 80
	h.msgs <- signal.Message{Type: "set_volume", Speaker: &speaker}
	got = h.waitSent("volume")
	if got.Speaker == nil || got.Mic == nil || *got.Speaker != 80 || *got.Mic != 55 {
		t.Fatalf("after set %+v %+v", got.Speaker, got.Mic)
	}
	if sp, mic := mix.levels(); sp != 80 || mic != 55 {
		t.Fatalf("mixer speaker=%d mic=%d", sp, mic)
	}
}

type countingBeeper struct {
	mu sync.Mutex
	n  int
}

func (c *countingBeeper) Announce(context.Context, string) error { return nil }

func (c *countingBeeper) Beep(context.Context) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func (c *countingBeeper) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestSpeakerAdjustBeepsOnce(t *testing.T) {
	prev := volumeBeepDelay
	volumeBeepDelay = 40 * time.Millisecond
	t.Cleanup(func() { volumeBeepDelay = prev })

	beeper := &countingBeeper{}
	mix := &fakeMix{speaker: 40, mic: 55}
	h := startWith(t, Config{DeviceID: "pi1", Announce: beeper}, mix)
	h.msgs <- signal.Message{Type: "registered"}
	h.waitSent("volume")

	first, second := 80, 90
	h.msgs <- signal.Message{Type: "set_volume", Speaker: &first}
	time.Sleep(15 * time.Millisecond)
	h.msgs <- signal.Message{Type: "set_volume", Speaker: &second}
	time.Sleep(120 * time.Millisecond)
	if n := beeper.count(); n != 1 {
		t.Fatalf("beeps=%d", n)
	}

	mic := 70
	h.msgs <- signal.Message{Type: "set_volume", Mic: &mic}
	time.Sleep(120 * time.Millisecond)
	if n := beeper.count(); n != 1 {
		t.Fatalf("mic change beeped, beeps=%d", n)
	}
}

func TestVolumePollReportsLocalChange(t *testing.T) {
	prev := volumePollEvery
	volumePollEvery = 20 * time.Millisecond
	t.Cleanup(func() { volumePollEvery = prev })

	mix := &fakeMix{speaker: 40, mic: 55}
	h := startWith(t, Config{DeviceID: "pi1"}, mix)
	h.msgs <- signal.Message{Type: "registered"}
	h.waitSent("volume")

	mix.mu.Lock()
	mix.speaker = 120
	mix.mic = 70
	mix.mu.Unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := h.waitSent("volume")
		if got.Speaker != nil && got.Mic != nil && *got.Speaker == 120 && *got.Mic == 70 {
			return
		}
	}
	t.Fatal("local volume change was not reported")
}

type gateAnnouncer struct {
	started chan string
	release chan struct{}
	calls   int
}

func (g *gateAnnouncer) Announce(ctx context.Context, name string) error {
	g.calls++
	g.started <- name
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAnnounceBeforeUnmute(t *testing.T) {
	gate := &gateAnnouncer{started: make(chan string, 1), release: make(chan struct{})}
	h := start(t, Config{DeviceID: "pi1", Announce: gate})
	h.joinRoom()
	h.msgs <- signal.Message{Type: "incoming", From: "alice"}
	h.media.occ <- 1
	select {
	case name := <-gate.started:
		if name != "alice" {
			t.Fatalf("name %s", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("announce did not start")
	}
	if !h.media.isMuted() || !h.media.isHeld() {
		t.Fatal("call audio should stay held while the prompt plays")
	}
	close(gate.release)
	h.wait(Active)
	if h.media.isMuted() || h.media.isHeld() {
		t.Fatal("call should open after the prompt")
	}
	h.media.occ <- 2
	time.Sleep(50 * time.Millisecond)
	if gate.calls != 1 {
		t.Fatalf("announced %d times", gate.calls)
	}
}

func TestAnnounceSkippedWhenCallerLeaves(t *testing.T) {
	gate := &gateAnnouncer{started: make(chan string, 1), release: make(chan struct{})}
	h := start(t, Config{DeviceID: "pi1", Announce: gate})
	h.joinRoom()
	h.media.occ <- 1
	time.Sleep(30 * time.Millisecond)
	h.msgs <- signal.Message{Type: "incoming", From: "bob"}
	select {
	case name := <-gate.started:
		if name != "bob" {
			t.Fatalf("name %s", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("announce did not start")
	}
	h.media.occ <- 0
	close(gate.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.media.isHeld() && h.media.isMuted() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("held=%v muted=%v", h.media.isHeld(), h.media.isMuted())
}
