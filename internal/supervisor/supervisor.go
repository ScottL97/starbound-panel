// Package supervisor owns the Starbound dedicated server process: it starts
// and stops it, streams its console output, tracks players and restarts it
// after a crash.
package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ScottL97/starbound-panel/internal/logbuf"
)

var (
	// ErrNotInstalled means the server binary is missing.
	ErrNotInstalled = errors.New("starbound server is not installed yet")
	// ErrRunning means a server process already exists.
	ErrRunning = errors.New("server is already running")
	// ErrNotRunning means there is no server process to control.
	ErrNotRunning = errors.New("server is not running")
)

// Config describes where the server lives.
type Config struct {
	// Dir is the installation directory.
	Dir string
	// WorkDir is the process working directory. It must contain sbinit.config,
	// which is how Starbound locates its assets, mods and storage directory;
	// the shipped run-server.sh does the equivalent of cd linux/.
	WorkDir string
	// StorageDir is the persistent save directory, symlinked as Dir/storage.
	StorageDir string
	// HomeDir is exported as HOME for the server process.
	HomeDir string
	// Binary is the executable path relative to Dir.
	Binary string
	// ExtraEnv is appended to the process environment.
	ExtraEnv []string
}

// Exit describes how the last server process ended.
type Exit struct {
	Code int       `json:"code"`
	At   time.Time `json:"at"`
}

// Status is the externally visible state of the supervised process.
type Status struct {
	Installed   bool       `json:"installed"`
	Running     bool       `json:"running"`
	PID         int        `json:"pid"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	UptimeSec   int64      `json:"uptimeSec"`
	Phase       string     `json:"phase"`
	AutoRestart bool       `json:"autoRestart"`
	LastExit    *Exit      `json:"lastExit,omitempty"`
}

// Hooks receive asynchronous state changes.
type Hooks struct {
	OnStatus  func(Status)
	OnPlayers func([]Player)
}

// Supervisor controls one dedicated server process.
type Supervisor struct {
	cfg   Config
	bus   *logbuf.Bus
	hooks Hooks
	log   *slog.Logger

	mu           sync.Mutex
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	pipeW        *io.PipeWriter
	done         chan struct{}
	stopping     bool
	autoRestart  bool
	stopTimeout  time.Duration
	status       Status
	restartDelay time.Duration

	statusCh  chan Status
	playersCh chan []Player

	p *parser
}

// New creates a supervisor. No process is started until Start is called.
func New(cfg Config, bus *logbuf.Bus, hooks Hooks, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Supervisor{
		cfg:          cfg,
		bus:          bus,
		hooks:        hooks,
		log:          logger,
		autoRestart:  true,
		stopTimeout:  90 * time.Second,
		restartDelay: 5 * time.Second,
		statusCh:     make(chan Status, 1),
		playersCh:    make(chan []Player, 1),
	}
	s.p = newParser(func(ps []Player) { s.emitPlayers(ps) })
	s.status = Status{
		Installed:   s.Installed(),
		Phase:       "stopped",
		AutoRestart: s.autoRestart,
	}
	go s.dispatch()
	return s
}

func (s *Supervisor) dispatch() {
	for {
		select {
		case st := <-s.statusCh:
			if s.hooks.OnStatus != nil {
				s.hooks.OnStatus(st)
			}
		case ps := <-s.playersCh:
			if s.hooks.OnPlayers != nil {
				s.hooks.OnPlayers(ps)
			}
		}
	}
}

// Installed reports whether the server binary exists.
func (s *Supervisor) Installed() bool {
	fi, err := os.Stat(filepath.Join(s.cfg.Dir, s.cfg.Binary))
	return err == nil && !fi.IsDir()
}

// Status returns a snapshot with a freshly computed uptime.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Supervisor) statusLocked() Status {
	st := s.status
	// Recompute on every read so an install performed outside the supervisor
	// (steamcmd) is reflected immediately.
	st.Installed = s.Installed()
	if st.Running && st.StartedAt != nil {
		st.UptimeSec = int64(time.Since(*st.StartedAt).Seconds())
	}
	return st
}

// Players returns the currently connected players.
func (s *Supervisor) Players() []Player {
	return s.p.Snapshot()
}

// SetAutoRestart toggles crash recovery.
func (s *Supervisor) SetAutoRestart(v bool) {
	s.mu.Lock()
	s.autoRestart = v
	s.status.AutoRestart = v
	s.emitStatusLocked()
	s.mu.Unlock()
}

// SetStopTimeout sets how long Stop waits before SIGKILL.
func (s *Supervisor) SetStopTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.stopTimeout = d
	s.mu.Unlock()
}

// Start launches the server process.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil {
		return ErrRunning
	}
	if !s.Installed() {
		s.status.Installed = false
		s.emitStatusLocked()
		return ErrNotInstalled
	}
	if err := s.ensureLayoutLocked(); err != nil {
		return err
	}

	s.status.Installed = true
	s.stopping = false
	s.status.Phase = "starting"
	s.emitStatusLocked()
	return s.spawnLocked()
}

func (s *Supervisor) spawnLocked() error {
	binary := filepath.Join(s.cfg.Dir, s.cfg.Binary)

	cmd := exec.Command(binary)
	cmd.Dir = s.cfg.WorkDir
	if cmd.Dir == "" {
		cmd.Dir = s.cfg.Dir
	}
	cmd.Env = s.env()
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin: %w", err)
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("start server: %w", err)
	}

	done := make(chan struct{})
	now := time.Now()
	s.cmd = cmd
	s.stdin = stdin
	s.pipeW = pw
	s.done = done
	s.status.Running = true
	s.status.PID = cmd.Process.Pid
	s.status.StartedAt = &now
	s.status.Phase = "running"
	s.emitStatusLocked()

	s.p.Reset()
	s.bus.Printf("panel", "started server (pid %d)", cmd.Process.Pid)

	go s.pump(pr, pw)
	go s.wait(cmd, done)
	return nil
}

func (s *Supervisor) pump(pr *io.PipeReader, pw *io.PipeWriter) {
	defer pr.Close()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		text := sc.Text()
		s.bus.Append(logbuf.Line{Source: "server", Text: text})
		s.p.Feed(text)
	}
	_ = pw // closed by wait()
}

func (s *Supervisor) wait(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()

	s.mu.Lock()
	pw := s.pipeW
	s.pipeW = nil
	s.mu.Unlock()
	if pw != nil {
		_ = pw.Close()
	}

	code := exitCode(err)
	now := time.Now()

	s.mu.Lock()
	s.cmd = nil
	s.stdin = nil
	stopping := s.stopping
	s.stopping = false
	uptime := time.Duration(0)
	if s.status.StartedAt != nil {
		uptime = now.Sub(*s.status.StartedAt)
	}
	if uptime > 2*time.Minute {
		s.restartDelay = 5 * time.Second
	}
	s.status.Running = false
	s.status.PID = 0
	s.status.StartedAt = nil
	s.status.LastExit = &Exit{Code: code, At: now}
	switch {
	case stopping:
		s.status.Phase = "stopped"
	case code == 0:
		s.status.Phase = "stopped"
	default:
		s.status.Phase = "crashed"
	}
	auto := s.autoRestart && !stopping
	s.emitStatusLocked()
	s.mu.Unlock()

	s.bus.Printf("panel", "server exited with code %d", code)

	// Nobody is connected any more: the log lines that would tell us about
	// disconnects are gone with the process.
	s.p.Reset()
	s.emitPlayers([]Player{})

	close(done)

	if auto {
		go s.scheduleRestart()
	}
}

func (s *Supervisor) scheduleRestart() {
	s.mu.Lock()
	delay := s.restartDelay
	if delay < 5*time.Second {
		delay = 5 * time.Second
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	s.restartDelay = delay * 2
	s.mu.Unlock()

	s.bus.Printf("panel", "restarting server in %s", delay.Round(time.Second))
	time.Sleep(delay)

	if err := s.Start(); err != nil && !errors.Is(err, ErrRunning) {
		s.bus.Printf("panel", "automatic restart failed: %v", err)
		s.mu.Lock()
		s.status.Phase = "stopped"
		s.emitStatusLocked()
		s.mu.Unlock()
	}
}

// Stop asks the server to shut down gracefully (SIGINT) and kills it if it
// does not exit within the configured timeout.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	timeout := s.stopTimeout
	if cmd == nil {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.stopping = true
	s.status.Phase = "stopping"
	s.emitStatusLocked()
	s.mu.Unlock()

	s.bus.Printf("panel", "stopping server")
	if err := interruptGroup(cmd.Process); err != nil {
		s.log.Warn("interrupt server", "err", err)
	}

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}

	s.bus.Printf("panel", "server did not stop within %s, killing it", timeout)
	_ = killGroup(cmd.Process)

	select {
	case <-done:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("server did not exit after SIGKILL")
	}
}

// Restart stops the server (if running) and starts it again.
func (s *Supervisor) Restart() error {
	if err := s.Stop(); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	return s.Start()
}

// Command sends one console command to the server.
func (s *Supervisor) Command(line string) error {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return nil
	}

	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return ErrNotRunning
	}

	if _, err := io.WriteString(stdin, line+"\n"); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	s.bus.Printf("panel", "> %s", line)
	return nil
}

func (s *Supervisor) env() []string {
	env := os.Environ()
	env = append(env, "HOME="+s.cfg.HomeDir)
	env = append(env, s.cfg.ExtraEnv...)
	return env
}

// ensureLayoutLocked creates the directory skeleton and links the persistent
// storage directory into the installation directory.
func (s *Supervisor) ensureLayoutLocked() error {
	dirs := []string{
		s.cfg.Dir,
		s.cfg.StorageDir,
		s.cfg.HomeDir,
		filepath.Join(s.cfg.Dir, "mods"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	link := filepath.Join(s.cfg.Dir, "storage")
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.Symlink(s.cfg.StorageDir, link)
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(link)
		if err != nil {
			return err
		}
		if target == s.cfg.StorageDir {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(s.cfg.StorageDir, link)
	default:
		// A real directory (e.g. created by a previous run without the
		// symlink): move its content to the persistent storage first.
		entries, err := os.ReadDir(link)
		if err != nil {
			return err
		}
		for _, e := range entries {
			from := filepath.Join(link, e.Name())
			to := filepath.Join(s.cfg.StorageDir, e.Name())
			if _, err := os.Stat(to); err == nil {
				continue
			}
			if err := os.Rename(from, to); err != nil {
				return fmt.Errorf("move %s to storage: %w", from, err)
			}
		}
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(s.cfg.StorageDir, link)
	}
}

func (s *Supervisor) emitStatusLocked() {
	st := s.statusLocked()
	select {
	case s.statusCh <- st:
	default:
		select {
		case <-s.statusCh:
		default:
		}
		select {
		case s.statusCh <- st:
		default:
		}
	}
}

func (s *Supervisor) emitPlayers(ps []Player) {
	select {
	case s.playersCh <- ps:
	default:
		select {
		case <-s.playersCh:
		default:
		}
		select {
		case s.playersCh <- ps:
		default:
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return signalCode(err)
	}
	return -1
}
