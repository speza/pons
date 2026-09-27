//go:build !unix

package bash

import "os/exec"

func configureCommand(cmd *exec.Cmd) {}

func stopCommand(cmd *exec.Cmd) {}
