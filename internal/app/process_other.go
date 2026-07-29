//go:build !windows

package app

import "os/exec"

func hideBackgroundProcess(_ *exec.Cmd) {}
