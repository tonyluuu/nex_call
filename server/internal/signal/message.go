// Package signal 实现设备与 Web 之间的房间信令。
package signal

import "time"

// 客户端发往服务端：join / call / leave / notify / kick / ping。
// 服务端发往客户端：registered / presence / joined / invite / notify / left / kicked / error / pong。
const (
	TypeJoin       = "join"
	TypeCall       = "call"
	TypeLeave      = "leave"
	TypeNotify     = "notify"
	TypeKick       = "kick"
	TypePing       = "ping"
	TypePong       = "pong"
	TypeRegistered = "registered"
	TypePresence   = "presence"
	TypeJoined     = "joined"
	TypeInvite     = "invite"
	TypeLeft       = "left"
	TypeKicked     = "kicked"
	TypeError      = "error"
)

const (
	ReasonNormal       = "normal"
	ReasonBusy         = "busy"
	ReasonOffline      = "offline"
	ReasonDisconnected = "disconnected"
	ReasonKicked       = "kicked"
	RoleAdmin          = "admin"
	RoleUser           = "user"
)

type Message struct {
	Type    string         `json:"type"`
	Room    string         `json:"room,omitempty"`
	Token   string         `json:"token,omitempty"`
	URL     string         `json:"url,omitempty"`
	From    string         `json:"from,omitempty"`
	To      string         `json:"to,omitempty"`
	Peer    string         `json:"peer,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Self    string         `json:"self,omitempty"`
	Devices []DeviceStatus `json:"devices,omitempty"`
	Users   []UserStatus   `json:"users,omitempty"`
	Rooms   []RoomStatus   `json:"rooms,omitempty"`
}

type DeviceStatus struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Online   bool       `json:"online"`
	Busy     bool       `json:"busy"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

type UserStatus struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
	Busy   bool   `json:"busy"`
	Room   string `json:"room,omitempty"`
}

type MemberStatus struct {
	Address  string    `json:"address"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	JoinedAt time.Time `json:"joinedAt"`
}

type RoomStatus struct {
	ID       string         `json:"id"`
	DeviceID string         `json:"deviceId,omitempty"`
	Members  []MemberStatus `json:"members"`
	Started  *time.Time     `json:"started,omitempty"`
}

// DeviceRoom 是设备常驻的 LiveKit 房间名。
func DeviceRoom(id string) string { return "device-" + id }
