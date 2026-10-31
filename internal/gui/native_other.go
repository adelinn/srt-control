//go:build !windows

package gui

import "fyne.io/fyne/v2"

// Native dialogs are only wired up on Windows so far; elsewhere the callers
// fall back to Fyne's own dialogs.

func nativeOpenFile(fyne.Window, string, func(string)) bool { return false }

func nativeMessage(fyne.Window, string, string, bool) bool { return false }
