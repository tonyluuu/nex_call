// Package auth 负责 Web 用户密码、会话令牌、设备密钥和 LiveKit Token。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidToken = errors.New("invalid token")

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// NewDeviceSecret 生成高熵随机密钥；只保存其 SHA-256，所以不需要慢哈希。
func NewDeviceSecret() (secret, hash string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(b)
	return secret, HashDeviceSecret(secret), nil
}

func HashDeviceSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func CheckDeviceSecret(hash, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(hash), []byte(HashDeviceSecret(secret))) == 1
}

// Sessions 用 HMAC 签名的无状态会话令牌：base64(username|expUnix).sig
type Sessions struct {
	key []byte
	ttl time.Duration
}

func NewSessions(secret string, ttl time.Duration) *Sessions {
	return &Sessions{key: []byte(secret), ttl: ttl}
}

func (s *Sessions) sign(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Sessions) Issue(username string) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(username + "|" + strconv.FormatInt(time.Now().Add(s.ttl).Unix(), 10)))
	return payload + "." + s.sign(payload)
}

func (s *Sessions) Verify(token string) (string, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return "", ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", ErrInvalidToken
	}
	user, expStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", ErrInvalidToken
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", ErrInvalidToken
	}
	return user, nil
}

type LiveKit struct {
	URL       string
	APIURL    string
	APIKey    string
	APISecret string
	TTL       time.Duration
}

// RoomToken 签发只能进入指定房间、只能发布麦克风的 Token。
func (l *LiveKit) RoomToken(room, identity, name string) (string, error) {
	grant := &auth.VideoGrant{RoomJoin: true, Room: room}
	grant.SetCanSubscribe(true)
	grant.SetCanPublishData(false)
	grant.SetCanPublishSources([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	ttl := l.TTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	t, err := auth.NewAccessToken(l.APIKey, l.APISecret).
		SetVideoGrant(grant).
		SetIdentity(identity).
		SetName(name).
		SetValidFor(ttl).
		ToJWT()
	if err != nil {
		return "", fmt.Errorf("livekit token: %w", err)
	}
	return t, nil
}

// RemoveParticipant 用 Room API 把参与者移出 LiveKit 房间。
func (l *LiveKit) RemoveParticipant(ctx context.Context, room, identity string) error {
	if l.APIURL == "" {
		return errors.New("LIVEKIT_API_URL not set")
	}
	token, err := auth.NewAccessToken(l.APIKey, l.APISecret).
		SetVideoGrant(&auth.VideoGrant{RoomCreate: true, RoomList: true, RoomAdmin: true, Room: room}).
		SetValidFor(time.Minute).
		ToJWT()
	if err != nil {
		return err
	}
	client := livekit.NewRoomServiceProtobufClient(l.APIURL, &http.Client{
		Timeout:   5 * time.Second,
		Transport: bearerTripper{token: token},
	})
	_, err = client.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
	if err != nil {
		return fmt.Errorf("remove participant: %w", err)
	}
	return nil
}

type bearerTripper struct {
	base  http.RoundTripper
	token string
}

func (b bearerTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}
