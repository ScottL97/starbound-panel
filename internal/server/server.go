// Package server wires the panel together and exposes its HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ScottL97/starbound-panel/internal/auth"
	"github.com/ScottL97/starbound-panel/internal/backup"
	"github.com/ScottL97/starbound-panel/internal/config"
	"github.com/ScottL97/starbound-panel/internal/engine"
	"github.com/ScottL97/starbound-panel/internal/event"
	"github.com/ScottL97/starbound-panel/internal/logbuf"
	"github.com/ScottL97/starbound-panel/internal/mods"
	"github.com/ScottL97/starbound-panel/internal/steamcmd"
	"github.com/ScottL97/starbound-panel/internal/store"
	"github.com/ScottL97/starbound-panel/internal/supervisor"
	"github.com/ScottL97/starbound-panel/internal/webui"
	"github.com/ScottL97/starbound-panel/internal/workshop"
)

const sessionCookie = "sb_session"

// Server is the running panel instance.
type Server struct {
	cfg  config.Config
	log  *slog.Logger
	st   *store.Store
	auth *auth.Manager
	bus  *logbuf.Bus
	hub  *event.Hub

	sup      *supervisor.Supervisor
	steam    *steamcmd.Runner
	mods     *mods.Manager
	backups  *backup.Manager
	engine   *engine.Engine
	workshop *workshop.Client

	installing atomic.Bool
	modBusy    atomic.Bool

	cancel context.CancelFunc
}

// New builds a panel from cfg. The game server is not started automatically.
func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}

	st, err := store.Open(cfg.StateDir)
	if err != nil {
		// A corrupt state file is recoverable: Open keeps a backup and
		// returns a usable store.
		if st == nil {
			return nil, err
		}
		logger.Warn("state recovered", "err", err)
	}

	s := &Server{
		cfg:  cfg,
		log:  logger,
		st:   st,
		auth: auth.New(st),
		bus:  logbuf.New(cfg.LogBuffer),
		hub:  event.New(),
	}

	if err := s.auth.SeedAdmin(cfg.AdminUser, cfg.AdminPassword); err != nil {
		logger.Warn("seed admin account", "err", err)
	}

	s.sup = supervisor.New(supervisor.Config{
		Dir:        cfg.ServerDir,
		WorkDir:    filepath.Dir(filepath.Join(cfg.ServerDir, cfg.ServerBinary)),
		StorageDir: cfg.StorageDir,
		HomeDir:    filepath.Join(cfg.DataDir, "home"),
		Binary:     cfg.ServerBinary,
	}, s.bus, supervisor.Hooks{
		OnStatus:  func(supervisor.Status) { s.hub.Publish("status", s.statusPayload()) },
		OnPlayers: func(players []supervisor.Player) { s.hub.Publish("players", players) },
	}, logger)

	s.steam = steamcmd.New(cfg.SteamCmd, cfg.SteamCmdDir, cfg.SteamCmdSeed, cfg.AppID, s.bus)
	s.mods = mods.New(st, cfg.ModsDir, cfg.ModsDisabledDir, s.steam, s.bus)
	s.backups = backup.New(cfg.StorageDir, cfg.BackupDir, s.bus)
	s.workshop = workshop.New()
	s.workshop.APIKey = st.Snapshot().Steam.WebAPIKey

	// Seed and bootstrap steamcmd in the background: the first run downloads a
	// couple of hundred MB, and doing it here keeps a Steam Guard code from
	// expiring while the client updates itself during the first login.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.steam.Ensure(ctx); err != nil {
			logger.Warn("steamcmd bootstrap failed", "err", err)
			s.bus.Printf("panel", "steamcmd bootstrap failed: %v", err)
		}
	}()

	s.engine = engine.New(cfg.ServerDir, cfg.DataDir, s.bus)
	s.engine.Token = cfg.GitHubToken

	settings := st.Snapshot().Settings
	s.sup.SetAutoRestart(settings.AutoRestart)
	s.sup.SetStopTimeout(time.Duration(settings.StopTimeoutSec) * time.Second)

	s.mods.Reconcile()

	// Forward console lines to the browser stream.
	lines, unsubscribe := s.bus.Subscribe()
	go func() {
		defer unsubscribe()
		for line := range lines {
			s.hub.Publish("log", line)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.runScheduler(ctx)

	if !s.steam.Available() {
		s.bus.Printf("panel", "warning: steamcmd not found at %s", cfg.SteamCmd)
	}
	s.bus.Printf("panel", "starbound panel %s ready", cfg.Version)

	return s, nil
}

// Handler returns the HTTP routes of the panel.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	mux.HandleFunc("GET /api/status", s.authed(s.handleStatus))
	mux.HandleFunc("GET /api/players", s.authed(s.handlePlayers))
	mux.HandleFunc("GET /api/logs", s.authed(s.handleLogs))
	mux.HandleFunc("GET /api/stream", s.authed(s.handleStream))

	mux.HandleFunc("POST /api/server/start", s.authed(s.handleStart))
	mux.HandleFunc("POST /api/server/stop", s.authed(s.handleStop))
	mux.HandleFunc("POST /api/server/restart", s.authed(s.handleRestart))
	mux.HandleFunc("POST /api/server/install", s.authed(s.handleInstall))
	mux.HandleFunc("POST /api/server/command", s.authed(s.handleCommand))

	mux.HandleFunc("GET /api/engine", s.authed(s.handleEngineInfo))
	mux.HandleFunc("POST /api/engine/switch", s.authed(s.handleSwitchEngine))

	mux.HandleFunc("GET /api/config", s.authed(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.authed(s.handlePutConfig))

	mux.HandleFunc("GET /api/mods", s.authed(s.handleListMods))
	mux.HandleFunc("POST /api/modfiles/{action}", s.authed(s.handleExternalMod))
	mux.HandleFunc("GET /api/workshop/details", s.authed(s.handleWorkshopDetails))
	mux.HandleFunc("GET /api/workshop/search", s.authed(s.handleWorkshopSearch))
	mux.HandleFunc("POST /api/mods", s.authed(s.handleAddMod))
	mux.HandleFunc("POST /api/mods/upload", s.authed(s.handleUploadMod))
	mux.HandleFunc("POST /api/mods/{id}/enable", s.authed(s.handleEnableMod))
	mux.HandleFunc("POST /api/mods/{id}/disable", s.authed(s.handleDisableMod))
	mux.HandleFunc("DELETE /api/mods/{id}", s.authed(s.handleRemoveMod))

	mux.HandleFunc("GET /api/backups", s.authed(s.handleListBackups))
	mux.HandleFunc("POST /api/backups", s.authed(s.handleCreateBackup))
	mux.HandleFunc("DELETE /api/backups/{name}", s.authed(s.handleDeleteBackup))
	mux.HandleFunc("GET /api/backups/{name}/download", s.authed(s.handleDownloadBackup))

	mux.HandleFunc("GET /api/settings", s.authed(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.authed(s.handlePutSettings))
	mux.HandleFunc("POST /api/settings/steam", s.authed(s.handleSteamCredentials))
	mux.HandleFunc("POST /api/settings/workshop", s.authed(s.handleWorkshopKey))
	mux.HandleFunc("POST /api/steam/test", s.authed(s.handleSteamTest))
	mux.HandleFunc("POST /api/settings/password", s.authed(s.handleChangePassword))

	assets := http.FileServer(http.FS(webui.FS()))
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(assets)))

	index, err := fs.ReadFile(webui.FS(), "index.html")
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})

	return securityHeaders(mux)
}

// Shutdown stops the game server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s.cancel != nil {
		s.cancel()
	}
	done := make(chan struct{})
	go func() {
		if err := s.sup.Stop(); err != nil && !errors.Is(err, supervisor.ErrNotRunning) {
			s.log.Warn("stop server", "err", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown timed out")
	}
}

// ------------------------------------------------------------------ middleware

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || !s.auth.Valid(cookie.Value) {
			writeError(w, http.StatusUnauthorized, errors.New("not signed in"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A custom header cannot be set by a cross-site form, which
			// together with SameSite=Lax blocks CSRF.
			if r.Header.Get("X-SB-Request") == "" {
				writeError(w, http.StatusForbidden, errors.New("missing X-SB-Request header"))
				return
			}
		}
		next(w, r)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: https://images.steamusercontent.com https://steamuserimages-a.akamaihd.net https://*.steamstatic.com; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The assets are embedded in the image and change with every release;
		// revalidate instead of serving a stale bundle for minutes after an
		// upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------ auth handlers

type sessionResponse struct {
	SetupRequired bool   `json:"setupRequired"`
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username"`
	Version       string `json:"version"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	resp := sessionResponse{
		SetupRequired: s.auth.SetupRequired(),
		Username:      s.auth.Username(),
		Version:       s.cfg.Version,
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil && s.auth.Valid(cookie.Value) {
		resp.Authenticated = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.auth.SetupRequired() {
		writeError(w, http.StatusConflict, errors.New("administrator account already exists"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.Setup(body.Username, body.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.bus.Printf("panel", "administrator account created")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.Authenticate(body.Username, body.Password, clientKey(r)); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrTooManyAttempts) {
			status = http.StatusTooManyRequests
		}
		writeError(w, status, err)
		return
	}

	token, err := s.auth.NewSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": s.auth.Username()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.auth.Destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ status

func (s *Server) statusPayload() map[string]any {
	snapshot := s.st.Snapshot()
	steam := snapshot.Steam

	backupInfo := map[string]any{"running": s.backups.Running()}
	if newest, ok := s.backups.Newest(); ok {
		backupInfo["newest"] = newest.UTC().Format(time.RFC3339)
	}
	if list, err := s.backups.List(); err == nil {
		backupInfo["count"] = len(list)
	}

	return map[string]any{
		"server":  s.sup.Status(),
		"players": s.sup.Players(),
		"steam": map[string]any{
			"configured": steam.Username != "" && steam.Password != "",
			"username":   steam.Username,
		},
		"backup": backupInfo,
		"panel": map[string]any{
			"version":       s.cfg.Version,
			"gamePort":      s.cfg.GamePort,
			"publicAddress": s.cfg.PublicAddress,
			"serverDir":     s.cfg.ServerDir,
			"installing":    s.installing.Load(),
			"flavor":        snapshot.Settings.ServerFlavor,
			"flavorVersion": snapshot.Settings.OpenStarboundInstalled,
		},
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusPayload())
}

func (s *Server) handlePlayers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"players": s.sup.Players()})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": s.bus.Recent(limit)})
}

// ------------------------------------------------------------------ server control

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.sup.Start(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.sup.Stop(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.sup.Restart(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sup.Command(body.Command); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if s.sup.Status().Running {
		writeError(w, http.StatusConflict, errors.New("stop the server before installing or updating it"))
		return
	}

	settings := s.st.Snapshot().Settings
	if settings.ServerFlavor == engine.FlavorOpenStarbound {
		if err := s.beginTask(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		go s.runOpenStarbound(settings.OpenStarboundTag)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "installing"})
		return
	}

	creds := s.credentials()
	if creds.Username == "" || creds.Password == "" {
		writeError(w, http.StatusBadRequest, steamcmd.ErrNoCredentials)
		return
	}

	// A Steam Guard code is valid for ~30 seconds, so the UI asks for a fresh
	// one for each install instead of reusing a stored value.
	var body struct {
		GuardCode string `json:"guardCode"`
	}
	if err := decodeOptionalJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if code := strings.TrimSpace(body.GuardCode); code != "" {
		creds.Guard = code
	}

	if err := s.beginTask(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}

	go func() {
		defer s.finishTask()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()

		s.bus.Printf("panel", "installing/updating the vanilla dedicated server")
		if err := s.steam.Install(ctx, creds, s.cfg.ServerDir); err != nil {
			s.bus.Printf("panel", "install failed: %v", err)
			s.logSteamHint()
			return
		}
		// Keep the switch-back copy in sync with the freshly installed build.
		_ = s.engine.BackupVanilla()
		s.setFlavor(engine.FlavorVanilla, "", "")
		s.bus.Printf("panel", "install finished; the server can be started now")
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "installing"})
}

// ------------------------------------------------------------------ engine

func (s *Server) handleEngineInfo(w http.ResponseWriter, r *http.Request) {
	settings := s.st.Snapshot().Settings

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	releases, releaseErr := s.engine.Releases(ctx, 10)

	resp := map[string]any{
		"flavor":              settings.ServerFlavor,
		"tag":                 settings.OpenStarboundTag,
		"installed":           settings.OpenStarboundInstalled,
		"arch":                s.engine.Arch,
		"asset":               s.engine.AssetName(),
		"openStarboundAssets": s.engine.HasOpenStarboundAssets(),
		"busy":                s.installing.Load(),
		"releases":            releases,
	}
	if releaseErr != nil {
		resp["releaseError"] = releaseErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSwitchEngine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Flavor string `json:"flavor"`
		Tag    string `json:"tag"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	flavor := strings.ToLower(strings.TrimSpace(body.Flavor))
	switch flavor {
	case engine.FlavorVanilla, engine.FlavorOpenStarbound:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown engine %q", body.Flavor))
		return
	}

	if s.sup.Status().Running {
		writeError(w, http.StatusConflict, errors.New("stop the server before switching engines"))
		return
	}
	if flavor == engine.FlavorOpenStarbound {
		if _, err := os.Stat(filepath.Join(s.cfg.ServerDir, "assets", "packed.pak")); err != nil {
			writeError(w, http.StatusBadRequest,
				errors.New("install the vanilla server once first: OpenStarbound needs assets/packed.pak"))
			return
		}
	}

	if err := s.beginTask(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}

	if flavor == engine.FlavorOpenStarbound {
		go s.runOpenStarbound(body.Tag)
	} else {
		go s.runVanillaSwitch()
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "switching"})
}

func (s *Server) runOpenStarbound(tag string) {
	defer s.finishTask()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rel, err := s.engine.Resolve(ctx, tag)
	if err != nil {
		s.bus.Printf("panel", "cannot resolve the OpenStarbound release: %v", err)
		return
	}

	s.bus.Printf("panel", "installing OpenStarbound %s (%s)", rel.Tag, rel.Asset)
	if err := s.engine.InstallOpenStarbound(ctx, rel); err != nil {
		s.bus.Printf("panel", "OpenStarbound install failed: %v", err)
		return
	}

	s.setFlavor(engine.FlavorOpenStarbound, tag, rel.Tag)
	s.bus.Printf("panel", "OpenStarbound %s installed; start the server to use it", rel.Tag)
}

func (s *Server) runVanillaSwitch() {
	defer s.finishTask()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	restored, err := s.engine.RestoreVanilla()
	if err != nil {
		s.bus.Printf("panel", "restoring the vanilla server failed: %v", err)
		return
	}
	if !restored {
		creds := s.credentials()
		if creds.Username == "" || creds.Password == "" {
			s.bus.Printf("panel", "no local vanilla backup and no Steam credentials: cannot reinstall the vanilla server")
			return
		}
		s.bus.Printf("panel", "no local vanilla backup, reinstalling with steamcmd")
		if err := s.steam.Install(ctx, creds, s.cfg.ServerDir); err != nil {
			s.bus.Printf("panel", "steamcmd install failed: %v", err)
			s.logSteamHint()
			return
		}
		_ = s.engine.RemoveOpenStarboundAssets()
		_ = s.engine.BackupVanilla()
	}

	s.setFlavor(engine.FlavorVanilla, "", "")
	s.bus.Printf("panel", "vanilla server restored")
}

func (s *Server) setFlavor(flavor, tag, installed string) {
	_ = s.st.Update(func(st *store.State) error {
		st.Settings.ServerFlavor = flavor
		if tag != "" {
			st.Settings.OpenStarboundTag = tag
		}
		// Cleared when switching back to vanilla so the UI does not report a
		// stale OpenStarbound version.
		st.Settings.OpenStarboundInstalled = installed
		return nil
	})
	s.hub.Publish("status", s.statusPayload())
}

// handleSteamTest verifies the Steam credentials without downloading the game
// and caches the Steam Guard session on the data volume.
func (s *Server) handleSteamTest(w http.ResponseWriter, r *http.Request) {
	creds := s.credentials()
	if creds.Username == "" || creds.Password == "" {
		writeError(w, http.StatusBadRequest, steamcmd.ErrNoCredentials)
		return
	}
	if s.sup.Status().Running {
		writeError(w, http.StatusConflict, errors.New("stop the server before running a Steam login"))
		return
	}
	if err := s.beginTask(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}

	go func() {
		defer s.finishTask()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		s.bus.Printf("panel", "testing the Steam login (the first run also bootstraps steamcmd)")
		if err := s.steam.Login(ctx, creds); err != nil {
			s.bus.Printf("panel", "Steam login failed: %v", err)
			s.logSteamHint()
			return
		}
		// The guard code is single-use; drop it so a stale code is not reused.
		_ = s.st.Update(func(st *store.State) error {
			st.Steam.GuardCode = ""
			return nil
		})
		s.bus.Printf("panel", "Steam login OK. This account needs a Steam Guard code for every steamcmd run (or an approval in the Steam app within ~90s); enter a fresh code when you press Install.")
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "testing"})
}

// logSteamHint turns the most relevant steamcmd line into actionable advice.
func (s *Server) logSteamHint() {
	lines := s.bus.Recent(150)
	for i := len(lines) - 1; i >= 0; i-- {
		text := strings.ToLower(lines[i].Text)
		switch {
		case strings.Contains(text, "waiting for confirmation"), strings.Contains(text, "steam guard"):
			s.bus.Printf("panel", "hint: the account uses Steam Guard. Press Install again and enter the current 5-character code in the prompt, or approve the login in the Steam mobile app within ~90s.")
			return
		case strings.Contains(text, "invalid password"), strings.Contains(text, "invalid login"):
			s.bus.Printf("panel", "hint: steamcmd rejected the username or password. Re-enter them in Settings -> Steam account (use the account name, not the email alias, if the alias fails).")
			return
		case strings.Contains(text, "no subscription"):
			s.bus.Printf("panel", "hint: 'No subscription' means the login did not complete, not that the account does not own Starbound. Fix the Steam Guard step, then run 'Test login'.")
			return
		}
	}
}

func (s *Server) beginTask() error {
	if !s.installing.CompareAndSwap(false, true) {
		return errors.New("another install or switch is already running")
	}
	return nil
}

func (s *Server) finishTask() {
	s.installing.Store(false)
	s.hub.Publish("status", s.statusPayload())
}

// ------------------------------------------------------------------ config file

// configPath returns the runtime configuration file. Starbound resolves
// "starbound_server.config" relative to its storage directory (see
// Root::toStoragePath), which is /data/storage in this image.
func (s *Server) configPath() string {
	return filepath.Join(s.cfg.StorageDir, "starbound_server.config")
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	path := s.configPath()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "exists": false, "content": ""})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "exists": true, "content": string(content)})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var probe map[string]any
	if err := json.Unmarshal([]byte(body.Content), &probe); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("configuration must be a JSON object: %w", err))
		return
	}

	path := s.configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.bus.Printf("panel", "server configuration updated; restart the server to apply it")
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ mods

func (s *Server) handleListMods(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mods":     s.mods.List(),
		"external": s.mods.Scan(),
	})
}

// handleExternalMod manages .pak files that were copied into the mod
// directories by hand (for example from a Steam Workshop download).
// handleUploadMod accepts .pak files or a .zip with one or more .pak files and
// installs them into the enabled mods directory.
func (s *Server) handleUploadMod(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 1 << 30 // 1 GiB per request

	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read upload: %w", err))
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no files were uploaded"))
		return
	}

	installed := make([]string, 0, len(files))
	failed := make([]string, 0)
	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			failed = append(failed, header.Filename+": "+err.Error())
			continue
		}
		names, err := s.mods.ImportFile(header.Filename, file, header.Size)
		file.Close()
		if err != nil {
			failed = append(failed, header.Filename+": "+err.Error())
			continue
		}
		installed = append(installed, names...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"installed": installed,
		"failed":    failed,
	})
}

func (s *Server) handleExternalMod(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File string `json:"file"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var err error
	switch r.PathValue("action") {
	case "enable":
		err = s.mods.SetExternalEnabled(body.File, true)
	case "disable":
		err = s.mods.SetExternalEnabled(body.File, false)
	case "delete":
		err = s.mods.RemoveExternal(body.File)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", r.PathValue("action")))
		return
	}

	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, mods.ErrInvalidFile):
			status = http.StatusBadRequest
		case errors.Is(err, os.ErrNotExist):
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWorkshopDetails enriches mod ids with Workshop metadata. It works
// without an API key.
func (s *Server) handleWorkshopDetails(w http.ResponseWriter, r *http.Request) {
	ids := parseIDs(r.URL.Query().Get("ids"))
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	items, err := s.workshop.Details(ctx, ids)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.decorateWorkshop(items)})
}

func (s *Server) handleWorkshopSearch(w http.ResponseWriter, r *http.Request) {
	if s.workshop.APIKey == "" {
		writeError(w, http.StatusBadRequest, workshop.ErrNoAPIKey)
		return
	}

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			page = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	items, total, err := s.workshop.Search(ctx, r.URL.Query().Get("q"), page, 20)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": s.decorateWorkshop(items),
		"total": total,
		"page":  page,
	})
}

// decorateWorkshop marks which items are already installed on the server.
func (s *Server) decorateWorkshop(items []workshop.Item) []map[string]any {
	installed := map[string]store.Mod{}
	for _, mod := range s.st.Snapshot().Mods {
		installed[mod.ID] = mod
	}

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry := map[string]any{
			"id":            item.ID,
			"title":         item.Title,
			"creator":       item.Creator,
			"description":   item.Description,
			"preview":       item.Preview,
			"subscriptions": item.Subscriptions,
			"favorited":     item.Favorited,
			"views":         item.Views,
			"size":          item.Size,
			"created":       item.Created,
			"updated":       item.Updated,
			"tags":          item.Tags,
			"banned":        item.Banned,
			"installed":     false,
			"enabled":       false,
		}
		if mod, ok := installed[item.ID]; ok {
			entry["installed"] = true
			entry["enabled"] = mod.Enabled
		}
		out = append(out, entry)
	}
	return out
}

// parseIDs validates a comma separated list of Workshop ids.
func parseIDs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" || len(id) > 20 || seen[id] {
			continue
		}
		valid := true
		for _, c := range id {
			if c < '0' || c > '9' {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= 50 {
			break
		}
	}
	return out
}

func (s *Server) handleAddMod(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string `json:"id"`
		GuardCode string `json:"guardCode"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	creds := s.credentials()
	if creds.Username == "" || creds.Password == "" {
		writeError(w, http.StatusBadRequest, steamcmd.ErrNoCredentials)
		return
	}
	if code := strings.TrimSpace(body.GuardCode); code != "" {
		creds.Guard = code
	}
	if !s.modBusy.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, errors.New("another mod operation is running"))
		return
	}

	go func() {
		defer func() {
			s.modBusy.Store(false)
			s.hub.Publish("status", s.statusPayload())
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		if err := s.mods.Add(ctx, body.ID, creds, s.cfg.ServerDir); err != nil {
			s.bus.Printf("panel", "adding mod %s failed: %v", body.ID, err)
			return
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "downloading"})
}

func (s *Server) handleEnableMod(w http.ResponseWriter, r *http.Request) {
	s.setModEnabled(w, r.PathValue("id"), true)
}

func (s *Server) handleDisableMod(w http.ResponseWriter, r *http.Request) {
	s.setModEnabled(w, r.PathValue("id"), false)
}

func (s *Server) setModEnabled(w http.ResponseWriter, id string, enabled bool) {
	if err := s.mods.SetEnabled(id, enabled); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mods.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveMod(w http.ResponseWriter, r *http.Request) {
	if err := s.mods.Remove(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mods.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ backups

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.backups.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backups": list,
		"running": s.backups.Running(),
	})
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if s.backups.Running() {
		writeError(w, http.StatusConflict, backup.ErrBusy)
		return
	}

	go func() {
		if _, err := s.backups.Create(); err != nil {
			s.bus.Printf("panel", "backup failed: %v", err)
			return
		}
		_ = s.backups.Prune(s.st.Snapshot().Settings.BackupRetention)
		s.hub.Publish("status", s.statusPayload())
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.backups.Delete(r.PathValue("name")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	path, err := s.backups.Path(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
	http.ServeFile(w, r, path)
}

// ------------------------------------------------------------------ settings

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	snap := s.st.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": snap.Settings,
		"steam": map[string]any{
			"configured":   snap.Steam.Username != "" && snap.Steam.Password != "",
			"username":     snap.Steam.Username,
			"webApiKeySet": snap.Steam.WebAPIKey != "",
		},
	})
}

// handleWorkshopKey stores the optional Steam Web API key used for searching.
func (s *Server) handleWorkshopKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	key := strings.TrimSpace(body.APIKey)
	if strings.ContainsAny(key, " \t\r\n\"'") {
		writeError(w, http.StatusBadRequest, errors.New("the API key must not contain whitespace or quotes"))
		return
	}

	if err := s.st.Update(func(st *store.State) error {
		st.Steam.WebAPIKey = key
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	s.workshop.APIKey = key
	if key == "" {
		s.bus.Printf("panel", "steam web api key cleared; workshop search is disabled")
	} else {
		s.bus.Printf("panel", "steam web api key updated; workshop search is enabled")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoRestart         *bool `json:"autoRestart"`
		StopTimeoutSec      *int  `json:"stopTimeoutSec"`
		BackupEnabled       *bool `json:"backupEnabled"`
		BackupIntervalHours *int  `json:"backupIntervalHours"`
		BackupRetention     *int  `json:"backupRetention"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	err := s.st.Update(func(st *store.State) error {
		if body.AutoRestart != nil {
			st.Settings.AutoRestart = *body.AutoRestart
		}
		if body.StopTimeoutSec != nil {
			v := clamp(*body.StopTimeoutSec, 10, 600)
			st.Settings.StopTimeoutSec = v
		}
		if body.BackupEnabled != nil {
			st.Settings.BackupEnabled = *body.BackupEnabled
		}
		if body.BackupIntervalHours != nil {
			st.Settings.BackupIntervalHrs = clamp(*body.BackupIntervalHours, 1, 720)
		}
		if body.BackupRetention != nil {
			st.Settings.BackupRetention = clamp(*body.BackupRetention, 1, 365)
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	settings := s.st.Snapshot().Settings
	s.sup.SetAutoRestart(settings.AutoRestart)
	s.sup.SetStopTimeout(time.Duration(settings.StopTimeoutSec) * time.Second)
	s.hub.Publish("status", s.statusPayload())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSteamCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		GuardCode string `json:"guardCode"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	body.Username = strings.TrimSpace(body.Username)
	body.Password = strings.TrimSpace(body.Password)
	body.GuardCode = strings.TrimSpace(body.GuardCode)

	if body.Username == "" {
		writeError(w, http.StatusBadRequest, errors.New("username is required"))
		return
	}
	for _, v := range []string{body.Username, body.Password, body.GuardCode} {
		if strings.ContainsAny(v, " \t\r\n\"'") {
			writeError(w, http.StatusBadRequest, steamcmd.ErrInvalidCredentials)
			return
		}
	}

	err := s.st.Update(func(st *store.State) error {
		if body.Password == "" {
			// Keep the stored password when only the account name changes.
			body.Password = st.Steam.Password
		}
		if body.Password == "" {
			return errors.New("password is required")
		}
		st.Steam.Username = body.Username
		st.Steam.Password = body.Password
		st.Steam.GuardCode = body.GuardCode
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.bus.Printf("panel", "steam credentials updated for %s", body.Username)
	s.hub.Publish("status", s.statusPayload())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.ChangePassword(body.Current, body.Next); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrInvalidCredentials) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	s.bus.Printf("panel", "administrator password changed")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) credentials() steamcmd.Credentials {
	steam := s.st.Snapshot().Steam
	return steamcmd.Credentials{Username: steam.Username, Password: steam.Password, Guard: steam.GuardCode}
}

// ------------------------------------------------------------------ sse

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(kind string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	events, cancel := s.hub.Subscribe()
	defer cancel()

	if err := send("hello", map[string]any{
		"status": s.statusPayload(),
		"lines":  s.bus.Recent(300),
	}); err != nil {
		return
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := send(ev.Kind, ev.Data); err != nil {
				return
			}
		case <-ticker.C:
			if err := send("ping", s.statusPayload()); err != nil {
				return
			}
		}
	}
}

// ------------------------------------------------------------------ scheduler

func (s *Server) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		settings := s.st.Snapshot().Settings
		if !settings.BackupEnabled || s.backups.Running() {
			continue
		}
		// Nothing to back up before the dedicated server is installed; an
		// empty archive would only confuse the operator.
		if !s.sup.Status().Installed {
			continue
		}
		interval := time.Duration(settings.BackupIntervalHrs) * time.Hour
		if newest, ok := s.backups.Newest(); ok && time.Since(newest) < interval {
			continue
		}

		go func(retention int) {
			if _, err := s.backups.Create(); err != nil {
				s.bus.Printf("panel", "scheduled backup failed: %v", err)
				return
			}
			_ = s.backups.Prune(retention)
			s.hub.Publish("status", s.statusPayload())
		}(settings.BackupRetention)
	}
}

// ------------------------------------------------------------------ helpers

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return err
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for endpoints whose body may be absent.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func clientKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
