package mods

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// maxImportFiles caps how many .pak files a single zip may contain.
	maxImportFiles = 200
	// maxImportBytes caps the uncompressed size extracted from one zip.
	maxImportBytes = 4 << 30
)

// ImportFile installs an uploaded .pak or .zip into the enabled mods
// directory and returns the installed file names. Zip archives may contain
// .pak files in subdirectories; they are flattened into the mods directory.
func (m *Manager) ImportFile(name string, src io.Reader, size int64) ([]string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".pak":
		base, err := safePakName(filepath.Base(name))
		if err != nil {
			return nil, err
		}
		if err := m.installStream(base, src, size); err != nil {
			return nil, err
		}
		m.bus.Printf("panel", "uploaded mod file %s; restart the server to apply", base)
		return []string{base}, nil

	case ".zip":
		return m.importZip(name, src)

	default:
		return nil, fmt.Errorf("%w: only .pak and .zip uploads are supported", ErrInvalidFile)
	}
}

func (m *Manager) importZip(name string, src io.Reader) ([]string, error) {
	tmp, err := os.CreateTemp("", "sb-upload-*.zip")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	archive, err := zip.OpenReader(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	defer archive.Close()

	// Collect the .pak entries first so a bad archive is rejected before
	// anything is written.
	type entry struct {
		file *zip.File
		name string
	}
	entries := make([]entry, 0)
	seen := map[string]bool{}
	var total uint64
	for _, file := range archive.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if !strings.EqualFold(path.Ext(file.Name), ".pak") {
			continue
		}
		base := path.Base(strings.ReplaceAll(file.Name, `\`, "/"))
		base, err = safePakName(base)
		if err != nil {
			return nil, err
		}
		if seen[base] {
			return nil, fmt.Errorf("zip contains %s more than once", base)
		}
		seen[base] = true
		total += file.UncompressedSize64
		if total > maxImportBytes {
			return nil, errors.New("zip is too large once extracted")
		}
		entries = append(entries, entry{file: file, name: base})
		if len(entries) > maxImportFiles {
			return nil, fmt.Errorf("zip contains more than %d .pak files", maxImportFiles)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s contains no .pak files", filepath.Base(name))
	}

	installed := make([]string, 0, len(entries))
	for _, item := range entries {
		reader, err := item.file.Open()
		if err != nil {
			return installed, err
		}
		err = m.installStream(item.name, reader, int64(item.file.UncompressedSize64))
		reader.Close()
		if err != nil {
			return installed, err
		}
		installed = append(installed, item.name)
	}

	m.bus.Printf("panel", "uploaded %d mod file(s) from %s; restart the server to apply", len(installed), filepath.Base(name))
	return installed, nil
}

// installStream writes a .pak into the enabled directory via a temp file, so a
// failed upload never leaves a half written mod behind.
func (m *Manager) installStream(name string, src io.Reader, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, mod := range m.st.Snapshot().Mods {
		for _, file := range mod.Files {
			if file == name {
				return fmt.Errorf("%s belongs to managed workshop item %s; unsubscribe it first", name, mod.ID)
			}
		}
	}

	if err := os.MkdirAll(m.enabledDir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(m.enabledDir, ".upload-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	written, err := io.Copy(tmp, io.LimitReader(src, maxImportBytes))
	if err != nil {
		tmp.Close()
		return err
	}
	if size > 0 && written != size {
		tmp.Close()
		return fmt.Errorf("upload of %s was truncated", name)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	target := filepath.Join(m.enabledDir, name)
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	return os.Chmod(target, 0o644)
}
