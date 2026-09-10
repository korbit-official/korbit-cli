// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// ColorScheme selects which two colors mark rising vs falling (and bid vs ask).
type ColorScheme uint8

const (
	// ColorSchemeGreenRed is the Western convention: up/bid green, down/ask red.
	// The zero value, so it is the default.
	ColorSchemeGreenRed ColorScheme = iota
	// ColorSchemeRedBlue is the East-Asian convention (the Digital X app's own
	// coloring): up/bid red, down/ask blue.
	ColorSchemeRedBlue
)

func (s ColorScheme) String() string {
	if s == ColorSchemeRedBlue {
		return "red-blue"
	}
	return "green-red"
}

// ParseColorScheme is the inverse of String: it maps a persisted scheme name
// back to a ColorScheme. Any unrecognized value (including "") yields the
// default ColorSchemeGreenRed, so a stale or empty stored preference degrades
// to the default rather than erroring.
func ParseColorScheme(name string) ColorScheme {
	if name == "red-blue" {
		return ColorSchemeRedBlue
	}
	return ColorSchemeGreenRed
}

// Next is the other color scheme — the runtime toggle wraps over the two.
func (s ColorScheme) Next() ColorScheme {
	if s == ColorSchemeGreenRed {
		return ColorSchemeRedBlue
	}
	return ColorSchemeGreenRed
}

// SideStyle is the rendering for one market side: a text foreground plus the dim
// background of its orderbook depth bar. A nil BarBG means this terminal can't
// render a subtle bar (16-color or no color), so the bar is dropped on that side.
type SideStyle struct {
	Fg    lipgloss.Style
	BarBG color.Color
}

// Palette is the resolved up/down styling for one (color scheme, profile) pair.
type Palette struct {
	Up, Down SideStyle
}

// DrawBars reports whether the current profile renders depth bars at all.
func (p Palette) DrawBars() bool { return p.Up.BarBG != nil }

// tint is one semantic color expressed for each terminal profile. On truecolor
// and 256-color the foreground is an EXPLICIT value, not a basic ANSI name: a
// user who has remapped their 16-color palette would otherwise see an arbitrary
// hue for "green"/"red". The depth-bar background is a very dark tint of the same
// hue. A 16-color or colorless terminal falls back to the basic ANSI name and
// draws no bar (a 256-cube dark tint would collapse to black, an invisible bar).
type tint struct {
	trueFg, trueBar string
	c256Fg, c256Bar string
	basic           color.Color
}

var (
	tintGreen = tint{trueFg: "#26d07c", trueBar: "#04231a", c256Fg: "41", c256Bar: "22", basic: lipgloss.Green}
	tintRed   = tint{trueFg: "#f0506e", trueBar: "#2a0008", c256Fg: "203", c256Bar: "52", basic: lipgloss.Red}
	tintBlue  = tint{trueFg: "#3d9bf0", trueBar: "#03152a", c256Fg: "75", c256Bar: "17", basic: lipgloss.Blue}
)

// side resolves this tint into a SideStyle for the given terminal profile.
func (t tint) side(p colorprofile.Profile) SideStyle {
	switch p {
	case colorprofile.TrueColor:
		return SideStyle{Fg: lipgloss.NewStyle().Foreground(lipgloss.Color(t.trueFg)), BarBG: lipgloss.Color(t.trueBar)}
	case colorprofile.ANSI256:
		return SideStyle{Fg: lipgloss.NewStyle().Foreground(lipgloss.Color(t.c256Fg)), BarBG: lipgloss.Color(t.c256Bar)}
	default: // ANSI (16), Ascii, NoTTY: basic foreground, no depth bar
		return SideStyle{Fg: lipgloss.NewStyle().Foreground(t.basic)}
	}
}

// PaletteFor resolves the up/down SideStyles for a color scheme on a terminal
// profile.
func PaletteFor(s ColorScheme, p colorprofile.Profile) Palette {
	up, down := tintGreen, tintRed
	if s == ColorSchemeRedBlue {
		up, down = tintRed, tintBlue
	}
	return Palette{Up: up.side(p), Down: down.side(p)}
}
