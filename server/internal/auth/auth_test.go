package auth

import (
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	s := NewSessions("secret-1", time.Hour)
	tok := s.Issue("alice", "sid-1")
	if u, sid, err := s.Verify(tok); err != nil || u != "alice" || sid != "sid-1" {
		t.Fatalf("verify: %q %q %v", u, sid, err)
	}
	if _, _, err := NewSessions("secret-2", time.Hour).Verify(tok); err == nil {
		t.Fatal("token signed with another key accepted")
	}
	if _, _, err := s.Verify(tok + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	expired := NewSessions("secret-1", -time.Minute).Issue("alice", "sid-1")
	if _, _, err := s.Verify(expired); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestLegacyTokenRejected(t *testing.T) {
	s := NewSessions("secret-1", time.Hour)
	payload := base64.RawURLEncoding.EncodeToString([]byte("alice|" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)))
	tok := payload + "." + s.sign(payload)
	if _, _, err := s.Verify(tok); err == nil {
		t.Fatal("legacy token accepted")
	}
}

func TestDeviceSecret(t *testing.T) {
	secret, hash, err := NewDeviceSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !CheckDeviceSecret(hash, secret) || CheckDeviceSecret(hash, secret+"x") {
		t.Fatal("device secret check wrong")
	}
}

func TestShareRoundTrip(t *testing.T) {
	a, err := SealShare("张三", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealShare("张三", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("same plaintext produced the same ciphertext")
	}
	user, gen, err := OpenShare(a)
	if err != nil || user != "张三" || gen != 1 {
		t.Fatalf("open: %q %d %v", user, gen, err)
	}
	unlimited, err := SealShare("张三", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenShare(unlimited); err != nil {
		t.Fatal(err)
	}
	if _, err := SealShare("张三", 1, -1); !errors.Is(err, ErrInvalidShareDays) {
		t.Fatalf("negative days: %v", err)
	}
	if _, err := SealShare("张三", 0, 1); err == nil {
		t.Fatal("zero generation accepted")
	}
	if _, _, err := OpenShare(a + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, _, err := OpenShare("not-a-token"); err == nil {
		t.Fatal("garbage token accepted")
	}
	expired, err := SealShareUntil("张三", 1, time.Now().Unix()-10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenShare(expired); !errors.Is(err, ErrShareExpired) {
		t.Fatalf("expired: %v", err)
	}
	legacy, err := sealPlain([]byte(`{"u":"张三","p":"password123","e":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenShare(legacy); err == nil {
		t.Fatal("legacy token without generation accepted")
	}
}

func TestRoomToken(t *testing.T) {
	lk := &LiveKit{APIKey: "key", APISecret: "secretsecretsecretsecretsecretsecret"}
	tok, err := lk.RoomToken("c_1", "web:alice", "alice")
	if err != nil || tok == "" {
		t.Fatalf("token: %v", err)
	}
}
