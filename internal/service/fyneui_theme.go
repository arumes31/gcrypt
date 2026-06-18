package service

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// brandTheme is a thin wrapper over Fyne's default theme that swaps in gcrypt's
// brand accent (the same blue used for the "syncing" status dot) for the
// primary/selection colour, so buttons, focus rings and the tab indicator feel
// like part of the product rather than stock Fyne. Everything else (fonts,
// icons, sizes, light/dark variants) is delegated to the default theme.
type brandTheme struct{}

var _ fyne.Theme = brandTheme{}

func (brandTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		return colBlue
	case theme.ColorNameSuccess:
		return colGreen
	case theme.ColorNameError:
		return colRed
	case theme.ColorNameWarning:
		return colAmber
	}
	return theme.DefaultTheme().Color(name, variant)
}

func (brandTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (brandTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (brandTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}
