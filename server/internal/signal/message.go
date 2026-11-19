// Package signal 实现设备与 Web 之间的呼叫信令。
package signal

import "time"

// 消息类型。客户端发往服务端：call / ringing / accept / reject / hangup / ping；
// 服务端发往客户端：registered / presence / outgoing / invite / ringing / accepted / hangup / error / pong。
const (
	TypeCall       = "call"
	TypeRinging    = "ringing"
	TypeAccept     = "accept"
	TypeReject     = "reject"
	TypeHangup     = "hangup"
	TypePing       = "ping"
	TypePong       = "pong"
	TypeRegistered = "registered"
	TypePresence   = "presence"
	TypeOutgoing   = "outgoing"
	TypeInvite     = "invite"
	TypeAccepted   = "accepted"
	TypeError      = "error"
)

// 挂断原因
const (
	ReasonNormal       = "normal"
	ReasonRejected     = "rejected"
	ReasonBusy         = "busy"
	ReasonTimeout      = "timeout"
	ReasonOffline      = "offline"
	ReasonDisconnected = "disconnected"
	ReasonElsewhere    = "answered_elsewhere"
	ReasonCancelled    = "cancelled"
)

type Message struct {
	Type    string         `json:"type"`
	CallID  string         `json:"callId,omitempty"`
	Room    string         `json:"room,omitempty"`
	Token   string         `json:"token,omitempty"`
	URL     string         `json:"url,omitempty"`
	From    string         `json:"from,omitempty"`
	To      string         `json:"to,omitempty"`
	Peer    string         `json:"peer,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Self    string         `json:"self,omitempty"`
	Devices []DeviceStatus `json:"devices,omitempty"`
}

type DeviceStatus struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Online   bool       `json:"online"`
	Busy     bool       `json:"busy"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}
