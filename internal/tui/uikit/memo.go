// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package uikit holds the shared presentation primitives and the component
// contract for the TUI: the per-component render cache ([Memo]), the comparable
// style identity ([StyleID]), and the shared styles, palette,
// box/panel helpers, and formatters that the individual pane components draw
// with. It depends only on the Charm stack and the standard library, never on
// the TUI model, the API, or wire types — so a component built on it is
// independent and unit-testable in isolation.
//
// # The component contract
//
// Each pane is a small component in its own package. It is a pure function of
// explicit inputs: the parent passes everything the component needs and the
// component reaches into no shared state. An input is split in two:
//
//   - a comparable Key — the geometry, the [StyleID], the relevant store
//     revision(s) (see the Store's *Rev accessors), the active symbol, and any
//     pane-local scalars (focus, cursor, scroll, search text). Because every
//     field is comparable, two equal Keys guarantee an identical render.
//   - the Data — the slices/values to render (orderbook levels, rows, …). The
//     Data is read only when the component actually renders; a store revision in
//     the Key stands in for "did the data change", so the Key alone decides a
//     cache hit.
//
// A component embeds a [Memo] keyed on its Key type and renders through it:
//
//	func (c *Model) View(k Key, d Data) string {
//	    return c.memo.Do(k, func() string { return draw(k, d) })
//	}
//
// When the Key is unchanged the cached string is returned and Data is ignored —
// so a frame that doesn't touch this pane (another pane's update, a clock tick, a
// resize of a different region) costs one comparison, not a re-render. The Key
// must include EVERY input that affects the output; a missing field risks a stale
// frame, while an extra one only costs a needless re-render, so when unsure,
// include it.
package uikit

import "github.com/charmbracelet/colorprofile"

// Memo caches one rendered string against a comparable key K. The zero value is
// an empty (cold) cache. It is not safe for concurrent use; the TUI drives every
// component from its single update/render goroutine.
type Memo[K comparable] struct {
	has  bool
	key  K
	text string
}

// Do returns the cached render when key equals the key from the previous call;
// otherwise it calls draw, caches the (key, result) pair, and returns it. draw
// is invoked only on a miss, so it is where the component reads its Data and does
// the actual (allocation-heavy) rendering.
func (m *Memo[K]) Do(key K, draw func() string) string {
	if m.has && key == m.key {
		return m.text
	}
	text := draw()
	m.key, m.text, m.has = key, text, true
	return text
}

// Invalidate drops the cached render, forcing the next Do to re-render. Rarely
// needed — an unequal key already forces a miss — but available for a component
// that must refresh on an input not captured by its key.
func (m *Memo[K]) Invalidate() { m.has = false }

// StyleID is a comparable stand-in for the (non-comparable) resolved palette: a
// component carries it in its Key so a color-scheme or terminal-profile change
// invalidates the cache, while the component resolves the actual [Palette] from
// it via [PaletteFor] only when it renders. Scheme is the colorScheme value
// (the parent passes uint8(scheme)); Profile is the terminal's detected color
// support.
type StyleID struct {
	Scheme  uint8
	Profile colorprofile.Profile
}
