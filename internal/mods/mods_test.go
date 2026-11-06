package mods

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
	"github.com/ScottL97/starbound-panel/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store, string, string) {
	t.Helper()
	root := t.TempDir()
	enabled := filepath.Join(root, "mods")
	disabled := filepath.Join(root, "mods-disabled")
	if err := os.MkdirAll(enabled, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(disabled, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(st, enabled, disabled, nil, logbuf.New(10)), st, enabled, disabled
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("pak"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSkipsManagedFiles(t *testing.T) {
	m, st, enabled, disabled := newTestManager(t)

	write(t, filepath.Join(enabled, "3485694504.pak"))
	write(t, filepath.Join(enabled, "2056569194.pak"))
	write(t, filepath.Join(disabled, "2469333879.pak"))
	write(t, filepath.Join(enabled, "readme.txt"))

	if err := st.Update(func(s *store.State) error {
		s.Mods = []store.Mod{{ID: "3485694504", Files: []string{"3485694504.pak"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	list := m.Scan()
	if len(list) != 2 {
		t.Fatalf("expected 2 unmanaged files, got %+v", list)
	}

	byFile := map[string]External{}
	for _, item := range list {
		byFile[item.File] = item
	}
	if got := byFile["2056569194.pak"]; !got.Enabled || got.ID != "2056569194" || got.Size == 0 {
		t.Fatalf("unexpected 222: %+v", got)
	}
	if got := byFile["2469333879.pak"]; got.Enabled || got.ID != "2469333879" {
		t.Fatalf("unexpected 333: %+v", got)
	}
}

func TestExternalEnableDisableDelete(t *testing.T) {
	m, _, enabled, disabled := newTestManager(t)
	write(t, filepath.Join(enabled, "2056569194.pak"))
	write(t, filepath.Join(disabled, "2469333879.pak"))

	if err := m.SetExternalEnabled("2056569194.pak", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disabled, "2056569194.pak")); err != nil {
		t.Fatalf("file not moved to disabled: %v", err)
	}

	if err := m.SetExternalEnabled("2056569194.pak", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(enabled, "2056569194.pak")); err != nil {
		t.Fatalf("file not moved back: %v", err)
	}

	if err := m.RemoveExternal("2469333879.pak"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disabled, "2469333879.pak")); !os.IsNotExist(err) {
		t.Fatal("file was not deleted")
	}
}

func TestExternalRejectsUnsafeNames(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	for _, name := range []string{"", "../evil.pak", "sub/evil.pak", `sub\evil.pak`, "not-a-pak.txt"} {
		if err := m.SetExternalEnabled(name, true); !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("expected %q to be rejected, got %v", name, err)
		}
		if err := m.RemoveExternal(name); !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("expected %q to be rejected on delete, got %v", name, err)
		}
	}
}
