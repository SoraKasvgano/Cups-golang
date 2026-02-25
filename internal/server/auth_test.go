package server

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"
)

func TestSetAuthChallengeNegotiate(t *testing.T) {
	rec := httptest.NewRecorder()
	setAuthChallenge(rec, "negotiate")
	if got := rec.Header().Get("WWW-Authenticate"); got != "Negotiate" {
		t.Fatalf("WWW-Authenticate = %q, want Negotiate", got)
	}
}

func TestAuthenticateNegotiateTrustedLoopbackHeader(t *testing.T) {
	s := newMoveTestServer(t)
	req := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	req.RemoteAddr = "127.0.0.1:63123"
	req.Header.Set("Authorization", "Negotiate dG9rZW4=")
	req.Header.Set("X-Remote-User", "alice")

	u, ok := s.authenticate(req, "negotiate")
	if !ok {
		t.Fatalf("authenticate negotiate failed")
	}
	if u.Username != "alice" {
		t.Fatalf("username = %q, want alice", u.Username)
	}
}

func TestAuthenticateNegotiateLoadsAdminFromStore(t *testing.T) {
	s := newMoveTestServer(t)
	ctx := context.Background()
	if err := s.Store.WithTx(ctx, false, func(tx *sql.Tx) error {
		return s.Store.CreateUser(ctx, tx, "bob", "secret", true)
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	req := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	req.RemoteAddr = "127.0.0.1:63123"
	req.Header.Set("Authorization", "Negotiate dG9rZW4=")
	req.Header.Set("X-Remote-User", "bob")

	u, ok := s.authenticate(req, "negotiate")
	if !ok {
		t.Fatalf("authenticate negotiate failed")
	}
	if !u.IsAdmin {
		t.Fatalf("expected IsAdmin=true for store user")
	}
}

func TestAuthenticateNegotiateRejectsNonLoopbackForwardedUser(t *testing.T) {
	s := newMoveTestServer(t)
	req := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	req.Header.Set("Authorization", "Negotiate dG9rZW4=")
	req.Header.Set("X-Remote-User", "mallory")

	if _, ok := s.authenticate(req, "negotiate"); ok {
		t.Fatalf("expected negotiate auth failure for untrusted forwarded user")
	}
}

func TestAuthenticateDefaultIncludesNegotiate(t *testing.T) {
	s := newMoveTestServer(t)
	req := httptest.NewRequest("POST", "http://localhost/ipp/print", nil)
	req.RemoteAddr = "127.0.0.1:63123"
	req.Header.Set("Authorization", "Negotiate dG9rZW4=")
	req.Header.Set("X-Remote-User", "alice")

	u, ok := s.authenticate(req, "")
	if !ok {
		t.Fatalf("expected default auth to accept negotiate")
	}
	if u.Username != "alice" {
		t.Fatalf("username = %q, want alice", u.Username)
	}
}
