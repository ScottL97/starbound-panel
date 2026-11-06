// Package engine switches the Starbound server installation between the
// official Steam build and OpenStarbound.
//
// Both engines share the same layout (linux/starbound_server, sbinit.config,
// assets/, mods/, storage/), so switching is an overlay of the files that
// differ. The vanilla binary is kept under <data>/engine/vanilla so a switch
// back works without contacting Steam again.
package engine

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

// Flavour names stored in the panel state.
const (
	FlavorVanilla       = "vanilla"
	FlavorOpenStarbound = "openstarbound"
)

const (
	apiBase      = "https://api.github.com"
	releaseRepo  = "OpenStarbound/OpenStarbound"
	releaseCache = 10 * time.Minute
)

var reTag = regexp.MustCompile(`^v[0-9][0-9A-Za-z.\-]*$`)

// Release is one downloadable OpenStarbound server build.
type Release struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"publishedAt"`
	Size        int64     `json:"size"`
	Asset       string    `json:"asset"`
	URL         string    `json:"url"`
	Digest      string    `json:"digest"`
}

// Engine downloads OpenStarbound builds and swaps the server files.
type Engine struct {
	ServerDir string
	DataDir   string
	Bus       *logbuf.Bus
	HTTP      *http.Client
	Token     string
	Arch      string
	// APIBase is the GitHub API root; overridable for tests.
	APIBase string

	mu      sync.Mutex
	cache   []Release
	cacheAt time.Time
}

// New creates an engine helper. bus may be nil.
func New(serverDir, dataDir string, bus *logbuf.Bus) *Engine {
	return &Engine{
		ServerDir: serverDir,
		DataDir:   dataDir,
		Bus:       bus,
		HTTP:      &http.Client{},
		Arch:      runtime.GOARCH,
		APIBase:   apiBase,
	}
}

func (e *Engine) log(format string, args ...any) {
	if e.Bus != nil {
		e.Bus.Printf("openstarbound", format, args...)
	}
}

// AssetName returns the Linux server archive for the current architecture.
func (e *Engine) AssetName() string {
	if e.Arch == "arm64" {
		return "OpenStarbound-Linux-ARM-Clang-Server.tar.lz"
	}
	return "OpenStarbound-Linux-Clang-Server.tar.lz"
}

// Resolve maps "latest" (or an empty tag) to the current release, and a
// pinned tag to that release.
func (e *Engine) Resolve(ctx context.Context, tag string) (Release, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.EqualFold(tag, "latest") {
		return e.fetchRelease(ctx, e.APIBase+"/repos/"+releaseRepo+"/releases/latest")
	}
	if !reTag.MatchString(tag) {
		return Release{}, fmt.Errorf("invalid OpenStarbound tag %q", tag)
	}
	return e.fetchRelease(ctx, e.APIBase+"/repos/"+releaseRepo+"/releases/tags/"+url.PathEscape(tag))
}

// Releases lists the recent stable releases (cached for a few minutes).
func (e *Engine) Releases(ctx context.Context, limit int) ([]Release, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}

	e.mu.Lock()
	if time.Since(e.cacheAt) < releaseCache && len(e.cache) > 0 {
		out := append([]Release(nil), e.cache...)
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()

	var raw []githubRelease
	if err := e.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", e.APIBase, releaseRepo, limit), &raw); err != nil {
		return nil, err
	}

	out := make([]Release, 0, len(raw))
	for _, item := range raw {
		if item.Draft || item.Prerelease {
			continue
		}
		if rel, err := e.pick(item); err == nil {
			out = append(out, rel)
		}
	}

	e.mu.Lock()
	e.cache = out
	e.cacheAt = time.Now()
	e.mu.Unlock()

	return out, nil
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (e *Engine) fetchRelease(ctx context.Context, endpoint string) (Release, error) {
	var raw githubRelease
	if err := e.getJSON(ctx, endpoint, &raw); err != nil {
		return Release{}, err
	}
	return e.pick(raw)
}

func (e *Engine) pick(raw githubRelease) (Release, error) {
	want := e.AssetName()
	for _, asset := range raw.Assets {
		if asset.Name != want {
			continue
		}
		return Release{
			Tag:         raw.TagName,
			PublishedAt: raw.PublishedAt,
			Size:        asset.Size,
			Asset:       asset.Name,
			URL:         asset.BrowserDownloadURL,
			Digest:      asset.Digest,
		}, nil
	}
	return Release{}, fmt.Errorf("release %s has no asset named %s", raw.TagName, want)
}

func (e *Engine) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "starbound-panel")
	req.Header.Set("Accept", "application/vnd.github+json")
	if e.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.Token)
	}

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github api %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst)
}

// InstallOpenStarbound backs up the vanilla binary and overlays the
// OpenStarbound server files into ServerDir.
func (e *Engine) InstallOpenStarbound(ctx context.Context, rel Release) error {
	if _, err := os.Stat(filepath.Join(e.ServerDir, "assets", "packed.pak")); err != nil {
		return errors.New("assets/packed.pak is missing: install the vanilla server once before switching to OpenStarbound")
	}
	if err := e.BackupVanilla(); err != nil {
		return fmt.Errorf("back up the vanilla server: %w", err)
	}

	archive, err := e.download(ctx, rel)
	if err != nil {
		return err
	}

	e.log("extracting %s", rel.Asset)
	if err := e.extract(ctx, archive); err != nil {
		return err
	}
	e.log("OpenStarbound %s installed", rel.Tag)
	return nil
}

func (e *Engine) download(ctx context.Context, rel Release) (string, error) {
	dir := filepath.Join(e.DataDir, "engine", "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, rel.Asset)
	partial := final + ".part"

	// Reuse a previously downloaded (or manually placed) archive when it still
	// matches the release.
	if fi, err := os.Stat(final); err == nil && (rel.Size == 0 || fi.Size() == rel.Size) {
		if rel.Digest == "" || fileDigest(final) == strings.TrimPrefix(rel.Digest, "sha256:") {
			e.log("using the cached archive %s", rel.Asset)
			return final, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "starbound-panel")

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", rel.URL, resp.Status)
	}

	f, err := os.Create(partial)
	if err != nil {
		return "", err
	}

	hasher := sha256.New()
	written := 0
	nextLog := int64(10 << 20)
	buf := make([]byte, 256<<10)

	e.log("downloading %s (%s)", rel.Asset, humanSize(rel.Size))
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(partial)
				return "", err
			}
			hasher.Write(buf[:n])
			written += n
			if int64(written) >= nextLog {
				e.log("downloaded %s / %s", humanSize(int64(written)), humanSize(rel.Size))
				nextLog += 10 << 20
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			os.Remove(partial)
			return "", readErr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(partial)
		return "", err
	}

	if rel.Size > 0 && int64(written) != rel.Size {
		os.Remove(partial)
		return "", fmt.Errorf("download is %d bytes, expected %d", written, rel.Size)
	}
	if digest := strings.TrimPrefix(rel.Digest, "sha256:"); digest != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, digest) {
			os.Remove(partial)
			return "", fmt.Errorf("sha256 mismatch: got %s, expected %s", got, digest)
		}
	}

	if err := os.Rename(partial, final); err != nil {
		os.Remove(partial)
		return "", err
	}
	return final, nil
}

func (e *Engine) extract(ctx context.Context, archive string) error {
	// The release archive wraps everything in a "server_distribution/"
	// directory, so unpack into staging first and overlay the real root.
	if err := os.MkdirAll(filepath.Join(e.DataDir, "engine"), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Join(e.DataDir, "engine"), "extract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	cmd := exec.CommandContext(ctx, "lzip", "-d", "-c", archive)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start lzip: %w", err)
	}

	extractErr := extractTar(stdout, staging)
	if extractErr != nil {
		_ = cmd.Process.Kill()
	}
	_ = stdout.Close()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("lzip: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if extractErr != nil {
		return extractErr
	}

	root, err := findRoot(staging)
	if err != nil {
		return err
	}
	return overlayTree(root, e.ServerDir)
}

// findRoot locates the directory that holds linux/ and assets/ inside an
// unpacked release (with or without a top-level wrapper directory).
func findRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "linux", "starbound_server")); err == nil {
		return dir, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var subdirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			subdirs = append(subdirs, entry.Name())
		}
	}
	if len(subdirs) == 1 {
		candidate := filepath.Join(dir, subdirs[0])
		if _, err := os.Stat(filepath.Join(candidate, "linux", "starbound_server")); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("archive does not contain linux/starbound_server")
}

// overlayTree copies src over dst, creating directories and preserving the
// executable bits of the server binaries.
func overlayTree(src, dst string) error {
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
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// extractTar unpacks a tar stream into dest, refusing entries that escape it.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry escapes the destination: %q", hdr.Name)
		}

		target := filepath.Join(dest, filepath.FromSlash(name))
		if !inside(dest, target) {
			return fmt.Errorf("archive entry escapes the destination: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		default:
			// Symlinks and devices are not part of the server distribution.
			continue
		}
	}
}

// BackupVanilla keeps a copy of the official server binary so the switch back
// does not depend on SteamCMD or the network.
func (e *Engine) BackupVanilla() error {
	srcDir := filepath.Join(e.ServerDir, "linux")
	src := filepath.Join(srcDir, "starbound_server")
	if _, err := os.Stat(src); err != nil {
		return err
	}

	dstDir := filepath.Join(e.DataDir, "engine", "vanilla", "linux")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	if err := copyFile(src, filepath.Join(dstDir, "starbound_server"), 0o755); err != nil {
		return err
	}
	_ = copyFile(filepath.Join(srcDir, "sbinit.config"), filepath.Join(dstDir, "sbinit.config"), 0o644)
	return nil
}

// RestoreVanilla puts the backed-up official binary back. It reports false
// when no backup exists, in which case the caller has to reinstall via Steam.
func (e *Engine) RestoreVanilla() (bool, error) {
	backupDir := filepath.Join(e.DataDir, "engine", "vanilla", "linux")
	src := filepath.Join(backupDir, "starbound_server")
	if _, err := os.Stat(src); err != nil {
		return false, nil
	}

	dstDir := filepath.Join(e.ServerDir, "linux")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return false, err
	}
	if err := copyFile(src, filepath.Join(dstDir, "starbound_server"), 0o755); err != nil {
		return false, err
	}
	_ = copyFile(filepath.Join(backupDir, "sbinit.config"), filepath.Join(dstDir, "sbinit.config"), 0o644)

	// The vanilla engine scans assets/ for .pak files and cannot load the
	// OpenStarbound asset pack.
	if err := e.RemoveOpenStarboundAssets(); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveOpenStarboundAssets deletes assets/opensb.pak.
func (e *Engine) RemoveOpenStarboundAssets() error {
	err := os.Remove(filepath.Join(e.ServerDir, "assets", "opensb.pak"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// HasOpenStarboundAssets reports whether assets/opensb.pak is present.
func (e *Engine) HasOpenStarboundAssets() bool {
	_, err := os.Stat(filepath.Join(e.ServerDir, "assets", "opensb.pak"))
	return err == nil
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// fileDigest returns the hex SHA-256 of a file, or "" on error.
func fileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func humanSize(n int64) string {
	if n <= 0 {
		return "unknown size"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
