package auth

import (
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	s := NewSessions("secret-1", time.Hour)
	tok := s.Issue("alice")
	if u, err := s.Verify(tok); err != nil || u != "alice" {
		t.Fatalf("verify: %q %v", u, err)
	}
	if _, err := NewSessions("secret-2", time.Hour).Verify(tok); err == nil {
		t.Fatal("token signed with another key accepted")
	}
	if _, err := s.Verify(tok + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	expired := NewSessions("secret-1", -time.Minute).Issue("alice")
	if _, err := s.Verify(expired); err == nil {
		t.Fatal("expired token accepted")
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

func TestRoomToken(t *testing.T) {
	lk := &LiveKit{APIKey: "key", APISecret: "secretsecretsecretsecretsecretsecret"}
	tok, err := lk.RoomToken("c_1", "web:alice", "alice")
	if err != nil || tok == "" {
		t.Fatalf("token: %v", err)
	}
}
