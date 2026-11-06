package server

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/ScottL97/starbound-panel/internal/config"
)

// TestHandlerRegistersRoutes guards against net/http mux pattern conflicts,
// which panic at startup rather than at request time.
func TestHandlerRegistersRoutes(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		Listen:          ":0",
		DataDir:         root,
		ServerDir:       filepath.Join(root, "server"),
		StorageDir:      filepath.Join(root, "storage"),
		StateDir:        filepath.Join(root, "state"),
		BackupDir:       filepath.Join(root, "backups"),
		ModsDir:         filepath.Join(root, "server", "mods"),
		ModsDisabledDir: filepath.Join(root, "mods-disabled"),
		SteamCmd:        filepath.Join(root, "steamcmd", "steamcmd.sh"),
		SteamCmdDir:     filepath.Join(root, "steamcmd"),
		SteamCmdSeed:    filepath.Join(root, "seed"),
		AppID:           "211820",
		ServerBinary:    filepath.Join("linux", "starbound_server"),
		GamePort:        21025,
		LogBuffer:       100,
		Version:         "test",
	}
	if err := os.MkdirAll(cfg.ServerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if srv.Handler() == nil {
		t.Fatal("nil handler")
	}
}
