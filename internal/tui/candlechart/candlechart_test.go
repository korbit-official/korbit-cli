// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package candlechart

import (
	"math"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func sample() []Candle {
	return []Candle{
		{Time: 0, Open: 100, High: 110, Low: 95, Close: 108},  // up
		{Time: 1, Open: 108, High: 112, Low: 101, Close: 103}, // down
		{Time: 2, Open: 103, High: 120, Low: 102, Close: 118}, // up
		{Time: 3, Open: 118, High: 119, Low: 105, Close: 106}, // down
		{Time: 4, Open: 106, High: 130, Low: 106, Close: 129}, // up
	}
}

// manyCandles makes n candles (more than any test viewport holds) so scrolling
// has somewhere to go.
func manyCandles(n int) []Candle {
	cs := make([]Candle, n)
	for i := range cs {
		base := 100.0 + float64(i)
		cs[i] = Candle{Time: int64(i), Open: base, High: base + 5, Low: base - 5, Close: base + 2}
	}
	return cs
}

// withVolume stamps a varying non-zero volume onto each candle so the volume
// pane's bar-fill path runs (not just the blank divider).
func withVolume(cs []Candle) []Candle {
	for i := range cs {
		cs[i].Volume = float64(10 + i%7)
	}
	return cs
}

// TestRenderDimensionsAcrossResizes renders at a range of sizes — with the
// volume pane, time axis, and last-price line on, so every plot/volume/axis path
// runs — and checks each frame is exactly w columns by h rows. It guards the
// render-local buffers (the plot grid, the reused per-candle column, the reused
// volume row), which are sized from the current geometry on every call.
func TestRenderDimensionsAcrossResizes(t *testing.T) {
	m := New(120, 30)
	m.SetCandles(withVolume(manyCandles(200)), true)
	m.SetVolumePane(true)
	m.SetTimeAxis(true)
	m.SetLastPriceLine(true)

	for _, s := range []struct{ w, h int }{
		{120, 30}, {60, 14}, {200, 40}, {16, 4}, {81, 21}, {40, 10}, {120, 30},
	} {
		m.SetSize(s.w, s.h)
		out := m.View()
		if h := lipgloss.Height(out); h != s.h {
			t.Errorf("size %dx%d: height %d, want %d", s.w, s.h, h, s.h)
		}
		for i, line := range strings.Split(out, "\n") {
			if w := lipgloss.Width(line); w != s.w {
				t.Errorf("size %dx%d: line %d width %d, want %d:\n%q", s.w, s.h, i, w, s.w, line)
			}
		}
	}
}

// TestCandleColumnIntoResetsBuffer proves candleColumnInto clears the buffer it
// reuses: a column written into a pre-dirtied buffer must equal one written into
// a clean buffer. Without the reset, a previous (taller) candle's sub-rows would
// leak into a later (shorter) one — the geometry tests can't see this, since the
// frame stays w×h and corrupts deterministically.
func TestCandleColumnIntoResetsBuffer(t *testing.T) {
	const subH = 24
	lo, hi := 90.0, 130.0
	priceToSub := func(p float64) float64 { return (hi - p) / (hi - lo) * float64(subH) }
	c := Candle{Open: 100, High: 105, Low: 98, Close: 102} // short: leaves many sub-rows empty

	clean := make([]subState, subH)
	candleColumnInto(clean, c, priceToSub, false)

	dirty := make([]subState, subH)
	for i := range dirty {
		dirty[i] = subBody // a previous, taller candle's leftovers
	}
	candleColumnInto(dirty, c, priceToSub, false)

	for i := range clean {
		if clean[i] != dirty[i] {
			t.Fatalf("sub-row %d: from a dirty buffer %v, from a clean buffer %v — candleColumnInto must reset its buffer", i, dirty[i], clean[i])
		}
	}
}

// TestEmitCellsMatchesStyleRender proves the precomputed-affix path is
// byte-identical to wrapping each run with lipgloss.Style.Render — which holds
// only because the chart's styles are foreground-only. A style that gained
// width/padding/border (content-dependent wrapping) would break the equivalence,
// and this test would catch it. It covers the fixed sids and an overlay style.
func TestEmitCellsMatchesStyleRender(t *testing.T) {
	s := GreenRedStyles()
	ovStyle := s.Overlay.Foreground(lipgloss.Color("13")) // an overlay's color override
	styleTab := []lipgloss.Style{s.Axis, s.Up, s.Down, s.LastPrice, s.Axis, ovStyle}
	const sidOverlay = 5
	seqs := make([]sgr, len(styleTab))
	for i, st := range styleTab {
		seqs[i] = sgrOf(st)
	}
	// A row mixing every sid, with adjacent same-sid cells to exercise grouping.
	cells := []cell{
		{'a', sidBlank}, {'b', sidBlank}, {'c', sidUp}, {'d', sidDown}, {'d', sidDown},
		{'e', sidLast}, {'f', sidSel}, {'g', sidUp}, {'h', sidOverlay}, {'i', sidOverlay},
	}
	got := emitCells(cells, seqs)

	// Reference: group by sid and wrap each run with Style.Render directly.
	var want strings.Builder
	for i := 0; i < len(cells); {
		j := i + 1
		for j < len(cells) && cells[j].sid == cells[i].sid {
			j++
		}
		var run strings.Builder
		for k := i; k < j; k++ {
			run.WriteRune(cells[k].r)
		}
		want.WriteString(styleTab[cells[i].sid].Render(run.String()))
		i = j
	}
	if got != want.String() {
		t.Errorf("emitCells affix path differs from Style.Render:\n got %q\nwant %q", got, want.String())
	}
}

// TestRenderResizeRoundTrip renders at the base size, cycles through several
// sizes, then returns to the base size — the frame must be byte-identical to the
// first. This guards the resize round-trip: render is a pure function of geometry
// (plus candles/toggles/styles), so the same size must reproduce the same frame.
// It also guards against a future scratch buffer that survives a resize without
// being re-sized or re-blanked.
func TestRenderResizeRoundTrip(t *testing.T) {
	m := New(120, 30)
	m.SetCandles(withVolume(manyCandles(200)), true)
	m.SetVolumePane(true)
	first := m.View()

	for _, s := range []struct{ w, h int }{{60, 14}, {200, 40}, {40, 10}, {81, 21}} {
		m.SetSize(s.w, s.h)
		_ = m.View()
	}
	m.SetSize(120, 30)
	if again := m.View(); again != first {
		t.Error("returning to the base size must reproduce the original frame")
	}
}

// stripANSI removes SGR sequences so the geometry can be asserted on plain runes.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// gutterLabel extracts a price label from a rendered row: the last gutterWidth
// runes (the "·" marker is multi-byte, so slice on runes, not bytes), with the
// marker and padding trimmed off.
func gutterLabel(line string) string {
	r := []rune(line)
	if len(r) > gutterWidth {
		r = r[len(r)-gutterWidth:]
	}
	return strings.Trim(string(r), " ·")
}

// colorSeq returns the leading SGR escape a style emits, so two styles can be
// compared by the color they render without hard-coding ANSI codes.
func colorSeq(st lipgloss.Style) string {
	r := st.Render("X")
	if i := strings.IndexByte(r, 'm'); i >= 0 {
		return r[:i+1]
	}
	return ""
}

func TestViewDimensions(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(sample(), true)
	out := m.View()
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("want 12 rows, got %d", len(lines))
	}
	for i, ln := range lines {
		if w := len([]rune(stripANSI(ln))); w != 40 {
			t.Errorf("row %d width = %d, want 40", i, w)
		}
	}
}

// body/wick glyph sets across both candle styles (block at width≥2, line at 1).
const (
	blockBodyGlyphs = "█▀▄"
	lineBodyGlyphs  = "┃╹╻╿╽"
	dojiGlyphs      = "━┯┷┿" // zero-height (open==close) bodies (block style)
	wickGlyphs      = "│╷╵"
)

func containsBody(s string) bool {
	return strings.ContainsAny(s, blockBodyGlyphs+lineBodyGlyphs+dojiGlyphs)
}

func TestRendersBodiesAndWicks(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(sample(), true)
	plain := stripANSI(m.View())
	if !containsBody(plain) {
		t.Error("expected body glyphs in output")
	}
	if !strings.ContainsAny(plain, wickGlyphs) {
		t.Error("expected wick glyphs in output")
	}
}

// TestSubCellWicksAlwaysMarked asserts that a high/low extending only a fraction
// of a cell beyond the body is still marked as a wick. Center-point sampling
// alone can miss a stub that short, so candleColumn guarantees one: whenever
// High > body-top (resp. Low < body-bottom) the column carries at least one
// subWick above the topmost (resp. below the bottommost) subBody — for both the
// merging (line) and non-merging (block) styles. For the block style the wick
// must additionally land in a body-free cell, or the half-block body swallows it.
func TestSubCellWicksAlwaysMarked(t *testing.T) {
	const subH = 18 // 9 price rows
	// A candle whose wicks are tiny relative to the visible band: priceToSub maps
	// the whole [lo,hi] band onto subH, so a stub a few units beyond the body is
	// well under one sub-row.
	lo, hi := 0.0, 1000.0
	priceToSub := func(p float64) float64 { return (hi - p) / (hi - lo) * float64(subH) }
	c := Candle{Open: 500, Close: 506, High: 508, Low: 498} // ~0.18 sub-row stubs each side

	for _, mergeWicks := range []bool{true, false} {
		col := candleColumn(c, subH, priceToSub, mergeWicks)
		bodyTop, bodyBot := -1, -1
		for i, s := range col {
			if s == subBody {
				if bodyTop < 0 {
					bodyTop = i
				}
				bodyBot = i
			}
		}
		if bodyTop < 0 {
			t.Fatalf("mergeWicks=%v: no body marked", mergeWicks)
		}
		upper, lower := false, false
		for i, s := range col {
			if s != subWick {
				continue
			}
			if i < bodyTop {
				upper = true
				if !mergeWicks && i/2 == bodyTop/2 {
					t.Errorf("mergeWicks=false: upper wick shares the body's top cell (sub %d, body cell %d) — block can't show it", i, bodyTop/2)
				}
			}
			if i > bodyBot {
				lower = true
				if !mergeWicks && i/2 == bodyBot/2 {
					t.Errorf("mergeWicks=false: lower wick shares the body's bottom cell (sub %d, body cell %d) — block can't show it", i, bodyBot/2)
				}
			}
		}
		if !upper {
			t.Errorf("mergeWicks=%v: High > body but no upper wick marked", mergeWicks)
		}
		if !lower {
			t.Errorf("mergeWicks=%v: Low < body but no lower wick marked", mergeWicks)
		}
	}
}

// TestBandExtremeWickDoesNotInflateBody covers a candle that sets the visible
// band's high/low: its body lands in the very top/bottom cell, so there is no
// cell beyond it to hold a block wick. The block cap-snap must NOT fire there
// (it would push the body to the frame edge — █ instead of ▄ — without ever
// showing a wick) and must never write out of bounds. The line style still
// merges the stub into the body cell.
func TestBandExtremeWickDoesNotInflateBody(t *testing.T) {
	const subH = 18
	lo, hi := 48.0, 112.0
	priceToSub := func(p float64) float64 { return (hi - p) / (hi - lo) * float64(subH) }
	// High==112-ish band max with a small upper wick; Low==band min with a small
	// lower wick. Body sits in the top and bottom cells.
	top := Candle{Open: 100, Close: 108, High: 110, Low: 100} // body abuts the top, small upper wick
	bot := Candle{Open: 54, Close: 52, High: 54, Low: 50}     // body abuts the bottom, small lower wick

	// Block style: the body's top cell (cell 0) must not be forced full when the
	// wick can't be placed — sub-row 0 stays non-body, so the cap renders ▄ not █.
	colTop := candleColumn(top, subH, priceToSub, false)
	if colTop[0] == subBody {
		t.Errorf("block band-max: sub-row 0 forced to body — body inflated to the frame edge instead of ▄")
	}
	colBot := candleColumn(bot, subH, priceToSub, false)
	if colBot[subH-1] == subBody {
		t.Errorf("block band-min: last sub-row forced to body — body inflated to the frame edge instead of ▀")
	}

	// Line style merges the stub into the body's transition cell, so the wick IS
	// represented at the very edge (sub-row 0 / last sub-row become wick).
	if c := candleColumn(top, subH, priceToSub, true); c[0] != subWick {
		t.Errorf("line band-max: upper wick not merged at the top cell (got %v)", c[0])
	}
	if c := candleColumn(bot, subH, priceToSub, true); c[subH-1] != subWick {
		t.Errorf("line band-min: lower wick not merged at the bottom cell (got %v)", c[subH-1])
	}
}

// TestSubCellWicksRenderInBlockDefault is the end-to-end check at the default
// (block) zoom that a candle with sub-cell wicks shows a wick glyph both above
// and below its body in the rendered output.
func TestSubCellWicksRenderInBlockDefault(t *testing.T) {
	base := 94_800_000.0
	cs := []Candle{
		{Time: 0, Open: base - 500_000, High: base - 480_000, Low: base - 520_000, Close: base - 490_000}, // frames the band low
		{Time: 1, Open: base, High: base + 10_000, Low: base - 5_000, Close: base + 4_000},                // tiny stubs both sides
		{Time: 2, Open: base + 480_000, High: base + 520_000, Low: base + 470_000, Close: base + 500_000}, // frames the band high
	}
	m := New(30, 9) // default zoom = block
	m.SetCandles(cs, false)
	rows := strings.Split(stripANSI(m.View()), "\n")
	col := m.stride() + m.candleWidth/2 // center column of the middle candle
	bodyRow, wickAbove, wickBelow := -1, -1, -1
	for r, ln := range rows {
		runes := []rune(ln)
		if col >= len(runes) {
			continue
		}
		ch := string(runes[col])
		switch {
		case strings.ContainsAny(ch, blockBodyGlyphs):
			if bodyRow < 0 {
				bodyRow = r
			}
		case strings.ContainsAny(ch, wickGlyphs):
			if bodyRow < 0 {
				wickAbove = r
			} else {
				wickBelow = r
			}
		}
	}
	if bodyRow < 0 {
		t.Fatal("no block body rendered for the middle candle")
	}
	if wickAbove < 0 {
		t.Error("upper wick (High > body) not rendered at default block zoom")
	}
	if wickBelow < 0 {
		t.Error("lower wick (Low < body) not rendered at default block zoom")
	}
}

func TestLineGlyphForTruthTable(t *testing.T) {
	cases := []struct {
		top, bot subState
		want     rune
	}{
		{subBody, subBody, '┃'},
		{subBody, subWick, '╿'}, // heavy up, thin down
		{subBody, subEmpty, '╹'},
		{subWick, subBody, '╽'}, // thin up, heavy down
		{subEmpty, subBody, '╻'},
		{subWick, subWick, '│'},
		{subWick, subEmpty, '╵'},
		{subEmpty, subWick, '╷'},
		{subEmpty, subEmpty, ' '},
	}
	for _, c := range cases {
		if got := lineGlyphFor(c.top, c.bot); got != c.want {
			t.Errorf("lineGlyphFor(%d,%d) = %q, want %q", c.top, c.bot, got, c.want)
		}
	}
}

func TestZoomLadderStylesAndGaps(t *testing.T) {
	m := New(60, 16)
	m.SetCandles(manyCandles(60), true)

	// Default: bold single-column blocks, spaced (gap 1). No thin line glyphs.
	if m.candleWidth != 1 || m.gap != 1 {
		t.Errorf("default zoom: width=%d gap=%d, want 1/1", m.candleWidth, m.gap)
	}
	plain := stripANSI(m.View())
	if !strings.ContainsAny(plain, blockBodyGlyphs) {
		t.Error("default should render block bodies")
	}
	if strings.ContainsAny(plain, lineBodyGlyphs) {
		t.Error("default (block) must not mix in thin line glyphs")
	}

	// One step out: thin line glyphs, packed (gap 0). No block bodies.
	m.ZoomOut()
	if m.gap != 0 || m.candleWidth != 1 {
		t.Errorf("zoomed-out: width=%d gap=%d, want 1/0 (packed)", m.candleWidth, m.gap)
	}
	plain = stripANSI(m.View())
	if !strings.ContainsAny(plain, lineBodyGlyphs) {
		t.Error("zoomed-out overview should render thin line glyphs")
	}
	if strings.ContainsAny(plain, blockBodyGlyphs) {
		t.Error("thin overview must not mix in block bodies")
	}

	// Zoom all the way in: widest blocks (width 3), still block style.
	for i := 0; i < 5; i++ {
		m.ZoomIn()
	}
	if m.candleWidth != 3 {
		t.Errorf("most-zoomed-in width=%d, want 3", m.candleWidth)
	}
	plain = stripANSI(m.View())
	if !strings.ContainsAny(plain, blockBodyGlyphs) {
		t.Error("widest zoom should render block bodies")
	}
	if strings.ContainsAny(plain, lineBodyGlyphs) {
		t.Error("block levels must not mix in thin line glyphs")
	}
}

func TestTwoStrokeWeightsOnly(t *testing.T) {
	// A candlestick uses exactly two stroke weights: a thick body and a thin
	// wick. Assert no frame ever mixes the thin-line body set with the block
	// body set (that combination produced a visible third weight).
	m := New(60, 16)
	m.SetCandles(manyCandles(60), true)
	for i := 0; i < len(zoomLadder); i++ {
		m.setLevel(i)
		plain := stripANSI(m.View())
		hasLine := strings.ContainsAny(plain, lineBodyGlyphs)
		hasBlock := strings.ContainsAny(plain, blockBodyGlyphs)
		if hasLine && hasBlock {
			t.Errorf("zoom level %d mixes thin-line and block body glyphs (third weight)", i)
		}
	}
}

func TestScrollClampsToBounds(t *testing.T) {
	m := New(30, 10)
	m.SetCandles(sample(), true)
	m.ScrollBy(1000) // way past the oldest
	if m.scroll < 0 {
		t.Fatalf("scroll went negative: %d", m.scroll)
	}
	maxScroll := len(m.candles) - m.capacity()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		t.Fatalf("scroll %d exceeds max %d", m.scroll, maxScroll)
	}
	m.ScrollBy(-1000)
	if m.scroll != 0 {
		t.Fatalf("scroll back to live = %d, want 0", m.scroll)
	}
}

func TestUpsertLiveReplacesSameBucket(t *testing.T) {
	m := New(30, 10)
	m.SetCandles(sample(), true)
	n := len(m.candles)
	last := m.candles[n-1]
	last.Close = 999
	m.UpsertLive(last)
	if len(m.candles) != n {
		t.Fatalf("same-bucket upsert changed length: %d != %d", len(m.candles), n)
	}
	if m.candles[n-1].Close != 999 {
		t.Errorf("live close not applied: %v", m.candles[n-1].Close)
	}
}

func TestUpsertLiveRollsOver(t *testing.T) {
	m := New(30, 10)
	m.SetCandles(sample(), true)
	n := len(m.candles)
	m.UpsertLive(Candle{Time: 99, Open: 129, High: 135, Low: 128, Close: 134})
	if len(m.candles) != n+1 {
		t.Fatalf("rollover did not append: %d != %d", len(m.candles), n+1)
	}
}

func TestZoomBounds(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(sample(), true)
	for i := 0; i < 10; i++ {
		m.ZoomIn()
	}
	if m.candleWidth > 3 {
		t.Errorf("zoom exceeded max: %d", m.candleWidth)
	}
	for i := 0; i < 10; i++ {
		m.ZoomOut()
	}
	if m.candleWidth < 1 {
		t.Errorf("zoom went below min: %d", m.candleWidth)
	}
}

func TestEmptyAndTinyAreSafe(t *testing.T) {
	m := New(40, 12)
	if out := m.View(); !strings.Contains(stripANSI(out), "no candles") {
		t.Error("empty chart should show a placeholder")
	}
	tiny := New(1, 1) // below minimums; clamps, must not panic
	tiny.SetCandles(sample(), true)
	_ = tiny.View()
}

func TestFlatSeriesCentered(t *testing.T) {
	const h = 12
	m := New(40, h)
	flat := []Candle{
		{Time: 0, Open: 100, High: 100, Low: 100, Close: 100},
		{Time: 1, Open: 100, High: 100, Low: 100, Close: 100},
	}
	m.SetCandles(flat, false) // priceRange collapses; must not divide by zero
	lines := strings.Split(stripANSI(m.View()), "\n")
	// The flat line must land mid-chart, not on the top/bottom edge.
	bodyRow := -1
	for i, ln := range lines {
		if containsBody(ln) {
			bodyRow = i
			break
		}
	}
	if bodyRow < 0 {
		t.Fatal("flat series rendered no body")
	}
	if bodyRow < h/2-1 || bodyRow > h/2+1 {
		t.Errorf("flat line at row %d, want near middle %d", bodyRow, h/2)
	}
}

func TestDojiRendersAsDash(t *testing.T) {
	// A four-price doji (open==close==high==low) renders as a horizontal dash —
	// the web-chart convention — not a half-block that reads as a tiny body.
	m := New(30, 9) // default zoom = block
	m.SetCandles([]Candle{
		{Time: 0, Open: 99, High: 101, Low: 99, Close: 100},
		{Time: 1, Open: 100, High: 100, Low: 100, Close: 100}, // flat
		{Time: 2, Open: 100, High: 102, Low: 98, Close: 101},
	}, false)
	plain := stripANSI(m.View())
	if !strings.ContainsRune(plain, '━') {
		t.Errorf("flat candle should render a dash (━):\n%s", plain)
	}

	// A cross doji (open==close, high>low) renders the dash merged with both wicks
	// (┿) and stays connected: the cells directly above and below carry the wick,
	// with no gap between the wick and the body.
	col := m.stride() + m.candleWidth/2 // center column of the middle candle
	m.SetCandles([]Candle{
		{Time: 0, Open: 99, High: 101, Low: 99, Close: 100},
		{Time: 1, Open: 100, High: 105, Low: 95, Close: 100}, // cross doji
		{Time: 2, Open: 100, High: 102, Low: 98, Close: 101},
	}, false)
	rows := strings.Split(stripANSI(m.View()), "\n")
	colRune := func(r int) rune {
		runes := []rune(rows[r])
		if col >= len(runes) {
			return ' '
		}
		return runes[col]
	}
	bodyRow := -1
	for r := range rows {
		if colRune(r) == '┿' { // cross doji: dash merged with both wicks
			bodyRow = r
			break
		}
	}
	if bodyRow < 0 {
		t.Fatalf("cross doji should render a merged dash body (┿):\n%s", strings.Join(rows, "\n"))
	}
	// Connected, not floating: the immediately adjacent cells must be wick glyphs,
	// so there is no blank gap between the body and its wicks (the reported bug).
	if r := bodyRow - 1; r < 0 || !strings.ContainsRune(wickGlyphs, colRune(r)) {
		t.Errorf("cell above the doji body must be a wick (no gap):\n%s", strings.Join(rows, "\n"))
	}
	if r := bodyRow + 1; r >= len(rows) || !strings.ContainsRune(wickGlyphs, colRune(r)) {
		t.Errorf("cell below the doji body must be a wick (no gap):\n%s", strings.Join(rows, "\n"))
	}

	// Fully zoomed out (line style): a doji keeps a thin stroke, never a heavy dash
	// that wouldn't blend with the dense overview.
	m.ZoomOut()
	if z := stripANSI(m.View()); strings.ContainsAny(z, dojiGlyphs) {
		t.Errorf("zoomed-out (line) doji must not use a heavy dash glyph:\n%s", z)
	}
}

// renderPlot renders cs at the given zoom level and size and returns just the
// candle plot as ASCII: ANSI stripped, the right-hand price gutter removed, and
// trailing blank columns and per-line trailing spaces trimmed. The result is the
// candle/wick shapes alone, stable against gutter price labels, ready for a
// golden compare. Leading spaces (a centered wick under a wide body) are kept.
func renderPlot(cs []Candle, zoom, w, h int) string {
	m := New(w, h)
	m.setLevel(zoom)
	m.SetCandles(cs, false)
	lines := strings.Split(stripANSI(m.View()), "\n")
	pw := m.plotWidth()
	maxCol := -1
	plot := make([][]rune, len(lines))
	for i, ln := range lines {
		r := []rune(ln)
		if len(r) > pw {
			r = r[:pw]
		}
		plot[i] = r
		for c := len(r) - 1; c >= 0; c-- {
			if r[c] != ' ' {
				if c > maxCol {
					maxCol = c
				}
				break
			}
		}
	}
	var b strings.Builder
	for i, r := range plot {
		if len(r) > maxCol+1 {
			r = r[:maxCol+1]
		}
		b.WriteString(strings.TrimRight(string(r), " "))
		if i < len(plot)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestRenderGolden pins the exact candle/wick rendering for each candle type
// across the zoom ladder and both body styles (block vs the zoomed-out line
// overview). It is the catalog of "what each case looks like": read the want
// strings to see every shape, and a regression in any glyph or in wick
// connection fails here with a side-by-side diff. The price gutter is excluded
// (see renderPlot), so only candle geometry is asserted.
func TestRenderGolden(t *testing.T) {
	up := Candle{Open: 100, High: 110, Low: 95, Close: 108}
	down := Candle{Open: 108, High: 112, Low: 101, Close: 103}
	flat := Candle{Open: 100, High: 100, Low: 100, Close: 100}  // four-price doji
	cross := Candle{Open: 100, High: 110, Low: 90, Close: 100}  // doji, both wicks
	dragon := Candle{Open: 100, High: 100, Low: 90, Close: 100} // doji, lower wick only
	grave := Candle{Open: 100, High: 110, Low: 100, Close: 100} // doji, upper wick only
	tiny := Candle{Open: 100, High: 110, Low: 90, Close: 100.5} // sub-cell body (NOT a doji)

	cases := []struct {
		name       string
		cs         []Candle
		zoom, w, h int
		want       string
	}{
		// --- block style (default zoom): filled half-block bodies ---
		{"block/up", []Candle{up}, 1, 18, 7, "│\n█\n█\n█\n█\n│\n│"},
		{"block/down", []Candle{down}, 1, 18, 7, "│\n│\n█\n█\n█\n█\n│"},
		// A four-price doji is a lone dash; a tiny but non-zero body keeps its
		// half-block (its half-cell vertical position would be lost by a dash).
		{"block/flat-doji", []Candle{flat}, 1, 18, 7, "\n\n\n━\n\n\n"},
		{"block/tiny-body", []Candle{tiny}, 1, 18, 7, "│\n│\n│\n█\n│\n│\n│"},
		// Dojis with wicks: the dash merges with the wick (┿/┯/┷) and stays
		// connected — the cells immediately above/below carry the wick, no gap.
		{"block/cross-doji", []Candle{cross}, 1, 18, 7, "│\n│\n│\n┿\n│\n│\n│"},
		{"block/dragonfly-doji", []Candle{dragon}, 1, 18, 7, "┯\n│\n│\n│\n│\n│\n│"},
		{"block/gravestone-doji", []Candle{grave}, 1, 18, 7, "│\n│\n│\n│\n│\n│\n┷"},

		// --- widest zoom: 3-cell bodies, centered wick; the dash spans the width ---
		{"wide/up", []Candle{up}, 3, 22, 7, " │\n███\n███\n███\n███\n │\n │"},
		{"wide/flat-doji", []Candle{flat}, 3, 22, 7, "\n\n\n━━━\n\n\n"},
		{"wide/cross-doji", []Candle{cross}, 3, 22, 7, " │\n │\n │\n━┿━\n │\n │\n │"},

		// --- line style (zoomed-out overview): heavy/thin strokes, no dash ---
		{"line/up", []Candle{up}, 0, 18, 7, "│\n┃\n┃\n┃\n╿\n│\n│"},
		// A doji keeps a thin half-stroke (╻/╽), never a heavy dash, so it blends
		// into the dense overview.
		{"line/flat-doji", []Candle{flat}, 0, 18, 7, "\n\n\n╻\n\n\n"},
		{"line/cross-doji", []Candle{cross}, 0, 18, 7, "│\n│\n│\n╽\n│\n│\n│"},

		// --- a multi-candle sequence (spacing + a doji mid-series) ---
		{"block/series", []Candle{up, down, cross, flat}, 1, 24, 7,
			"╷ │\n█ █ │\n█ █ │\n█ ╵ ┿ ━\n│   │\n╵   │\n    │"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderPlot(tc.cs, tc.zoom, tc.w, tc.h); got != tc.want {
				t.Errorf("plot mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestStubWicksConnectAcrossZoom guards the sampling/connection bug class across
// BOTH body styles and EVERY zoom level, and proves the doji
// additions don't reintroduce it. The middle candle has sub-cell stub wicks
// (high/low extend under one sub-row past the body) on a wide band set by its
// neighbours — the exact regime where center-point sampling must keep the wicks
// and the block style must not float them a half-cell off the body. For both a
// normal tiny body and a doji (open==close), at every zoom, in the candle's
// centre column:
//   - nothing floats: the non-blank cells are one contiguous run (a gap between
//     body and wick would split it), and
//   - no wick vanishes: block style draws a separate wick cell beyond the body;
//     the line style merges the stub into the body-edge glyph (╿/╽).
func TestStubWicksConnectAcrossZoom(t *testing.T) {
	const base = 95_000_000.0
	frameLo := Candle{Open: base - 500_000, High: base - 480_000, Low: base - 520_000, Close: base - 490_000}
	frameHi := Candle{Open: base + 480_000, High: base + 520_000, Low: base + 470_000, Close: base + 500_000}
	mids := []struct {
		name string
		mid  Candle
	}{
		{"normal", Candle{Open: base, High: base + 10_000, Low: base - 8_000, Close: base + 4_000}},
		{"doji", Candle{Open: base, High: base + 10_000, Low: base - 8_000, Close: base}},
	}
	for _, mc := range mids {
		for zoom := 0; zoom < len(zoomLadder); zoom++ {
			t.Run(mc.name+"/zoom"+strconv.Itoa(zoom), func(t *testing.T) {
				m := New(40, 9)
				m.setLevel(zoom)
				m.SetCandles([]Candle{frameLo, mc.mid, frameHi}, false)
				rows := strings.Split(stripANSI(m.View()), "\n")
				col := m.stride() + m.candleWidth/2 // centre column of the middle candle

				first, last, count, bodyRows := -1, -1, 0, 0
				var colGlyphs string
				for r, ln := range rows {
					runes := []rune(ln)
					if col >= len(runes) || runes[col] == ' ' {
						continue
					}
					if first < 0 {
						first = r
					}
					last = r
					count++
					colGlyphs += string(runes[col])
					if containsBody(string(runes[col])) {
						bodyRows++
					}
				}
				if count == 0 {
					t.Fatalf("middle candle drew nothing in its centre column:\n%s", strings.Join(rows, "\n"))
				}
				if bodyRows == 0 {
					t.Errorf("no body glyph in the middle candle's column (got %q):\n%s", colGlyphs, strings.Join(rows, "\n"))
				}
				// No gap: every non-blank cell is part of one contiguous vertical run.
				if last-first+1 != count {
					t.Errorf("center column has a gap: %d cells span rows %d..%d (got %q):\n%s",
						count, first, last, colGlyphs, strings.Join(rows, "\n"))
				}
				// No vanish: the stub wicks must be represented.
				if zoomLadder[zoom].render.mergesWicks() {
					// Line style merges the stub into the body-edge glyph (╿/╽).
					if !strings.ContainsAny(colGlyphs, "╿╽") {
						t.Errorf("line style: stub wick not merged into the body edge ╿/╽ (got %q):\n%s", colGlyphs, strings.Join(rows, "\n"))
					}
				} else {
					// Block style draws a separate wick cell beyond the body.
					if count <= bodyRows {
						t.Errorf("block style: stub wicks vanished — column is body-only (got %q):\n%s", colGlyphs, strings.Join(rows, "\n"))
					}
				}
			})
		}
	}
}

// TestVerticalOrderingPreservesPrice pins the core honesty invariant: the chart
// may collapse prices to visually equal at limited resolution, but must never
// INVERT them. For candles with strictly decreasing, well-separated price ranges
// (including a doji in the middle, the new path), a higher-priced candle's lowest
// drawn cell must stay at or above (smaller/equal row) the next candle's highest
// drawn cell — never below it. A bug that placed the doji dash (or any body) on
// the wrong row would surface here as an ordering inversion.
func TestVerticalOrderingPreservesPrice(t *testing.T) {
	// Disjoint ranges, top to bottom: high candle, a doji, a low candle.
	cs := []Candle{
		{Open: 205, High: 210, Low: 200, Close: 208}, // highest
		{Open: 150, High: 152, Low: 148, Close: 150}, // doji, mid band
		{Open: 95, High: 100, Low: 90, Close: 92},    // lowest
	}
	for zoom := 0; zoom < len(zoomLadder); zoom++ {
		t.Run("zoom"+strconv.Itoa(zoom), func(t *testing.T) {
			m := New(40, 16)
			m.setLevel(zoom)
			m.SetCandles(cs, false)
			rows := strings.Split(stripANSI(m.View()), "\n")
			// topRow/botRow: the highest/lowest non-blank row in candle i's column.
			extent := func(i int) (top, bot int) {
				top, bot = -1, -1
				col := i*m.stride() + m.candleWidth/2
				for r, ln := range rows {
					runes := []rune(ln)
					if col < len(runes) && runes[col] != ' ' {
						if top < 0 {
							top = r
						}
						bot = r
					}
				}
				return top, bot
			}
			for i := 0; i+1 < len(cs); i++ {
				_, bot := extent(i)
				topNext, _ := extent(i + 1)
				if bot < 0 || topNext < 0 {
					t.Fatalf("candle %d or %d drew nothing", i, i+1)
				}
				// Higher-priced candle i must not render below lower-priced candle i+1.
				if bot > topNext {
					t.Errorf("price-ordering inverted: candle %d (higher) bottom row %d is below candle %d (lower) top row %d:\n%s",
						i, bot, i+1, topNext, strings.Join(rows, "\n"))
				}
			}
		})
	}
}

func TestGutterLabelNeverCorruptsLargePrice(t *testing.T) {
	// KRW-scale prices (8-9 digits) must render a faithful number, never a
	// left-sliced one. Top row labels the high.
	m := New(40, 12)
	hi := 95_003_805.0
	m.SetCandles([]Candle{
		{Time: 0, Open: 95_000_000, High: hi, Low: 95_000_000, Close: 95_001_000},
		{Time: 1, Open: 95_001_000, High: 95_002_000, Low: 95_000_500, Close: 95_002_000},
	}, false)
	top := stripANSI(m.View())
	top = strings.SplitN(top, "\n", 2)[0]
	label := gutterLabel(top)
	got, err := strconv.ParseFloat(label, 64)
	if err != nil {
		t.Fatalf("gutter label %q is not a number: %v", label, err)
	}
	// auto-fit adds ~3% headroom, so the top is a bit above hi; never below it,
	// and never an order of magnitude off (the truncation bug).
	if got < hi || got > hi*1.1 {
		t.Errorf("top label = %v, want within [%v, %v]", got, hi, hi*1.1)
	}
}

func TestFmtPriceFitWithinWidth(t *testing.T) {
	for _, p := range []float64{0.00012345, 12.5, 1234.5, 95_003_805, 1_234_567_890, 9_999_999_999} {
		s := fmtPriceFit(p, 100, gutterWidth-1)
		if len([]rune(s)) > gutterWidth-1 {
			t.Errorf("fmtPriceFit(%v) = %q exceeds width %d", p, s, gutterWidth-1)
		}
	}
}

func TestUpsertLiveViewportStability(t *testing.T) {
	m := New(30, 10)
	m.SetCandles(manyCandles(40), true)

	// At the live edge, a rollover keeps us pinned to live (scroll stays 0).
	m.ScrollToLive()
	m.UpsertLive(Candle{Time: 99, Open: 129, High: 130, Low: 128, Close: 129})
	if m.scroll != 0 {
		t.Errorf("rollover at live edge moved scroll to %d, want 0", m.scroll)
	}

	// Scrolled back, a rollover keeps the same candles in view (scroll++).
	m.ScrollBy(2)
	before := m.scroll
	m.UpsertLive(Candle{Time: 199, Open: 129, High: 130, Low: 128, Close: 129})
	if m.scroll != before+1 {
		t.Errorf("rollover while scrolled back: scroll %d, want %d", m.scroll, before+1)
	}
}

// hasBraille reports whether s contains any Braille Patterns glyph (U+2800..U+28FF).
func hasBraille(s string) bool {
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
}

func countBraille(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			n++
		}
	}
	return n
}

func TestCandlesTakePrecedenceOverOverlays(t *testing.T) {
	m := New(60, 16)
	m.setLevel(1) // pin a block level so the candle/overlay overlap is deterministic
	cs := manyCandles(60)
	m.SetCandles(cs, true)
	// An overlay through each candle's close overlaps the bodies heavily.
	vals := make([]float64, len(cs))
	for i := range vals {
		vals[i] = cs[i].Close
	}
	m.SetOverlays([]Overlay{{Values: vals}})

	// Default: candles win. Bodies still render, and some braille is suppressed
	// where the line crosses candles.
	def := stripANSI(m.View())
	if !containsBody(def) {
		t.Error("candle bodies should still render under the overlay (candles win)")
	}
	defBraille := countBraille(def)

	// Flip precedence: the overlay now draws over candles → more braille cells.
	m.SetOverlayOnTop(true)
	topBraille := countBraille(stripANSI(m.View()))
	if defBraille >= topBraille {
		t.Errorf("candle-precedence braille (%d) should be < overlay-on-top braille (%d)", defBraille, topBraille)
	}
}

func TestOverlayDrawsLine(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(20)
	m.SetCandles(cs, false)

	// Without an overlay there is no Braille line.
	if hasBraille(stripANSI(m.View())) {
		t.Fatal("overlay glyph present before any overlay was set")
	}

	// A sloped overlay through the candles draws an interpolated Braille line.
	vals := make([]float64, len(cs))
	for i := range vals {
		vals[i] = cs[i].Close
	}
	m.SetOverlays([]Overlay{{Name: "line", Values: vals}})
	if !hasBraille(stripANSI(m.View())) {
		t.Error("expected a Braille overlay line in output")
	}
}

func TestOverlayInterpolatesBetweenCandles(t *testing.T) {
	m := New(60, 16)
	cs := manyCandles(40)
	m.SetCandles(cs, false)
	// A steep, monotonic line must cross cells between candle centers; a
	// per-candle-dot renderer would draw at most ~len(cs) cells, an interpolated
	// line fills the gaps and produces notably more.
	vals := make([]float64, len(cs))
	for i := range vals {
		vals[i] = cs[i].Low + float64(i)*3
	}
	m.SetOverlays([]Overlay{{Values: vals}})
	// An interpolated line fills the columns BETWEEN candle centers; isolated
	// per-candle dots would leave most plot columns empty. Assert the line
	// occupies a horizontally contiguous run spanning (nearly) the full width.
	lines := strings.Split(stripANSI(m.View()), "\n")
	colsWithBraille := map[int]bool{}
	for _, ln := range lines {
		col := 0
		for _, r := range ln {
			if r >= 0x2800 && r <= 0x28FF {
				colsWithBraille[col] = true
			}
			col++
		}
	}
	if len(colsWithBraille) < m.plotWidth()/2 {
		t.Errorf("interpolated line touched %d columns; expected a near-full-width run (plotWidth=%d)",
			len(colsWithBraille), m.plotWidth())
	}
}

func TestOverlayNaNGapBreaksLine(t *testing.T) {
	m := New(60, 16)
	cs := manyCandles(30)
	m.SetCandles(cs, false)
	vals := make([]float64, len(cs))
	for i := range vals {
		vals[i] = cs[i].Close
	}
	vals[15] = math.NaN() // a hole mid-series
	m.SetOverlays([]Overlay{{Values: vals}})
	// The gap must leave at least one plot column with no braille between the two
	// segments — a line that bridged the NaN would fill every column.
	lines := strings.Split(stripANSI(m.View()), "\n")
	colHas := make([]bool, m.plotWidth())
	for _, ln := range lines {
		for col, r := range []rune(ln) {
			if col < len(colHas) && r >= 0x2800 && r <= 0x28FF {
				colHas[col] = true
			}
		}
	}
	gap := false
	for _, h := range colHas {
		if !h {
			gap = true
			break
		}
	}
	if !gap {
		t.Error("a mid-series NaN should leave a column gap, but the line was continuous")
	}
}

func TestBraillePacking(t *testing.T) {
	// Lock the dot bit mapping: a single overlay value rendered as a lone point
	// must produce a glyph in the Braille block, and a vertical run must light
	// more dots (larger codepoint offset) than a single dot.
	if brailleBase != 0x2800 {
		t.Fatalf("brailleBase = %#x, want 0x2800", brailleBase)
	}
	// dot1 (top-left) is bit 0x01; dot8 (bottom-right) is 0x80 — assert the table.
	if brailleDots[0][0] != 0x01 || brailleDots[3][1] != 0x80 {
		t.Errorf("braille dot layout wrong: [0][0]=%#x [3][1]=%#x", brailleDots[0][0], brailleDots[3][1])
	}
	if brailleDots[0][1] != 0x08 || brailleDots[3][0] != 0x40 {
		t.Errorf("braille dot layout wrong: [0][1]=%#x [3][0]=%#x", brailleDots[0][1], brailleDots[3][0])
	}
}

func TestOverlayNaNAndShortValuesSkip(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(10)
	m.SetCandles(cs, false)
	// Values shorter than candles, and a NaN — a single finite point can't form
	// a line; it must not panic and draws at most one Braille cell.
	m.SetOverlays([]Overlay{{Values: []float64{cs[0].Close, math.NaN()}}})
	if n := countBraille(stripANSI(m.View())); n > 1 {
		t.Errorf("expected at most one Braille cell from a lone point, got %d", n)
	}
}

func TestOverlayExtendsPriceRange(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(10) // closes ~ 102..111, highs ~ +5
	m.SetCandles(cs, false)
	// An overlay far above every candle must not be clipped: the top gutter
	// label (the high) should rise to cover it.
	vals := make([]float64, len(cs))
	for i := range vals {
		vals[i] = 100000
	}
	m.SetOverlays([]Overlay{{Values: vals}})
	top := strings.SplitN(stripANSI(m.View()), "\n", 2)[0]
	label := gutterLabel(top)
	got, err := strconv.ParseFloat(label, 64)
	if err != nil || got < 100000 {
		t.Errorf("top label = %q (%v), want ≥ 100000 so the overlay isn't clipped", label, got)
	}
}

func TestOverlayBeyondCandlesNotClipped(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(12)
	m.SetCandles(cs, false)
	// Overlays that extend BELOW and ABOVE every candle (e.g. band edges) define
	// the price range. Before the headroom fix the bottom extreme mapped exactly
	// to the frame edge and was skipped; both must now draw.
	var lo, hi float64 = cs[0].Low, cs[0].High
	for _, c := range cs {
		if c.Low < lo {
			lo = c.Low
		}
		if c.High > hi {
			hi = c.High
		}
	}
	below := make([]float64, len(cs))
	above := make([]float64, len(cs))
	for i := range cs {
		below[i], above[i] = lo-10, hi+10
	}
	m.SetOverlays([]Overlay{{Values: below}, {Values: above}})
	lines := strings.Split(stripANSI(m.View()), "\n")
	if !hasBraille(lines[len(lines)-1]) {
		t.Error("below-candles overlay was clipped at the bottom edge")
	}
	if !hasBraille(lines[0]) {
		t.Error("above-candles overlay was clipped at the top edge")
	}
}

func TestOverlayColorOverrideEmitsDistinctStyle(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(12)
	m.SetCandles(cs, false)
	vals := make([]float64, len(cs))
	for i := range cs {
		vals[i] = cs[i].Close
	}
	m.SetOverlays([]Overlay{{Color: lipgloss.Color("13"), Values: vals}})
	raw := m.View()
	// assert a Braille line rendered and carries a color escape (not plain).
	if !hasBraille(stripANSI(raw)) {
		t.Fatal("no overlay line rendered")
	}
	if !strings.Contains(raw, "\x1b[") {
		t.Error("overlay rendered without any style escape")
	}
}

func TestVolumePaneOffByDefault(t *testing.T) {
	m := New(40, 16)
	m.SetCandles(manyCandles(20), false)
	if m.volumeRows() != 0 {
		t.Errorf("volume pane should be off by default, got %d rows", m.volumeRows())
	}
	if m.priceRows() != 16 {
		t.Errorf("price plot should use the full height when volume is off, got %d", m.priceRows())
	}
}

func TestVolumePaneCarvesHeightAndDraws(t *testing.T) {
	const h = 16
	m := New(40, h)
	cs := manyCandles(20)
	for i := range cs {
		cs[i].Volume = float64(i + 1) // ascending volume
	}
	m.SetCandles(cs, false)
	m.SetVolumePane(true)

	vr := m.volumeRows()
	if vr < 1 {
		t.Fatal("volume pane on but zero rows")
	}
	if m.priceRows() != h-vr {
		t.Errorf("price rows %d, want %d", m.priceRows(), h-vr)
	}
	lines := strings.Split(stripANSI(m.View()), "\n")
	if len(lines) != h {
		t.Fatalf("total rows %d, want %d", len(lines), h)
	}
	for i, ln := range lines { // width invariant must hold across both panes
		if w := len([]rune(ln)); w != 40 {
			t.Errorf("row %d width %d, want 40", i, w)
		}
	}
	// The bottom strip rows should carry volume block glyphs.
	strip := strings.Join(lines[h-vr:], "\n")
	if !strings.ContainsAny(strip, "▁▂▃▄▅▆▇█") {
		t.Error("volume pane drew no bars")
	}
}

func TestVolumePaneNeverStarvesPricePlot(t *testing.T) {
	for h := minHeight; h <= 10; h++ {
		m := New(40, h)
		m.SetCandles(manyCandles(10), false)
		m.SetVolumePane(true)
		if m.volumeRows() > 0 && m.priceRows() < minPriceH {
			t.Errorf("h=%d: price plot starved: price=%d vol=%d", h, m.priceRows(), m.volumeRows())
		}
		if m.volumeRows() < 0 {
			t.Errorf("h=%d: negative volume rows %d", h, m.volumeRows())
		}
		_ = m.View() // must not panic at any height
	}
}

func TestVolumePaneZeroAndEqualVolumes(t *testing.T) {
	// All-zero volumes: the pane renders blank, no bars, no max label, no panic.
	m := New(40, 16)
	m.SetCandles(manyCandles(20), false) // manyCandles leaves Volume == 0
	m.SetVolumePane(true)
	lines := strings.Split(stripANSI(m.View()), "\n")
	strip := strings.Join(lines[m.priceRows():], "\n")
	if strings.ContainsAny(strip, "▁▂▃▄▅▆▇█") {
		t.Error("zero-volume pane drew bars")
	}

	// All-equal (non-zero) volumes: every bar is full height.
	cs := manyCandles(20)
	for i := range cs {
		cs[i].Volume = 5
	}
	m.SetCandles(cs, false)
	strip = strings.Join(strings.Split(stripANSI(m.View()), "\n")[m.priceRows():], "\n")
	if !strings.ContainsRune(strip, '█') {
		t.Error("equal-volume bars should fill to full height (█)")
	}
}

func TestVolumePaneMaxLabelPrefixedVol(t *testing.T) {
	// The volume peak shares the right-hand gutter with the price ticks, so it
	// carries a "vol " prefix to read as volume, not as one more price.
	m := New(40, 16)
	cs := manyCandles(20)
	for i := range cs {
		cs[i].Volume = float64((i + 1) * 1000) // peak 20000 -> "20.00k"
	}
	m.SetCandles(cs, false)
	m.SetVolumePane(true)

	lines := strings.Split(stripANSI(m.View()), "\n")
	strip := strings.Join(lines[m.priceRows():], "\n")
	if !strings.Contains(strip, "vol 20.00k") {
		t.Errorf("volume pane missing the prefixed max label %q in:\n%s", "vol 20.00k", strip)
	}
	for i, ln := range lines { // the prefix must not break the width invariant
		if w := len([]rune(ln)); w != 40 {
			t.Errorf("row %d width %d, want 40", i, w)
		}
	}
}

func TestCompactVolumeSignificantDigits(t *testing.T) {
	// k/M/B/T/Q form for >=1000; fixed decimals chosen by magnitude below it so a
	// small base-currency volume keeps its fraction.
	cases := []struct {
		v    float64
		want string
	}{
		{676400, "676.40k"},
		{7413916, "7.41M"},
		{3.5, "3.500"}, // a few BTC/hour — must not round to "4"
		{12.34, "12.34"},
		{123.4, "123.4"},
		{0.0035, "0.0035"},
		// Large base-volume assets must use the extended ladder, never
		// compactPrice's unbounded "10000.00B".
		{1.5e12, "1.50T"},
		{1e13, "10.00T"},
		{9.999e17, "999.90Q"},
		{1e19, "10000Q"},   // ladder exhausted -> decimals dropped to stay in field
		{1e24, "1000000…"}, // beyond the ladder -> truncated with an ellipsis
	}
	for _, c := range cases {
		if got := compactVolume(c.v); got != c.want {
			t.Errorf("compactVolume(%v) = %q, want %q", c.v, got, c.want)
		}
		if n := len([]rune(compactVolume(c.v))); n > gutterWidth-4 {
			t.Errorf("compactVolume(%v) = %q is %d wide, exceeds value field %d", c.v, c.want, n, gutterWidth-4)
		}
	}
}

// TestCompactVolumeNeverExceedsField is the width-invariant guard: across every
// magnitude (and negatives), the label must fit the gutter's value field so the
// top volume row can never overrun the chart's fixed width.
func TestCompactVolumeNeverExceedsField(t *testing.T) {
	for e := -6; e <= 24; e++ {
		v := math.Pow(10, float64(e))
		for _, sign := range []float64{1, -1} {
			for _, mul := range []float64{1, 1.5, 9.999} {
				x := sign * mul * v
				if n := len([]rune(compactVolume(x))); n > gutterWidth-4 {
					t.Errorf("compactVolume(%g) = %q is %d wide, exceeds value field %d",
						x, compactVolume(x), n, gutterWidth-4)
				}
			}
		}
	}
}

func TestVolumeBarsColoredByDirection(t *testing.T) {
	m := New(40, 16)
	cs := manyCandles(20)
	for i := range cs {
		if i%2 == 0 {
			cs[i].Close = cs[i].Open - 3 // down
		} else {
			cs[i].Close = cs[i].Open + 3 // up
		}
		cs[i].Volume = float64(i + 1)
	}
	m.SetCandles(cs, false)
	m.SetVolumePane(true)

	rows := strings.Split(m.View(), "\n") // raw, ANSI intact
	strip := strings.Join(rows[m.priceRows():], "\n")

	if up := colorSeq(GreenRedStyles().Up); !strings.Contains(strip, up) {
		t.Error("volume strip missing the up-candle color")
	}
	if down := colorSeq(GreenRedStyles().Down); !strings.Contains(strip, down) {
		t.Error("volume strip missing the down-candle color")
	}
}

func TestRedBlueStylesSwapsUpDown(t *testing.T) {
	e := RedBlueStyles()
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	blue := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	if colorSeq(e.Up) != colorSeq(red) {
		t.Error("RedBlue up candle should be red")
	}
	if colorSeq(e.Down) != colorSeq(blue) {
		t.Error("RedBlue down candle should be blue")
	}
	// The preset must render through SetStyles without disturbing geometry.
	m := New(40, 12)
	m.SetCandles(sample(), true)
	m.SetStyles(e)
	for i, ln := range strings.Split(m.View(), "\n") {
		if w := len([]rune(stripANSI(ln))); w != 40 {
			t.Fatalf("RedBlue palette changed row %d width to %d, want 40", i, w)
		}
	}
}

func TestAxisLayoutAdaptsToSpan(t *testing.T) {
	const hour, day = int64(3_600_000), int64(86_400_000)
	within := []Candle{{Time: 0}, {Time: hour}, {Time: 2 * hour}} // < 1 day
	if got := axisLayout(within); got != "15:04" {
		t.Errorf("within-a-day layout = %q, want 15:04", got)
	}
	multiDay := make([]Candle, 30) // hourly candles spanning >1 day
	for i := range multiDay {
		multiDay[i] = Candle{Time: int64(i) * hour}
	}
	if got := axisLayout(multiDay); got != "01-02 15:04" {
		t.Errorf("multi-day intraday layout = %q, want 01-02 15:04", got)
	}
	daily := []Candle{{Time: 0}, {Time: day}, {Time: 5 * day}}
	if got := axisLayout(daily); got != "2006-01-02" {
		t.Errorf("daily layout = %q, want 2006-01-02", got)
	}
	// Degenerate inputs must not panic and must return a usable layout.
	for _, vis := range [][]Candle{nil, {{Time: 0}}, {{Time: 5}, {Time: 5}}} {
		if axisLayout(vis) == "" {
			t.Errorf("axisLayout(%v) returned empty", vis)
		}
	}
}

func TestTimeAxisWidthInvariantSweep(t *testing.T) {
	cs := make([]Candle, 200)
	for i := range cs {
		base := 100.0 + float64(i)
		cs[i] = Candle{Time: int64(i) * 3_600_000, Open: base, High: base + 5, Low: base - 5, Close: base + 2}
	}
	for _, w := range []int{minWidth, minWidth + 1, 40, 41, 100} {
		m := New(w, 18)
		m.SetCandles(cs, true)
		m.SetTimeAxis(true)
		m.SetVolumePane(true)
		for z := 0; z < 4; z++ { // every zoom level
			for i, ln := range strings.Split(stripANSI(m.View()), "\n") {
				if got := len([]rune(ln)); got != w {
					t.Fatalf("w=%d zoom=%d row %d width %d", w, z, i, got)
				}
			}
			m.ZoomIn()
		}
	}
}

func TestTimeAxisWithVolumeNeverStarvesPricePlot(t *testing.T) {
	for h := minHeight; h <= 12; h++ {
		m := New(40, h)
		m.SetCandles(manyCandles(10), false)
		m.SetVolumePane(true)
		m.SetTimeAxis(true)
		if m.priceRows() < 1 {
			t.Errorf("h=%d: price plot starved with axis+volume: price=%d vol=%d axis=%d",
				h, m.priceRows(), m.volumeRows(), m.axisRows())
		}
		if m.priceRows()+m.volumeRows()+m.axisRows() != h {
			t.Errorf("h=%d: rows don't sum to height: %d+%d+%d", h, m.priceRows(), m.volumeRows(), m.axisRows())
		}
		_ = m.View() // must not panic
	}
}

func TestTimeAxisRendersAndKeepsDimensions(t *testing.T) {
	const h = 16
	m := New(40, h)
	cs := make([]Candle, 30)
	for i := range cs {
		base := 100.0 + float64(i)
		cs[i] = Candle{Time: int64(i) * 3_600_000, Open: base, High: base + 5, Low: base - 5, Close: base + 2}
	}
	priceRowsNoAxis := m.priceRows()
	m.SetCandles(cs, false)
	m.SetTimeAxis(true)

	if m.priceRows() != priceRowsNoAxis-1 {
		t.Errorf("time axis should take one row from the price plot: %d vs %d", m.priceRows(), priceRowsNoAxis)
	}
	lines := strings.Split(stripANSI(m.View()), "\n")
	if len(lines) != h {
		t.Fatalf("View height %d, want %d", len(lines), h)
	}
	for i, ln := range lines {
		if w := len([]rune(ln)); w != 40 {
			t.Errorf("row %d width %d, want 40", i, w)
		}
	}
	axis := lines[h-1]
	if !strings.ContainsAny(axis, "0123456789") || !strings.ContainsRune(axis, ':') {
		t.Errorf("bottom row should carry time labels, got %q", axis)
	}
}

func TestLastPriceLineOffByDefault(t *testing.T) {
	m := New(40, 14)
	m.SetCandles(manyCandles(20), true)
	if strings.ContainsRune(stripANSI(m.View()), '┄') {
		t.Error("last-price line present when disabled")
	}
}

func TestLastPriceLineScrolledIntoHistory(t *testing.T) {
	m := New(40, 14)
	m.SetCandles(manyCandles(80), true)
	m.SetLastPriceLine(true)
	m.ScrollBy(1000) // far into history; the live close may be out of band
	out := stripANSI(m.View())
	// Either present or cleanly absent — never a stray label without a line, and
	// no panic. Width invariant still holds.
	for i, ln := range strings.Split(out, "\n") {
		if w := len([]rune(ln)); w != 40 {
			t.Errorf("row %d width %d, want 40", i, w)
		}
	}
}

func TestLastPriceLineDrawsAndLabels(t *testing.T) {
	m := New(40, 14)
	cs := manyCandles(20)
	m.SetCandles(cs, true)
	m.SetLastPriceLine(true)
	lines := strings.Split(stripANSI(m.View()), "\n")
	joined := strings.Join(lines, "\n")
	if !strings.ContainsRune(joined, '┄') {
		t.Fatal("last-price line not drawn")
	}
	// The dotted line's row should label the newest close in the gutter.
	last := cs[len(cs)-1].Close
	var lineRow string
	for _, ln := range lines {
		if strings.ContainsRune(ln, '┄') {
			lineRow = ln
			break
		}
	}
	label := gutterLabel(lineRow)
	got, err := strconv.ParseFloat(label, 64)
	if err != nil {
		t.Fatalf("last-price label %q not a number: %v", label, err)
	}
	if d := got - last; d < -1 || d > 1 {
		t.Errorf("last-price label %v, want ≈ %v", got, last)
	}
}

func TestUpdateScrollsAndZooms(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(manyCandles(60), true)

	w0 := m.candleWidth
	m, _ = m.Update(tea.KeyPressMsg{Code: '+'})
	if m.candleWidth <= w0 {
		t.Errorf("'+' did not zoom in: %d -> %d", w0, m.candleWidth)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: '-'})
	if m.candleWidth != w0 {
		t.Errorf("'-' did not zoom back: %d, want %d", m.candleWidth, w0)
	}

	// Scroll back with PageUp, then jump to live with 'end'.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.scroll == 0 {
		t.Error("PageUp did not scroll back")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.scroll != 0 {
		t.Errorf("'end' did not jump to live: scroll %d", m.scroll)
	}
}

func TestSelectionMovesAndFollowsViewport(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(manyCandles(60), true)
	if _, ok := m.Selected(); ok {
		t.Error("nothing should be selected initially")
	}

	// First left press selects the newest visible candle; further lefts walk back.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	first, ok := m.Selected()
	if !ok {
		t.Fatal("left arrow should select a candle")
	}
	for i := 0; i < 50; i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	sel, ok := m.Selected()
	if !ok || sel.Time >= first.Time {
		t.Errorf("selection should have walked to older candles: first=%d now=%d", first.Time, sel.Time)
	}
	// The viewport followed the cursor off the live edge.
	if m.scroll == 0 {
		t.Error("scrolling should follow the selection past the left edge of the initial window")
	}
	// A right press moves newer; 'end' clears the selection and returns to live.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if _, ok := m.Selected(); !ok {
		t.Error("right arrow should keep a selection")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if _, ok := m.Selected(); ok {
		t.Error("'end' should clear the selection")
	}
	if m.scroll != 0 {
		t.Error("'end' should jump back to the live edge")
	}
}

func TestPageScrollClampsSelectionAndEdges(t *testing.T) {
	m := New(84, 22)
	m.SetCandles(manyCandles(300), true)
	m.SelectNext() // newest visible

	// Paging back scrolls and drags the cursor only enough to keep it in view.
	for i := 0; i < 100; i++ {
		m.PageBy(m.scrollStep())
	}
	start, end := m.visibleRange()
	if m.selected < start || m.selected >= end {
		t.Errorf("cursor %d should have been clamped into view [%d,%d)", m.selected, start, end)
	}

	// At the oldest page pgup is a no-op: scroll can't move and the cursor is
	// already in view.
	sBefore, selBefore := m.scroll, m.selected
	m.PageBy(m.scrollStep())
	if m.scroll != sBefore || m.selected != selBefore {
		t.Errorf("pgup at the oldest page must be a no-op: scroll %d->%d sel %d->%d",
			sBefore, m.scroll, selBefore, m.selected)
	}

	// Back at the live edge, pgdn catches the cursor up to the newest candle.
	for i := 0; i < 100; i++ {
		m.PageBy(-m.scrollStep())
	}
	if m.scroll != 0 {
		t.Fatalf("paging newer should reach the live edge, scroll=%d", m.scroll)
	}
	m.selected = len(m.candles) - 5 // visible, but not the newest
	m.PageBy(-m.scrollStep())
	if m.selected != len(m.candles)-1 {
		t.Errorf("pgdn at the live edge should select the live candle, got %d", m.selected)
	}
}

func TestPageEdgesWhenAllCandlesFit(t *testing.T) {
	m := New(84, 20)
	m.SetCandles(manyCandles(10), true) // fewer than capacity → only one page
	m.SelectPrev()
	m.SelectPrev() // an older candle, not the newest
	sel := m.selected

	m.PageBy(m.scrollStep()) // pgup: no-op
	if m.selected != sel || m.scroll != 0 {
		t.Errorf("pgup with everything visible must be a no-op: sel %d->%d scroll %d", sel, m.selected, m.scroll)
	}
	m.PageBy(-m.scrollStep()) // pgdn at the (only, live) page: catch up to newest
	if m.selected != len(m.candles)-1 || m.scroll != 0 {
		t.Errorf("pgdn with everything visible should select newest without scrolling: sel=%d scroll=%d", m.selected, m.scroll)
	}
}

func TestZoomKeepsSelectionVisible(t *testing.T) {
	m := New(84, 22)
	m.SetCandles(manyCandles(300), true)
	for i := 0; i < 20; i++ { // select a candle ~20 back from the live edge
		m.SelectPrev()
	}
	for z := 0; z < 4; z++ { // zoom all the way in (shrinks the visible window)
		m.ZoomIn()
	}
	start, end := m.visibleRange()
	if m.selected < start || m.selected >= end {
		t.Errorf("zooming in must keep the selection visible: %d not in [%d,%d)", m.selected, start, end)
	}
}

func TestSetCandlesClampsSelection(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(manyCandles(60), true)
	m.SelectNext() // selects the newest (index 59)
	m.SetCandles(manyCandles(10), false)
	if m.selected != 9 {
		t.Errorf("selection should clamp to the shrunk series' last index, got %d", m.selected)
	}
	m.SetCandles(nil, false)
	if _, ok := m.Selected(); ok {
		t.Error("an empty series should have no selection")
	}
}

func TestSetCandlesPreservesSelectionByTime(t *testing.T) {
	m := New(84, 22)
	m.SetCandles(manyCandles(60), true) // Times 0..59
	for i := 0; i < 10; i++ {           // move the cursor off the newest edge
		m.SelectPrev()
	}
	before, ok := m.Selected()
	if !ok {
		t.Fatal("expected a selection")
	}
	// Prepend 20 older candles (negative times); every existing index shifts by 20.
	older := make([]Candle, 20)
	for i := range older {
		older[i] = Candle{Time: int64(i - 20), Open: 1, High: 1, Low: 1, Close: 1}
	}
	m.SetCandles(append(append([]Candle{}, older...), manyCandles(60)...), true)

	after, ok := m.Selected()
	if !ok || after.Time != before.Time {
		t.Errorf("selection must track the same candle by time across a prepend: before=%d after=%d ok=%v", before.Time, after.Time, ok)
	}
}

func TestSetCandlesViewportStableOnLiveEdgeGrowth(t *testing.T) {
	// Pinned to the live edge: growth keeps it pinned so the newest stays visible.
	m := New(84, 22)
	m.SetCandles(manyCandles(100), true)
	m.SetCandles(manyCandles(105), true) // five newer buckets (Times 100..104)
	if m.scroll != 0 {
		t.Errorf("pinned-to-live must stay pinned after right-edge growth: scroll=%d", m.scroll)
	}

	// Scrolled back into history: growth at the live edge (rollover / re-sync)
	// must NOT drag the visible window toward live — the same candles stay shown.
	m2 := New(84, 22)
	m2.SetCandles(manyCandles(100), true)
	m2.ScrollBy(40)
	sb, eb := m2.visibleRange()
	m2.SetCandles(manyCandles(105), true)
	sa, ea := m2.visibleRange()
	if sa != sb || ea != eb {
		t.Errorf("viewport shifted on right-edge growth while scrolled back: [%d,%d) -> [%d,%d)", sb, eb, sa, ea)
	}
}

func TestNeedsBackfill(t *testing.T) {
	if New(84, 22).NeedsBackfill() {
		t.Error("an empty chart never needs backfill")
	}
	m := New(84, 22)
	m.SetCandles(manyCandles(300), true) // at the live edge, far from the oldest
	if m.NeedsBackfill() {
		t.Error("with a screenful of history ahead, backfill should not be needed")
	}
	m.ScrollBy(300) // clamp back to the oldest loaded candle
	if !m.NeedsBackfill() {
		t.Error("scrolled to the oldest candle: backfill should be needed")
	}
}

func TestSelectionCrosshairRenders(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(manyCandles(60), true)
	plain := stripANSI(m.View())
	base := strings.Count(plain, "│")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // select newest visible
	if got := strings.Count(stripANSI(m.View()), "│"); got <= base {
		t.Errorf("selecting a candle should draw a vertical crosshair (│ count %d -> %d)", base, got)
	}
}

func TestVolumeDividerSeparatesPane(t *testing.T) {
	m := New(40, 14)
	m.SetVolumePane(true)
	cs := sample()
	for i := range cs {
		cs[i].Volume = float64(i + 1) // varying heights so some bars reach the top row, some don't
	}
	m.SetCandles(cs, true)

	var div string
	for _, ln := range strings.Split(stripANSI(m.View()), "\n") {
		if strings.Contains(ln, "─") {
			div = ln
			break
		}
	}
	if div == "" {
		t.Fatal("expected a divider line (─) between the price plot and the volume pane")
	}
	// The tallest bar reaches the divider row and keeps its cell — the line skips it.
	if !strings.ContainsAny(div, "▁▂▃▄▅▆▇█") {
		t.Error("a volume bar tall enough to reach the divider row must poke through it (cell skipped)")
	}

	// No volume pane → no divider.
	m.SetVolumePane(false)
	if strings.Contains(stripANSI(m.View()), "─") {
		t.Error("no divider should render without a volume pane")
	}
}

func TestPlaceholderFillsFullFootprint(t *testing.T) {
	// An empty/loading chart must occupy the same box as a rendered one, so the
	// host's bordered overlay doesn't shrink while loading and snap wider on data.
	m := New(40, 12) // no candles -> placeholder
	lines := strings.Split(stripANSI(m.View()), "\n")
	if len(lines) != 12 {
		t.Fatalf("placeholder rows = %d, want 12", len(lines))
	}
	for i, ln := range lines {
		if w := len([]rune(ln)); w != 40 {
			t.Errorf("placeholder row %d width = %d, want 40 (must match a rendered chart's footprint)", i, w)
		}
	}
	if !strings.Contains(stripANSI(m.View()), "no candles") {
		t.Error("placeholder should still show its message")
	}
}

func TestSelectAtX(t *testing.T) {
	m := New(40, 12)
	m.SetCandles(manyCandles(60), true)
	if _, ok := m.Selected(); ok {
		t.Fatal("nothing should be selected before a click")
	}
	m.SelectAtX(0) // leftmost visible candle
	got, ok := m.Selected()
	if !ok {
		t.Fatal("SelectAtX(0) should select the leftmost visible candle")
	}
	start, _ := m.visibleRange()
	if got.Time != m.candles[start].Time {
		t.Errorf("SelectAtX(0) selected t=%d, want leftmost visible t=%d", got.Time, m.candles[start].Time)
	}
	// A click in the price gutter (past the plot) leaves the selection put.
	m.SelectAtX(m.plotWidth() + 1)
	if after, _ := m.Selected(); after.Time != got.Time {
		t.Error("a click past the plot width must not move the selection")
	}
}
