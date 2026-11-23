//go:build !windows

package rtc

import "os/exec"

func configureChildProcess(*exec.Cmd) {}
