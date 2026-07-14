// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/candlechart"
	"github.com/korbit-official/korbit-cli/internal/tui/chartind"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// chartIndicatorOptions is the cycle of overlay indicators the chart key 'i'
// steps through. Index 0 is "off"; each later entry names an indicator and the
// factory that builds it. Add an entry to offer another indicator — the chart
// and the candle feed need no change, since candlechart owns the wiring (see
// [candlechart.Indicator]).
var chartIndicatorOptions = []struct {
	label string
	make  func() []candlechart.Indicator
}{
	{"off", nil},
	{"EMA 20", func() []candlechart.Indicator { return []candlechart.Indicator{chartind.EMA{Period: 20}} }},
}

// indicatorsFor returns the candlechart indicators for option index i (nil when
// off or out of range).
func indicatorsFor(i int) []candlechart.Indicator {
	if i <= 0 || i >= len(chartIndicatorOptions) || chartIndicatorOptions[i].make == nil {
		return nil
	}
	return chartIndicatorOptions[i].make()
}

// candlesLoadedMsg carries the result of a candles fetch. It is applied only if
// gen/sym/iv still match the open chart (else it is stale). older marks a
// scroll-back history page (merged at the front via Backfill) versus a recent
// seed/re-sync (merged at the live edge via Seed).
type candlesLoadedMsg struct {
	gen   int
	sym   string
	iv    string
	bars  []candles.Bar
	older bool
	err   error
}

// chartResyncMsg fires on the re-sync cadence while the chart is open.
type chartResyncMsg struct{ gen int }

// chartActive reports whether the candle feed should be kept live — i.e. the
// full-screen overlay is open OR the inline pane is actually shown. The re-sync
// loop and live trade-folding gate on this, so the feed stays fresh for either
// consumer and goes quiet once both are gone. It tracks inline *visibility* (not
// just the preference), so a pane hidden on a short terminal polls nothing.
func (m model) chartActive() bool { return m.mode == modeChart || m.inlineChartVisible() }

// ensureChartLoaded seeds (or re-points) the feed when the chart is active but
// the feed isn't current for the active symbol+interval — covering the inline
// pane's first appearance, a resize that reveals it, and a regrow after the
// market changed while it was hidden. A no-op when the feed already matches (so a
// resize during the overlay never resets its scroll/selection).
func (m model) ensureChartLoaded() (tea.Model, tea.Cmd) {
	if !m.chartActive() || m.cfg.Candles == nil || len(m.cfg.Symbols) == 0 {
		return m, nil
	}
	iv := m.chartIv
	if iv == "" {
		iv = defaultChartInterval
	}
	if m.chartSym != m.symbol() || m.feed.Interval() != iv {
		return m.loadChart(m.symbol(), iv)
	}
	return m, nil
}

// toggleInlineChart flips the inline candle pane (G). Turning it on seeds the
// feed for the active symbol+interval if it isn't already loaded, and starts the
// re-sync loop; turning it off lets the loop die (unless the overlay is open).
// The pane shares the overlay's feed and settings, so opening 'g' afterwards
// reuses the same data.
func (m model) toggleInlineChart() (tea.Model, tea.Cmd) {
	if m.cfg.Candles == nil {
		m.setToast(i18n.T("candle chart unavailable"), true)
		return m, nil
	}
	if m.chartInline {
		m.chartInline = false
		m.setToast(i18n.T("inline chart off"), false)
		return m, nil // the re-sync loop stops via the chartActive() gate
	}
	m.chartInline = true
	if !m.inlineChartFits() {
		// Hidden on this terminal: stay quiet (no feed). A later resize reveals it
		// and the WindowSizeMsg handler seeds the feed then.
		m.setToast(i18n.T("inline chart on — shows when the terminal is taller"), false)
		return m, nil
	}
	m.setToast(i18n.T("inline chart on"), false)
	if m.chartIv == "" {
		m.chartIv = defaultChartInterval
	}
	sym := m.symbol()
	if m.chartSym != sym || m.feed.Interval() != m.chartIv || m.feed.Empty() {
		// Not loaded for this pair/interval, or empty (never loaded, or a prior
		// load failed) — do a real load so it retries now and clears any error.
		return m.loadChart(sym, m.chartIv)
	}
	// Already loaded (e.g. the overlay was used): just (re)start the re-sync loop.
	m.chartGen++
	return m, chartResyncCmd(m.chartGen, m.chartIv)
}

// openChart enters the chart overlay for the active symbol, seeding (or
// re-using) the feed for the current interval.
func (m model) openChart() (tea.Model, tea.Cmd) {
	m.mode = modeChart
	if m.chartIv == "" {
		m.chartIv = defaultChartInterval
	}
	sym := m.symbol()
	if m.chartSym != sym || m.feed.Interval() != m.chartIv || m.feed.Empty() {
		// Different pair/interval, or empty (never loaded, or a prior load failed)
		// — do a real load so reopening an errored chart retries now (and clears it).
		return m.loadChart(sym, m.chartIv)
	}
	// Already loaded for this symbol+interval: reopen instantly and restart the
	// re-sync loop under a fresh generation.
	m.chartGen++
	return m, chartResyncCmd(m.chartGen, m.chartIv)
}

// loadChart points the feed at sym+iv, clears the chart, and kicks off the seed
// fetch plus the re-sync loop under a new generation (so any in-flight fetch or
// tick for the previous symbol/interval is ignored).
func (m model) loadChart(sym, iv string) (tea.Model, tea.Cmd) {
	m.chartSym = sym
	m.chartIv = iv
	m.feed.Reset(iv)
	m.chartLoadingOlder = false
	m.chartAtOldest = false
	m.chartErr = "" // fresh attempt: a prior failure no longer applies
	m.chart.SetCandles(nil, true)
	m.chart.SetVolumePane(m.chartVol)
	m.chart.SetLastPriceLine(true)
	m.chart.SetTimeAxis(true)
	m.chartGen++
	cmds := []tea.Cmd{
		m.fetchCandlesCmd(m.chartGen, sym, iv),
		chartResyncCmd(m.chartGen, iv),
	}
	if m.mode == modeChart {
		// Animate the title's loading indicator while the seed (re)loads — the
		// overlay frame stays put (placeholder fills it), only the spinner moves.
		cmds = append(cmds, m.chartSpin.Tick)
	}
	return m, tea.Batch(cmds...)
}

// cycleInterval steps the interval ladder by dir (clamped) and reloads.
func (m model) cycleInterval(dir int) (tea.Model, tea.Cmd) {
	i := indexOfInterval(m.chartIv) + dir
	if i < 0 || i >= len(chartIntervals) {
		return m, nil
	}
	return m.loadChart(m.chartSym, chartIntervals[i])
}

// applyCandles installs a fetch result, dropping it if the chart has since moved
// on. The seed is authoritative: it overwrites any folded approximation, and
// asOfTradeID is the newest trade already known so the snapshot's trades are not
// re-folded.
func (m model) applyCandles(msg candlesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.chartGen || msg.sym != m.chartSym || msg.iv != m.chartIv {
		return m, nil // stale: the chart moved on (closed, or symbol/interval changed)
	}
	if msg.older {
		m.chartLoadingOlder = false
		if msg.err != nil {
			if m.mode == modeChart {
				m.setToast(i18n.T("candle chart: loading older candles failed: %s", msg.err.Error()), true)
			}
			return m, nil // leave chartAtOldest unset so a later scroll retries
		}
		if m.feed.Backfill(msg.bars) == 0 {
			m.chartAtOldest = true // the page held nothing new — start of history
		}
		// Right-anchored scroll keeps the visible window put while the prepended
		// history fills in on the left; the selection follows its candle by time.
		// The trim caps a page that overshot maxFeedCandles at the oldest end (the
		// far end not yet in view), so scroll-back stops exactly at the window.
		m.setChartFromFeed()
		return m, nil
	}
	if msg.err != nil {
		if m.feed.Empty() {
			// Nothing loaded yet — surface why, persistently, instead of an endless
			// "loading…". A later re-sync that succeeds clears it.
			m.chartErr = errorText(msg.err)
		} else if m.mode == modeChart {
			// The chart already has data; keep showing it and note the failed refresh.
			m.setToast(i18n.T("candle chart: refresh failed: %s", errorText(msg.err)), true)
		}
		return m, nil
	}
	m.chartErr = "" // a successful seed clears any prior load error
	m.feed.Seed(msg.bars, newestTradeID(m.store, msg.sym))
	m.setChartFromFeed()
	return m, nil
}

// setChartFromFeed trims the retained series to maxFeedCandles and installs it
// on the chart. Trimming on every feed change keeps memory bounded at all times;
// paired with maybeBackfill's cap guard the series is a hard window of the newest
// maxFeedCandles buckets — bounded no matter how long the session runs or how far
// back the user scrolls. At the window boundary the oldest bucket slides off as a
// newer one rolls in.
func (m *model) setChartFromFeed() {
	m.feed.TrimOldest(maxFeedCandles)
	m.chart.SetCandles(chartCandles(&m.feed), true)
}

// maybeBackfill fetches one older page when the viewport has scrolled near the
// oldest loaded candle, unless a page is already in flight or the start of
// history has been reached. The result arrives as a candlesLoadedMsg{older:true}
// and the spinner animates until it lands. Calling it after every scroll/select
// key keeps history loading just ahead of the user.
func (m *model) maybeBackfill() tea.Cmd {
	if m.mode != modeChart || m.chartLoadingOlder || m.chartAtOldest {
		return nil
	}
	if m.feed.Empty() || !m.chart.NeedsBackfill() {
		return nil
	}
	if m.feed.Len() >= maxFeedCandles {
		return nil // scroll-back limit: the retained window is full, stop paging older
	}
	m.chartLoadingOlder = true
	// end just before the oldest loaded bucket, so the page is strictly older;
	// merge dedups any boundary overlap anyway.
	end := m.feed.OldestTime() - 1
	return tea.Batch(
		m.fetchOlderCandlesCmd(m.chartGen, m.chartSym, m.chartIv, end),
		m.chartSpin.Tick,
	)
}

// foldChartTrades folds the active symbol's newly-arrived trades into the
// chart's live bucket while the chart is open. It asks the store only for trades
// past the series' fold high-water id (oldest first, the order the id high-water
// wants), so a batch that brought nothing new for this symbol allocates and
// folds nothing. It re-installs the candles only when a trade actually changed
// the series — the common no-op batch touches neither the feed nor the chart —
// and returns a fetch command when a trade rolled the bucket over, so the
// just-closed bucket is replaced with authoritative data.
func (m *model) foldChartTrades() tea.Cmd {
	if !m.chartActive() || m.chartSym == "" || m.feed.Empty() {
		return nil
	}
	trades := m.store.TradesSince(m.chartSym, m.feed.LastTradeID())
	changed, rolled := false, false
	for _, t := range trades { // oldest→newest
		finalized, updated := m.feed.FoldTrade(t.TradeID, t.Price, t.Qty, t.Timestamp)
		if updated {
			changed = true
		}
		if len(finalized) > 0 {
			rolled = true
		}
	}
	if changed {
		m.setChartFromFeed() // bound the retained series as buckets roll over
	}
	if rolled {
		return m.fetchCandlesCmd(m.chartGen, m.chartSym, m.chartIv)
	}
	return nil
}

// handleChartKey routes keys while the chart overlay owns the keyboard.
func (m model) handleChartKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal // the re-sync loop sees the mode change and stops
		return m, nil
	case "[":
		return m.cycleInterval(-1)
	case "]":
		return m.cycleInterval(1)
	case "v":
		m.chartVol = !m.chartVol
		m.chart.SetVolumePane(m.chartVol)
		return m, nil
	case "i":
		m.chartInd = (m.chartInd + 1) % len(chartIndicatorOptions)
		m.chart.SetIndicators(indicatorsFor(m.chartInd))
		m.setToast(i18n.T("indicator: %s", chartIndicatorOptions[m.chartInd].label), false)
		return m, nil
	case "C":
		m.toggleColorScheme() // recolors the chart too (renderChart derives styles from the palette)
		return m, nil
	default:
		m.chart, _ = m.chart.Update(msg) // scroll / zoom / jump-to-live
		return m, m.maybeBackfill()      // scrolling back may need older history
	}
}

// fetchCandlesCmd fetches a seed/re-sync for sym+iv via the configured seam
// (endMs 0 = up to the current bucket).
func (m model) fetchCandlesCmd(gen int, sym, iv string) tea.Cmd {
	fetch := m.cfg.Candles
	return func() tea.Msg {
		bars, err := fetch(sym, iv, chartHistoryLimit, 0)
		return candlesLoadedMsg{gen: gen, sym: sym, iv: iv, bars: bars, err: err}
	}
}

// fetchOlderCandlesCmd fetches one history page ending just before endMs, for
// scroll-back backfill. The result carries older:true so applyCandles merges it
// at the front rather than re-seeding the live edge.
func (m model) fetchOlderCandlesCmd(gen int, sym, iv string, endMs int64) tea.Cmd {
	fetch := m.cfg.Candles
	return func() tea.Msg {
		bars, err := fetch(sym, iv, chartBackfillLimit, endMs)
		return candlesLoadedMsg{gen: gen, sym: sym, iv: iv, bars: bars, older: true, err: err}
	}
}

// chartResyncCmd schedules the next re-sync tick for a generation.
func chartResyncCmd(gen int, iv string) tea.Cmd {
	return tea.Tick(chartResyncEvery(iv), func(time.Time) tea.Msg {
		return chartResyncMsg{gen: gen}
	})
}

// chartResyncEvery is the authoritative re-fetch cadence for an interval:
// often enough to heal folded-volume drift (the TUI folds from the state
// store's bounded trade ring, so folded volume is provisional between
// re-syncs), but bounded so a single active chart never hammers REST. It
// scales with the interval and is clamped to [5s, 30s].
func chartResyncEvery(interval string) time.Duration {
	ms := candles.IntervalMs(interval)
	if ms <= 0 {
		return 30 * time.Second
	}
	d := time.Duration(ms/12) * time.Millisecond
	if d < 5*time.Second {
		return 5 * time.Second
	}
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// chartCandles converts the feed's decimal-string candles to the float candles
// the candlechart renders. This and the chart are the one place decimal price
// strings become floats — a chart is a visual artifact; money stays strings
// everywhere else. Zero-volume buckets are OMITTED here, by the data provider:
// no trades means no traded price, and the slot-based chart would otherwise
// draw the carried-forward filler price as a flat candle (candlechart renders
// values faithfully — it cannot tell a filler from a genuine four-price doji,
// so keeping fillers off the chart is the provider's job). Omission also
// compresses the time axis across the empty span, which is what a chart of a
// thin market should do.
//
// It reads the series in place via Len/At rather than a Candles copy: on a
// long-lived chart the series grows to thousands of buckets and this runs on
// every feed change, so the intermediate whole-series allocation is the copy
// worth not making.
func chartCandles(s *candles.Series) []candlechart.Candle {
	n := s.Len()
	out := make([]candlechart.Candle, 0, n)
	for i := 0; i < n; i++ {
		c := s.At(i)
		if c.VolumeF == 0 {
			continue
		}
		out = append(out, candlechart.Candle{
			Time: c.Time, Open: c.OpenF, High: c.HighF, Low: c.LowF, Close: c.CloseF, Volume: c.VolumeF,
		})
	}
	return out
}

// newestTradeID is the highest trade id known for sym (0 if none).
func newestTradeID(store *state.Store, sym string) int64 {
	ts := store.Trades(sym) // newest first
	if len(ts) == 0 {
		return 0
	}
	return ts[0].TradeID
}

// indexOfInterval returns the ladder index of iv (0 if unknown).
func indexOfInterval(iv string) int {
	for i, v := range chartIntervals {
		if v == iv {
			return i
		}
	}
	return 0
}

// chartDims is the candle chart's size inside its overlay: the terminal minus
// the header and footer (2 rows each), the overlay border (2 rows), the title,
// readout, and help rows (3), and a small horizontal margin. The stored chart is
// sized to this on resize so its scroll math matches what renderChart shows.
func (m model) chartDims() (w, h int) {
	w, h = m.w-6, m.h-9
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// chartReadout is the one-line detail of the selected candle (or a hint when
// nothing is selected): time and OHLCV. The chart's float values are display
// copies — money stays decimal strings everywhere else.
func (m model) chartReadout() string {
	c, ok := m.chart.Selected()
	if !ok {
		return uikit.StyDim.Render("←/→ select a candle to see its OHLC")
	}
	when := time.UnixMilli(c.Time).Format("2006-01-02 15:04")
	return fmt.Sprintf("%s  O %s  H %s  L %s  C %s  V %s",
		uikit.StyTitle.Render(when),
		fmtChartNum(c.Open), fmtChartNum(c.High), fmtChartNum(c.Low),
		fmtChartNum(c.Close), fmtChartNum(c.Volume))
}

// fmtChartNum renders a chart float for the readout, thousands-grouped.
func fmtChartNum(v float64) string {
	return uikit.GroupThousands(strconv.FormatFloat(v, 'f', -1, 64))
}

// chartStyles builds the candle chart's styling from the TUI's resolved color
// palette, so the chart's rising/falling candles (and volume bars) use the exact
// same color-scheme- and profile-aware colors as the orderbook's up/down sides — one
// source of truth for direction color. The neutral elements (axis, last-price
// line, overlays) keep the component's defaults. renderChart applies this every
// frame, so a color-scheme toggle recolors the chart with no stored state.
func chartStyles(pal uikit.Palette) candlechart.Styles {
	s := candlechart.GreenRedStyles()
	s.Up = pal.Up.Fg
	s.Down = pal.Down.Fg
	return s
}

// chartTitle is the candle chart's base title — symbol, interval, and the active
// indicator label (when one is on) — shared by the full overlay and the inline
// pane so both name the indicator. Callers append their own status (loading,
// errors) after it.
func (m model) chartTitle() string {
	t := i18n.T("candles · %s · %s", uikit.FmtSymbol(m.chartSym), intervalLabel(m.chartIv))
	if m.chartInd > 0 {
		t += " · " + chartIndicatorOptions[m.chartInd].label
	}
	return t
}

// intervalLabel is the human label for a candles interval.
func intervalLabel(iv string) string {
	switch iv {
	case "1":
		return "1m"
	case "5":
		return "5m"
	case "15":
		return "15m"
	case "30":
		return "30m"
	case "60":
		return "1h"
	case "240":
		return "4h"
	default:
		return iv // 1D, 1W
	}
}
