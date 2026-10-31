package gui

import (
	"runtime"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

// Native Win32 dialogs: the standard "Open" dialog and MessageBox. Both run on
// their own OS thread, owned by the app window, so the window stays disabled
// but keeps drawing (the meters keep moving) while a dialog is open.

var (
	comdlg32             = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
	user32               = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW      = user32.NewProc("MessageBoxW")
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
)

const (
	ofnHideReadOnly  = 0x00000004
	ofnNoChangeDir   = 0x00000008
	ofnPathMustExist = 0x00000800
	ofnFileMustExist = 0x00001000
	ofnExplorer      = 0x00080000

	mbIconError    = 0x00000010
	mbIconWarning  = 0x00000030
	coinitApartmnt = 0x2
)

// openFileNameW mirrors OPENFILENAMEW.
type openFileNameW struct {
	structSize    uint32
	owner         uintptr
	instance      uintptr
	filter        *uint16
	customFilter  *uint16
	maxCustFilter uint32
	filterIndex   uint32
	file          *uint16
	maxFile       uint32
	fileTitle     *uint16
	maxFileTitle  uint32
	initialDir    *uint16
	title         *uint16
	flags         uint32
	fileOffset    uint16
	fileExtension uint16
	defExt        *uint16
	custData      uintptr
	hook          uintptr
	templateName  *uint16
	reserved      uintptr
	reserved2     uint32
	flagsEx       uint32
}

func windowHandle(w fyne.Window) uintptr {
	var hwnd uintptr
	if nw, ok := w.(driver.NativeWindow); ok {
		nw.RunNative(func(ctx any) {
			if c, ok := ctx.(driver.WindowsWindowContext); ok {
				hwnd = c.HWND
			}
		})
	}
	return hwnd
}

func onDialogThread(fn func()) {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if hr, _, _ := procCoInitializeEx.Call(0, coinitApartmnt); int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}
		fn()
	}()
}

// nativeOpenFile shows the Windows "Open" dialog; picked runs on the UI
// thread with the chosen path. It returns false where there is no native dialog.
func nativeOpenFile(w fyne.Window, title string, picked func(string)) bool {
	owner := windowHandle(w)
	onDialogThread(func() {
		file := make([]uint16, 4096)
		filter := utf16.Encode([]rune("Programs (*.exe)\x00*.exe\x00All files (*.*)\x00*.*\x00\x00"))
		titlePtr, _ := syscall.UTF16PtrFromString(title)
		ofn := openFileNameW{
			owner:   owner,
			filter:  &filter[0],
			file:    &file[0],
			maxFile: uint32(len(file)),
			title:   titlePtr,
			flags:   ofnExplorer | ofnFileMustExist | ofnPathMustExist | ofnNoChangeDir | ofnHideReadOnly,
		}
		ofn.structSize = uint32(unsafe.Sizeof(ofn))
		ok, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
		if ok != 0 {
			path := syscall.UTF16ToString(file)
			fyne.Do(func() { picked(path) })
		}
	})
	return true
}

// nativeMessage shows a Windows message box. It returns false where there is
// no native one.
func nativeMessage(w fyne.Window, title, msg string, isError bool) bool {
	owner := windowHandle(w)
	icon := uintptr(mbIconWarning)
	if isError {
		icon = mbIconError
	}
	onDialogThread(func() {
		t, _ := syscall.UTF16PtrFromString(title)
		m, _ := syscall.UTF16PtrFromString(msg)
		procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), icon)
	})
	return true
}
