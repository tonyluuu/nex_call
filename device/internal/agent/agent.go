// Package agent 是设备端的呼叫状态机，单协程处理信令消息、按键和媒体事件。
package agent

import (
	"context"
	"log/slog"

	"nexcall/device/internal/gpio"
	"nexcall/device/internal/signal"
)

type Signaler interface {
	Send(signal.Message) error
}

type Media interface {
	PeerLeft() <-chan struct{}
	Done() <-chan struct{}
	Close()
}

type JoinFunc func(url, token string) (Media, error)

type Ringer interface {
	Start(incoming bool)
	Stop()
}

type Indicator interface {
	SetPattern(gpio.Pattern)
}

type Config struct {
	AutoAnswer bool
	// CallTarget 为空时呼叫所有在线 Web 用户
	CallTarget string
}

type State int

const (
	Idle State = iota
	Dialing
	Calling
	Incoming
	Connecting
	Active
)

func (s State) String() string {
	return [...]string{"idle", "dialing", "calling", "incoming", "connecting", "active"}[s]
}

type joinResult struct {
	seq   int
	media Media
	err   error
}

type mediaEnded struct {
	seq    int
	reason string
}

type Agent struct {
	cfg    Config
	sig    Signaler
	join   JoinFunc
	ringer Ringer
	ind    Indicator

	ctx     context.Context
	events  chan any
	online  bool
	state   State
	callID  string
	media   Media
	joinSeq int
	// OnState 测试用的状态变化回调
	OnState func(State)
}

func New(cfg Config, sig Signaler, join JoinFunc, ringer Ringer, ind Indicator) *Agent {
	return &Agent{cfg: cfg, sig: sig, join: join, ringer: ringer, ind: ind, events: make(chan any, 8)}
}

func (a *Agent) Run(ctx context.Context, msgs <-chan signal.Message, presses <-chan gpio.Press) {
	a.ctx = ctx
	a.updateLED()
	for {
		select {
		case <-ctx.Done():
			a.hangup()
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
		slog.Info("call state", "from", a.state, "to", s, "call", a.callID)
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
	case a.state == Active:
		p = gpio.Active
	case a.state == Incoming:
		p = gpio.Incoming
	case a.state != Idle:
		p = gpio.Calling
	case a.online:
		p = gpio.Idle
	default:
		p = gpio.Offline
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
		a.updateLED()
	case signal.EventDisconnected:
		a.online = false
		// 服务端在连接断开时会结束这台设备的所有通话
		a.toIdle()
	case "registered":
		slog.Info("registered", "self", m.Self, "livekit", m.URL)
	case "invite":
		if a.state != Idle {
			a.send(signal.Message{Type: "reject", CallID: m.CallID})
			return
		}
		slog.Info("incoming call", "call", m.CallID, "from", m.From)
		a.callID = m.CallID
		a.setState(Incoming)
		a.send(signal.Message{Type: "ringing", CallID: m.CallID})
		if a.cfg.AutoAnswer {
			a.accept()
		} else {
			a.ringer.Start(true)
		}
	case "outgoing":
		if a.state == Dialing {
			a.callID = m.CallID
			a.setState(Calling)
		}
	case "ringing":
		if m.CallID == a.callID {
			slog.Info("remote ringing", "peer", m.Peer)
		}
	case "accepted":
		if m.CallID == a.callID && (a.state == Calling || a.state == Connecting) {
			a.ringer.Stop()
			a.startJoin(m.URL, m.Token)
		}
	case "hangup":
		if (m.CallID != "" && m.CallID == a.callID) || a.state == Dialing {
			slog.Info("call ended by server", "call", m.CallID, "reason", m.Reason)
			a.toIdle()
		}
	case "error":
		slog.Warn("server error", "reason", m.Reason, "call", m.CallID)
		if a.state == Dialing {
			a.toIdle()
		}
	}
}

func (a *Agent) onPress(p gpio.Press) {
	switch a.state {
	case Idle:
		if p == gpio.Short && a.online {
			a.dial()
		}
	case Incoming:
		if p == gpio.Short {
			a.accept()
		} else {
			a.hangup()
		}
	default:
		a.hangup()
	}
}

func (a *Agent) dial() {
	to := ""
	if a.cfg.CallTarget != "" {
		to = "web:" + a.cfg.CallTarget
	}
	a.setState(Dialing)
	a.send(signal.Message{Type: "call", To: to})
	a.ringer.Start(false)
}

func (a *Agent) accept() {
	a.ringer.Stop()
	a.send(signal.Message{Type: "accept", CallID: a.callID})
	a.setState(Connecting)
}

// hangup 本地主动结束：通知服务端后回到空闲。
func (a *Agent) hangup() {
	if a.callID != "" {
		typ := "hangup"
		if a.state == Incoming {
			typ = "reject"
		}
		a.send(signal.Message{Type: typ, CallID: a.callID})
	}
	a.toIdle()
}

func (a *Agent) toIdle() {
	a.ringer.Stop()
	a.joinSeq++
	if a.media != nil {
		a.media.Close()
		a.media = nil
	}
	a.callID = ""
	a.setState(Idle)
}

func (a *Agent) post(ev any) {
	select {
	case a.events <- ev:
	case <-a.ctx.Done():
	}
}

func (a *Agent) startJoin(url, token string) {
	a.joinSeq++
	seq := a.joinSeq
	a.setState(Connecting)
	go func() {
		m, err := a.join(url, token)
		a.post(joinResult{seq: seq, media: m, err: err})
	}()
}

func (a *Agent) onEvent(ev any) {
	switch e := ev.(type) {
	case joinResult:
		if e.seq != a.joinSeq || a.state != Connecting {
			if e.media != nil {
				e.media.Close()
			}
			return
		}
		if e.err != nil {
			slog.Error("join media failed", "err", e.err)
			a.hangup()
			return
		}
		a.media = e.media
		a.setState(Active)
		go func(seq int, m Media) {
			select {
			case <-m.PeerLeft():
				a.post(mediaEnded{seq: seq, reason: "peer left"})
			case <-m.Done():
				a.post(mediaEnded{seq: seq, reason: "media disconnected"})
			case <-a.ctx.Done():
			}
		}(e.seq, e.media)
	case mediaEnded:
		if e.seq == a.joinSeq && a.state == Active {
			slog.Info("media ended", "reason", e.reason)
			a.hangup()
		}
	}
}
