//go:build !windows

package bridge

import (
	"os"
	"os/exec"
	"syscall"
)

func hideWindow(*exec.Cmd) {}

// interrupt sends SIGTERM, which lets ffmpeg close its SRT socket cleanly.
func interrupt(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// afterStart has nothing to do here: the GUI and headless mode stop the engine
// (and so every ffmpeg) on SIGINT/SIGTERM.
func afterStart(*os.Process) {}
