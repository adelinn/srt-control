package gui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Custom color names used by the widgets in this package.
const (
	colorPanel       fyne.ThemeColorName = "srtPanel"
	colorPanelBorder fyne.ThemeColorName = "srtPanelBorder"
)

// bridgeTheme is Fyne's default theme with a tinted window background, raised
// panels and a slightly denser layout. It follows the OS light/dark setting.
type bridgeTheme struct{}

func hex(rgb uint32) color.NRGBA {
	return color.NRGBA{R: uint8(rgb >> 16), G: uint8(rgb >> 8), B: uint8(rgb), A: 0xff}
}

func (bridgeTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	dark := v == theme.VariantDark
	pick := func(light, darkC uint32) color.Color {
		if dark {
			return hex(darkC)
		}
		return hex(light)
	}
	switch n {
	case theme.ColorNameBackground:
		return pick(0xeef1f5, 0x121316)
	case colorPanel:
		return pick(0xffffff, 0x1c1e22)
	case colorPanelBorder:
		return pick(0xdfe3ea, 0x2a2d33)
	case theme.ColorNameInputBackground:
		return pick(0xf4f6f9, 0x25282d)
	case theme.ColorNameInputBorder:
		return pick(0xc5cdd8, 0x3d424a)
	case theme.ColorNameDisabled:
		return pick(0xa3abb8, 0x5c626c)
	case theme.ColorNameButton:
		return pick(0xe8ecf1, 0x2b2f35)
	case theme.ColorNameDisabledButton:
		return pick(0xf0f2f5, 0x23262b)
	case theme.ColorNameForeground:
		return pick(0x1f2937, 0xeceef1)
	case theme.ColorNamePlaceHolder:
		return pick(0x6b7280, 0x9ca3af)
	case theme.ColorNameSeparator:
		return pick(0xe3e7ed, 0x2a2d33)
	case theme.ColorNamePrimary:
		return pick(0x2563eb, 0x3b82f6)
	case theme.ColorNameSuccess:
		return pick(0x16a34a, 0x22c55e)
	case theme.ColorNameWarning:
		return pick(0xd97706, 0xf59e0b)
	case theme.ColorNameError:
		return pick(0xdc2626, 0xef4444)
	}
	return theme.DefaultTheme().Color(n, v)
}

func (bridgeTheme) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (bridgeTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }

func (bridgeTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNamePadding:
		return 3
	case theme.SizeNameInnerPadding:
		return 7
	case theme.SizeNameText:
		return 13
	case theme.SizeNameCaptionText:
		return 11
	}
	return theme.DefaultTheme().Size(n)
}
