//go:build windows

package supervisor

import (
	"os"
	"os/exec"
)

// Windows support exists so the panel can be developed and unit tested
// natively. Production deployments run in a Linux container.

func configureProcess(cmd *exec.Cmd) {}

func interruptGroup(p *os.Process) error {
	return p.Kill()
}

func killGroup(p *os.Process) error {
	return p.Kill()
}

// signalCode is not meaningful on Windows.
func signalCode(err error) int {
	return -1
}
