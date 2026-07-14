// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

// Shortcut-key glyphs shown inside hint text. They are interpolated into a hint
// as arguments rather than written into the localized string directly, so a
// keybinding relabel is a single edit here and never touches a translation, and
// the glyph's brackets stay out of the format string.
const (
	keyTickStep1  = "[ ]" // [ / ] — nudge the armed price by ±1 tick
	keyTickStep10 = "{ }" // { / } — nudge the armed price by ±10 ticks
)
