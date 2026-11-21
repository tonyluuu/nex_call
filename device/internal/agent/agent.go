// Package agent 是设备端的呼叫状态机，单协程处理信令消息、按键和媒体事件。
// 设备连上后常驻自己的房间：没有网页用户时麦克风静音，有人进入就开麦。
package agent

import (
	"context"
	"log/slog"
	"time"

	"nexcall/device/internal/gpio"
	"nexcall/device/internal/signal"
)

type Signaler interface {
	Send(signal.Message) error
}

type Media interface {
	Occupancy() <-chan int
	Done() <-chan struct{}
	SetMuted(bool)
	// SetHeld 为 true 时仍接收远端音频，但不从喇叭放出来。
	SetHeld(bool)
	Close()
}

// Announcer 在接通前把来电人念出来。失败时通话仍应继续。
type Announcer interface {
	Announce(ctx context.Context, name string) error
}

type JoinFunc func(url, token string) (Media, error)

type Indicator interface {
	SetPattern(gpio.Pattern)
}

// Mixer 读取和设置喇叭、麦克风的系统音量，百分比 0–200。nil 的那一路不改。
type Mixer interface {
	Get() (speaker, mic int, err error)
	Set(speaker, mic *int) (speakerOut, micOut int, err error)
}

type Config struct {
	DeviceID string
	// CallTarget 短按提醒的 Web 用户名，空表示提醒所有在线用户
	CallTarget string
	// Announce 来电提示。为空则人数一变就接通。
	Announce Announcer
}

type State int

const (
	Offline State = iota
	Idle
	Active
)

func (s State) String() string {
	return [...]string{"offline", "idle", "active"}[s]
}

type joinResult struct {
	seq   int
	media Media
	err   error
}

type mediaEnded struct{ seq int }

type occupancy struct {
	seq int
	n   int
}

type announceDone struct{ seq int }

type volumeDone struct {
	speaker int
	mic     int
	err     error
	beep    bool
}

type speakerBeep struct{ gen int }

// volumeBeepDelay 是喇叭最后一次设定之后再响提示音的间隔。
// 网页拖动滑块时会连续下发，停手后再响一次。
var volumeBeepDelay = 400 * time.Millisecond

type volumePoll struct{}

// volumePollEvery 是检查本机音量是否被别处改过的间隔。
var volumePollEvery = time.Second

type Agent struct {
	cfg  Config
	sig  Signaler
	join JoinFunc
	ind  Indicator
	mix  Mixer

	ctx        context.Context
	events     chan any
	online     bool
	state      State
	room       string
	url        string
	token      string
	media      Media
	joinSeq    int
	joining    bool
	remote     int
	userMuted  bool
	caller     string
	holding    bool
	announcing bool
	prompted     bool
	waitName     chan string
	OnState      func(State)
	volumeKnown    bool
	lastSpeaker    int
	lastMic        int
	volumePending  bool
	pendingSpeaker int
	pendingMic     int
	beepGen        int
}

func New(cfg Config, sig Signaler, join JoinFunc, ind Indicator, mix Mixer) *Agent {
	return &Agent{cfg: cfg, sig: sig, join: join, ind: ind, mix: mix, events: make(chan any, 8)}
}

func (a *Agent) Run(ctx context.Context, msgs <-chan signal.Message, presses <-chan gpio.Press) {
	a.ctx = ctx
	a.updateLED()
	go a.watchVolume(ctx)
	for {
		select {
		case <-ctx.Done():
			a.closeMedia()
			return
		case m := <-msgs:
			a.onMessage(m)
		case p := <-presses:
			a.onPress(p)
		case ev := <-a.events:
			a.onEvent(ev)
		}
	}
}

func (a *Agent) setState(s State) {
	if a.state != s {
		slog.Info("call state", "from", a.state, "to", s, "room", a.room)
	}
	a.state = s
	a.updateLED()
	if a.OnState != nil {
		a.OnState(s)
	}
}

func (a *Agent) updateLED() {
	var p gpio.Pattern
	switch {
	case !a.online:
		p = gpio.Offline
	case a.state == Active && a.userMuted:
		p = gpio.Incoming
	case a.state == Active:
		p = gpio.Active
	default:
		p = gpio.Idle
	}
	a.ind.SetPattern(p)
}

func (a *Agent) send(m signal.Message) {
	if err := a.sig.Send(m); err != nil {
		slog.Warn("signal send failed", "type", m.Type, "err", err)
	}
}

func (a *Agent) onMessage(m signal.Message) {
	switch m.Type {
	case signal.EventConnected:
		a.online = true
		a.setState(Idle)
	case signal.EventDisconnected:
		a.online = false
		a.closeMedia()
		a.setState(Offline)
	case "registered":
		slog.Info("registered", "self", m.Self, "livekit", m.URL)
		a.readVolume()
	case "set_volume":
		a.applyVolume(m.Speaker, m.Mic)
	case "joined":
		slog.Info("room token", "room", m.Room)
		a.room = m.Room
		a.url = m.URL
		a.token = m.Token
		if a.media == nil {
			a.startJoin()
		}
	case "left":
		if m.Room == a.room {
			a.closeMedia()
			a.remote = 0
			a.userMuted = false
			if a.online {
				a.setState(Idle)
				a.send(signal.Message{Type: "join", Room: "device:" + a.cfg.DeviceID})
			}
		}
	case "incoming":
		if m.From != "" {
			a.caller = m.From
		}
		if a.waitName != nil && a.caller != "" {
			select {
			case a.waitName <- a.caller:
			default:
			}
		}
		// 网页要等提示音放完才进房间，所以人数还没变就要开始念。
		if !a.announcing && !a.prompted && a.remote == 0 {
			a.prompted = true
			if a.cfg.Announce == nil {
				a.send(signal.Message{Type: "ready", Room: a.room})
			} else {
				a.beginAnnounce()
			}
		}
	case "error":
		slog.Warn("server error", "reason", m.Reason, "room", m.Room)
	}
}

func (a *Agent) onPress(p gpio.Press) {
	if !a.online || p != gpio.Short {
		return
	}
	if a.state == Active {
		a.userMuted = !a.userMuted
		a.applyMute()
		return
	}
	to := ""
	if a.cfg.CallTarget != "" {
		to = "web:" + a.cfg.CallTarget
	}
	a.send(signal.Message{Type: "notify", To: to})
}

func (a *Agent) watchVolume(ctx context.Context) {
	ticker := time.NewTicker(volumePollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.post(volumePoll{})
		}
	}
}

func (a *Agent) readVolume() {
	if a.mix == nil {
		return
	}
	go func() {
		speaker, mic, err := a.mix.Get()
		a.post(volumeDone{speaker: speaker, mic: mic, err: err})
	}()
}

func (a *Agent) applyVolume(speaker, mic *int) {
	if a.mix == nil || (speaker == nil && mic == nil) {
		return
	}
	beep := speaker != nil
	go func() {
		outSpeaker, outMic, err := a.mix.Set(speaker, mic)
		a.post(volumeDone{speaker: outSpeaker, mic: outMic, err: err, beep: beep && err == nil})
	}()
}

func (a *Agent) scheduleSpeakerBeep() {
	if _, ok := a.cfg.Announce.(interface {
		Beep(context.Context) error
	}); !ok {
		return
	}
	a.beepGen++
	gen := a.beepGen
	time.AfterFunc(volumeBeepDelay, func() {
		a.post(speakerBeep{gen: gen})
	})
}

func (a *Agent) reportVolume(speaker, mic int) {
	a.send(signal.Message{Type: "volume", Speaker: &speaker, Mic: &mic})
}

func (a *Agent) playSpeakerBeep(gen int) {
	if gen != a.beepGen {
		return
	}
	beeper, ok := a.cfg.Announce.(interface {
		Beep(context.Context) error
	})
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		defer cancel()
		if err := beeper.Beep(ctx); err != nil {
			slog.Warn("volume beep", "err", err)
		}
	}()
}

func (a *Agent) holdMedia(hold bool) {
	if a.media != nil {
		a.media.SetHeld(hold)
	}
}

// beginAnnounce 先念提示音。念的过程中保持静音，念完再按当前人数决定是否开麦。
func (a *Agent) beginAnnounce() {
	a.announcing = true
	a.holding = true
	a.holdMedia(true)
	a.applyMute()
	name := a.caller
	var wait chan string
	if name == "" {
		wait = make(chan string, 1)
		a.waitName = wait
	}
	seq := a.joinSeq
	go func() {
		if name == "" {
			select {
			case name = <-wait:
			case <-time.After(time.Second):
			case <-a.ctx.Done():
				a.post(announceDone{seq: seq})
				return
			}
		}
		if name == "" {
			name = "有人"
		}
		ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
		err := a.cfg.Announce.Announce(ctx, name)
		cancel()
		if err != nil && a.ctx.Err() == nil {
			slog.Warn("announce", "err", err)
		}
		a.post(announceDone{seq: seq})
	}()
}

func (a *Agent) applyMute() {
	muted := a.holding || a.remote == 0 || a.userMuted
	if a.media != nil {
		a.media.SetMuted(muted)
	}
	if !a.online {
		a.setState(Offline)
		return
	}
	if a.remote > 0 && !a.holding {
		a.setState(Active)
		return
	}
	if a.remote == 0 {
		a.userMuted = false
	}
	if !a.holding {
		a.setState(Idle)
	}
}

func (a *Agent) closeMedia() {
	a.joinSeq++
	a.joining = false
	if a.media != nil {
		a.media.Close()
		a.media = nil
	}
}

func (a *Agent) post(ev any) {
	select {
	case a.events <- ev:
	case <-a.ctx.Done():
	}
}

func (a *Agent) startJoin() {
	if a.joining || a.media != nil || a.url == "" || a.token == "" {
		return
	}
	a.joining = true
	a.joinSeq++
	seq := a.joinSeq
	url, token := a.url, a.token
	go func() {
		backoff := time.Second
		for fails := 1; ; fails++ {
			m, err := a.join(url, token)
			if err == nil {
				a.post(joinResult{seq: seq, media: m})
				return
			}
			slog.Error("join media failed", "err", err)
			if m != nil {
				m.Close()
			}
			if fails >= 3 {
				a.post(joinResult{seq: seq, err: err})
				return
			}
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}()
}

func (a *Agent) onEvent(ev any) {
	switch e := ev.(type) {
	case joinResult:
		if e.seq != a.joinSeq {
			if e.media != nil {
				e.media.Close()
			}
			return
		}
		a.joining = false
		if e.err != nil || e.media == nil {
			a.send(signal.Message{Type: "join", Room: "device:" + a.cfg.DeviceID})
			return
		}
		a.media = e.media
		a.applyMute()
		go func(seq int, m Media) {
			for {
				select {
				case n, ok := <-m.Occupancy():
					if !ok {
						return
					}
					a.post(occupancy{seq: seq, n: n})
				case <-m.Done():
					a.post(mediaEnded{seq: seq})
					return
				case <-a.ctx.Done():
					return
				}
			}
		}(e.seq, e.media)
	case occupancy:
		if e.seq != a.joinSeq || a.media == nil {
			return
		}
		prev := a.remote
		a.remote = e.n
		if e.n == 0 {
			a.userMuted = false
			if !a.announcing {
				a.holding = false
				a.holdMedia(false)
				if prev > 0 {
					a.prompted = false
					a.caller = ""
				}
			}
			a.applyMute()
			return
		}
		if prev == 0 && a.cfg.Announce != nil && !a.announcing && !a.prompted {
			a.prompted = true
			a.beginAnnounce()
			return
		}
		a.applyMute()
	case announceDone:
		if e.seq != a.joinSeq || a.media == nil {
			return
		}
		a.announcing = false
		a.holding = false
		a.waitName = nil
		a.holdMedia(false)
		a.send(signal.Message{Type: "ready", Room: a.room})
		a.applyMute()
	case volumePoll:
		if a.online && a.mix != nil {
			a.readVolume()
		}
	case speakerBeep:
		a.playSpeakerBeep(e.gen)
	case volumeDone:
		if e.err != nil {
			slog.Warn("volume", "err", e.err)
			return
		}
		if e.beep {
			a.scheduleSpeakerBeep()
		}
		// 调节音量时虚拟设备会被短暂写成 100%。连续两次读到同一个新值再上报，
		// 避免网页上闪一下错误音量。
		if !a.volumeKnown {
			a.volumeKnown = true
			a.lastSpeaker = e.speaker
			a.lastMic = e.mic
			slog.Info("volume", "speaker", e.speaker, "mic", e.mic)
			a.reportVolume(e.speaker, e.mic)
			return
		}
		if a.lastSpeaker == e.speaker && a.lastMic == e.mic {
			a.volumePending = false
			return
		}
		if !a.volumePending || a.pendingSpeaker != e.speaker || a.pendingMic != e.mic {
			a.volumePending = true
			a.pendingSpeaker = e.speaker
			a.pendingMic = e.mic
			return
		}
		a.volumePending = false
		a.lastSpeaker = e.speaker
		a.lastMic = e.mic
		slog.Info("volume", "speaker", e.speaker, "mic", e.mic)
		a.reportVolume(e.speaker, e.mic)
	case mediaEnded:
		if e.seq != a.joinSeq {
			return
		}
		slog.Info("media ended, rejoining")
		a.closeMedia()
		a.remote = 0
		a.userMuted = false
		if a.online {
			a.setState(Idle)
			a.send(signal.Message{Type: "join", Room: "device:" + a.cfg.DeviceID})
		}
	}
}
