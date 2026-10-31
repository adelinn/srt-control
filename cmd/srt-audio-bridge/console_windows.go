package main

import (
	"os"
	"syscall"
)

// attachConsole lets the exe, built for the GUI subsystem (no console window
// on double-click), print to the terminal it was started from when it gets
// command-line options. Redirected output (> file, pipes) is left alone.
// cmd.exe and PowerShell don't wait for GUI-subsystem programs; use
// "start /wait" or Start-Process -Wait -NoNewWindow when that matters.
func attachConsole() {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("AttachConsole")
	if ok, _, _ := proc.Call(attachParentProcess); ok == 0 {
		return // not started from a console
	}
	reopen := func(std int, name string, flag int, target **os.File) {
		if h, err := syscall.GetStdHandle(std); err == nil && h != 0 && h != syscall.InvalidHandle {
			return // already redirected
		}
		if f, err := os.OpenFile(name, flag, 0); err == nil {
			*target = f
		}
	}
	reopen(syscall.STD_OUTPUT_HANDLE, "CONOUT$", os.O_WRONLY, &os.Stdout)
	reopen(syscall.STD_ERROR_HANDLE, "CONOUT$", os.O_WRONLY, &os.Stderr)
	reopen(syscall.STD_INPUT_HANDLE, "CONIN$", os.O_RDONLY, &os.Stdin)
}
