package signal

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	writeTimeout = 10 * time.Second
	pingInterval = 25 * time.Second
	// 超过这个时间收不到任何消息（含应用层 ping）就认为连接已死
	readTimeout = 70 * time.Second
)

// Serve 把一条已鉴权的 HTTP 请求升级为 WebSocket 并收发信令，直到连接断开。
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, p *Peer, originPatterns []string) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: originPatterns})
	if err != nil {
		slog.Warn("websocket accept", "err", err)
		return
	}
	conn.SetReadLimit(16 << 10)
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	h.Register(p)
	defer func() {
		p.Close()
		h.Unregister(p)
	}()

	go h.writeLoop(ctx, conn, p, cancel)

	for {
		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
				slog.Info("websocket read ended", "peer", p.Address(), "err", err)
			}
			return
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			p.Send(Message{Type: TypeError, Reason: "invalid json"})
			continue
		}
		h.Handle(p, m)
	}
}

func (h *Hub) writeLoop(ctx context.Context, conn *websocket.Conn, p *Peer, cancel context.CancelFunc) {
	defer cancel()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.Done():
			conn.Close(websocket.StatusPolicyViolation, "replaced")
			return
		case m := <-p.Outbox():
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(wctx, conn, m)
			wcancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
