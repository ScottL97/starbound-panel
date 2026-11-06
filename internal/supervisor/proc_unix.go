//go:build !windows

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcess puts the game server in its own process group so the panel
// can signal it (and only it) without touching its own process group.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interruptGroup sends SIGINT, which makes Starbound save and exit cleanly.
func interruptGroup(p *os.Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGINT)
}

// killGroup force-terminates the whole process group.
func killGroup(p *os.Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// signalCode maps a signal termination to a shell-style exit code.
func signalCode(err error) int {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return -1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return -1
}
