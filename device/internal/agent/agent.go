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
	Close()
}

type JoinFunc func(url, token string) (Media, error)

type Indicator interface {
	SetPattern(gpio.Pattern)
}

type Config struct {
	DeviceID string
	// CallTarget 短按提醒的 Web 用户名，空表示提醒所有在线用户
	CallTarget string
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

type Agent struct {
	cfg  Config
	sig  Signaler
	join JoinFunc
	ind  Indicator

	ctx       context.Context
	events    chan any
	online    bool
	state     State
	room      string
	url       string
	token     string
	media     Media
	joinSeq   int
	joining   bool
	remote    int
	userMuted bool
	OnState   func(State)
}

func New(cfg Config, sig Signaler, join JoinFunc, ind Indicator) *Agent {
	return &Agent{cfg: cfg, sig: sig, join: join, ind: ind, events: make(chan any, 8)}
}

func (a *Agent) Run(ctx context.Context, msgs <-chan signal.Message, presses <-chan gpio.Press) {
	a.ctx = ctx
	a.updateLED()
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

func (a *Agent) applyMute() {
	muted := a.remote == 0 || a.userMuted
	if a.media != nil {
		a.media.SetMuted(muted)
	}
	if !a.online {
		a.setState(Offline)
		return
	}
	if a.remote > 0 {
		a.setState(Active)
	} else {
		a.userMuted = false
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
		a.remote = e.n
		if e.n == 0 {
			a.userMuted = false
		}
		a.applyMute()
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
