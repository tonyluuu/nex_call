package signal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	KindDevice = "device"
	KindWeb    = "web"
	// AllWeb 设备呼叫时不指定用户，则振铃所有在线 Web 用户
	AllWeb = "web:*"
)

// Peer 是一条信令连接。同一 Web 用户可以有多个 Peer（多个标签页）。
type Peer struct {
	Kind string
	ID   string
	Name string
	send chan Message
	once sync.Once
	done chan struct{}
}

func NewPeer(kind, id, name string) *Peer {
	return &Peer{Kind: kind, ID: id, Name: name, send: make(chan Message, 32), done: make(chan struct{})}
}

// Address 形如 device:pi1 / web:alice，同时也是进房的 LiveKit identity。
func (p *Peer) Address() string { return p.Kind + ":" + p.ID }

func (p *Peer) Outbox() <-chan Message { return p.send }
func (p *Peer) Done() <-chan struct{}  { return p.done }

func (p *Peer) Send(m Message) {
	select {
	case p.send <- m:
	case <-p.done:
	default:
		slog.Warn("peer outbox full, closing", "peer", p.Address())
		p.Close()
	}
}

func (p *Peer) Close() { p.once.Do(func() { close(p.done) }) }

type TokenIssuer interface {
	RoomToken(room, identity, name string) (string, error)
}

type Recorder interface {
	CallStarted(ctx context.Context, id, caller, callee string, t time.Time) error
	CallAnswered(ctx context.Context, id, callee string, t time.Time) error
	CallEnded(ctx context.Context, id, reason string, t time.Time) error
}

type DeviceDirectory interface {
	// Devices 返回所有已登记设备（不含在线状态）
	Devices(ctx context.Context) ([]DeviceStatus, error)
	Touch(ctx context.Context, id string, t time.Time)
}

type callState int

const (
	stateRinging callState = iota
	stateActive
)

type call struct {
	id      string
	room    string
	caller  *Peer
	callee  string // 地址；设备呼叫全部 Web 用户时为 AllWeb
	invited map[*Peer]struct{}
	answer  *Peer
	state   callState
	timer   *time.Timer
}

func (c *call) involves(p *Peer) bool {
	if c.caller == p || c.answer == p {
		return true
	}
	_, ok := c.invited[p]
	return ok
}

type Hub struct {
	tokens      TokenIssuer
	recorder    Recorder
	devices     DeviceDirectory
	livekitURL  string
	ringTimeout time.Duration

	mu        sync.Mutex
	devPeers  map[string]*Peer
	webPeers  map[string]map[*Peer]struct{}
	calls     map[string]*call
	peerCalls map[*Peer]string
}

func NewHub(tokens TokenIssuer, recorder Recorder, devices DeviceDirectory, livekitURL string, ringTimeout time.Duration) *Hub {
	if ringTimeout == 0 {
		ringTimeout = 45 * time.Second
	}
	return &Hub{
		tokens:      tokens,
		recorder:    recorder,
		devices:     devices,
		livekitURL:  livekitURL,
		ringTimeout: ringTimeout,
		devPeers:    map[string]*Peer{},
		webPeers:    map[string]map[*Peer]struct{}{},
		calls:       map[string]*call{},
		peerCalls:   map[*Peer]string{},
	}
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Register 登记新连接。设备重复连接时踢掉旧连接。
func (h *Hub) Register(p *Peer) {
	h.mu.Lock()
	var old *Peer
	switch p.Kind {
	case KindDevice:
		old = h.devPeers[p.ID]
		h.devPeers[p.ID] = p
	case KindWeb:
		if h.webPeers[p.ID] == nil {
			h.webPeers[p.ID] = map[*Peer]struct{}{}
		}
		h.webPeers[p.ID][p] = struct{}{}
	}
	h.mu.Unlock()

	if old != nil {
		slog.Info("device reconnected, closing old connection", "device", p.ID)
		h.Unregister(old)
		old.Close()
	}

	p.Send(Message{Type: TypeRegistered, Self: p.Address(), URL: h.livekitURL})
	slog.Info("peer registered", "peer", p.Address())

	if p.Kind == KindDevice {
		h.devices.Touch(context.Background(), p.ID, time.Now())
		h.broadcastPresence()
	} else {
		p.Send(h.presence())
	}
}

func (h *Hub) Unregister(p *Peer) {
	h.mu.Lock()
	removed := false
	switch p.Kind {
	case KindDevice:
		if h.devPeers[p.ID] == p {
			delete(h.devPeers, p.ID)
			removed = true
		}
	case KindWeb:
		if set := h.webPeers[p.ID]; set != nil {
			if _, ok := set[p]; ok {
				delete(set, p)
				removed = true
			}
			if len(set) == 0 {
				delete(h.webPeers, p.ID)
			}
		}
	}

	var toEnd *call
	if id, ok := h.peerCalls[p]; ok {
		if c := h.calls[id]; c != nil {
			if c.caller == p || c.answer == p {
				toEnd = c
			} else {
				delete(c.invited, p)
				delete(h.peerCalls, p)
				if len(c.invited) == 0 && c.state == stateRinging {
					toEnd = c
				}
			}
		}
	}
	if toEnd != nil {
		h.endLocked(toEnd, ReasonDisconnected, p)
	}
	h.mu.Unlock()

	if removed {
		slog.Info("peer unregistered", "peer", p.Address())
		if p.Kind == KindDevice {
			h.devices.Touch(context.Background(), p.ID, time.Now())
		}
		h.broadcastPresence()
	}
}

// Handle 处理客户端发来的一条消息。
func (h *Hub) Handle(p *Peer, m Message) {
	switch m.Type {
	case TypePing:
		p.Send(Message{Type: TypePong})
	case TypeCall:
		h.startCall(p, m.To)
	case TypeRinging:
		h.ringing(p, m.CallID)
	case TypeAccept:
		h.accept(p, m.CallID)
	case TypeReject:
		h.reject(p, m.CallID)
	case TypeHangup:
		h.hangup(p, m.CallID)
	default:
		p.Send(Message{Type: TypeError, Reason: "unknown message type: " + m.Type})
	}
}

func (h *Hub) startCall(p *Peer, to string) {
	if p.Kind == KindDevice && (to == "" || to == "web:") {
		to = AllWeb
	}
	kind, target, _ := strings.Cut(to, ":")
	if (kind != KindDevice && kind != KindWeb) || target == "" {
		p.Send(Message{Type: TypeError, Reason: "invalid target: " + to})
		return
	}
	if to == p.Address() {
		p.Send(Message{Type: TypeError, Reason: "cannot call yourself"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, busy := h.peerCalls[p]; busy {
		p.Send(Message{Type: TypeError, Reason: "already in a call"})
		return
	}

	var invitees []*Peer
	switch {
	case kind == KindDevice:
		if dp := h.devPeers[target]; dp != nil {
			invitees = []*Peer{dp}
		}
	case to == AllWeb:
		for _, set := range h.webPeers {
			for wp := range set {
				invitees = append(invitees, wp)
			}
		}
	default:
		for wp := range h.webPeers[target] {
			invitees = append(invitees, wp)
		}
	}

	callID := newID("c_")
	if len(invitees) == 0 {
		p.Send(Message{Type: TypeHangup, CallID: callID, Peer: to, Reason: ReasonOffline})
		return
	}
	free := invitees[:0]
	for _, ip := range invitees {
		if _, busy := h.peerCalls[ip]; !busy {
			free = append(free, ip)
		}
	}
	if len(free) == 0 {
		p.Send(Message{Type: TypeHangup, CallID: callID, Peer: to, Reason: ReasonBusy})
		return
	}

	c := &call{id: callID, room: callID, caller: p, callee: to, invited: map[*Peer]struct{}{}, state: stateRinging}
	for _, ip := range free {
		token, err := h.tokens.RoomToken(c.room, ip.Address(), ip.Name)
		if err != nil {
			slog.Error("issue token", "err", err)
			p.Send(Message{Type: TypeError, Reason: "internal error"})
			return
		}
		c.invited[ip] = struct{}{}
		h.peerCalls[ip] = callID
		ip.Send(Message{Type: TypeInvite, CallID: callID, Room: c.room, Token: token, URL: h.livekitURL, From: p.Address()})
	}
	h.calls[callID] = c
	h.peerCalls[p] = callID
	c.timer = time.AfterFunc(h.ringTimeout, func() { h.timeout(callID) })

	p.Send(Message{Type: TypeOutgoing, CallID: callID, To: to})
	_ = h.recorder.CallStarted(context.Background(), callID, p.Address(), to, time.Now())
	slog.Info("call started", "call", callID, "from", p.Address(), "to", to, "invited", len(free))
	h.broadcastPresenceLocked()
}

func (h *Hub) lookup(p *Peer, callID string) *call {
	c := h.calls[callID]
	if c == nil || !c.involves(p) {
		p.Send(Message{Type: TypeError, CallID: callID, Reason: "no such call"})
		return nil
	}
	return c
}

func (h *Hub) ringing(p *Peer, callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.lookup(p, callID)
	if c == nil || c.state != stateRinging {
		return
	}
	c.caller.Send(Message{Type: TypeRinging, CallID: callID, Peer: p.Address()})
}

func (h *Hub) accept(p *Peer, callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.lookup(p, callID)
	if c == nil {
		return
	}
	if _, invited := c.invited[p]; !invited || c.state != stateRinging {
		p.Send(Message{Type: TypeError, CallID: callID, Reason: "call not ringing"})
		return
	}
	c.timer.Stop()
	c.state = stateActive
	c.answer = p
	for ip := range c.invited {
		if ip != p {
			delete(h.peerCalls, ip)
			ip.Send(Message{Type: TypeHangup, CallID: callID, Reason: ReasonElsewhere})
		}
	}
	c.invited = map[*Peer]struct{}{p: {}}

	callerToken, err := h.tokens.RoomToken(c.room, c.caller.Address(), c.caller.Name)
	if err != nil {
		slog.Error("issue token", "err", err)
		h.endLocked(c, ReasonCancelled, nil)
		return
	}
	calleeToken, err := h.tokens.RoomToken(c.room, p.Address(), p.Name)
	if err != nil {
		slog.Error("issue token", "err", err)
		h.endLocked(c, ReasonCancelled, nil)
		return
	}
	c.caller.Send(Message{Type: TypeAccepted, CallID: callID, Room: c.room, Token: callerToken, URL: h.livekitURL, Peer: p.Address()})
	p.Send(Message{Type: TypeAccepted, CallID: callID, Room: c.room, Token: calleeToken, URL: h.livekitURL, Peer: c.caller.Address()})

	_ = h.recorder.CallAnswered(context.Background(), callID, p.Address(), time.Now())
	slog.Info("call accepted", "call", callID, "by", p.Address())
}

func (h *Hub) reject(p *Peer, callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.lookup(p, callID)
	if c == nil {
		return
	}
	if c.state != stateRinging || c.caller == p {
		h.endLocked(c, ReasonNormal, p)
		return
	}
	h.endLocked(c, ReasonRejected, p)
}

func (h *Hub) hangup(p *Peer, callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.lookup(p, callID)
	if c == nil {
		return
	}
	reason := ReasonNormal
	if c.state == stateRinging {
		if c.caller == p {
			reason = ReasonCancelled
		} else {
			reason = ReasonRejected
		}
	}
	h.endLocked(c, reason, p)
}

func (h *Hub) timeout(callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.calls[callID]; c != nil && c.state == stateRinging {
		h.endLocked(c, ReasonTimeout, nil)
	}
}

// endLocked 结束通话并通知除 by 以外的所有相关方。
func (h *Hub) endLocked(c *call, reason string, by *Peer) {
	if c.timer != nil {
		c.timer.Stop()
	}
	delete(h.calls, c.id)
	notify := map[*Peer]struct{}{c.caller: {}}
	for ip := range c.invited {
		notify[ip] = struct{}{}
	}
	for np := range notify {
		if h.peerCalls[np] == c.id {
			delete(h.peerCalls, np)
		}
		if np != by {
			np.Send(Message{Type: TypeHangup, CallID: c.id, Reason: reason})
		}
	}
	_ = h.recorder.CallEnded(context.Background(), c.id, reason, time.Now())
	slog.Info("call ended", "call", c.id, "reason", reason)
	h.broadcastPresenceLocked()
}

func (h *Hub) presence() Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.presenceLocked()
}

func (h *Hub) presenceLocked() Message {
	list, err := h.devices.Devices(context.Background())
	if err != nil {
		slog.Error("list devices", "err", err)
	}
	for i := range list {
		if dp := h.devPeers[list[i].ID]; dp != nil {
			list[i].Online = true
			_, list[i].Busy = h.peerCalls[dp]
		}
	}
	return Message{Type: TypePresence, Devices: list}
}

func (h *Hub) broadcastPresence() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastPresenceLocked()
}

func (h *Hub) broadcastPresenceLocked() {
	m := h.presenceLocked()
	for _, set := range h.webPeers {
		for wp := range set {
			wp.Send(m)
		}
	}
}

// Snapshot 返回设备在线状态（供 HTTP 接口使用）。
func (h *Hub) Snapshot() []DeviceStatus {
	return h.presence().Devices
}
