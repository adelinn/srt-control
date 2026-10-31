package bridge

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// hideWindow stops ffmpeg from flashing a console window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// interrupt has no SIGTERM to send on Windows, so it ends the process.
func interrupt(p *os.Process) error { return p.Kill() }

// Every ffmpeg goes into a job object that kills its members when the last
// handle to it closes. The handle is never closed explicitly, so Windows
// closes it when this program exits, crash or Task Manager included, and no
// orphaned ffmpeg keeps holding an SRT port.
var killJob struct {
	once   sync.Once
	handle windows.Handle
}

func afterStart(p *os.Process) {
	killJob.once.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(h)
			return
		}
		killJob.handle = h
	})
	if killJob.handle == 0 {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(ph)
	_ = windows.AssignProcessToJobObject(killJob.handle, ph)
}
