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
)

// Peer 是一条信令连接。同一 Web 用户可以有多个 Peer（多个标签页）。
type Peer struct {
	Kind string
	ID   string
	Name string
	Role string
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

// Recorder 把「房间里出现网页用户」到「网页用户全部离开」记成一次通话。
type Recorder interface {
	RoomOpened(ctx context.Context, id, caller, callee string, t time.Time) error
	ParticipantJoined(ctx context.Context, callID, identity string, t time.Time) error
	ParticipantLeft(ctx context.Context, callID, identity, reason string, t time.Time) error
	RoomClosed(ctx context.Context, id, reason string, t time.Time) error
}

type Remover interface {
	RemoveParticipant(ctx context.Context, room, identity string) error
}

type DeviceDirectory interface {
	Devices(ctx context.Context) ([]DeviceStatus, error)
	Touch(ctx context.Context, id string, t time.Time)
}

type room struct {
	id       string
	deviceID string
	target   string
	members  map[*Peer]time.Time
	recorded map[*Peer]struct{}
	recordID string
	started  time.Time
}

type Hub struct {
	tokens     TokenIssuer
	recorder   Recorder
	devices    DeviceDirectory
	remover    Remover
	livekitURL string

	mu       sync.Mutex
	devPeers map[string]*Peer
	webPeers map[string]map[*Peer]struct{}
	rooms    map[string]*room
	peerRoom map[*Peer]string
}

func NewHub(tokens TokenIssuer, recorder Recorder, devices DeviceDirectory, livekitURL string, _ time.Duration) *Hub {
	return &Hub{
		tokens:     tokens,
		recorder:   recorder,
		devices:    devices,
		livekitURL: livekitURL,
		devPeers:   map[string]*Peer{},
		webPeers:   map[string]map[*Peer]struct{}{},
		rooms:      map[string]*room{},
		peerRoom:   map[*Peer]string{},
	}
}

func (h *Hub) SetRemover(r Remover) { h.remover = r }

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Register 登记新连接。设备重复连接时踢掉旧连接，并进入自己的常驻房间。
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
		h.mu.Lock()
		h.enterDeviceLocked(p)
		h.mu.Unlock()
	} else {
		p.Send(h.presence())
	}
	h.broadcastPresence()
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
	if h.peerRoom[p] != "" {
		h.leaveLocked(p, ReasonDisconnected)
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

func (h *Hub) Handle(p *Peer, m Message) {
	switch m.Type {
	case TypePing:
		p.Send(Message{Type: TypePong})
	case TypeJoin:
		h.join(p, m.Room)
	case TypeCall:
		h.call(p, m.To)
	case TypeLeave:
		h.leave(p)
	case TypeNotify:
		h.notify(p, m.To)
	case TypeKick:
		h.kick(p, m.Room, m.Peer)
	default:
		p.Send(Message{Type: TypeError, Reason: "unknown message type: " + m.Type})
	}
}

func (h *Hub) enterDeviceLocked(p *Peer) {
	r := h.ensureDeviceRoomLocked(p.ID)
	if h.peerRoom[p] == r.id {
		h.sendJoinedLocked(p, r)
		return
	}
	h.addMemberLocked(r, p, "", "")
	h.sendJoinedLocked(p, r)
}

func (h *Hub) ensureDeviceRoomLocked(id string) *room {
	rid := DeviceRoom(id)
	r := h.rooms[rid]
	if r == nil {
		r = &room{id: rid, deviceID: id, members: map[*Peer]time.Time{}, recorded: map[*Peer]struct{}{}}
		h.rooms[rid] = r
	}
	return r
}

func (h *Hub) join(p *Peer, ref string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, errText := h.resolveLocked(ref, true)
	if r == nil {
		p.Send(Message{Type: TypeError, Reason: errText})
		return
	}
	if p.Kind == KindDevice && r.deviceID != p.ID {
		p.Send(Message{Type: TypeError, Room: r.id, Reason: "device can only join its own room"})
		return
	}
	if cur := h.peerRoom[p]; cur != "" && cur != r.id {
		p.Send(Message{Type: TypeError, Room: cur, Reason: "already in a room"})
		return
	}
	if h.peerRoom[p] == r.id {
		h.sendJoinedLocked(p, r)
		return
	}
	caller, callee := "", ""
	if p.Kind == KindWeb {
		caller = p.Address()
		if r.deviceID != "" {
			callee = KindDevice + ":" + r.deviceID
		} else {
			callee = r.target
		}
	}
	h.addMemberLocked(r, p, caller, callee)
	if !h.sendJoinedLocked(p, r) {
		h.leaveLocked(p, ReasonNormal)
		return
	}
	slog.Info("joined room", "room", r.id, "peer", p.Address())
	h.broadcastPresenceLocked()
}

func (h *Hub) resolveLocked(ref string, createDevice bool) (*room, string) {
	if id, ok := strings.CutPrefix(ref, "device:"); ok && id != "" && !strings.Contains(id, ":") {
		if !createDevice {
			return h.rooms[DeviceRoom(id)], "no such room"
		}
		return h.ensureDeviceRoomLocked(id), ""
	}
	if r := h.rooms[ref]; r != nil {
		return r, ""
	}
	if ref == "" {
		return nil, "invalid room"
	}
	return nil, "no such room"
}

func (h *Hub) call(p *Peer, to string) {
	if p.Kind != KindWeb {
		p.Send(Message{Type: TypeError, Reason: "only web users can call"})
		return
	}
	id, ok := strings.CutPrefix(to, "web:")
	if !ok || id == "" {
		p.Send(Message{Type: TypeError, Reason: "invalid target: " + to})
		return
	}
	if to == p.Address() {
		p.Send(Message{Type: TypeError, Reason: "cannot call yourself"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peerRoom[p] != "" {
		p.Send(Message{Type: TypeError, Reason: "already in a room"})
		return
	}
	set := h.webPeers[id]
	if len(set) == 0 {
		p.Send(Message{Type: TypeError, To: to, Reason: ReasonOffline})
		return
	}
	var invitee *Peer
	for wp := range set {
		if h.peerRoom[wp] == "" {
			invitee = wp
			break
		}
	}
	if invitee == nil {
		p.Send(Message{Type: TypeError, To: to, Reason: ReasonBusy})
		return
	}

	r := &room{
		id:       newID("r_"),
		target:   to,
		members:  map[*Peer]time.Time{},
		recorded: map[*Peer]struct{}{},
	}
	h.rooms[r.id] = r
	h.addMemberLocked(r, p, p.Address(), to)
	if !h.sendJoinedLocked(p, r) {
		h.leaveLocked(p, ReasonNormal)
		return
	}
	invitee.Send(Message{Type: TypeInvite, Room: r.id, From: p.Address()})
	slog.Info("adhoc room", "room", r.id, "from", p.Address(), "to", to)
	h.broadcastPresenceLocked()
}

func (h *Hub) leave(p *Peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peerRoom[p] == "" {
		p.Send(Message{Type: TypeError, Reason: "not in a room"})
		return
	}
	h.leaveLocked(p, ReasonNormal)
	h.broadcastPresenceLocked()
}

func (h *Hub) notify(p *Peer, to string) {
	if p.Kind != KindDevice {
		p.Send(Message{Type: TypeError, Reason: "only devices can notify"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := Message{Type: TypeNotify, From: p.Address(), Room: DeviceRoom(p.ID)}
	if to == "" || to == "web:" {
		for _, set := range h.webPeers {
			for wp := range set {
				wp.Send(msg)
			}
		}
		return
	}
	id, ok := strings.CutPrefix(to, "web:")
	if !ok || id == "" {
		p.Send(Message{Type: TypeError, Reason: "invalid target: " + to})
		return
	}
	if len(h.webPeers[id]) == 0 {
		p.Send(Message{Type: TypeError, To: to, Reason: ReasonOffline})
		return
	}
	for wp := range h.webPeers[id] {
		wp.Send(msg)
	}
}

func (h *Hub) kick(p *Peer, roomID, identity string) {
	if p.Role != RoleAdmin {
		p.Send(Message{Type: TypeError, Reason: "forbidden"})
		return
	}
	if err := h.Kick(roomID, identity, p.Address()); err != nil {
		p.Send(Message{Type: TypeError, Room: roomID, Reason: err.Error()})
	}
}

// Kick 把网页参与者移出房间。by 是操作者地址，写入 kicked 消息。
func (h *Hub) Kick(roomID, identity, by string) error {
	if strings.HasPrefix(identity, KindDevice+":") {
		return errString("cannot kick device")
	}
	h.mu.Lock()
	r := h.rooms[roomID]
	if r == nil {
		h.mu.Unlock()
		return errString("no such room")
	}
	var targets []*Peer
	for m := range r.members {
		if m.Address() == identity && m.Kind == KindWeb {
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		h.mu.Unlock()
		return errString("not in room")
	}
	h.mu.Unlock()

	if h.remover != nil {
		if err := h.remover.RemoveParticipant(context.Background(), roomID, identity); err != nil {
			slog.Warn("livekit remove participant", "room", roomID, "identity", identity, "err", err)
		}
	}

	h.mu.Lock()
	r = h.rooms[roomID]
	if r == nil {
		h.mu.Unlock()
		return nil
	}
	targets = nil
	for m := range r.members {
		if m.Address() == identity && m.Kind == KindWeb {
			targets = append(targets, m)
		}
	}
	for _, m := range targets {
		m.Send(Message{Type: TypeKicked, Room: roomID, Peer: by, Reason: ReasonKicked})
		h.leaveLocked(m, ReasonKicked)
	}
	slog.Info("kicked", "room", roomID, "identity", identity, "by", by)
	h.broadcastPresenceLocked()
	h.mu.Unlock()
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

func (h *Hub) addMemberLocked(r *room, p *Peer, caller, callee string) {
	now := time.Now()
	if _, ok := r.members[p]; !ok {
		r.members[p] = now
	}
	h.peerRoom[p] = r.id
	if p.Kind == KindWeb && r.recordID == "" {
		r.recordID = newID("c_")
		r.started = now
		if caller == "" {
			caller = p.Address()
		}
		if callee == "" {
			callee = r.target
		}
		if err := h.recorder.RoomOpened(context.Background(), r.recordID, caller, callee, now); err != nil {
			slog.Error("room opened", "err", err)
		}
		for m, joined := range r.members {
			h.recordJoinLocked(r, m, joined)
		}
		return
	}
	if r.recordID != "" {
		h.recordJoinLocked(r, p, r.members[p])
	}
}

func (h *Hub) recordJoinLocked(r *room, p *Peer, at time.Time) {
	if _, ok := r.recorded[p]; ok {
		return
	}
	r.recorded[p] = struct{}{}
	if err := h.recorder.ParticipantJoined(context.Background(), r.recordID, p.Address(), at); err != nil {
		slog.Error("participant joined", "err", err)
	}
}

func (h *Hub) leaveLocked(p *Peer, reason string) {
	rid := h.peerRoom[p]
	r := h.rooms[rid]
	if r == nil {
		delete(h.peerRoom, p)
		return
	}
	delete(r.members, p)
	delete(h.peerRoom, p)
	if r.recordID != "" {
		if _, ok := r.recorded[p]; ok {
			delete(r.recorded, p)
			if err := h.recorder.ParticipantLeft(context.Background(), r.recordID, p.Address(), reason, time.Now()); err != nil {
				slog.Error("participant left", "err", err)
			}
		}
	}
	if reason != ReasonKicked {
		p.Send(Message{Type: TypeLeft, Room: r.id, Reason: reason})
	}
	if h.webCount(r) == 0 && r.recordID != "" {
		now := time.Now()
		for m := range r.members {
			if _, ok := r.recorded[m]; ok {
				delete(r.recorded, m)
				_ = h.recorder.ParticipantLeft(context.Background(), r.recordID, m.Address(), reason, now)
			}
		}
		if err := h.recorder.RoomClosed(context.Background(), r.recordID, reason, now); err != nil {
			slog.Error("room closed", "err", err)
		}
		r.recordID = ""
		r.started = time.Time{}
		r.recorded = map[*Peer]struct{}{}
	}
	if r.deviceID == "" && h.webCount(r) == 0 {
		delete(h.rooms, r.id)
	}
	if r.deviceID != "" && len(r.members) == 0 {
		delete(h.rooms, r.id)
	}
	slog.Info("left room", "room", rid, "peer", p.Address(), "reason", reason)
}

func (h *Hub) webCount(r *room) int {
	n := 0
	for m := range r.members {
		if m.Kind == KindWeb {
			n++
		}
	}
	return n
}

func (h *Hub) sendJoinedLocked(p *Peer, r *room) bool {
	token, err := h.tokens.RoomToken(r.id, p.Address(), p.Name)
	if err != nil {
		slog.Error("issue token", "err", err)
		p.Send(Message{Type: TypeError, Room: r.id, Reason: "internal error"})
		return false
	}
	p.Send(Message{Type: TypeJoined, Room: r.id, Token: token, URL: h.livekitURL})
	return true
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
		dp := h.devPeers[list[i].ID]
		if dp == nil {
			continue
		}
		list[i].Online = true
		if r := h.rooms[h.peerRoom[dp]]; r != nil && h.webCount(r) > 0 {
			list[i].Busy = true
		}
	}
	var users []UserStatus
	for id, set := range h.webPeers {
		u := UserStatus{ID: id, Online: true}
		for wp := range set {
			if rid := h.peerRoom[wp]; rid != "" {
				u.Busy = true
				u.Room = rid
				break
			}
		}
		users = append(users, u)
	}
	var rooms []RoomStatus
	for _, r := range h.rooms {
		if len(r.members) == 0 {
			continue
		}
		rs := RoomStatus{ID: r.id, DeviceID: r.deviceID}
		if !r.started.IsZero() {
			t := r.started
			rs.Started = &t
		}
		for m, at := range r.members {
			rs.Members = append(rs.Members, MemberStatus{
				Address: m.Address(), Name: m.Name, Kind: m.Kind, JoinedAt: at,
			})
		}
		rooms = append(rooms, rs)
	}
	return Message{Type: TypePresence, Devices: list, Users: users, Rooms: rooms}
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
