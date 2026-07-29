//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

// hideBackgroundProcess prevents adb and scrcpy helper invocations from
// flashing a console window while the desktop app is running.
func hideBackgroundProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
