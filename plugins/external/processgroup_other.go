//go:build !unix

package external

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
