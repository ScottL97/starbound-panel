package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateListPrune(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "storage")
	dir := filepath.Join(root, "backups")

	writeFile(t, filepath.Join(source, "universe", "a.world"), "world-bytes")
	writeFile(t, filepath.Join(source, "player", "b.player"), "player-bytes")
	// Starbound keeps its runtime configuration in the storage directory.
	writeFile(t, filepath.Join(source, "starbound_server.config"), `{"serverPort":21025}`)

	m := New(source, dir, logbuf.New(10))

	info, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.Size == 0 {
		t.Fatal("empty archive")
	}

	list, err := m.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != info.Name {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Verify the archive content.
	f, err := os.Open(filepath.Join(dir, info.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)

	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[hdr.Name] = true
	}
	for _, want := range []string{
		"storage/universe/a.world",
		"storage/player/b.player",
		"storage/starbound_server.config",
	} {
		if !found[want] {
			t.Fatalf("archive is missing %s (has %v)", want, found)
		}
	}

	// Prune keeps the newest entry only.
	if err := m.Prune(0); err != nil {
		t.Fatalf("prune with zero should be a no-op: %v", err)
	}
	if err := m.Delete(info.Name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ = m.List()
	if len(list) != 0 {
		t.Fatalf("backup not deleted: %+v", list)
	}
}

func TestPathValidation(t *testing.T) {
	m := New("", t.TempDir(), logbuf.New(10))
	for _, bad := range []string{"", "../x", "starbound-1.tar.gz", "starbound-20260101-000000.tar", "/etc/passwd"} {
		if _, err := m.Path(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
	if _, err := m.Path("starbound-20260101-000000.tar.gz"); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
}

func TestCreateWithoutStorage(t *testing.T) {
	root := t.TempDir()
	m := New(filepath.Join(root, "missing"), filepath.Join(root, "backups"), logbuf.New(10))
	if _, err := m.Create(); err != nil {
		t.Fatalf("backup without saves should still work: %v", err)
	}
}
