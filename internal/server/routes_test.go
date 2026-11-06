package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestAPITokenAuth covers the bearer token path used by automation.
func TestAPITokenAuth(t *testing.T) {
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
		APIToken:        "s3cret-token",
	}
	if err := os.MkdirAll(cfg.ServerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler := srv.Handler()

	get := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
	if code := get("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", code)
	}
	if code := get("s3cret-token"); code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", code)
	}

	// A write with the token must not need the CSRF header.
	req := httptest.NewRequest(http.MethodPost, "/api/server/stop", nil)
	req.Header.Set("Authorization", "Bearer s3cret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusConflict {
		t.Fatalf("token write: got %d", rec.Code)
	}
}
