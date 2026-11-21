//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// Handle tracks a child process group for clean shutdown.
type Handle struct {
	pid int
}

// ResolveExecutable resolves bin via PATH.
func ResolveExecutable(bin string) (string, error) {
	return exec.LookPath(bin)
}

// Configure isolates a tracked subprocess in its own process group.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}

// StartDetached spawns a background worker in its own session (Setsid only;
// never combine Setsid and Setpgid, which fails with EPERM on Unix).
func StartDetached(bin string, args []string, dir string, env []string) (bool, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	_ = cmd.Process.Release()
	return false, nil
}

// StartTracked starts cmd in its own process group.
func StartTracked(cmd *exec.Cmd) (*Handle, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	return &Handle{pid: pid}, nil
}

// Close kills any remaining processes in the child process group.
func (h *Handle) Close() {
	if h == nil || h.pid <= 0 {
		return
	}
	_ = syscall.Kill(-h.pid, syscall.SIGKILL)
	h.pid = 0
}
