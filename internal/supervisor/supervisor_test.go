//go:build !windows

package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestSupervisorLifecycle(t *testing.T) {
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	if err := os.MkdirAll(filepath.Join(serverDir, "linux"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
echo "cwd: $(pwd)"
echo "[Info] UniverseServer: Client 'Tester' <1> (10.0.0.1) connected"
while IFS= read -r line; do
  echo "[Info] Chat: <Tester> $line"
done
`
	binary := filepath.Join(serverDir, "linux", "starbound_server")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	bus := logbuf.New(100)
	sup := New(Config{
		Dir:        serverDir,
		WorkDir:    filepath.Join(serverDir, "linux"),
		StorageDir: filepath.Join(root, "storage"),
		HomeDir:    filepath.Join(root, "home"),
		Binary:     filepath.Join("linux", "starbound_server"),
	}, bus, Hooks{}, nil)

	if !sup.Installed() {
		t.Fatal("server should be reported as installed")
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop() })

	waitFor(t, 3*time.Second, func() bool { return sup.Status().Running })
	waitFor(t, 3*time.Second, func() bool { return len(sup.Players()) == 1 })

	if err := sup.Command("/say hello"); err != nil {
		t.Fatalf("command: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		for _, line := range bus.Recent(50) {
			if strings.Contains(line.Text, "Chat: <Tester> /say hello") {
				return true
			}
		}
		return false
	})

	// The storage symlink must point at the persistent directory.
	target, err := os.Readlink(filepath.Join(serverDir, "storage"))
	if err != nil {
		t.Fatalf("storage symlink: %v", err)
	}
	if target != filepath.Join(root, "storage") {
		t.Fatalf("unexpected storage target %q", target)
	}

	// Starbound reads sbinit.config from the working directory, so the process
	// must not run from the installation root.
	found := false
	for _, line := range bus.Recent(100) {
		if strings.HasPrefix(line.Text, "cwd: ") {
			found = true
			if line.Text != "cwd: "+filepath.Join(serverDir, "linux") {
				t.Fatalf("unexpected working directory: %q", line.Text)
			}
		}
	}
	if !found {
		t.Fatal("the server did not report its working directory")
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if sup.Status().Running {
		t.Fatal("server still running after stop")
	}
	if got := sup.Players(); len(got) != 0 {
		t.Fatalf("players not cleared after stop: %+v", got)
	}
}

func TestStartFailsWhenNotInstalled(t *testing.T) {
	sup := New(Config{
		Dir:        t.TempDir(),
		StorageDir: filepath.Join(t.TempDir(), "storage"),
		HomeDir:    filepath.Join(t.TempDir(), "home"),
		Binary:     filepath.Join("linux", "starbound_server"),
	}, logbuf.New(10), Hooks{}, nil)

	if err := sup.Start(); err != ErrNotInstalled {
		t.Fatalf("expected ErrNotInstalled, got %v", err)
	}
	if err := sup.Stop(); err != ErrNotRunning {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
	if err := sup.Command("/say hi"); err != ErrNotRunning {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}
