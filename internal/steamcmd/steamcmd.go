// Package steamcmd wraps the SteamCMD console tool for installing the
// Starbound dedicated server and downloading Workshop mods.
package steamcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

var (
	// ErrNoCredentials means no Steam account was configured.
	ErrNoCredentials = errors.New("steam credentials are not configured")
	// ErrInvalidCredentials means the credentials cannot be passed to a
	// steamcmd script safely.
	ErrInvalidCredentials = errors.New("steam user, password and guard code must not contain spaces, quotes or newlines")
)

// Credentials is the Steam account steamcmd logs in with.
type Credentials struct {
	Username string
	Password string
	Guard    string
}

func (c Credentials) validate() error {
	if c.Username == "" || c.Password == "" {
		return ErrNoCredentials
	}
	for _, v := range []string{c.Username, c.Password, c.Guard} {
		if strings.ContainsAny(v, " \t\r\n\"'") {
			return ErrInvalidCredentials
		}
	}
	return nil
}

// Runner executes steamcmd commands serially.
type Runner struct {
	// Script is the path to steamcmd.sh (on the data volume).
	Script string
	// Dir is the steamcmd working directory.
	Dir string
	// SeedDir is the copy shipped in the image, used when Dir is empty.
	SeedDir string
	// AppID is the Starbound Steam application id.
	AppID string
	// Bus receives the steamcmd console output.
	Bus *logbuf.Bus

	mu       sync.Mutex
	ensureMu sync.Mutex
	ready    bool
	readyErr error
}

// New creates a runner.
func New(script, dir, seedDir, appID string, bus *logbuf.Bus) *Runner {
	return &Runner{Script: script, Dir: dir, SeedDir: seedDir, AppID: appID, Bus: bus}
}

// Available reports whether steamcmd is installed (in the image or on the
// data volume).
func (r *Runner) Available() bool {
	if _, err := os.Stat(r.Script); err == nil {
		return true
	}
	if r.SeedDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(r.SeedDir, "steamcmd.sh"))
	return err == nil
}

// Ensure seeds the data volume from the image and bootstraps the Steam client.
//
// The bootstrap (first `steamcmd +quit`) downloads ~200MB and takes a while;
// doing it before the login keeps a Steam Guard code from expiring while the
// client updates itself.
func (r *Runner) Ensure(ctx context.Context) error {
	r.ensureMu.Lock()
	defer r.ensureMu.Unlock()

	if r.ready {
		return nil
	}
	if r.readyErr != nil {
		return r.readyErr
	}

	if err := r.seed(); err != nil {
		r.readyErr = err
		return err
	}

	// `+quit` is a no-op once the client is up to date.
	cmd := exec.CommandContext(ctx, r.Script, "+quit")
	cmd.Dir = r.Dir
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			r.Bus.Append(logbuf.Line{Source: "steamcmd", Text: sc.Text()})
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("steamcmd bootstrap failed: %w", err)
	}

	r.mu.Lock()
	r.ready = true
	r.mu.Unlock()
	return nil
}

// seed copies the steamcmd build shipped in the image onto the data volume.
func (r *Runner) seed() error {
	if _, err := os.Stat(r.Script); err == nil {
		return nil
	}
	if r.SeedDir == "" {
		return fmt.Errorf("steamcmd not found at %s", r.Script)
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	r.Bus.Printf("panel", "seeding steamcmd into %s", r.Dir)
	return copyTree(r.SeedDir, r.Dir)
}

// Login verifies the credentials without downloading the game. The resulting
// sentry file makes later installs work without a Steam Guard prompt.
func (r *Runner) Login(ctx context.Context, creds Credentials) error {
	if err := creds.validate(); err != nil {
		return err
	}
	if err := r.Ensure(ctx); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	lines := []string{
		"@ShutdownOnFailedCommand 1",
		"@NoPromptForPassword 1",
	}
	if creds.Guard != "" {
		lines = append(lines, "set_steam_guard_code "+creds.Guard)
	}
	lines = append(lines, "login "+creds.Username+" "+creds.Password, "quit")
	return r.run(ctx, "steam login", lines)
}

// Install downloads or updates the dedicated server into installDir.
func (r *Runner) Install(ctx context.Context, creds Credentials, installDir string) error {
	if err := creds.validate(); err != nil {
		return err
	}
	if err := r.Ensure(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	lines := r.loginLines(creds, installDir)
	lines = append(lines, "app_update "+r.AppID+" validate", "quit")
	return r.run(ctx, "server install", lines)
}

// DownloadWorkshop fetches Workshop items into the steamcmd cache.
func (r *Runner) DownloadWorkshop(ctx context.Context, creds Credentials, installDir string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := creds.validate(); err != nil {
		return err
	}
	if err := r.Ensure(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	lines := r.loginLines(creds, installDir)
	for _, id := range ids {
		lines = append(lines, "workshop_download_item "+r.AppID+" "+id)
	}
	lines = append(lines, "quit")
	return r.run(ctx, "workshop download", lines)
}

// WorkshopDir returns the local directory of a downloaded Workshop item.
func (r *Runner) WorkshopDir(id string) string {
	return filepath.Join(r.Dir, "steamapps", "workshop", "content", r.AppID, id)
}

func (r *Runner) loginLines(creds Credentials, installDir string) []string {
	lines := []string{
		"@ShutdownOnFailedCommand 1",
		"@NoPromptForPassword 1",
		"force_install_dir " + installDir,
	}
	if creds.Guard != "" {
		lines = append(lines, "set_steam_guard_code "+creds.Guard)
	}
	lines = append(lines, "login "+creds.Username+" "+creds.Password)
	return lines
}

func (r *Runner) run(ctx context.Context, label string, lines []string) error {
	if !r.Available() {
		return fmt.Errorf("steamcmd not found at %s", r.Script)
	}

	script, err := os.CreateTemp("", "sb-steamcmd-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(script.Name())

	if err := script.Chmod(0o600); err != nil {
		script.Close()
		return err
	}
	if _, err := script.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		script.Close()
		return err
	}
	if err := script.Close(); err != nil {
		return err
	}

	r.Bus.Printf("steamcmd", "running %s (this can take a while)", label)

	cmd := exec.CommandContext(ctx, r.Script, "+runscript", script.Name())
	cmd.Dir = r.Dir
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}

	done := make(chan struct{})
	var lastErr string
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			text := sc.Text()
			r.Bus.Append(logbuf.Line{Source: "steamcmd", Text: text})
			if strings.Contains(text, "ERROR") || strings.Contains(text, "FAILED") {
				lastErr = strings.TrimSpace(text)
			}
		}
	}()

	err = cmd.Wait()
	pw.Close()
	<-done

	if err != nil {
		if lastErr != "" {
			return fmt.Errorf("%s failed: %w (%s)", label, err, lastErr)
		}
		return fmt.Errorf("%s failed: %w", label, err)
	}
	if lastErr != "" {
		return fmt.Errorf("%s failed: %s", label, lastErr)
	}
	return nil
}

// copyTree copies regular files and directories from src to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Chmod(target, info.Mode().Perm())
	})
}
