// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package candlechart renders OHLC candlestick charts in a terminal cell grid.
//
// It is a self-contained Bubble Tea component: it depends only on the Charm
// stack (bubbletea and lipgloss) and the standard library, never on an API,
// stream, or wire type. Feed it plain [Candle] values and it owns its own
// viewport (scroll and zoom) and rendering. Prices are float64 — a chart is a
// visual artifact, so this is the one place a decimal price string is parsed to
// a float; the caller converts at the boundary, money stays a string elsewhere.
//
// # Rendering model
//
// Each terminal cell is split into two vertical sub-rows ("half-cell"
// resolution), so a price plot H rows tall resolves 2H levels. For each visible
// candle, each cell's two sub-rows are classified Empty, Body, or Wick, and a
// candle renderer turns that pair into a glyph. A candlestick uses exactly two
// stroke weights — a thick body and a thin wick — so a renderer never mixes
// block bodies with line glyphs (that reads as a third weight). Two renderers
// exist:
//
//   - thin: a single-column candle — heavy body ┃ ╹ ╻, thin wick │ ╵ ╷, and
//     merged boundary cells ╿ (heavy-up/thin-down) and ╽ (thin-up/heavy-down)
//     that keep the wick connected to the body. Both weights are line strokes.
//   - block: filled half-block bodies █ ▀ ▄ across the candle width with a
//     centred thin wick │ ╵ ╷. No boundary merge — a ╿/╽ here would be a line
//     stroke against block bodies, a third weight.
//
// Up candles (close ≥ open) are green, down candles red; the wick takes the
// body color. Colors are configurable via [Styles] and [Model.SetStyles]: the
// default [GreenRedStyles] is green-up/red-down, and [RedBlueStyles] is the
// red-up/blue-down preset for the East Asian convention.
//
// Single-pass rendering is correct because a candle's range is ordered
// high … bodyTop … bodyBottom … low, so the upper-wick, body, and lower-wick
// regions are vertically disjoint: each cell falls in exactly one region. Only
// the cell straddling a body edge is ambiguous, and there the body takes the
// cell while the wick continues in the neighbouring cell.
//
// # Zoom and scroll
//
// Zoom is a ladder of {renderer, width, gap} steps, not a bare width. From most
// zoomed out to in: thin lines packed with no gap (a dense overview), bold
// single-column blocks with a gap (the default), then two- and three-cell
// block candles. [Model.ZoomIn] and [Model.ZoomOut] step the ladder;
// [Model.ScrollBy] and [Model.ScrollToLive] pan through retained history,
// pinned to the live edge while already there. [Model.SelectPrev] and
// [Model.SelectNext] move a candle-selection cursor (a vertical crosshair drawn
// through the selected candle); the viewport follows it so it stays on screen,
// and [Model.Selected] returns the selected candle for a caller-rendered
// readout. [Model.UpsertLive] updates the in-progress bar (replacing the last
// bucket or appending a new one on an interval rollover) while keeping the
// viewport stable. [Model.Update] maps the arrow keys / h,l to selection,
// pgup/pgdn and the mouse wheel to paging ([Model.PageBy] — a scroll that drags
// the selection only enough to keep it on screen; pgup at the oldest page is a
// no-op and pgdn at the live edge catches the cursor up to the newest candle),
// +/- to zoom, and end/g to jump-to-live, and composes with a host model's own
// keys.
//
// Scroll is anchored to the right (newest) edge, so a host can prepend older
// history with [Model.SetCandles] and the visible window stays put while the new
// bars fill in on the left. [Model.SetCandles] re-locates the selection by
// candle bucket time (not slot), so the cursor keeps its candle across such a
// prepend or a re-seed. [Model.NeedsBackfill] reports when the viewport has
// scrolled within a screenful of the oldest loaded candle, so the host knows to
// fetch more history; the component never fetches.
//
// # Price axis
//
// The price (vertical) axis auto-fits the visible candles with a small headroom
// (extended to cover visible overlay points so an indicator line is never
// clipped) and labels the high, midpoint, and low in a right-hand gutter. The
// gutter width is fixed; a price too wide for it degrades to fewer decimals and
// then a scaled suffix (k/M/B) rather than a truncated, wrong number.
//
// # Overlays, volume, last-price line
//
// [Overlay] line series are drawn as Braille lines (2×4 sub-points per cell)
// interpolated between candles, so a line reads as continuous. Overlays carry
// caller-supplied values; this package draws them but computes no indicator
// math. There are two ways to supply them: [Model.SetOverlays] sets the lines
// directly, while [Model.SetIndicators] registers [Indicator] values that the
// chart turns into overlays — so a host wires a technical indicator once and the
// line tracks the data. An indicator reads only the closed candles (the live
// in-progress bucket is excluded), and the result is cached against the closed
// series, so a live-price tick or a re-render that leaves the closed bars
// unchanged reuses the cached line instead of recomputing. The indicator math
// lives outside this package; [Indicator] is only the wiring seam. By default
// candles take precedence where a line crosses a body or wick;
// [Model.SetOverlayOnTop] inverts that.
//
// Three display features are off by default: a volume histogram pane
// ([Model.SetVolumePane]), drawn with eighth-block bars each colored by its
// candle's direction (up/down) and sized so the price plot keeps a usable
// minimum height; a last-price line ([Model.SetLastPriceLine]), a dotted line
// plus gutter label at the newest close, shown only while that price is within
// the visible band; and a time axis ([Model.SetTimeAxis]), a bottom row of
// time labels placed under the candles whose format adapts to the visible span
// (clock time within a day, date and time across days, date alone for
// daily/weekly candles). Each carves its rows from the total height, never
// shrinking the price plot below its minimum.
//
// # Resolution
//
// Vertical resolution is two sub-rows per cell, so a body shorter than half a
// cell renders as a single half-block and a doji (open ≈ close) renders as one
// half-block. Glyphs assume a Unicode font with half-block, box-drawing, and
// Braille coverage; colors use the 16 basic ANSI colors so any terminal theme
// renders sanely. [Model.View] returns exactly the configured number of lines,
// each exactly the configured width.
package candlechart
