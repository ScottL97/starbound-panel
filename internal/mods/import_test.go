package mods

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ScottL97/starbound-panel/internal/store"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	writer := zip.NewWriter(buf)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportSinglePak(t *testing.T) {
	m, _, enabled, _ := newTestManager(t)

	names, err := m.ImportFile("My Mod.pak", strings.NewReader("pak-data"), 8)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(names) != 1 || names[0] != "My Mod.pak" {
		t.Fatalf("unexpected names: %v", names)
	}
	if _, err := os.Stat(filepath.Join(enabled, "My Mod.pak")); err != nil {
		t.Fatalf("file not installed: %v", err)
	}
}

func TestImportZipFlattensAndSkipsOtherFiles(t *testing.T) {
	m, _, enabled, _ := newTestManager(t)

	archive := makeZip(t, map[string]string{
		"pack/one.pak":   "one",
		"pack/two.pak":   "two",
		"pack/readme.md": "ignore me",
	})

	names, err := m.ImportFile("bundle.zip", bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("import zip: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 paks, got %v", names)
	}
	for _, name := range []string{"one.pak", "two.pak"} {
		if _, err := os.Stat(filepath.Join(enabled, name)); err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(enabled, "readme.md")); !os.IsNotExist(err) {
		t.Fatal("non-pak file should not be installed")
	}
}

func TestImportRejectsTraversalAndEmptyArchives(t *testing.T) {
	m, _, enabled, _ := newTestManager(t)

	traversal := makeZip(t, map[string]string{"../evil.pak": "x"})
	names, err := m.ImportFile("evil.zip", bytes.NewReader(traversal), int64(len(traversal)))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// The entry is flattened to a plain name inside the mods directory.
	if len(names) != 1 || names[0] != "evil.pak" {
		t.Fatalf("unexpected names: %v", names)
	}
	if _, err := os.Stat(filepath.Join(enabled, "evil.pak")); err != nil {
		t.Fatalf("flattened file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(enabled), "evil.pak")); !os.IsNotExist(err) {
		t.Fatal("file escaped the mods directory")
	}

	empty := makeZip(t, map[string]string{"readme.txt": "nothing"})
	if _, err := m.ImportFile("empty.zip", bytes.NewReader(empty), int64(len(empty))); err == nil {
		t.Fatal("expected an error for an archive without .pak files")
	}
}

func TestImportRejectsManagedFile(t *testing.T) {
	m, st, _, _ := newTestManager(t)
	if err := st.Update(func(s *store.State) error {
		s.Mods = []store.Mod{{ID: "3485694504", Files: []string{"shared.pak"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ImportFile("shared.pak", strings.NewReader("x"), 1); err == nil {
		t.Fatal("expected the managed file to be rejected")
	}
}
