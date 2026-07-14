// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// chartTestModel builds a ready model wired with a candles seam that returns a
// single live bucket starting at time 0 (so any positive trade timestamp folds
// into it).
func chartTestModel(t *testing.T, w, h int) model {
	t.Helper()
	m := newModel(Config{
		Symbols: []string{"btc_krw", "eth_krw"},
		Now:     func() int64 { return 1_700_000_000_000 },
		Candles: func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
			return []candles.Bar{
				{Timestamp: 0, Open: "100", High: "110", Low: "90", Close: "100", Volume: "1"},
			}, nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return mm.(model)
}

// seedChart simulates the seed fetch completing for the currently open chart.
func seedChart(t *testing.T, m model, bars []candles.Bar) model {
	t.Helper()
	return send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, bars: bars})
}

func TestChartKeyDisabledWithoutSeam(t *testing.T) {
	m := testModel(t, false, nil) // no Candles seam
	m, _ = press(t, m, k('g', "g"))
	if m.mode == modeChart {
		t.Error("g must not open the chart when no candles seam is wired")
	}
}

func TestChartOpensAndSeeds(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	if m.mode != modeChart {
		t.Fatalf("g should open the chart, mode = %v", m.mode)
	}
	if m.chartSym != "btc_krw" || m.chartIv != defaultChartInterval {
		t.Errorf("chart opened on %s %s, want btc_krw %s", m.chartSym, m.chartIv, defaultChartInterval)
	}
	// Before the seed lands the overlay shows a loading hint.
	if !strings.Contains(plain(m.render()), "loading") {
		t.Error("chart should show loading before the seed arrives")
	}
	m = seedChart(t, m, []candles.Bar{
		{Timestamp: 0, Open: "100", High: "110", Low: "90", Close: "105", Volume: "1"},
	})
	if m.feed.Empty() {
		t.Fatal("feed should hold candles after the seed")
	}
	out := plain(m.render())
	if !strings.Contains(out, "candles · BTC/KRW · 30m") {
		t.Errorf("chart title missing/wrong:\n%s", out)
	}
}

// TestChartContentNeverExceedsContentWidth pins the fix for the click-drift bug:
// a selected candle with large OHLCV (a wide readout) must not widen the overlay
// box beyond chartContentW, or lipgloss.Place would center it differently than the
// click hit-test geometry assumes. renderChart truncates the title/readout to keep
// every content line within chartContentW.
func TestChartContentNeverExceedsContentWidth(t *testing.T) {
	m := chartTestModel(t, 84, 24) // narrow enough that a wide readout could lead
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{{
		Timestamp: 0, Open: "99999999999", High: "99999999999",
		Low: "99999999999", Close: "99999999999", Volume: "99999999.99999999",
	}})
	m.chart.SelectPrev() // select the newest candle so the readout shows full OHLCV
	if _, ok := m.chart.Selected(); !ok {
		t.Fatal("expected a selected candle so the readout is exercised")
	}
	cw := m.chartContentW()
	for i, ln := range strings.Split(m.renderChart(), "\n") {
		if w := lipgloss.Width(ln); w > cw {
			t.Errorf("chart content line %d width %d exceeds chartContentW %d: %q", i, w, cw, ln)
		}
	}
}

func TestChartEscCloses(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "1", High: "1", Low: "1", Close: "1", Volume: "1"}})
	// q must not close the overlay (esc is the one close key; inside the chart
	// g means jump-to-live, and q means nothing).
	m, _ = press(t, m, k('q', "q"))
	if m.mode != modeChart {
		t.Errorf("q must not close the chart, mode = %v", m.mode)
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if m.mode != modeNormal {
		t.Errorf("esc should close the chart, mode = %v", m.mode)
	}
}

func TestChartIntervalSwitch(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g")) // opens at "30"
	genBefore := m.chartGen
	m, cmd := press(t, m, k(']', "]")) // next interval -> "60"
	if m.chartIv != "60" {
		t.Errorf("interval after ] = %q, want 60", m.chartIv)
	}
	if m.chartGen == genBefore {
		t.Error("interval switch should bump chartGen to invalidate the old fetch/loop")
	}
	if cmd == nil {
		t.Error("interval switch should kick off a fetch + re-sync")
	}
	// Stepping below the bottom of the ladder is a no-op.
	m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "1", High: "1", Low: "1", Close: "1", Volume: "1"}})
	for i := 0; i < 10; i++ {
		m, _ = press(t, m, k('[', "["))
	}
	if m.chartIv != "1" {
		t.Errorf("interval clamped at bottom = %q, want 1", m.chartIv)
	}
}

func TestChartFoldsLiveTrade(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{
		{Timestamp: 0, Open: "100", High: "100", Low: "100", Close: "100", Volume: "1"},
	})
	// A live trade inside the bucket [0, 30m) folds into the current candle.
	m = feed(t, m, dataEvent("trade", "btc_krw", stream.OriginSnapshot, 200, "",
		`{"data":[{"timestamp":1000,"price":"108","qty":"0.5","isBuyerTaker":true,"tradeId":5}]}`))
	cs := m.chart.Candles()
	if len(cs) == 0 {
		t.Fatal("chart has no candles after fold")
	}
	if got := cs[len(cs)-1].Close; got != 108 {
		t.Errorf("live close = %v, want 108 after folding the trade", got)
	}
}

func TestChartStaleSeedIgnored(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	// A seed from a previous generation must be dropped.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen - 1, sym: m.chartSym, iv: m.chartIv,
		bars: []candles.Bar{{Timestamp: 0, Open: "9", High: "9", Low: "9", Close: "9", Volume: "9"}}})
	if !m.feed.Empty() {
		t.Error("a stale-generation seed must be ignored")
	}
}

func TestChartResyncStopsWhenClosed(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('G', "G")) // turn off the default-on inline pane
	m, _ = press(t, m, k('g', "g"))
	m, _ = press(t, m, special(tea.KeyEscape)) // closed (overlay AND inline off)
	_, cmd := m.Update(chartResyncMsg{gen: m.chartGen})
	if cmd != nil {
		t.Error("the re-sync loop must not reschedule once the chart is fully closed")
	}
}

func TestChartRolloverTriggersRefetch(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{ // live bucket [0, 30m)
		{Timestamp: 0, Open: "100", High: "100", Low: "100", Close: "100", Volume: "1"},
	})
	// A trade two buckets past the edge rolls over and must trigger an
	// authoritative re-fetch, carrying the CURRENT generation so its result
	// still applies. The skipped empty bucket [30m, 60m) is NOT synthesized —
	// no trades means no traded price, so the chart compresses the axis across
	// it (the feed runs in plain, non-gapless mode).
	mm, cmd := m.Update(streamEventMsg{ev: dataEvent("trade", "btc_krw", stream.OriginSnapshot, 300, "",
		`{"data":[{"timestamp":3700000,"price":"120","qty":"1","isBuyerTaker":true,"tradeId":7}]}`)})
	m = mm.(model)
	cs := m.chart.Candles()
	if len(cs) != 2 {
		t.Fatalf("rollover should append only the new bucket (empty gap skipped), have %d candles", len(cs))
	}
	if cs[1].Time != 3600000 || cs[1].Open != 120 {
		t.Errorf("new bucket = %+v, want open 120 at 3600000", cs[1])
	}
	if cmd == nil {
		t.Fatal("rollover should return a re-fetch command")
	}
	msg, ok := cmd().(candlesLoadedMsg)
	if !ok {
		t.Fatalf("re-fetch produced %T, want candlesLoadedMsg", cmd())
	}
	if msg.gen != m.chartGen {
		t.Errorf("re-fetch gen %d != current %d — its result would be wrongly dropped", msg.gen, m.chartGen)
	}
}

func TestChartReopenDropsStaleResyncTick(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "1", High: "1", Low: "1", Close: "1", Volume: "1"}})
	staleGen := m.chartGen
	m, _ = press(t, m, special(tea.KeyEscape)) // close
	m, _ = press(t, m, k('g', "g"))            // reopen: bumps gen, starts a fresh loop
	if m.chartGen == staleGen {
		t.Fatal("reopen should bump chartGen")
	}
	// The previous loop's pending tick (old gen) must not reschedule.
	_, cmd := m.Update(chartResyncMsg{gen: staleGen})
	if cmd != nil {
		t.Error("a stale-generation resync tick must die, not spawn another loop")
	}
}

// TestChartScrollMatchesTerminalSize guards the sizing fix: the stored chart is
// sized to the terminal in the WindowSizeMsg handler (not a render-time copy),
// so its scroll math uses the real capacity. On a large terminal, one pgdn after
// paging to the oldest must visibly scroll — it would not if the stored chart
// were stuck at its default size (scroll clamped against a phantom range).
func TestChartScrollMatchesTerminalSize(t *testing.T) {
	bars := make([]candles.Bar, 300)
	for i := range bars {
		v := strconv.Itoa(100 + i)
		bars[i] = candles.Bar{Timestamp: int64(i) * 3600000, Open: v, High: v, Low: v, Close: v, Volume: "1"}
	}
	m := newModel(Config{
		Symbols: []string{"btc_krw"}, Now: func() int64 { return 0 },
		Candles: func(s, iv string, l int, endMs int64) ([]candles.Bar, error) { return bars, nil },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50}) // large terminal
	m = mm.(model)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, bars)
	for i := 0; i < 100; i++ {
		m, _ = press(t, m, special(tea.KeyPgUp))
	}
	before := plain(m.render())
	m, _ = press(t, m, special(tea.KeyPgDown))
	if plain(m.render()) == before {
		t.Error("one pgdn after paging to the oldest must scroll at a large terminal size")
	}
}

// hourlyBars builds n consecutive 1h buckets starting at startMs.
func hourlyBars(startMs int64, n int) []candles.Bar {
	const hour = 3600000
	bars := make([]candles.Bar, n)
	for i := 0; i < n; i++ {
		bars[i] = candles.Bar{Timestamp: startMs + int64(i)*hour, Open: "1", High: "1", Low: "1", Close: "1", Volume: "1"}
	}
	return bars
}

func TestChartBackfillsOnScrollBack(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	// Seed 50h in so the older page below stays at positive bucket times (a
	// negative unix-ms bucket start is rejected as corrupt data).
	m = seedChart(t, m, hourlyBars(50*3600000, 200)) // a recent window far from its oldest edge

	for i := 0; i < 80; i++ { // page back past the oldest loaded candle
		m, _ = press(t, m, special(tea.KeyPgUp))
	}
	if !m.chartLoadingOlder {
		t.Fatal("scrolling to the oldest candle should start a history backfill")
	}
	if !strings.Contains(plain(m.render()), "loading history") {
		t.Error("a backfill in flight should show the loading-history hint")
	}

	// The older page lands: it merges at the front and clears the in-flight flag.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv,
		older: true, bars: hourlyBars(0, 50)})
	if m.chartLoadingOlder {
		t.Error("the in-flight flag must clear when the older page lands")
	}
	if got := len(m.chart.Candles()); got != 250 {
		t.Errorf("older page should merge to 250 candles, got %d", got)
	}
}

func TestChartBackfillStopsAtHistoryStart(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 200))
	for i := 0; i < 80; i++ {
		m, _ = press(t, m, special(tea.KeyPgUp))
	}
	// An empty older page means there is no more history.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, older: true, bars: nil})
	if !m.chartAtOldest {
		t.Fatal("an empty older page should latch chartAtOldest")
	}
	for i := 0; i < 10; i++ {
		m, _ = press(t, m, special(tea.KeyPgUp))
	}
	if m.chartLoadingOlder {
		t.Error("at the start of history, backfill must not retry")
	}
}

func TestChartStaleOlderPageIgnored(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(50*3600000, 200))
	before := len(m.chart.Candles())
	// An older page from a previous generation (e.g. the interval changed) must
	// not merge into the current series.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen - 1, sym: m.chartSym, iv: m.chartIv,
		older: true, bars: hourlyBars(0, 50)})
	if got := len(m.chart.Candles()); got != before {
		t.Errorf("a stale-generation older page must be ignored: %d -> %d", before, got)
	}
}

// TestChartStylesFollowPalette pins that the chart's up/down candle colors are
// derived from the shared palette (so they match the orderbook for the current
// color scheme + terminal profile), not the component's standalone presets.
func TestChartStylesFollowPalette(t *testing.T) {
	for _, s := range []uikit.ColorScheme{uikit.ColorSchemeGreenRed, uikit.ColorSchemeRedBlue} {
		pal := uikit.PaletteFor(s, colorprofile.TrueColor)
		cs := chartStyles(pal)
		if cs.Up.Render("x") != pal.Up.Fg.Render("x") {
			t.Errorf("color scheme %v: chart Up color must match the palette's up side", s)
		}
		if cs.Down.Render("x") != pal.Down.Fg.Render("x") {
			t.Errorf("color scheme %v: chart Down color must match the palette's down side", s)
		}
	}
}

// TestChartColorSchemeToggleRecolors drives the overlay: 'C' toggles the color scheme while
// the chart is open and the chart recolors live (renderChart re-derives styles
// from the palette every frame).
func TestChartColorSchemeToggleRecolors(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m = send(t, m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 50))
	if m.colorScheme != uikit.ColorSchemeGreenRed {
		t.Fatalf("default color scheme should be green-red, got %v", m.colorScheme)
	}
	before := m.render()
	m, _ = press(t, m, k('C', "C"))
	if m.colorScheme != uikit.ColorSchemeRedBlue {
		t.Fatalf("C in the chart overlay must toggle the color scheme, got %v", m.colorScheme)
	}
	if m.render() == before {
		t.Error("toggling the color scheme must recolor the chart")
	}
}

func TestChartRendersAtMinSize(t *testing.T) {
	m := chartTestModel(t, minWidth, minHeight)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "1", High: "2", Low: "1", Close: "2", Volume: "1"}})
	_ = m.View() // must not panic at the minimum terminal size
}

// --- inline candle pane (G) ---

func TestInlineChartOnByDefaultAndToggles(t *testing.T) {
	m := chartTestModel(t, 160, 44) // tall enough to fit the inline pane
	if !m.chartInline {
		t.Fatal("the inline chart should be on by default")
	}
	if m.mode != modeNormal {
		t.Errorf("the default inline pane must not enter the overlay, mode = %v", m.mode)
	}
	m = seedChart(t, m, hourlyBars(0, 50)) // the initial load's seed lands
	out := plain(m.render())
	if !strings.Contains(out, "candles · BTC/KRW · 30m") {
		t.Errorf("inline pane title missing while shown:\n%s", out)
	}
	if !strings.Contains(out, "orderbook") {
		t.Error("the orderbook must still render below the inline chart")
	}
	// G toggles it off…
	m, _ = press(t, m, k('G', "G"))
	if m.chartInline {
		t.Fatal("G should turn the inline chart off")
	}
	if strings.Contains(plain(m.render()), "candles · BTC/KRW") {
		t.Error("the inline pane must be gone after toggling off")
	}
	// …and back on (the feed is still loaded, so it shows immediately).
	m, _ = press(t, m, k('G', "G"))
	if !m.chartInline {
		t.Fatal("a second G should turn the inline chart back on")
	}
	if !strings.Contains(plain(m.render()), "candles · BTC/KRW · 30m") {
		t.Error("the inline pane should render again after toggling back on")
	}
}

// TestInlineChartHiddenOnShortTerminal: below the inline height threshold the
// pane hides and stays quiet. Every supported terminal (≥ the hard size floor)
// clears the threshold, so this drives a sub-floor height — the layout math
// still runs there (renderBody stays total at any size; the size gate only
// swaps what render() shows), and the pane must hide rather than fight the
// book for rows.
func TestInlineChartHiddenOnShortTerminal(t *testing.T) {
	m := chartTestModel(t, 120, 22) // bodyH 18: below the inline threshold
	if !m.chartInline {
		t.Fatal("the inline chart is on by default even on a short terminal")
	}
	if m.inlineChartVisible() {
		t.Fatal("the inline pane must not show on a short terminal")
	}
	// Hidden ⇒ quiet: the feed must not even be loaded (no REST poll / fold loop).
	if m.chartSym != "" || !m.feed.Empty() {
		t.Errorf("a hidden inline pane must not load the feed (sym=%q empty=%v)", m.chartSym, m.feed.Empty())
	}
	if m.chartActive() {
		t.Error("a hidden inline pane (overlay closed) must not be chartActive")
	}
	if strings.Contains(plain(m.renderBody(m.bodyHeight())), "candles · BTC/KRW") {
		t.Error("a short terminal must not render the inline pane")
	}
	// Growing the terminal reveals it and seeds the feed without another keypress.
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 44})
	m = mm.(model)
	if !m.inlineChartVisible() {
		t.Fatal("the inline pane should appear once the terminal is tall enough")
	}
	if m.chartSym != "btc_krw" {
		t.Errorf("the resize that reveals the pane should seed the feed, chartSym = %q", m.chartSym)
	}
	if !strings.Contains(plain(m.render()), "candles · BTC/KRW") {
		t.Error("the inline pane should render after the resize")
	}
}

// TestInlineChartPinnedToLive proves the inline pane ignores the overlay's
// scroll position: scrolling the overlay deep into history and closing it must
// leave the inline render unchanged (it always shows the current/live edge).
func TestInlineChartPinnedToLive(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('G', "G"))
	m = seedChart(t, m, hourlyBars(0, 300))
	live := plain(m.render())
	// Open the overlay, page far back, then close it.
	m, _ = press(t, m, k('g', "g"))
	for i := 0; i < 100; i++ {
		m, _ = press(t, m, special(tea.KeyPgUp))
	}
	m, _ = press(t, m, special(tea.KeyEscape))
	if got := plain(m.render()); got != live {
		t.Error("the inline pane must stay pinned to the live edge regardless of overlay scrolling")
	}
}

// TestInlineChartKeepsFeedLiveWithoutOverlay drives the chartActive() gate: with
// only the inline pane on (no overlay), live trades still fold and the re-sync
// loop keeps ticking; turning the pane off lets the loop die.
func TestInlineChartKeepsFeedLiveWithoutOverlay(t *testing.T) {
	m := chartTestModel(t, 160, 44) // inline on by default
	m = seedChart(t, m, []candles.Bar{{Timestamp: 0, Open: "100", High: "100", Low: "100", Close: "100", Volume: "1"}})
	if m.mode == modeChart {
		t.Fatal("the overlay must not be open in this test")
	}
	// A live trade folds into the current bucket via foldChartTrades.
	m = feed(t, m, dataEvent("trade", "btc_krw", stream.OriginSnapshot, 200, "",
		`{"data":[{"timestamp":1000,"price":"108","qty":"0.5","isBuyerTaker":true,"tradeId":5}]}`))
	cs := m.chart.Candles()
	if len(cs) == 0 || cs[len(cs)-1].Close != 108 {
		t.Error("a live trade must fold into the feed while the inline pane is on")
	}
	// The re-sync loop reschedules while the pane is on.
	if _, cmd := m.Update(chartResyncMsg{gen: m.chartGen}); cmd == nil {
		t.Error("the re-sync loop must keep ticking while the inline pane is on")
	}
	// Turning the pane off lets it die.
	m, _ = press(t, m, k('G', "G"))
	if _, cmd := m.Update(chartResyncMsg{gen: m.chartGen}); cmd != nil {
		t.Error("the re-sync loop must stop once the inline pane is off and the overlay is closed")
	}
}

// TestFeedCapBoundsRetainedSeries pins the long-run memory bound: an open
// chart's series is trimmed to maxFeedCandles on live-edge growth, so a session
// left running for weeks cannot grow it without limit as buckets roll over. The
// newest (live) candle is always kept.
func TestFeedCapBoundsRetainedSeries(t *testing.T) {
	m := chartTestModel(t, 160, 44)
	// A seed larger than the cap stands in for the accumulation a long-lived
	// chart reaches via rollovers; the seed path trims the same way.
	over := hourlyBars(0, maxFeedCandles+1000)
	m = seedChart(t, m, over)
	if got := m.feed.Len(); got != maxFeedCandles {
		t.Fatalf("feed length = %d, want it capped at %d", got, maxFeedCandles)
	}
	// The retained tail is the newest buckets, not the oldest.
	newest := over[len(over)-1].Timestamp
	if live, ok := m.feed.Live(); !ok || live.Time != newest {
		t.Errorf("live bucket = %v (ok=%v), want the newest seeded bucket %d", live, ok, newest)
	}
}

// TestFeedCapLimitsScrollBack pins the scroll-back limit half of the retention
// cap: a backfill page that overshoots maxFeedCandles is trimmed back to it, and
// once the retained window is full maybeBackfill stops paging older history — so
// deep scroll-back cannot grow the series past the bound and there is no
// fetch/trim churn at the oldest edge.
func TestFeedCapLimitsScrollBack(t *testing.T) {
	const hour = 3600000
	base := int64(200_000) * hour // high base so older backfill pages stay positive
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(base, maxFeedCandles-100)) // just under the cap

	// An older page that pushes past the cap is trimmed back to exactly the window.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv,
		older: true, bars: hourlyBars(base-300*hour, 300)})
	if got := m.feed.Len(); got != maxFeedCandles {
		t.Fatalf("overshooting backfill should trim to the cap: feed=%d, want %d", got, maxFeedCandles)
	}

	// Jump to the oldest retained candle so NeedsBackfill would otherwise fire.
	m.chart.ScrollBy(maxFeedCandles)
	if !m.chart.NeedsBackfill() {
		t.Fatal("expected the viewport at the oldest edge to want a backfill")
	}
	if cmd := m.maybeBackfill(); cmd != nil {
		t.Error("maybeBackfill must stop paging older once the series hit maxFeedCandles")
	}
	if m.chartLoadingOlder {
		t.Error("no older page should be started once the retained window is full")
	}
}

// TestInlineChartFollowsActiveMarket pins that the inline pane re-points to the
// new symbol when the active market settles (on the same debounce as the
// orderbook/trade re-subscribe).
func TestInlineChartFollowsActiveMarket(t *testing.T) {
	m := newModel(Config{
		Symbols:         []string{"btc_krw", "eth_krw"},
		Now:             func() int64 { return 1_700_000_000_000 },
		SetActiveMarket: func(prev, next, nextLevel string) {}, // wired so setActive schedules the settle
		Candles: func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
			return hourlyBars(0, 10), nil
		},
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	m = mm.(model)
	m = seedChart(t, m, hourlyBars(0, 10)) // inline on by default; deliver its initial seed
	if m.chartSym != "btc_krw" {
		t.Fatalf("inline chart should start on btc_krw, got %q", m.chartSym)
	}
	m, _ = press(t, m, special(tea.KeyDown)) // select eth_krw (schedules the settle)
	mm, cmd := m.Update(resubscribeMsg{gen: m.subGen})
	m = mm.(model)
	if m.chartSym != "eth_krw" {
		t.Errorf("inline chart should follow to eth_krw, got %q", m.chartSym)
	}
	if cmd == nil {
		t.Error("following the market should kick off a fresh seed fetch")
	}
}

// TestChartIndicatorKeyCycles drives the 'i' key: EMA 20 is on by default and
// named in the title, 'i' steps it off, and another 'i' wraps back on. The
// inline pane names the active indicator too (shared chartTitle).
func TestChartIndicatorKeyCycles(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('g', "g")) // open the chart overlay
	m = seedChart(t, m, hourlyBars(0, 300))
	if m.chartInd != 1 {
		t.Fatalf("indicator should default to EMA 20 (index 1), got %d", m.chartInd)
	}
	if !strings.Contains(m.renderChart(), "EMA 20") {
		t.Error("the chart title should name the default indicator")
	}
	if !strings.Contains(m.renderInlineChart(60, 20), "EMA 20") {
		t.Error("the inline pane title should name the active indicator too")
	}

	// 'i' steps to "off" (only off + EMA registered).
	m, _ = press(t, m, k('i', "i"))
	if m.chartInd != 0 {
		t.Fatalf("after 'i' chartInd = %d, want 0 (off)", m.chartInd)
	}
	if strings.Contains(m.renderChart(), "EMA 20") {
		t.Error("no indicator label expected while off")
	}

	// Another 'i' wraps back to EMA 20.
	m, _ = press(t, m, k('i', "i"))
	if m.chartInd != 1 {
		t.Fatalf("indicator should wrap back to EMA 20, got %d", m.chartInd)
	}
}

// TestInlineChartRendersInheritedCandles guards the no-re-feed optimization: the
// inline pane renders from the stored chart's candles (kept current by the Update
// path), so after a seed it shows candle glyphs — not the empty/loading
// placeholder — without calling SetCandles on its value-copy.
func TestInlineChartRendersInheritedCandles(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m = seedChart(t, m, hourlyBars(0, 300)) // feeds m.chart via applyCandles
	out := m.renderInlineChart(80, 20)
	if strings.Contains(out, "no candles") || strings.Contains(out, "loading") {
		t.Fatalf("inline pane did not render the inherited candles: %q", out)
	}
	if !strings.ContainsAny(out, "█▀▄┃╹╻") {
		t.Error("inline pane shows no candle glyphs; it may not be reading the stored chart's series")
	}
}

func TestChartOverlayMouseClickSelects(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 300))
	if _, ok := m.chart.Selected(); ok {
		t.Fatal("nothing should be selected before a click")
	}
	col, row := m.chartScreenOrigin()
	m = send(t, m, mclick(col+10, row+2)) // a cell well inside the plot
	if _, ok := m.chart.Selected(); !ok {
		t.Error("a left click inside the overlay plot should select a candle")
	}
}

func TestChartOverlayMouseClickOutsideIgnored(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 300))
	col, row := m.chartScreenOrigin()
	m = send(t, m, mclick(col, row-3)) // above the chart (title/readout area)
	if _, ok := m.chart.Selected(); ok {
		t.Error("a click outside the chart rows must not select a candle")
	}
}

func TestInlineChartClickOpensOverlay(t *testing.T) {
	m := chartTestModel(t, 200, 50) // inline on by default, overlay closed
	m = seedChart(t, m, hourlyBars(0, 300))
	if m.mode == modeChart {
		t.Fatal("overlay should be closed before the click")
	}
	sideW, _, _, _ := m.colWidths()
	cx, cy := sideW+2, m.bodyTop()+1
	if !m.inlineChartAt(cx, cy) {
		t.Fatal("test point is not over the inline chart strip")
	}
	m = send(t, m, mclick(cx, cy)) // click the inline strip
	if m.mode != modeChart {
		t.Errorf("clicking the inline chart should open the full overlay; mode = %v", m.mode)
	}
}

func TestChartOverlayClickOutsideCloses(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 300))
	if m.mode != modeChart {
		t.Fatal("overlay should be open")
	}
	left, top, _, _ := m.chartOverlayBounds()
	if left <= 0 {
		t.Skip("no left margin at this width to click into")
	}
	m = send(t, m, mclick(left-1, top+1)) // just outside the box frame
	if m.mode != modeNormal {
		t.Errorf("a click outside the overlay box should dismiss it; mode = %v", m.mode)
	}
}

func TestChartOverlayClickFrameStaysOpen(t *testing.T) {
	m := chartTestModel(t, 200, 50)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 300))
	left, top, _, _ := m.chartOverlayBounds()
	m = send(t, m, mclick(left, top)) // the top-left border cell counts as inside
	if m.mode != modeChart {
		t.Error("a click on the overlay frame must not dismiss it")
	}
}

func TestChartShowsLoadErrorWhenEmpty(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	// The seed fetch fails before anything has loaded.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, err: errors.New("network down")})
	if m.chartErr == "" {
		t.Fatal("a failed seed with an empty feed must record the error")
	}
	// Assert on the chart overlay itself (renderChart), not the whole frame — the
	// header's ticker line shows its own "loading…" when no ticker has arrived.
	out := plain(m.renderChart())
	if !strings.Contains(out, "load failed") || !strings.Contains(out, "network down") {
		t.Errorf("overlay should show the load error:\n%s", out)
	}
	if strings.Contains(out, "loading…") {
		t.Error("a failed load must not still say loading…")
	}
	// A later successful seed clears the error and shows the chart.
	m = seedChart(t, m, hourlyBars(0, 50))
	if m.chartErr != "" {
		t.Error("a successful seed must clear the load error")
	}
	if strings.Contains(plain(m.renderChart()), "load failed") {
		t.Error("the error must be gone after a successful seed")
	}
}

func TestChartRefreshErrorKeepsData(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = seedChart(t, m, hourlyBars(0, 50)) // data loaded
	n := len(m.chart.Candles())
	// A later re-sync fetch fails: keep the data, set no persistent error.
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, err: errors.New("boom")})
	if m.chartErr != "" {
		t.Error("a refresh failure while data exists must not set the persistent error")
	}
	if len(m.chart.Candles()) != n {
		t.Error("a failed refresh must keep the existing candles")
	}
	if strings.Contains(plain(m.render()), "load failed") {
		t.Error("the chart must keep rendering its data, not a load-failed title")
	}
}

func TestChartLoadErrorClearedOnIntervalChange(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, err: errors.New("boom")})
	if m.chartErr == "" {
		t.Fatal("precondition: load error set")
	}
	m, _ = press(t, m, k(']', "]")) // change interval -> loadChart resets
	if m.chartErr != "" {
		t.Error("changing interval should clear the prior load error")
	}
}

func TestInlineChartShowsLoadError(t *testing.T) {
	m := chartTestModel(t, 160, 44) // inline on by default, overlay closed
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, err: errors.New("boom")})
	if m.chartErr == "" {
		t.Fatal("an inline load failure should record the error")
	}
	if !strings.Contains(plain(m.render()), "load failed") {
		t.Errorf("the inline pane should show a load-failed hint:\n%s", plain(m.render()))
	}
}

func TestChartReopenRetriesAfterError(t *testing.T) {
	m := chartTestModel(t, 120, 32)
	m, _ = press(t, m, k('g', "g"))
	m = send(t, m, candlesLoadedMsg{gen: m.chartGen, sym: m.chartSym, iv: m.chartIv, err: errors.New("boom")})
	if m.chartErr == "" {
		t.Fatal("precondition: load error set")
	}
	m, _ = press(t, m, special(tea.KeyEscape)) // close
	m, cmd := press(t, m, k('g', "g"))         // reopen: empty feed -> a real (re)load
	if m.chartErr != "" {
		t.Error("reopening an errored chart should clear the error and retry")
	}
	if cmd == nil {
		t.Error("reopening an empty chart should kick a fresh fetch")
	}
}

func TestChartResyncEveryClampAndScale(t *testing.T) {
	if d := chartResyncEvery("1"); d != 5*time.Second { // 60000/12=5000ms
		t.Errorf("1m resync = %v, want 5s", d)
	}
	if d := chartResyncEvery("5"); d != 25*time.Second { // 300000/12=25000ms
		t.Errorf("5m resync = %v, want 25s", d)
	}
	if d := chartResyncEvery("1D"); d != 30*time.Second { // clamped
		t.Errorf("1D resync = %v, want 30s (clamped)", d)
	}
	if d := chartResyncEvery("bogus"); d != 30*time.Second {
		t.Errorf("unknown resync = %v, want 30s", d)
	}
}

// TestChartCandlesOmitsNoTradeFillers pins the provider-side contract: a
// zero-volume bucket (no trades → no traded price) never reaches the chart,
// so the slot-based renderer compresses the time axis across it instead of
// drawing the carried-forward filler price as a flat candle.
func TestChartCandlesOmitsNoTradeFillers(t *testing.T) {
	var s candles.Series
	s.ResetGapless("1") // gapless mode synthesizes fillers on a jump
	s.Seed([]candles.Bar{{Timestamp: 60_000, Open: "100", High: "100", Low: "100", Close: "100", Volume: "1"}}, 0)
	s.FoldTrade(1, "108", "1", 245_000) // jump: fillers at 120000/180000, real bar at 240000

	cs := chartCandles(&s)
	if len(cs) != 2 {
		t.Fatalf("chart candles = %d, want 2 (fillers omitted): %+v", len(cs), cs)
	}
	if cs[0].Time != 60_000 || cs[1].Time != 240_000 {
		t.Errorf("times = %d,%d — want the two traded buckets only", cs[0].Time, cs[1].Time)
	}
}
