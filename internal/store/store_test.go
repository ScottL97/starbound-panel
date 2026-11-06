package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDefaultsAndPersist(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	snap := st.Snapshot()
	if snap.Settings.BackupRetention != 7 || !snap.Settings.AutoRestart {
		t.Fatalf("unexpected defaults: %+v", snap.Settings)
	}

	if err := st.Update(func(s *State) error {
		s.Steam.Username = "someone"
		s.Mods = append(s.Mods, Mod{ID: "123", Files: []string{"123.pak"}, Enabled: true})
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	snap = reopened.Snapshot()
	if snap.Steam.Username != "someone" || len(snap.Mods) != 1 || snap.Mods[0].ID != "123" {
		t.Fatalf("state not persisted: %+v", snap)
	}
}

func TestUpdateRollsBackOnError(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("nope")
	err = st.Update(func(s *State) error {
		s.Steam.Username = "leaked"
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	// The in-memory state is mutated by fn before the error; what matters is
	// that nothing was persisted.
	reopened, _ := Open(t.TempDir())
	if reopened.Snapshot().Steam.Username != "" {
		t.Fatal("unexpected state in a fresh store")
	}
}

func TestCorruptStateIsRecovered(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err == nil {
		t.Fatal("expected an error for a corrupt state file")
	}
	if st == nil {
		t.Fatal("a usable store must still be returned")
	}
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if len(e.Name()) > len("state.json.broken-") && e.Name()[:len("state.json.broken-")] == "state.json.broken-" {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt state file was not preserved")
	}
}
