// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package candlechart

import (
	"fmt"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Candle is one OHLC bar. Time is the bucket-start in unix milliseconds.
// Prices are plain float64 — a chart is a visual artifact, so this is the one
// place a decimal price string is parsed to a float (callers convert at the
// boundary). Volume is optional (0 if unknown).
type Candle struct {
	Time   int64
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Overlay is a line series aligned 1:1 with the chart's candle slice (index i
// is the value at candle i; NaN breaks the line, e.g. an indicator still
// warming up). It is the seam for technical-indicator lines (moving averages,
// bands): the caller computes the values, the chart draws an interpolated
// Braille line through them. Color overrides Styles.Overlay when non-nil.
type Overlay struct {
	Name   string
	Color  color.Color
	Values []float64
}

// Indicator is the wiring seam for technical indicators: it turns a candle
// series into the overlay line(s) to draw. The chart owns the wiring and the
// drawing; an Indicator owns only the math, so indicator logic lives outside
// this package. Returning a slice lets one indicator yield several lines (e.g.
// the three Bollinger bands). The returned values must be aligned 1:1 with cs
// (index i is candle i; NaN breaks the line for a warm-up gap); an indicator
// with no value yet returns nil.
//
// cs is the chart's completed candles — the in-progress live bucket is excluded,
// so an indicator reads only closed bars (the conventional TA input, and what
// keeps the line steady between bucket closes). cs is read-only: an indicator
// must not retain or mutate it.
//
// Register indicators with [Model.SetIndicators]; the chart recomputes their
// overlays whenever the closed-candle series changes, so a host sets them once.
type Indicator interface {
	Overlays(cs []Candle) []Overlay
}

// Styles are the chart's colors. Start from [GreenRedStyles] (the default) or
// [RedBlueStyles] and override. Volume bars take the Up/Down color of the
// candle they belong to, so recoloring the candles recolors the volume pane.
type Styles struct {
	Up        lipgloss.Style // up candle (close ≥ open) body + wick + volume bar
	Down      lipgloss.Style // down candle body + wick + volume bar
	Axis      lipgloss.Style // price gutter and labels
	Overlay   lipgloss.Style // overlay points without their own Color
	LastPrice lipgloss.Style // last-price line + its label (when shown)
}

// GreenRedStyles is the default palette: green up, red down (the Western
// convention). It uses the 16 basic ANSI colors so any terminal theme renders
// sanely (the runtime down-samples truecolor anyway).
func GreenRedStyles() Styles {
	return Styles{
		Up:        lipgloss.NewStyle().Foreground(lipgloss.Color("2")),  // green
		Down:      lipgloss.NewStyle().Foreground(lipgloss.Color("1")),  // red
		Axis:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),  // grey
		Overlay:   lipgloss.NewStyle().Foreground(lipgloss.Color("4")),  // blue
		LastPrice: lipgloss.NewStyle().Foreground(lipgloss.Color("11")), // yellow
	}
}

// RedBlueStyles is [GreenRedStyles] with the up/down colors swapped to red up,
// blue down (the East Asian convention).
func RedBlueStyles() Styles {
	s := GreenRedStyles()
	s.Up = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))   // red
	s.Down = lipgloss.NewStyle().Foreground(lipgloss.Color("4")) // blue
	return s
}

// Model is the candle-chart component. The zero value is not ready; use New.
type Model struct {
	candles []Candle
	live    bool // is the last candle the in-progress (live) bar?

	overlays      []Overlay
	indicators    []Indicator // recomputed into overlays when the closed series changes
	ovCacheClosed []Candle    // clone of the closed series the cached overlays were computed for
	ovCacheValid  bool        // is ovCacheClosed/overlays a usable indicator cache?
	overlaysOnTop bool        // draw overlay lines over candles; default false (candles win)
	showVolume    bool
	showLastPrice bool
	showTimeAxis  bool // draw a bottom row of time labels

	width, height int // total component size in cells (incl. price gutter)

	zoom        int // index into zoomLadder
	candleWidth int // body width in cells, derived from the zoom level
	gap         int // blank cells between candles, derived from the zoom level
	scroll      int // candles hidden off the right edge (0 = newest visible)
	selected    int // absolute index of the selected candle, or -1 for none

	styles Styles

	// rev bumps whenever the candle or overlay CONTENT changes (the slice inputs a
	// render can't cheaply compare); the layout/selection/toggle/style inputs are
	// captured directly in the frame key. frame caches the last rendered string so
	// a redraw with unchanged inputs reuses it instead of re-rendering. It is a
	// pointer so the value-receiver View writes through it and a value-copy of the
	// Model (the host's inline pane) shares one cache; the chart is driven from a
	// single goroutine, so no lock.
	rev   uint64
	frame *frameCache
}

// frameKey is the comparable identity of a rendered frame: equal keys produce an
// identical string. The slice content (candles/overlays) is stood in for by rev,
// and the non-comparable styles by a rendered probe; everything else is a scalar
// the View reads.
type frameKey struct {
	rev           uint64
	style         string
	w, h          int
	scroll        int
	selected      int
	zoom          int
	candleWidth   int
	gap           int
	showVolume    bool
	showLastPrice bool
	showTimeAxis  bool
	overlaysOnTop bool
	live          bool
}

type frameCache struct {
	valid bool
	key   frameKey
	text  string
}

const (
	gutterWidth = 12 // right-hand price-label column (fits prices into the hundreds of millions)
	minHeight   = 4
	minWidth    = gutterWidth + 4
	minPriceH   = 3 // never let the volume pane shrink the price plot below this
)

// zoomLevel is one step on the zoom ladder: which renderer draws the candles,
// their body width in cells, and the inter-candle gap.
type zoomLevel struct {
	render candleRenderer
	width  int
	gap    int
}

// zoomLadder lists the zoom steps from most zoomed out (index 0) to most zoomed
// in. The two zoomed-out steps are single-column candles — thin lines packed
// for a dense overview, then bold blocks with a gap — and the wider steps are
// filled-block candles. defaultZoom is the bold single-column step.
var zoomLadder = []zoomLevel{
	{lineCandles{}, 1, 0},  // 0: thin single-column lines, packed (dense overview)
	{blockCandles{}, 1, 1}, // 1: bold single-column blocks, spaced (default)
	{blockCandles{}, 2, 1}, // 2
	{blockCandles{}, 3, 1}, // 3: widest blocks (most zoomed in)
}

const defaultZoom = 1

// lowerEighths indexes lower-block glyphs by eighths filled from the bottom
// (0 = empty … 8 = full); used for the volume bars.
var lowerEighths = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// brailleBase is the U+2800 Braille Patterns block origin; brailleDots[row][col]
// is the dot bit for a 2-wide × 4-tall sub-cell. OR the bits onto brailleBase to
// form a glyph. Overlay lines use this for 2×4 = 8 sub-points per cell.
const brailleBase = 0x2800

var brailleDots = [4][2]rune{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

// New returns a chart sized to w×h cells (including the price gutter).
func New(w, h int) Model {
	m := Model{styles: GreenRedStyles(), selected: -1, frame: &frameCache{}}
	m.setLevel(defaultZoom)
	m.SetSize(w, h)
	return m
}

// SetStyles overrides the chart colors.
func (m *Model) SetStyles(s Styles) { m.styles = s }

// SetOverlays sets the overlay line series directly, drawn as interpolated
// Braille lines through the per-candle values. This is the raw seam for a caller
// that computes its own lines; it clears any registered indicators, so the two
// ways of supplying overlays never compete (last setter wins).
func (m *Model) SetOverlays(o []Overlay) {
	m.indicators = nil
	m.ovCacheValid = false
	m.overlays = o
	m.rev++
}

// SetIndicators registers the technical indicators to overlay. The chart
// computes their lines from the current candle series and recomputes them on
// every subsequent candle change ([Model.SetCandles]/[Model.UpsertLive]), so a
// host sets them once and the lines track the data. Passing nil (or an empty
// slice) removes all indicator overlays. It clears any overlays set via
// [Model.SetOverlays] (last setter wins).
func (m *Model) SetIndicators(inds []Indicator) {
	m.indicators = inds
	m.overlays = nil
	m.ovCacheValid = false // the indicator set changed: force a fresh compute
	m.recomputeIndicators()
	m.rev++
}

// closedCandles is the candle series with the in-progress live bucket excluded —
// the indicator input. The returned slice aliases the candle backing array and
// is read-only.
func (m Model) closedCandles() []Candle {
	n := len(m.candles)
	if m.live && n > 0 {
		n--
	}
	return m.candles[:n]
}

// recomputeIndicators rebuilds the overlay series from the registered indicators
// over the closed candles, caching the result so an unchanged closed series
// (a live-only price tick, the inline pane re-rendering, a redundant re-feed of
// the same data) costs one slice compare instead of re-running every indicator.
// It is a no-op when no indicators are registered, so overlays set directly via
// [Model.SetOverlays] survive a candle change untouched.
//
// Correctness rests on two rules. The cache keys on an exact clone of the closed
// series (not a boundary fingerprint), so an in-place re-sync rewrite of a closed
// bar is never mistaken for "unchanged". And a miss allocates fresh slices rather
// than reusing the cache backing: the inline pane value-copies the Model, so a
// recompute on the copy must not write through into the original's cache, or a
// later compare there could falsely hit and draw a stale line.
func (m *Model) recomputeIndicators() {
	if len(m.indicators) == 0 {
		m.ovCacheValid = false
		return
	}
	closed := m.closedCandles()
	if m.ovCacheValid && slices.Equal(closed, m.ovCacheClosed) {
		return // closed series unchanged: the cached overlays still apply
	}
	var ov []Overlay
	for _, ind := range m.indicators {
		ov = append(ov, ind.Overlays(closed)...)
	}
	m.overlays = ov
	m.ovCacheClosed = slices.Clone(closed) // fresh backing: never aliased by a value-copy
	m.ovCacheValid = true
}

// SetOverlayOnTop controls overlap precedence: when false (the default) candles
// keep the cell where an overlay line crosses a body or wick; when true the
// overlay line draws over the candle.
func (m *Model) SetOverlayOnTop(on bool) { m.overlaysOnTop = on }

// SetVolumePane toggles the bottom volume histogram (default off). It is sized
// automatically and never shrinks the price plot below a usable minimum.
func (m *Model) SetVolumePane(show bool) { m.showVolume = show }

// SetLastPriceLine toggles a dotted line plus gutter label at the newest
// candle's close (default off). It renders only while that price is within the
// visible price band, so it can be absent when scrolled far into history.
func (m *Model) SetLastPriceLine(show bool) { m.showLastPrice = show }

// SetTimeAxis toggles a bottom row of time labels under the candles (default
// off). The label granularity adapts to the visible span: clock time within a
// day, date+time across days, date alone for daily/weekly candles.
func (m *Model) SetTimeAxis(show bool) { m.showTimeAxis = show }

// Candles returns the current series (read-only; valid until the next
// mutation). Useful for recomputing overlay values aligned to it.
func (m Model) Candles() []Candle { return m.candles }

// SetCandles replaces the series. If live is true the final candle is treated
// as the in-progress bar (it is the one UpsertLive mutates). The slice is
// copied, so the caller may reuse its buffer.
//
// The selection is preserved across the reindex by candle identity — its bucket
// time — not its slot, so prepending history (a backfill) or re-seeding the
// window keeps the cursor on the same candle even as every index shifts. If that
// candle is gone the cursor clamps into the new range (cleared when empty).
// Scroll is anchored to the right (newest) edge, so prepended history appears on
// the left while the visible window stays put.
func (m *Model) SetCandles(cs []Candle, live bool) {
	contentChanged := !slices.Equal(cs, m.candles) // bump rev only on a real change
	var selTime int64
	hadSel := m.selected >= 0 && m.selected < len(m.candles)
	if hadSel {
		selTime = m.candles[m.selected].Time
	}
	// Preserve the viewport against growth at the newest (right) edge. When the
	// user is scrolled back (scroll > 0), candles appended after the old live
	// bucket — a rollover, or a re-sync that picked up newer buckets — would
	// otherwise drag the visible window toward the live edge, since scroll counts
	// from the right. Push scroll by the number of candles newer than the old
	// edge so the same candles stay in view. (Pinned to live, scroll stays 0 and
	// the view tracks the newest; prepended history is older than the old edge,
	// so it adds nothing and the window stays put.)
	var grewRight int
	if n := len(m.candles); n > 0 && m.scroll > 0 {
		prevNewest := m.candles[n-1].Time
		for _, c := range cs {
			if c.Time > prevNewest {
				grewRight++
			}
		}
	}
	m.candles = append(m.candles[:0:0], cs...)
	m.live = live
	m.scroll += grewRight
	if hadSel {
		if idx := indexByTime(m.candles, selTime); idx >= 0 {
			m.selected = idx
		}
		// else: the candle's bucket is gone; the clamp below keeps it valid.
	}
	if m.selected >= len(m.candles) {
		m.selected = len(m.candles) - 1 // -1 when empty
	}
	m.clampScroll()
	m.recomputeIndicators()
	if contentChanged {
		m.rev++
	}
}

// indexByTime returns the index of the candle with bucket time t, or -1 if none.
func indexByTime(cs []Candle, t int64) int {
	for i := range cs {
		if cs[i].Time == t {
			return i
		}
	}
	return -1
}

// UpsertLive updates (or appends) the in-progress candle. If c.Time matches the
// last candle's bucket it replaces it; otherwise it is appended as a new live
// bar (a rollover into the next interval). No-op semantics keep the caller from
// having to know whether the bucket already exists.
func (m *Model) UpsertLive(c Candle) {
	m.live = true
	if n := len(m.candles); n > 0 && m.candles[n-1].Time == c.Time {
		m.candles[n-1] = c
		m.recomputeIndicators()
		m.rev++
		return
	}
	atRight := m.scroll == 0
	m.candles = append(m.candles, c)
	if atRight {
		m.scroll = 0 // stay pinned to the live edge
	} else {
		m.scroll++ // keep the same candles in view as history grows
	}
	m.recomputeIndicators()
	m.rev++
}

// SetSize resizes the component.
func (m *Model) SetSize(w, h int) {
	if w < minWidth {
		w = minWidth
	}
	if h < minHeight {
		h = minHeight
	}
	m.width, m.height = w, h
	m.clampScroll()
}

// plotWidth is the candle area width (excludes the price gutter).
func (m Model) plotWidth() int { return m.width - gutterWidth }

// axisRows is the height of the bottom time-axis (0 when hidden).
func (m Model) axisRows() int {
	if m.showTimeAxis {
		return 1
	}
	return 0
}

// volumeRows is the height of the volume pane (0 when hidden). It is bounded so
// the price plot keeps at least minPriceH rows (within the area left after the
// time axis).
func (m Model) volumeRows() int {
	if !m.showVolume {
		return 0
	}
	avail := m.height - m.axisRows() // rows for the price plot + volume pane
	r := avail / 4
	if r < 2 {
		r = 2
	}
	if r > 6 {
		r = 6
	}
	if avail-r < minPriceH {
		r = avail - minPriceH
	}
	if r < 1 {
		return 0
	}
	return r
}

// priceRows is the height of the price plot (total minus the volume pane and
// the time axis).
func (m Model) priceRows() int { return m.height - m.volumeRows() - m.axisRows() }

// stride is the per-candle horizontal advance in cells.
func (m Model) stride() int { return m.candleWidth + m.gap }

// capacity is how many candles fit across the plot at the current zoom.
func (m Model) capacity() int {
	if m.stride() <= 0 {
		return 0
	}
	return m.plotWidth() / m.stride()
}

func (m *Model) clampScroll() {
	maxScroll := len(m.candles) - m.capacity()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// ScrollBy pans by n candles (positive = back in time / left). Pins are clamped.
func (m *Model) ScrollBy(n int) { m.scroll += n; m.clampScroll() }

// ScrollToLive jumps back to the newest (live) edge.
func (m *Model) ScrollToLive() { m.scroll = 0 }

// NeedsBackfill reports whether the viewport has scrolled close enough to the
// oldest loaded candle that the caller should fetch more history — true once
// fewer than a screenful of older candles remain to the left of the visible
// window. It is advisory: the chart never fetches, it only tells the caller when
// scrolling back is about to run out of data.
func (m Model) NeedsBackfill() bool {
	if len(m.candles) == 0 {
		return false
	}
	start, _ := m.visibleRange()
	return start <= m.capacity()
}

// SelectPrev / SelectNext move the candle-selection cursor one candle older
// (left) or newer (right). The first move from no selection lands on the newest
// visible candle; afterwards the viewport follows the cursor so it stays on
// screen.
func (m *Model) SelectPrev() { m.moveSelection(-1) }
func (m *Model) SelectNext() { m.moveSelection(+1) }

func (m *Model) moveSelection(d int) {
	if len(m.candles) == 0 {
		m.selected = -1
		return
	}
	if m.selected < 0 {
		_, end := m.visibleRange()
		m.selected = end - 1 // first press selects the newest visible candle
	} else {
		m.selected += d
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.candles) {
		m.selected = len(m.candles) - 1
	}
	m.ensureSelectedVisible()
}

// ClearSelection removes the selection cursor.
func (m *Model) ClearSelection() { m.selected = -1 }

// SelectAtX moves the selection to the candle under plot column x — x == 0 is the
// left edge of the plot area (the price gutter on the right is excluded). A click
// in the gutter or past the visible candles is ignored. This is the seam a host
// program uses to turn a mouse click into a selection.
func (m *Model) SelectAtX(x int) {
	if x < 0 || x >= m.plotWidth() || m.stride() <= 0 {
		return
	}
	start, end := m.visibleRange()
	if i := start + x/m.stride(); i >= start && i < end {
		m.selected = i
		m.ensureSelectedVisible()
	}
}

// Selected returns the selected candle and true, or false when nothing is
// selected (or the chart is empty).
func (m Model) Selected() (Candle, bool) {
	if m.selected < 0 || m.selected >= len(m.candles) {
		return Candle{}, false
	}
	return m.candles[m.selected], true
}

// PageBy scrolls by n candles (positive = back in time). Scrolling is its job;
// the selection is dragged along only to stay on screen — the cursor keeps its
// candle until that candle scrolls out of view, then clamps to the nearer
// visible edge. Two edge cases: at the oldest page pgup (n > 0) can't scroll and
// the cursor is already in view, so it is a no-op; paging newer (n < 0) while
// already pinned to the live edge can't scroll either, so it catches the cursor
// up to the live (newest) candle.
func (m *Model) PageBy(n int) {
	before := m.scroll
	m.ScrollBy(n)
	if m.selected < 0 {
		return // nothing selected: a plain scroll
	}
	m.clampSelectionToView() // drag the cursor along only if it scrolled out of view
	if n < 0 && m.scroll == before && m.scroll == 0 {
		m.selected = len(m.candles) - 1 // pgdn at the live edge: catch up to the newest
	}
}

// clampSelectionToView moves the selection the least amount needed to bring it
// back into the visible window — a no-op while it is already visible.
func (m *Model) clampSelectionToView() {
	if m.selected < 0 {
		return
	}
	start, end := m.visibleRange()
	if end <= start {
		return
	}
	if m.selected < start {
		m.selected = start
	}
	if m.selected > end-1 {
		m.selected = end - 1
	}
}

// ensureSelectedVisible nudges the scroll offset so the selected candle stays
// within the visible window.
func (m *Model) ensureSelectedVisible() {
	cap := m.capacity()
	if m.selected < 0 || cap <= 0 {
		return
	}
	n := len(m.candles)
	if m.selected > n-1-m.scroll { // off the right edge
		m.scroll = n - 1 - m.selected
	}
	if m.selected < n-m.scroll-cap { // off the left edge
		m.scroll = n - cap - m.selected
	}
	m.clampScroll()
}

// ZoomIn / ZoomOut step along the zoom ladder. Zooming in widens candles (fewer
// on screen, more detail per candle); zooming out narrows them, ending at a
// dense single-column thin-line overview.
func (m *Model) ZoomIn()  { m.setLevel(m.zoom + 1) }
func (m *Model) ZoomOut() { m.setLevel(m.zoom - 1) }

// setLevel selects a zoom-ladder index (clamped) and derives the candle width
// and gap from it; renderer() reads the same level.
func (m *Model) setLevel(i int) {
	if i < 0 {
		i = 0
	}
	if i >= len(zoomLadder) {
		i = len(zoomLadder) - 1
	}
	m.zoom = i
	lv := zoomLadder[i]
	m.candleWidth, m.gap = lv.width, lv.gap
	m.clampScroll()
	m.ensureSelectedVisible() // a tighter zoom can shrink the window past the cursor
}

// renderer is the candle renderer for the current zoom level.
func (m Model) renderer() candleRenderer { return zoomLadder[m.zoom].render }

// Update handles keyboard/mouse navigation: ←/→ (h/l) move the selection,
// pgup/pgdn and the wheel scroll, +/- zoom, end/g jumps to live. It is
// independent of the host program's other keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "left", "h":
			m.SelectPrev()
		case "right", "l":
			m.SelectNext()
		case "pgup":
			m.PageBy(m.scrollStep())
		case "pgdown":
			m.PageBy(-m.scrollStep())
		case "+", "=":
			m.ZoomIn()
		case "-", "_":
			m.ZoomOut()
		case "end", "g":
			m.ScrollToLive()
			m.ClearSelection()
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.PageBy(m.scrollStep())
		case tea.MouseWheelDown:
			m.PageBy(-m.scrollStep())
		}
	}
	return m, nil
}

func (m Model) scrollStep() int {
	if s := m.capacity() / 4; s > 1 {
		return s
	}
	return 1
}

// subState is a sub-row's content within a candle column.
type subState uint8

const (
	subEmpty subState = iota
	subWick
	subBody
)

// cell is one rendered terminal cell: a rune and an index into the per-frame
// style table (indices group into one styled run during emission).
type cell struct {
	r   rune
	sid int
}

// Fixed style-table indices. The per-frame styles slice in View is built in
// this order; overlay styles are appended after, so their ids are >= sidSel+1.
const (
	sidBlank = iota
	sidUp
	sidDown
	sidLast
	sidSel
)

// isCandleSid reports whether a cell holds a candle body/wick (up or down).
func isCandleSid(s int) bool { return s == sidUp || s == sidDown }

// View returns the chart as a string of m.height lines, reusing the cached frame
// when none of the render inputs changed since the last call (geometry, zoom,
// scroll, selection, toggles, the styles, and the candle/overlay content). A host
// that redraws every event therefore re-renders the chart only when it actually
// changes. The cache lives behind a pointer shared by value-copies of the Model,
// so the host's inline pane (a per-frame value-copy) hits it too; a zero-value
// Model (no New) has no cache and renders every call.
func (m Model) View() string {
	if m.frame == nil {
		return m.render()
	}
	key := frameKey{
		rev: m.rev, style: m.styleProbe(),
		w: m.width, h: m.height, scroll: m.scroll, selected: m.selected,
		zoom: m.zoom, candleWidth: m.candleWidth, gap: m.gap,
		showVolume: m.showVolume, showLastPrice: m.showLastPrice,
		showTimeAxis: m.showTimeAxis, overlaysOnTop: m.overlaysOnTop, live: m.live,
	}
	if m.frame.valid && m.frame.key == key {
		return m.frame.text
	}
	text := m.render()
	m.frame.key, m.frame.text, m.frame.valid = key, text, true
	return text
}

// styleProbe captures the (non-comparable) styles as a comparable string by
// rendering a fixed marker through each: if any style's output changes (e.g. a
// color-scheme or profile change), the probe changes and the frame cache misses.
func (m Model) styleProbe() string {
	const p = "\x00"
	return m.styles.Up.Render(p) + m.styles.Down.Render(p) + m.styles.Axis.Render(p) +
		m.styles.Overlay.Render(p) + m.styles.LastPrice.Render(p)
}

// render draws the chart to a string of m.height lines.
func (m Model) render() string {
	if m.width < minWidth || m.height < minHeight {
		return ""
	}
	if len(m.candles) == 0 {
		return m.placeholder("no candles")
	}
	start, end := m.visibleRange()
	vis := m.candles[start:end]
	if len(vis) == 0 {
		return m.placeholder("no candles in view")
	}

	lo, hi := m.priceBounds(vis, start)
	priceRows := m.priceRows()
	subH := priceRows * 2 // half-cell vertical resolution
	priceToSub := func(p float64) float64 { return (hi - p) / (hi - lo) * float64(subH) }

	// Style table: fixed entries first (indexed by the sid* constants), then one
	// per overlay.
	styles := []lipgloss.Style{m.styles.Axis, m.styles.Up, m.styles.Down, m.styles.LastPrice, m.styles.Axis}
	ovSid := make([]int, len(m.overlays))
	for i, o := range m.overlays {
		st := m.styles.Overlay
		if o.Color != nil {
			st = st.Foreground(o.Color)
		}
		ovSid[i] = len(styles)
		styles = append(styles, st)
	}
	// Capture each style's ANSI wrap once. emitCells and the gutters then wrap a
	// run by string concatenation (prefix + text + suffix) instead of re-running
	// the full style pipeline on every run.
	seqs := make([]sgr, len(styles))
	for i, st := range styles {
		seqs[i] = sgrOf(st)
	}
	axisSeq, lastSeq := sgrOf(m.styles.Axis), sgrOf(m.styles.LastPrice)
	sidFor := func(c Candle) int {
		if c.Close < c.Open {
			return sidDown
		}
		return sidUp
	}
	plotW := m.plotWidth()
	fillBlank := func(row []cell) {
		for i := range row {
			row[i] = cell{' ', sidBlank}
		}
	}

	// ---- price plot grid (back to front: last-price line, candles, overlays) ----
	// One backing slice sub-sliced into rows keeps the grid at two allocations
	// instead of one per row; the cells are written by index, never appended, so
	// the rows are capped to their own width.
	backing := make([]cell, plotW*priceRows)
	fillBlank(backing)
	grid := make([][]cell, priceRows)
	for r := range grid {
		grid[r] = backing[r*plotW : (r+1)*plotW : (r+1)*plotW]
	}

	lastClose := m.candles[len(m.candles)-1].Close
	lastRow := -1
	if m.showLastPrice {
		if sr := priceToSub(lastClose); sr >= 0 && sr < float64(subH) {
			lastRow = int(sr) / 2
			for c := range grid[lastRow] {
				grid[lastRow][c] = cell{'┄', sidLast}
			}
		}
	}

	style := m.renderer()
	col := make([]subState, subH) // reused across candles
	for i, c := range vis {
		candleColumnInto(col, c, priceToSub, style.mergesWicks())
		sid := sidFor(c)
		x := i * m.stride()
		for y := 0; y < priceRows; y++ {
			style.draw(grid[y], x, m.candleWidth, col[2*y], col[2*y+1], sid)
		}
		// A doji (open==close) has a zero-height body. Redraw it as a horizontal
		// dash merged with its wicks (┿/┷/┯/━), the web-chart convention, using the
		// full wick context the per-cell pass can't see. Block style only — the line
		// overview keeps its thin body (dojiBody returns ok=false).
		if c.Open == c.Close {
			if g, ok := style.dojiBody(c.High > c.Close, c.Low < c.Close); ok {
				drawDojiBody(grid, x, m.candleWidth, c, priceToSub, sid, g)
			}
		}
	}

	m.drawOverlays(grid, vis, start, priceToSub, ovSid)

	// Selection crosshair: a vertical guide through the selected candle, drawn
	// only in cells the candle/overlay didn't already fill (so the candle shows
	// on top of the line).
	if m.selected >= start && m.selected < end {
		cx := (m.selected-start)*m.stride() + m.candleWidth/2
		for y := 0; y < priceRows; y++ {
			if cx < len(grid[y]) && grid[y][cx].sid == sidBlank {
				grid[y][cx] = cell{'│', sidSel}
			}
		}
	}

	lines := make([]string, 0, m.height)
	for y := 0; y < priceRows; y++ {
		lines = append(lines, emitCells(grid[y], seqs)+m.priceGutter(y, priceRows, lo, hi, lastRow, lastClose, axisSeq, lastSeq))
	}

	// ---- volume pane ----
	if vr := m.volumeRows(); vr > 0 {
		maxVol := 0.0
		for _, c := range vis {
			if c.Volume > maxVol {
				maxVol = c.Volume
			}
		}
		volRow := make([]cell, plotW) // reused across volume rows
		for tr := 0; tr < vr; tr++ {
			fillBlank(volRow)
			row := volRow
			fromBottom := vr - 1 - tr
			if maxVol > 0 {
				for i, c := range vis {
					eighths := int(c.Volume / maxVol * float64(vr*8))
					fill := eighths - fromBottom*8
					if fill <= 0 {
						continue
					}
					if fill > 8 {
						fill = 8
					}
					x := i * m.stride()
					for k := 0; k < m.candleWidth && x+k < len(row); k++ {
						row[x+k] = cell{lowerEighths[fill], sidFor(c)}
					}
				}
			}
			// Weave a divider through the top volume row, separating it from the
			// price plot above. It fills only the empty cells — a volume bar tall
			// enough to reach this row keeps its cell, so the line skips it.
			if tr == 0 {
				for k := range row {
					if row[k].r == ' ' {
						row[k] = cell{'─', sidBlank}
					}
				}
			}
			lines = append(lines, emitCells(row, seqs)+m.volGutter(tr, maxVol, axisSeq))
		}
	}

	// ---- time axis ----
	if m.axisRows() > 0 {
		lines = append(lines, m.timeAxisLine(vis, axisSeq))
	}

	return strings.Join(lines, "\n")
}

// drawOverlays renders each overlay as a smooth Braille line interpolated
// between consecutive (candle, value) points — 2×4 sub-points per cell, so a
// line reads as continuous, not one dot per candle. Each overlay is drawn into
// its own sub-pixel grid (dots within one overlay OR-merge); NaN breaks the
// line; off-range points are clipped. By default candles take precedence — an
// overlay cell that lands on a candle body/wick is skipped — unless
// overlaysOnTop is set. Where two overlays share a cell the later one wins (one
// color per cell).
func (m Model) drawOverlays(grid [][]cell, vis []Candle, start int, priceToSub func(float64) float64, ovSid []int) {
	priceRows := len(grid)
	if priceRows == 0 {
		return
	}
	plotW := m.plotWidth()
	subCols, subRows := plotW*2, priceRows*4

	// centerSubX is the sub-pixel x of candle i's center column; subY maps a
	// price to a sub-pixel y (priceToSub gives [0,priceRows*2]; ×2 → [0,subRows]).
	centerSubX := func(i int) int { return (i*m.stride()+m.candleWidth/2)*2 + 1 }
	subY := func(v float64) int { return int(priceToSub(v) * 2) }

	for oi, o := range m.overlays {
		dots := make([]bool, subCols*subRows)
		set := func(sx, sy int) {
			if sx >= 0 && sx < subCols && sy >= 0 && sy < subRows {
				dots[sy*subCols+sx] = true
			}
		}
		prevX, prevY, havePrev := 0, 0, false
		for i := range vis {
			abs := start + i
			if abs >= len(o.Values) || math.IsNaN(o.Values[abs]) {
				havePrev = false
				continue
			}
			x, y := centerSubX(i), subY(o.Values[abs])
			if havePrev {
				plotLine(prevX, prevY, x, y, set)
			} else {
				set(x, y)
			}
			prevX, prevY, havePrev = x, y, true
		}

		for cr := 0; cr < priceRows; cr++ {
			for cc := 0; cc < plotW; cc++ {
				var bits rune
				for dy := 0; dy < 4; dy++ {
					for dx := 0; dx < 2; dx++ {
						if dots[(cr*4+dy)*subCols+(cc*2+dx)] {
							bits |= brailleDots[dy][dx]
						}
					}
				}
				if bits == 0 {
					continue
				}
				if !m.overlaysOnTop && isCandleSid(grid[cr][cc].sid) {
					continue // candle keeps the cell
				}
				grid[cr][cc] = cell{brailleBase | bits, ovSid[oi]}
			}
		}
	}
}

// plotLine walks a Bresenham line from (x0,y0) to (x1,y1), calling set per point.
func plotLine(x0, y0, x1, y1 int, set func(x, y int)) {
	dx, dy := absInt(x1-x0), -absInt(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		set(x0, y0)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sgr is the ANSI prefix/suffix a style wraps content in, captured once so a run
// can be wrapped by string concatenation rather than re-running the style
// pipeline per run. It is valid only for styles that wrap content the same way
// regardless of the content (no width/padding/border) — the chart's styles are
// foreground-only, so a run's rendering is exactly prefix + text + suffix.
type sgr struct{ pre, suf string }

// sgrOf captures a style's wrap by rendering a marker and splitting around it. A
// style that does not wrap (no color) yields empty affixes.
func sgrOf(st lipgloss.Style) sgr {
	const mark = "\x00"
	out := st.Render(mark)
	i := strings.Index(out, mark)
	if i < 0 {
		return sgr{}
	}
	return sgr{pre: out[:i], suf: out[i+len(mark):]}
}

func (s sgr) wrap(text string) string { return s.pre + text + s.suf }

// emitCells renders a row, grouping cells with the same style id into one styled
// run (fewer escape sequences). Each run is wrapped with its style's precomputed
// ANSI affixes (seqs is parallel to the style table).
func emitCells(cells []cell, seqs []sgr) string {
	var b strings.Builder
	for i := 0; i < len(cells); {
		j := i + 1
		for j < len(cells) && cells[j].sid == cells[i].sid {
			j++
		}
		s := seqs[cells[i].sid]
		b.WriteString(s.pre)
		for k := i; k < j; k++ {
			b.WriteRune(cells[k].r)
		}
		b.WriteString(s.suf)
		i = j
	}
	return b.String()
}

// visibleRange returns the [start,end) candle indices currently on screen.
func (m Model) visibleRange() (start, end int) {
	end = len(m.candles) - m.scroll
	if end > len(m.candles) {
		end = len(m.candles)
	}
	start = end - m.capacity()
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return start, end
}

// priceBounds is the auto-fit price range for the visible candles, extended to
// cover any visible overlay points so an indicator line is never clipped. The
// 3% headroom is applied once to the combined range, so an overlay extreme
// gets the same margin a candle high/low does (it never lands on the frame
// edge, where it would be clipped).
func (m Model) priceBounds(vis []Candle, start int) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, c := range vis {
		if c.Low < lo {
			lo = c.Low
		}
		if c.High > hi {
			hi = c.High
		}
	}
	for _, o := range m.overlays {
		for i := range vis {
			abs := start + i
			if abs >= len(o.Values) {
				continue
			}
			if v := o.Values[abs]; !math.IsNaN(v) {
				if v < lo {
					lo = v
				}
				if v > hi {
					hi = v
				}
			}
		}
	}
	if hi <= lo {
		// Flat: expand symmetrically so the line sits mid-chart, not on an edge.
		hi, lo = lo+0.5, lo-0.5
	}
	pad := (hi - lo) * 0.03
	return lo - pad, hi + pad
}

// candleColumn maps one candle to its per-sub-row states. mergeWicks reports
// whether the renderer can draw a body half and a wick in the same cell (the
// line style can, via ╿/╽; the block style cannot — a half-block body fills the
// cell). It governs only where a too-short wick stub is forced visible: for the
// line style the stub sits in the body's own transition cell; for the block
// style it is lifted to the adjacent body-free cell, since a block cell showing
// a body half would otherwise swallow it.
func candleColumn(c Candle, subH int, priceToSub func(float64) float64, mergeWicks bool) []subState {
	col := make([]subState, subH)
	candleColumnInto(col, c, priceToSub, mergeWicks)
	return col
}

// candleColumnInto writes one candle's column into col (len is the sub-row
// resolution), resetting it first, so a caller can reuse a single buffer across
// candles instead of allocating one per candle.
func candleColumnInto(col []subState, c Candle, priceToSub func(float64) float64, mergeWicks bool) {
	subH := len(col)
	for i := range col {
		col[i] = subEmpty
	}

	wTop := priceToSub(c.High)
	wBot := priceToSub(c.Low)
	bodyHi := math.Max(c.Open, c.Close)
	bodyLo := math.Min(c.Open, c.Close)
	bTop := priceToSub(bodyHi)
	bBot := priceToSub(bodyLo)

	bodyTop, bodyBot := -1, -1
	for i := 0; i < subH; i++ {
		center := float64(i) + 0.5
		switch {
		case center >= bTop && center <= bBot:
			col[i] = subBody
			if bodyTop < 0 {
				bodyTop = i
			}
			bodyBot = i
		case center >= wTop && center <= wBot:
			col[i] = subWick
		}
	}
	// A body too short to fill a sub-row: force one so the candle stays visible. A
	// true doji (open==close, zero-height body) is additionally redrawn as a
	// horizontal dash by drawDojiBody during rendering (block style only), where
	// the full wick context is available to merge the wick into the dash.
	if bodyTop < 0 {
		i := int(math.Round((bTop+bBot)/2 - 0.5))
		if i < 0 {
			i = 0
		}
		if i >= subH {
			i = subH - 1
		}
		col[i] = subBody
		bodyTop, bodyBot = i, i
	}

	// Connect the wick to the body whenever a high/low extends beyond it. The
	// line style merges a wick into the body's transition cell (╿/╽), so the stub
	// sits one sub-row past the body. The block style cannot: a half-block cap
	// (▄/▀) puts the body's edge at a cell's MIDDLE, so a wick — which can only
	// start at a cell boundary — would float a half-cell away (the gap the eye
	// sees). For block, snap the body's cap up to its cell's edge and place the
	// wick in the next cell, flush against it. This also revives a sub-cell stub
	// the half-cell sampling above missed entirely (rounded to a half-cell tick,
	// the display-resolution floor).
	if bodyHi < c.High { // upper wick
		at := bodyTop - 1
		if top := bodyTop / 2 * 2; !mergeWicks && top-1 >= 0 {
			// Block style with a cell above to hold the wick: snap the cap up to
			// its cell's top edge so it renders █, not ▄, and place the wick in
			// that cell, flush. When the body already sits in the top cell
			// (top-1 < 0) there is no cell above for a block wick, so leave the
			// cap unraised — don't inflate the band-extreme candle's body to the
			// frame edge. The stub is then unrenderable in block (its cell holds
			// a body half); the line style still shows it via ╿/╽ below.
			col[top] = subBody
			at = top - 1
		}
		if at >= 0 && col[at] != subBody {
			col[at] = subWick
		}
	}
	if bodyLo > c.Low { // lower wick
		at := bodyBot + 1
		if bot := bodyBot/2*2 + 1; !mergeWicks && bot+1 < subH {
			col[bot] = subBody
			at = bot + 1
		}
		if at < subH && col[at] != subBody {
			col[at] = subWick
		}
	}
}

// drawDojiBody overwrites a doji's body row with a horizontal dash spanning the
// candle width [x, x+w), placing the wick-merged glyph (center) at the candle's
// center column so the dash stays connected to the wicks drawn above and below.
// The row is the cell holding the open==close price (same sub-row candleColumnInto
// forced the body into), so the dash sits exactly where the body would.
func drawDojiBody(grid [][]cell, x, w int, c Candle, priceToSub func(float64) float64, sid int, center rune) {
	subH := len(grid) * 2
	i := int(math.Round(priceToSub(c.Close) - 0.5))
	if i < 0 {
		i = 0
	}
	if i >= subH {
		i = subH - 1
	}
	row := grid[i/2]
	cx := x + w/2
	for k := 0; k < w && x+k < len(row); k++ {
		r := '━'
		if x+k == cx {
			r = center
		}
		row[x+k] = cell{r, sid}
	}
}

// candleRenderer draws one candle's cell for a pair of vertical sub-states into
// a plot row, where the candle occupies cells [x, x+w). Each style keeps its
// glyph logic separate; the zoom ladder picks one per level.
type candleRenderer interface {
	draw(row []cell, x, w int, top, bot subState, sid int)
	// mergesWicks reports whether a single cell can show both a body half and a
	// wick (true for the line style's ╿/╽ merge glyphs, false for half-blocks).
	mergesWicks() bool
	// dojiBody returns the center glyph for a zero-height (open==close) body given
	// whether wicks extend above/below it, and whether this style draws a dedicated
	// doji dash at all. ok=false keeps the style's normal body rendering — the line
	// (zoomed-out overview) style returns false so a heavy dash never sticks out
	// among its thin strokes.
	dojiBody(hasUpper, hasLower bool) (glyph rune, ok bool)
}

// dojiGlyph is the center glyph for a doji body: a heavy horizontal dash whose
// light-vertical stems reach the cell edges so wicks above/below connect flush.
func dojiGlyph(hasUpper, hasLower bool) rune {
	switch {
	case hasUpper && hasLower:
		return '┿' // cross doji
	case hasUpper:
		return '┷' // gravestone (upper wick only)
	case hasLower:
		return '┯' // dragonfly (lower wick only)
	default:
		return '━' // four-price doji (no wicks)
	}
}

// blockCandles draws filled half-block bodies (█ ▀ ▄) across the candle width
// with a centred thin wick (│ ╵ ╷).
type blockCandles struct{}

func (blockCandles) mergesWicks() bool { return false }

func (blockCandles) dojiBody(hasUpper, hasLower bool) (rune, bool) {
	return dojiGlyph(hasUpper, hasLower), true
}

func (blockCandles) draw(row []cell, x, w int, top, bot subState, sid int) {
	glyph, isBody := glyphFor(top, bot)
	if glyph == " " {
		return
	}
	r := []rune(glyph)[0]
	if isBody {
		for k := 0; k < w && x+k < len(row); k++ {
			row[x+k] = cell{r, sid}
		}
	} else if cx := x + w/2; cx < len(row) {
		row[cx] = cell{r, sid}
	}
}

// lineCandles draws a single-column candlestick: a heavy body (┃ ╹ ╻), a thin
// wick (│ ╵ ╷), and merged boundary cells (╿ heavy-up/thin-down, ╽ thin-up/
// heavy-down) so the wick stays connected to the body. Used at 1-cell width.
type lineCandles struct{}

func (lineCandles) mergesWicks() bool { return true }

// dojiBody returns ok=false: the dense single-column overview keeps its thin
// half-stroke body (╹/╻) for a doji rather than a heavy dash that wouldn't blend.
func (lineCandles) dojiBody(_, _ bool) (rune, bool) { return 0, false }

func (lineCandles) draw(row []cell, x, _ int, top, bot subState, sid int) {
	if r := lineGlyphFor(top, bot); r != ' ' && x >= 0 && x < len(row) {
		row[x] = cell{r, sid}
	}
}

// lineGlyphFor maps a cell's (top, bottom) sub-states to a single-column line
// glyph, merging a body edge and a wick that share the cell.
func lineGlyphFor(top, bot subState) rune {
	switch {
	case top == subBody && bot == subBody:
		return '┃'
	case top == subBody && bot == subWick:
		return '╿' // heavy up, thin down
	case top == subBody:
		return '╹' // heavy up
	case top == subWick && bot == subBody:
		return '╽' // thin up, heavy down
	case bot == subBody:
		return '╻' // heavy down
	case top == subWick && bot == subWick:
		return '│'
	case top == subWick:
		return '╵'
	case bot == subWick:
		return '╷'
	}
	return ' '
}

// glyphFor maps a cell's (top, bottom) sub-states to a glyph. Body wins over
// wick within a cell (they are vertically disjoint except at the transition
// cell, where the body edge is preferred and the wick continues in the
// neighbouring cell).
func glyphFor(top, bot subState) (glyph string, isBody bool) {
	bt, bb := top == subBody, bot == subBody
	switch {
	case bt && bb:
		return "█", true
	case bt:
		return "▀", true
	case bb:
		return "▄", true
	}
	wt, wb := top == subWick, bot == subWick
	switch {
	case wt && wb:
		return "│", false
	case wt:
		return "╵", false
	case wb:
		return "╷", false
	}
	return " ", false
}

// priceGutter renders the right-hand axis for price-plot row y: top=high,
// bottom=low, middle=midpoint, and the last-price row (when shown) wins.
func (m Model) priceGutter(y, rows int, lo, hi float64, lastRow int, lastClose float64, axis, last sgr) string {
	if m.showLastPrice && y == lastRow {
		return gutterText(fmtPriceFit(lastClose, hi-lo, gutterWidth-2), last)
	}
	var price float64
	switch y {
	case 0:
		price = hi
	case rows / 2:
		price = (hi + lo) / 2
	default:
		// The bottom (low) row is deliberately unlabeled: a short value pinned to
		// the last row reads like an overflow digit dropped from the row above it.
		// The high and the midpoint give the scale; the per-label "·" marker keeps
		// each remaining label visually its own.
		return axis.wrap(strings.Repeat(" ", gutterWidth))
	}
	return gutterText(fmtPriceFit(price, hi-lo, gutterWidth-2), axis)
}

// volGutter labels the volume pane: the max volume on its top row, else blank.
// The "vol " prefix (instead of the price ticks' "· " marker) is what tells the
// label apart from a price — both share the same right-hand gutter, so without
// it the volume peak reads as one more price tick.
func (m Model) volGutter(tr int, maxVol float64, axis sgr) string {
	if tr == 0 && maxVol > 0 {
		return axis.wrap(fmt.Sprintf("vol %-*s", gutterWidth-4, compactVolume(maxVol)))
	}
	return axis.wrap(strings.Repeat(" ", gutterWidth))
}

// gutterText renders a label left-justified in the gutter behind a "· " marker,
// so each axis label reads as its own value and never runs into a neighbour.
func gutterText(s string, seq sgr) string {
	return seq.wrap(fmt.Sprintf("· %-*s", gutterWidth-2, s))
}

// timeAxisLine renders the bottom row of time labels under the candles. It spans
// the plot width plus a blank gutter, so the line is exactly m.width wide.
// Labels are placed greedily left to right at candle centers, spaced so they
// never overlap; the format adapts to the visible span (see axisLayout).
func (m Model) timeAxisLine(vis []Candle, axis sgr) string {
	w := m.plotWidth()
	runes := make([]rune, w)
	for i := range runes {
		runes[i] = ' '
	}
	layout := axisLayout(vis)
	labelW := len(layout)
	next := 0 // leftmost free column
	for i := range vis {
		x := i*m.stride() + m.candleWidth/2 // candle center column
		if x < next || x+labelW > w {
			continue
		}
		for k, r := range time.UnixMilli(vis[i].Time).Format(layout) {
			runes[x+k] = r
		}
		next = x + labelW + 2 // keep at least two blanks between labels
	}
	return axis.wrap(string(runes)) + axis.wrap(strings.Repeat(" ", gutterWidth))
}

// axisLayout chooses the time-label format from the visible span: clock time
// within a single day, date and time across days with intraday candles, or date
// alone for daily/weekly candles.
func axisLayout(vis []Candle) string {
	const day = 86_400_000
	if len(vis) < 2 {
		return "01-02 15:04"
	}
	span := vis[len(vis)-1].Time - vis[0].Time
	avg := span / int64(len(vis)-1)
	switch {
	case span < day:
		return "15:04"
	case avg < day:
		return "01-02 15:04"
	default:
		return "2006-01-02"
	}
}

func (m Model) placeholder(msg string) string {
	// Fill the FULL footprint (every row padded to m.width), so an empty/loading
	// chart occupies exactly the same box as a rendered one — the bordered overlay
	// must not shrink to the message width and then snap wider when data arrives.
	blank := strings.Repeat(" ", m.width)
	lines := make([]string, m.height)
	for i := range lines {
		lines[i] = blank
	}
	if mid := m.height / 2; mid < len(lines) && m.width >= len(msg) {
		pad := (m.width - len(msg)) / 2
		lines[mid] = m.styles.Axis.Render(strings.Repeat(" ", pad) + msg + strings.Repeat(" ", m.width-pad-len(msg)))
	}
	return strings.Join(lines, "\n")
}

// fmtPriceFit formats an axis price into at most max characters, choosing
// decimals from the visible span and degrading gracefully so a large value is
// never silently truncated: preferred decimals → no decimals → a scaled
// suffix (k/M/B). It never returns a left-sliced (corrupted) number.
func fmtPriceFit(p, span float64, max int) string {
	dec := 0
	switch {
	case span < 1:
		dec = 4
	case span < 100:
		dec = 2
	case span < 10000:
		dec = 1
	}
	if s := strconv.FormatFloat(p, 'f', dec, 64); len(s) <= max {
		return s
	}
	if s := strconv.FormatFloat(p, 'f', 0, 64); len(s) <= max {
		return s
	}
	return compactPrice(p)
}

// compactVolume renders a volume for the gutter at ~4 significant digits, and —
// unlike compactPrice — it is width-bounded: the result never exceeds the
// gutter's value field, whatever the magnitude. Above 1000 it scales through a
// k/M/B/T/Q ladder that keeps the mantissa in [1,1000), so the form stays
// "999.99X"-narrow (7 cells) instead of compactPrice's unbounded "10000.00B".
// Below 1000 it keeps fixed decimals chosen by magnitude so a small
// base-currency volume (e.g. a few BTC on a quiet hour) keeps its fraction
// instead of rounding to a bare integer ("0.9999" is the widest, 6 cells).
// Volume is secondary to the price axis, so this trades precision for fit.
func compactVolume(v float64) string {
	const maxW = gutterWidth - 4 // value field the "vol " prefix leaves behind
	abs := math.Abs(v)
	if abs < 1e3 {
		dec := 4 // abs < 1
		switch {
		case abs >= 100:
			dec = 1
		case abs >= 10:
			dec = 2
		case abs >= 1:
			dec = 3
		}
		return strconv.FormatFloat(v, 'f', dec, 64)
	}
	const suffixes = "kMBTQ" // 1e3 … 1e15
	m, i := v, -1
	for math.Abs(m) >= 1e3 && i < len(suffixes)-1 {
		m /= 1e3
		i++
	}
	suf := string(suffixes[i])
	// Pick the widest form that still fits the field, degrading decimals and
	// finally truncating with an ellipsis, so even a value that exhausts the
	// ladder (mantissa >= 1000) can never overrun the field and break the chart's
	// fixed-width geometry. Such a volume is unreachable in practice; the
	// truncation just keeps the invariant unconditional while retaining the
	// leading digits as a rough magnitude hint.
	for _, dec := range [...]int{2, 0} {
		if s := strconv.FormatFloat(m, 'f', dec, 64) + suf; len(s) <= maxW {
			return s
		}
	}
	r := []rune(strconv.FormatFloat(m, 'f', 0, 64) + suf)
	return string(r[:maxW-1]) + "…" // e.g. "1000000…"
}

// compactPrice renders a value with a magnitude suffix (e.g. 1.23M) for the
// rare case too wide for a plain integer in the gutter.
func compactPrice(p float64) string {
	abs := math.Abs(p)
	switch {
	case abs >= 1e9:
		return strconv.FormatFloat(p/1e9, 'f', 2, 64) + "B"
	case abs >= 1e6:
		return strconv.FormatFloat(p/1e6, 'f', 2, 64) + "M"
	case abs >= 1e3:
		return strconv.FormatFloat(p/1e3, 'f', 2, 64) + "k"
	default:
		return strconv.FormatFloat(p, 'f', 0, 64)
	}
}
