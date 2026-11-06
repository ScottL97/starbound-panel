//go:build !windows

package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

func makeTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarWritesFiles(t *testing.T) {
	dest := t.TempDir()
	data := makeTar(t, map[string]string{
		"linux/starbound_server": "binary",
		"assets/opensb.pak":      "assets",
	})

	if err := extractTar(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "linux", "starbound_server"))
	if err != nil || string(got) != "binary" {
		t.Fatalf("unexpected file: %q %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dest, "linux", "starbound_server"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary is not executable: %v", fi.Mode())
	}
}

func TestExtractTarRejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{"../evil.txt", "../../etc/passwd", "/abs/evil"} {
		data := makeTar(t, map[string]string{name: "x"})
		if err := extractTar(bytes.NewReader(data), dest); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
		t.Fatal("traversal wrote outside the destination")
	}
}

func TestResolvePicksArchAsset(t *testing.T) {
	rel := githubRelease{
		TagName:     "v0.1.15.1",
		PublishedAt: time.Now(),
		Assets: []struct {
			Name               string `json:"name"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: "OpenStarbound-Linux-Clang-Server.tar.lz", Size: 10, BrowserDownloadURL: "https://example.test/amd64"},
			{Name: "OpenStarbound-Linux-ARM-Clang-Server.tar.lz", Size: 20, BrowserDownloadURL: "https://example.test/arm64"},
		},
	}

	amd := New(t.TempDir(), t.TempDir(), logbuf.New(10))
	amd.Arch = "amd64"
	got, err := amd.pick(rel)
	if err != nil || got.URL != "https://example.test/amd64" {
		t.Fatalf("amd64: %+v %v", got, err)
	}

	arm := New(t.TempDir(), t.TempDir(), logbuf.New(10))
	arm.Arch = "arm64"
	got, err = arm.pick(rel)
	if err != nil || got.URL != "https://example.test/arm64" {
		t.Fatalf("arm64: %+v %v", got, err)
	}
}

// fakeLzip puts a "lzip" on PATH that ignores compression and streams the file.
func fakeLzip(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat \"$3\"\n"
	if err := os.WriteFile(filepath.Join(dir, "lzip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestInstallOpenStarbound(t *testing.T) {
	fakeLzip(t)

	serverDir := t.TempDir()
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(serverDir, "linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(serverDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "linux", "starbound_server"), []byte("vanilla"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "assets", "packed.pak"), []byte("assets"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := makeTar(t, map[string]string{
		"server_distribution/linux/starbound_server": "openstarbound",
		"server_distribution/assets/opensb.pak":      "opensb",
	})
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+releaseRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.1.15.1",
			"published_at": time.Now(),
			"assets": []map[string]any{{
				"name":                 "OpenStarbound-Linux-Clang-Server.tar.lz",
				"size":                 len(payload),
				"digest":               digest,
				"browser_download_url": "http://" + r.Host + "/download",
			}},
		})
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := New(serverDir, dataDir, logbuf.New(100))
	e.Arch = "amd64"
	e.APIBase = srv.URL

	rel, err := e.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rel.Tag != "v0.1.15.1" || rel.Size != int64(len(payload)) {
		t.Fatalf("unexpected release: %+v", rel)
	}

	if err := e.InstallOpenStarbound(context.Background(), rel); err != nil {
		t.Fatalf("install: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(serverDir, "linux", "starbound_server"))
	if string(got) != "openstarbound" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "assets", "opensb.pak")); err != nil {
		t.Fatalf("opensb.pak missing: %v", err)
	}
	// The vanilla assets must survive the overlay.
	if _, err := os.Stat(filepath.Join(serverDir, "assets", "packed.pak")); err != nil {
		t.Fatalf("packed.pak was removed: %v", err)
	}
	// And the vanilla binary must have been backed up.
	backup, err := os.ReadFile(filepath.Join(dataDir, "engine", "vanilla", "linux", "starbound_server"))
	if err != nil || string(backup) != "vanilla" {
		t.Fatalf("vanilla backup: %q %v", backup, err)
	}
}

func TestFindRoot(t *testing.T) {
	flat := t.TempDir()
	if err := os.MkdirAll(filepath.Join(flat, "linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "linux", "starbound_server"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findRoot(flat); err != nil || got != flat {
		t.Fatalf("flat: %q %v", got, err)
	}

	wrapped := t.TempDir()
	nested := filepath.Join(wrapped, "server_distribution")
	if err := os.MkdirAll(filepath.Join(nested, "linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "linux", "starbound_server"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findRoot(wrapped); err != nil || got != nested {
		t.Fatalf("wrapped: %q %v", got, err)
	}

	if _, err := findRoot(t.TempDir()); err == nil {
		t.Fatal("expected an error for an archive without the server binary")
	}
}

func TestDownloadRejectsBadDigest(t *testing.T) {
	payload := []byte("not the expected bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := New(t.TempDir(), t.TempDir(), logbuf.New(10))
	rel := Release{
		Tag:    "v0.1.15.1",
		Asset:  "OpenStarbound-Linux-Clang-Server.tar.lz",
		URL:    srv.URL + "/download",
		Size:   int64(len(payload)),
		Digest: "sha256:" + strings.Repeat("0", 64),
	}
	if _, err := e.download(context.Background(), rel); err == nil {
		t.Fatal("expected a digest error")
	}
}

func TestBackupAndRestoreVanilla(t *testing.T) {
	serverDir := t.TempDir()
	dataDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(serverDir, "linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(serverDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(serverDir, "linux", "starbound_server")
	if err := os.WriteFile(binary, []byte("vanilla"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "assets", "opensb.pak"), []byte("opensb"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := New(serverDir, dataDir, logbuf.New(10))
	if err := e.BackupVanilla(); err != nil {
		t.Fatalf("backup: %v", err)
	}

	if err := os.WriteFile(binary, []byte("openstarbound"), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, err := e.RestoreVanilla()
	if err != nil || !ok {
		t.Fatalf("restore: %v %v", ok, err)
	}
	got, _ := os.ReadFile(binary)
	if string(got) != "vanilla" {
		t.Fatalf("binary not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "assets", "opensb.pak")); !os.IsNotExist(err) {
		t.Fatal("opensb.pak should be removed when switching back")
	}
}

func TestRestoreWithoutBackup(t *testing.T) {
	e := New(t.TempDir(), t.TempDir(), logbuf.New(10))
	ok, err := e.RestoreVanilla()
	if err != nil || ok {
		t.Fatalf("expected a clean miss, got ok=%v err=%v", ok, err)
	}
}

func TestResolveRejectsBadTag(t *testing.T) {
	e := New(t.TempDir(), t.TempDir(), logbuf.New(10))
	for _, tag := range []string{"../x", "0.1.0", "v1/../../etc"} {
		if _, err := e.Resolve(context.Background(), tag); err == nil {
			t.Fatalf("expected %q to be rejected", tag)
		}
	}
}

func TestHumanSize(t *testing.T) {
	if got := humanSize(0); got != "unknown size" {
		t.Fatalf("got %q", got)
	}
	if got := humanSize(1536); !strings.Contains(got, "KiB") {
		t.Fatalf("got %q", got)
	}
	if got := fmt.Sprint(humanSize(55 << 20)); got == "" {
		t.Fatal("empty")
	}
}
