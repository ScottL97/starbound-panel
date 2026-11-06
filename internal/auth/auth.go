// Package auth implements the single administrator account and its sessions.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ScottL97/starbound-panel/internal/store"
)

const (
	scheme     = "pbkdf2-sha256"
	iterations = 210000
	keyLen     = 32
	saltLen    = 16
	sessionTTL = 7 * 24 * time.Hour
)

// ErrInvalidCredentials is returned for a wrong user name or password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrWeakPassword is returned when a new password is too short.
var ErrWeakPassword = errors.New("password must be at least 8 characters")

// ErrTooManyAttempts is returned while a client is rate limited.
var ErrTooManyAttempts = errors.New("too many failed attempts, try again later")

// Manager owns password hashing and session tokens.
type Manager struct {
	st *store.Store

	mu       sync.Mutex
	attempts map[string]*attempt
}

type attempt struct {
	fails int
	until time.Time
}

// New creates a manager backed by st.
func New(st *store.Store) *Manager {
	return &Manager{st: st, attempts: map[string]*attempt{}}
}

// HashPassword derives a PBKDF2-HMAC-SHA256 hash string.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s",
		scheme, iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func verifyHash(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SetupRequired reports whether the administrator account still has to be
// created (first start of a fresh installation).
func (m *Manager) SetupRequired() bool {
	return m.st.Snapshot().Admin.Hash == ""
}

// Setup creates the administrator account. It only works once.
func (m *Manager) Setup(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		username = "admin"
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return m.st.Update(func(s *store.State) error {
		if s.Admin.Hash != "" {
			return errors.New("administrator account already exists")
		}
		s.Admin = store.Admin{Username: username, Hash: hash, UpdatedAt: time.Now().UTC()}
		return nil
	})
}

// SeedAdmin creates the administrator account when SB_ADMIN_PASSWORD is set
// and no account exists yet.
func (m *Manager) SeedAdmin(username, password string) error {
	if password == "" || !m.SetupRequired() {
		return nil
	}
	return m.Setup(username, password)
}

// Authenticate checks the credentials and enforces a per-client rate limit.
func (m *Manager) Authenticate(username, password, clientKey string) error {
	m.mu.Lock()
	a := m.attempts[clientKey]
	if a != nil && time.Now().Before(a.until) {
		m.mu.Unlock()
		return ErrTooManyAttempts
	}
	m.mu.Unlock()

	snap := m.st.Snapshot()
	ok := snap.Admin.Hash != "" &&
		subtle.ConstantTimeCompare([]byte(snap.Admin.Username), []byte(username)) == 1 &&
		verifyHash(snap.Admin.Hash, password)

	if !ok {
		m.mu.Lock()
		if m.attempts[clientKey] == nil {
			m.attempts[clientKey] = &attempt{}
		}
		cur := m.attempts[clientKey]
		cur.fails++
		if cur.fails >= 5 {
			cur.until = time.Now().Add(30 * time.Second)
			cur.fails = 0
		}
		m.mu.Unlock()
		// Keep the response time roughly constant for wrong users/passwords.
		time.Sleep(300 * time.Millisecond)
		return ErrInvalidCredentials
	}

	m.mu.Lock()
	delete(m.attempts, clientKey)
	m.mu.Unlock()
	return nil
}

// NewSession creates a session token and returns it to the caller.
func (m *Manager) NewSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expiry := time.Now().Add(sessionTTL).UTC().Format(time.RFC3339)

	err := m.st.Update(func(s *store.State) error {
		now := time.Now().UTC()
		for key, exp := range s.Sessions {
			if t, err := time.Parse(time.RFC3339, exp); err != nil || now.After(t) {
				delete(s.Sessions, key)
			}
		}
		s.Sessions[tokenKey(token)] = expiry
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Valid reports whether token belongs to a live session.
func (m *Manager) Valid(token string) bool {
	if token == "" {
		return false
	}
	key := tokenKey(token)
	snap := m.st.Snapshot()
	exp, ok := snap.Sessions[key]
	if !ok {
		return false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil || time.Now().After(t) {
		_ = m.st.Update(func(s *store.State) error {
			delete(s.Sessions, key)
			return nil
		})
		return false
	}
	return true
}

// Destroy invalidates a session token.
func (m *Manager) Destroy(token string) {
	if token == "" {
		return
	}
	key := tokenKey(token)
	_ = m.st.Update(func(s *store.State) error {
		delete(s.Sessions, key)
		return nil
	})
}

// ChangePassword verifies the current password before setting a new one.
func (m *Manager) ChangePassword(current, next string) error {
	snap := m.st.Snapshot()
	if !verifyHash(snap.Admin.Hash, current) {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	return m.st.Update(func(s *store.State) error {
		s.Admin.Hash = hash
		s.Admin.UpdatedAt = time.Now().UTC()
		// Force a fresh login everywhere.
		s.Sessions = map[string]string{}
		return nil
	})
}

// Username returns the configured administrator name.
func (m *Manager) Username() string {
	return m.st.Snapshot().Admin.Username
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
