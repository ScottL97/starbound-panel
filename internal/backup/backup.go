// Package backup creates and rotates tar.gz archives of the server saves.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

// ErrBusy is returned when a backup is already running.
var ErrBusy = errors.New("a backup is already running")

var reName = regexp.MustCompile(`^starbound-[0-9]{8}-[0-9]{6}\.tar\.gz$`)

// Info describes one backup archive.
type Info struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// Manager writes backups of sourceDir into dir.
//
// The source is the whole storage directory, which also contains
// starbound_server.config: Starbound resolves the runtime configuration
// relative to its storage directory.
type Manager struct {
	sourceDir string
	dir       string
	bus       *logbuf.Bus

	mu      sync.Mutex
	running bool
}

// New creates a manager.
func New(sourceDir, dir string, bus *logbuf.Bus) *Manager {
	return &Manager{sourceDir: sourceDir, dir: dir, bus: bus}
}

// Running reports whether a backup is in progress.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// List returns the available backups, newest first.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}

	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !reName.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Create writes a new archive and returns its metadata.
func (m *Manager) Create() (Info, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return Info{}, ErrBusy
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return Info{}, err
	}

	name := "starbound-" + time.Now().Format("20060102-150405") + ".tar.gz"
	final := filepath.Join(m.dir, name)
	tmp := filepath.Join(m.dir, "."+name+".tmp")

	start := time.Now()
	m.bus.Printf("panel", "creating backup %s", name)
	if err := m.writeArchive(tmp); err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}

	m.bus.Printf("panel", "backup %s written (%s) in %s",
		name, humanSize(fi.Size()), time.Since(start).Round(time.Second))

	return Info{Name: name, Size: fi.Size(), Created: fi.ModTime()}, nil
}

// Delete removes one backup by name.
func (m *Manager) Delete(name string) error {
	path, err := m.Path(name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// Path validates name and returns its absolute path.
func (m *Manager) Path(name string) (string, error) {
	if !reName.MatchString(name) {
		return "", errors.New("invalid backup name")
	}
	return filepath.Join(m.dir, name), nil
}

// Prune keeps only the newest keep archives.
func (m *Manager) Prune(keep int) error {
	if keep <= 0 {
		return nil
	}
	list, err := m.List()
	if err != nil {
		return err
	}
	for i := keep; i < len(list); i++ {
		if err := os.Remove(filepath.Join(m.dir, list[i].Name)); err != nil {
			return err
		}
		m.bus.Printf("panel", "pruned old backup %s", list[i].Name)
	}
	return nil
}

// Newest returns the creation time of the most recent backup, if any.
func (m *Manager) Newest() (time.Time, bool) {
	list, err := m.List()
	if err != nil || len(list) == 0 {
		return time.Time{}, false
	}
	return list[0].Created, true
}

func (m *Manager) writeArchive(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := m.addTree(tw, m.sourceDir, "storage"); err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		return err
	}

	if err := tw.Close(); err != nil {
		gz.Close()
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (m *Manager) addTree(tw *tar.Writer, root, prefix string) error {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Sockets, devices and symlinks are not restorable save data.
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
