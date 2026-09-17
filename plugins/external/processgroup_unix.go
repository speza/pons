//go:build unix

package external

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so a kill can
// also take down grandchildren (plugin children spawning helpers).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcess kills the child's whole process group, falling back to the
// direct child (e.g. if it already exited).
func killProcess(cmd *exec.Cmd) {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
