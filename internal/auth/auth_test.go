package auth

import (
	"errors"
	"testing"

	"github.com/ScottL97/starbound-panel/internal/store"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(st)
}

func TestHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !verifyHash(hash, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if verifyHash(hash, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("expected ErrWeakPassword, got %v", err)
	}
}

func TestSetupAndLogin(t *testing.T) {
	m := newManager(t)

	if !m.SetupRequired() {
		t.Fatal("fresh store should require setup")
	}
	if err := m.Setup("admin", "hunter2hunter2"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if m.SetupRequired() {
		t.Fatal("setup should be complete")
	}
	if err := m.Setup("admin", "another-password"); err == nil {
		t.Fatal("second setup must fail")
	}

	if err := m.Authenticate("admin", "hunter2hunter2", "127.0.0.1"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := m.Authenticate("admin", "nope", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	m := newManager(t)
	if err := m.Setup("admin", "hunter2hunter2"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	token, err := m.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if !m.Valid(token) {
		t.Fatal("fresh session should be valid")
	}
	if m.Valid("garbage") {
		t.Fatal("garbage token accepted")
	}

	m.Destroy(token)
	if m.Valid(token) {
		t.Fatal("destroyed session still valid")
	}
}

func TestChangePasswordInvalidatesSessions(t *testing.T) {
	m := newManager(t)
	if err := m.Setup("admin", "hunter2hunter2"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	token, _ := m.NewSession()

	if err := m.ChangePassword("wrong", "new-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	if err := m.ChangePassword("hunter2hunter2", "new-password-1"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if m.Valid(token) {
		t.Fatal("sessions must be invalidated after a password change")
	}
	if err := m.Authenticate("admin", "new-password-1", "10.0.0.1"); err != nil {
		t.Fatalf("authenticate with new password: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	m := newManager(t)
	if err := m.Setup("admin", "hunter2hunter2"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for i := 0; i < 5; i++ {
		_ = m.Authenticate("admin", "bad", "1.2.3.4")
	}
	if err := m.Authenticate("admin", "hunter2hunter2", "1.2.3.4"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("expected rate limit, got %v", err)
	}
}
