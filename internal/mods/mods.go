// Package mods manages Starbound mods (.pak files) sourced from the Steam
// Workshop.
//
// Starbound loads every .pak in <server>/mods, so "disabling" a mod means
// moving its files out of that directory. The registry in the panel state
// records which Workshop ids are managed and where their files live.
package mods

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
	"github.com/ScottL97/starbound-panel/internal/steamcmd"
	"github.com/ScottL97/starbound-panel/internal/store"
)

// ErrNotFound is returned when a mod id is not managed by the panel.
var ErrNotFound = errors.New("mod is not managed by the panel")

// ErrInvalidFile is returned for a file name that is not a plain .pak name.
var ErrInvalidFile = errors.New("invalid mod file name")

var (
	reWorkshopID = regexp.MustCompile(`^[0-9]{1,20}$`)
	reLeadingID  = regexp.MustCompile(`^([0-9]{6,20})`)
)

// Manager owns the mod registry and the mod directories.
type Manager struct {
	st          *store.Store
	enabledDir  string
	disabledDir string
	runner      *steamcmd.Runner
	bus         *logbuf.Bus

	mu sync.Mutex
}

// New creates a manager.
func New(st *store.Store, enabledDir, disabledDir string, runner *steamcmd.Runner, bus *logbuf.Bus) *Manager {
	return &Manager{
		st:          st,
		enabledDir:  enabledDir,
		disabledDir: disabledDir,
		runner:      runner,
		bus:         bus,
	}
}

// List returns the managed mods.
func (m *Manager) List() []store.Mod {
	return m.st.Snapshot().Mods
}

// External is a .pak file that was put into mods/ by hand (for example copied
// from a Steam Workshop download) and is not managed by the panel.
type External struct {
	File     string    `json:"file"`
	Enabled  bool      `json:"enabled"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// ID is the numeric prefix of the file name, when it looks like a Workshop id.
	ID string `json:"id,omitempty"`
}

// Scan lists unmanaged .pak files in the mods directories.
func (m *Manager) Scan() []External {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scanLocked()
}

func (m *Manager) scanLocked() []External {
	managed := map[string]bool{}
	for _, mod := range m.st.Snapshot().Mods {
		for _, name := range mod.Files {
			managed[name] = true
		}
	}

	out := make([]External, 0)
	for _, dir := range []struct {
		path    string
		enabled bool
	}{
		{m.enabledDir, true},
		{m.disabledDir, false},
	} {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || managed[name] || !strings.EqualFold(filepath.Ext(name), ".pak") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			item := External{
				File:     name,
				Enabled:  dir.enabled,
				Size:     info.Size(),
				Modified: info.ModTime(),
			}
			if match := reLeadingID.FindString(name); match != "" {
				item.ID = match
			}
			out = append(out, item)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// SetExternalEnabled moves an unmanaged .pak between the two mod directories.
func (m *Manager) SetExternalEnabled(file string, enabled bool) error {
	name, err := safePakName(file)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Enabling moves files out of the disabled directory, disabling moves them
	// back into it.
	from, to := m.disabledDir, m.enabledDir
	if !enabled {
		from, to = m.enabledDir, m.disabledDir
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	if err := moveFile(filepath.Join(from, name), filepath.Join(to, name)); err != nil {
		return err
	}
	m.bus.Printf("panel", "mod file %s %s; restart the server to apply", name, map[bool]string{true: "enabled", false: "disabled"}[enabled])
	return nil
}

// RemoveExternal deletes an unmanaged .pak file.
func (m *Manager) RemoveExternal(file string) error {
	name, err := safePakName(file)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, dir := range []string{m.enabledDir, m.disabledDir} {
		err := os.Remove(filepath.Join(dir, name))
		if err == nil {
			m.bus.Printf("panel", "deleted unmanaged mod file %s", name)
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.ErrNotExist
}

// safePakName rejects path components so a request cannot escape the mod dirs.
func safePakName(file string) (string, error) {
	name := strings.TrimSpace(file)
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", ErrInvalidFile
	}
	if !strings.EqualFold(filepath.Ext(name), ".pak") {
		return "", fmt.Errorf("%w: only .pak files can be managed", ErrInvalidFile)
	}
	return name, nil
}

// Add downloads a Workshop item and installs its .pak files as enabled.
func (m *Manager) Add(ctx context.Context, id string, creds steamcmd.Credentials, installDir string) error {
	id = strings.TrimSpace(id)
	if !reWorkshopID.MatchString(id) {
		return errors.New("workshop id must be numeric")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, mod := range m.st.Snapshot().Mods {
		if mod.ID == id {
			return fmt.Errorf("workshop item %s is already managed", id)
		}
	}

	if err := m.runner.DownloadWorkshop(ctx, creds, installDir, []string{id}); err != nil {
		return err
	}

	files, err := m.copyInto(m.runner.WorkshopDir(id), id)
	if err != nil {
		return err
	}

	m.bus.Printf("panel", "installed workshop item %s (%d pak file(s)); restart the server to load it", id, len(files))
	return m.st.Update(func(s *store.State) error {
		s.Mods = append(s.Mods, store.Mod{
			ID:      id,
			Files:   files,
			Enabled: true,
			AddedAt: time.Now().UTC(),
		})
		return nil
	})
}

// SetEnabled moves the mod files in or out of the game's mods directory.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var mod *store.Mod
	snap := m.st.Snapshot()
	for i := range snap.Mods {
		if snap.Mods[i].ID == id {
			mod = &snap.Mods[i]
			break
		}
	}
	if mod == nil {
		return ErrNotFound
	}
	if mod.Enabled == enabled {
		return nil
	}

	// Enabling moves files out of the disabled directory, disabling moves them
	// back into it.
	from, to := m.disabledDir, m.enabledDir
	if !enabled {
		from, to = m.enabledDir, m.disabledDir
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, name := range mod.Files {
		if err := moveFile(filepath.Join(from, name), filepath.Join(to, name)); err != nil {
			return err
		}
	}

	m.bus.Printf("panel", "mod %s %s; restart the server to apply", id, map[bool]string{true: "enabled", false: "disabled"}[enabled])
	return m.st.Update(func(s *store.State) error {
		for i := range s.Mods {
			if s.Mods[i].ID == id {
				s.Mods[i].Enabled = enabled
			}
		}
		return nil
	})
}

// Remove deletes the mod files and its registry entry.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var mod *store.Mod
	snap := m.st.Snapshot()
	for i := range snap.Mods {
		if snap.Mods[i].ID == id {
			mod = &snap.Mods[i]
			break
		}
	}
	if mod == nil {
		return ErrNotFound
	}

	dir := m.enabledDir
	if !mod.Enabled {
		dir = m.disabledDir
	}
	for _, name := range mod.Files {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	m.bus.Printf("panel", "removed mod %s", id)
	return m.st.Update(func(s *store.State) error {
		out := s.Mods[:0]
		for _, item := range s.Mods {
			if item.ID != id {
				out = append(out, item)
			}
		}
		s.Mods = out
		return nil
	})
}

// Reconcile verifies at start-up that the registered files still exist.
func (m *Manager) Reconcile() {
	if err := os.MkdirAll(m.enabledDir, 0o755); err != nil {
		m.bus.Printf("panel", "cannot create mods directory: %v", err)
		return
	}
	for _, mod := range m.st.Snapshot().Mods {
		dir := m.enabledDir
		if !mod.Enabled {
			dir = m.disabledDir
		}
		for _, name := range mod.Files {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				m.bus.Printf("panel", "warning: mod %s file %s is missing", mod.ID, name)
			}
		}
	}
}

// copyInto copies the .pak files of a Workshop item into the enabled dir.
func (m *Manager) copyInto(src, id string) ([]string, error) {
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("workshop item %s was not downloaded: %w", id, err)
	}

	var paks []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".pak") {
			return nil
		}
		paks = append(paks, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paks) == 0 {
		return nil, fmt.Errorf("workshop item %s contains no .pak files", id)
	}

	if err := os.MkdirAll(m.enabledDir, 0o755); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(paks))
	for _, pak := range paks {
		name := id + ".pak"
		if len(paks) > 1 {
			rel, err := filepath.Rel(src, pak)
			if err != nil {
				return nil, err
			}
			name = id + "-" + strings.ReplaceAll(filepath.ToSlash(rel), "/", "-")
		}
		if err := copyFile(pak, filepath.Join(m.enabledDir, name)); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}
