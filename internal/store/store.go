// Package store persists the panel state as a single JSON document.
//
// The state file contains the administrator password hash, the Steam
// credentials used by steamcmd, the mod registry and the panel settings. It is
// written with 0600 and must therefore live on a private volume.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CurrentVersion is the schema version written to new state files.
const CurrentVersion = 1

// State is the whole persisted panel state.
type State struct {
	Version  int               `json:"version"`
	Admin    Admin             `json:"admin"`
	Sessions map[string]string `json:"sessions"`
	Steam    Steam             `json:"steam"`
	Settings Settings          `json:"settings"`
	Mods     []Mod             `json:"mods"`
}

// Admin is the single administrator account of the panel.
type Admin struct {
	Username  string    `json:"username"`
	Hash      string    `json:"hash"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Steam holds the account steamcmd logs in with. Starbound dedicated servers
// cannot be downloaded anonymously, so these are required before installing.
type Steam struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	GuardCode string `json:"guardCode,omitempty"`
	// WebAPIKey is an optional Steam Web API key used to search the Workshop.
	WebAPIKey string `json:"webApiKey,omitempty"`
}

// Settings are the user-tunable panel options.
type Settings struct {
	AutoRestart       bool `json:"autoRestart"`
	StopTimeoutSec    int  `json:"stopTimeoutSec"`
	BackupEnabled     bool `json:"backupEnabled"`
	BackupIntervalHrs int  `json:"backupIntervalHours"`
	BackupRetention   int  `json:"backupRetention"`

	// ServerFlavor is "vanilla" (SteamCMD) or "openstarbound".
	ServerFlavor string `json:"serverFlavor"`
	// OpenStarboundTag is the requested release ("latest" or a pinned tag).
	OpenStarboundTag string `json:"openStarboundTag"`
	// OpenStarboundInstalled is the tag that is actually on disk.
	OpenStarboundInstalled string `json:"openStarboundInstalled"`
}

// Mod is one Steam Workshop item managed by the panel.
type Mod struct {
	ID      string    `json:"id"`
	Files   []string  `json:"files"`
	Enabled bool      `json:"enabled"`
	AddedAt time.Time `json:"addedAt"`
}

// Default returns the state used for a fresh installation.
func Default() State {
	return State{
		Version:  CurrentVersion,
		Sessions: map[string]string{},
		Settings: Settings{
			AutoRestart:       true,
			StopTimeoutSec:    90,
			BackupEnabled:     true,
			BackupIntervalHrs: 24,
			BackupRetention:   7,
			ServerFlavor:      "vanilla",
			OpenStarboundTag:  "latest",
		},
	}
}

// Clone returns a deep copy so callers can read a consistent snapshot without
// holding the store lock.
func (s State) Clone() State {
	c := s
	c.Sessions = make(map[string]string, len(s.Sessions))
	for k, v := range s.Sessions {
		c.Sessions[k] = v
	}
	c.Mods = make([]Mod, len(s.Mods))
	for i, m := range s.Mods {
		c.Mods[i] = m
		c.Mods[i].Files = append([]string(nil), m.Files...)
	}
	return c
}

func (s *State) normalize() {
	if s.Sessions == nil {
		s.Sessions = map[string]string{}
	}
	if s.Version == 0 {
		s.Version = CurrentVersion
	}
	if s.Settings.StopTimeoutSec <= 0 {
		s.Settings.StopTimeoutSec = 90
	}
	if s.Settings.BackupIntervalHrs <= 0 {
		s.Settings.BackupIntervalHrs = 24
	}
	if s.Settings.BackupRetention <= 0 {
		s.Settings.BackupRetention = 7
	}
	if s.Settings.ServerFlavor == "" {
		s.Settings.ServerFlavor = "vanilla"
	}
	if s.Settings.OpenStarboundTag == "" {
		s.Settings.OpenStarboundTag = "latest"
	}
}

// Store is a concurrency-safe, atomically persisted State.
type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

// Open loads (or initialises) the state file inside dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &Store{path: filepath.Join(dir, "state.json"), state: Default()}

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		// A corrupt state file must not brick the panel: keep a copy for
		// forensics and start over.
		broken := s.path + ".broken-" + time.Now().Format("20060102-150405")
		_ = os.Rename(s.path, broken)
		s.state = Default()
		return s, fmt.Errorf("state was corrupt, moved to %s: %w", broken, err)
	}
	s.state.normalize()
	return s, nil
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Clone()
}

// Update applies fn under the write lock and persists the result.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.state); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}
