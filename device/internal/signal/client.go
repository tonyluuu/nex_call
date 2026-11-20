// Package signal 是设备到 call-server 的信令客户端，断线后指数退避重连。
package signal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Message 与服务端 server/internal/signal.Message 保持一致。
type Message struct {
	Type    string `json:"type"`
	CallID  string `json:"callId,omitempty"`
	Room    string `json:"room,omitempty"`
	Token   string `json:"token,omitempty"`
	URL     string `json:"url,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Self    string `json:"self,omitempty"`
	Speaker *int   `json:"speaker,omitempty"`
	Mic     *int   `json:"mic,omitempty"`
}

type Config struct {
	ServerURL string
	DeviceID  string
	Secret    string
	CACert    string
}

type Client struct {
	cfg        Config
	url        string
	httpClient *http.Client

	// Messages 收到的服务端消息；连接状态变化以 Type="_connected"/"_disconnected" 的消息通知
	Messages chan Message

	mu   sync.Mutex
	conn *websocket.Conn
}

const (
	EventConnected    = "_connected"
	EventDisconnected = "_disconnected"
)

func New(cfg Config) (*Client, error) {
	u := strings.TrimRight(cfg.ServerURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	hc, err := httpClient(cfg.CACert)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, url: u + "/ws/device", httpClient: hc, Messages: make(chan Message, 32)}, nil
}

// httpClient 指定 CA_CERT 时只信任该证书（自签名兜底时的证书固定），否则用系统根证书。
func httpClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return http.DefaultClient, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA_CERT: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA_CERT %s contains no certificate", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr}, nil
}

// Run 保持连接直到 ctx 结束。
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		slog.Warn("signal disconnected, retrying", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, c.url, &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.DeviceID + ":" + c.cfg.Secret}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("unauthorized: check DEVICE_ID / DEVICE_SECRET")
		}
		return err
	}
	conn.SetReadLimit(64 << 10)
	defer conn.CloseNow()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		c.deliver(ctx, Message{Type: EventDisconnected})
	}()

	slog.Info("signal connected", "url", c.url)
	c.deliver(ctx, Message{Type: EventConnected})

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()
	go c.keepalive(sessCtx)

	for {
		readCtx, readCancel := context.WithTimeout(sessCtx, 70*time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			return err
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Warn("bad signal message", "err", err)
			continue
		}
		if m.Type == "pong" {
			continue
		}
		c.deliver(ctx, m)
	}
}

func (c *Client) keepalive(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Send(Message{Type: "ping"})
		}
	}
}

func (c *Client) deliver(ctx context.Context, m Message) {
	select {
	case c.Messages <- m:
	case <-ctx.Done():
	}
}

// Send 发送一条消息；未连接时返回错误。
func (c *Client) Send(m Message) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("signal not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, m)
}
