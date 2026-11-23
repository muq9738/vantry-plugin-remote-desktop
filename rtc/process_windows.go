//go:build windows

package rtc

import (
	"os/exec"
	"syscall"
)

// Native codecs and SSH are owned background transports, not console windows.
func configureChildProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
