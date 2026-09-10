// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Command demo shows the candlechart component in a real terminal with a
// simulated live feed (no API, synthetic data). It is a dev aid, not part of
// the CLI.
//
//	go run ./internal/tui/candlechart/demo/          # interactive
//	go run ./internal/tui/candlechart/demo/ frames   # dump static frames (no TTY)
//
// Interactive keys: ←/→ or h/l select a candle · pgup/pgdn or wheel scroll ·
// +/- zoom · end/g jump to live · v volume pane · p last-price line ·
// c toggle palette · q quit.
package main

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/digitalx-official/digitalx-cli/internal/tui/candlechart"
)

const intervalMs = 3_600_000 // synthetic 1h candles

// synth builds n deterministic OHLC candles resembling a noisy trend.
func synth(n int, base int64) []candlechart.Candle {
	cs := make([]candlechart.Candle, n)
	price := float64(base)
	for i := 0; i < n; i++ {
		drift := math.Sin(float64(i)/9.0)*180 + math.Sin(float64(i)/3.3)*70
		open := price
		closeP := price + drift + float64((i*37)%51) - 25
		hi := math.Max(open, closeP) + float64((i*53)%40)
		lo := math.Min(open, closeP) - float64((i*29)%40)
		cs[i] = candlechart.Candle{
			Time:   intervalMs * int64(i),
			Open:   open,
			High:   hi,
			Low:    lo,
			Close:  closeP,
			Volume: float64((i*7)%20 + 1),
		}
		price = closeP
	}
	return cs
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "frames" {
		printFrames()
		return
	}
	if _, err := tea.NewProgram(newModel()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo error:", err)
		os.Exit(1)
	}
}

// ---- interactive model ----

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type model struct {
	chart candlechart.Model
	rng   *rand.Rand

	live      candlechart.Candle // the in-progress bar
	ticksLeft int                // ticks until this bar rolls over

	showVol  bool
	showLast bool
	redBlue  bool // red-up/blue-down palette

	w, h int
}

func newModel() model {
	candles := synth(220, 95_000_000)
	last := candles[len(candles)-1]
	live := candlechart.Candle{
		Time:  last.Time + intervalMs,
		Open:  last.Close,
		High:  last.Close,
		Low:   last.Close,
		Close: last.Close,
	}
	ch := candlechart.New(80, 24)
	ch.SetCandles(candles, true)
	ch.UpsertLive(live)
	m := model{
		chart:     ch,
		rng:       rand.New(rand.NewSource(1)),
		live:      live,
		ticksLeft: 8,
	}
	m.refreshOverlay()
	return m
}

// refreshOverlay recomputes a 20-period simple moving average and hands it to
// the chart as an overlay. The indicator math lives HERE, in the demo — the
// candlechart package draws overlay values but computes no indicators.
func (m *model) refreshOverlay() {
	cs := m.chart.Candles()
	sma := movingAverage(cs, 20)
	m.chart.SetOverlays([]candlechart.Overlay{{
		Name:   "SMA20",
		Color:  lipgloss.Color("4"),
		Values: sma,
	}})
}

// movingAverage returns the n-period SMA of closes, NaN until warmed.
func movingAverage(cs []candlechart.Candle, n int) []float64 {
	out := make([]float64, len(cs))
	var sum float64
	for i, c := range cs {
		sum += c.Close
		if i >= n {
			sum -= cs[i-n].Close
		}
		if i >= n-1 {
			out[i] = sum / float64(n)
		} else {
			out[i] = math.NaN()
		}
	}
	return out
}

func (m model) Init() tea.Cmd { return tick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.chart.SetSize(msg.Width, msg.Height-2) // header + help
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "v":
			m.showVol = !m.showVol
			m.chart.SetVolumePane(m.showVol)
			return m, nil
		case "p":
			m.showLast = !m.showLast
			m.chart.SetLastPriceLine(m.showLast)
			return m, nil
		case "c":
			m.redBlue = !m.redBlue
			if m.redBlue {
				m.chart.SetStyles(candlechart.RedBlueStyles())
			} else {
				m.chart.SetStyles(candlechart.GreenRedStyles())
			}
			return m, nil
		}
		m.chart, _ = m.chart.Update(msg) // scroll / zoom / jump-to-live
		return m, nil

	case tea.MouseWheelMsg:
		m.chart, _ = m.chart.Update(msg)
		return m, nil

	case tickMsg:
		m.stepLive()
		m.chart.UpsertLive(m.live)
		m.refreshOverlay() // recompute the SMA over the updated series (demo-side)
		return m, tick()
	}
	return m, nil
}

// stepLive nudges the in-progress candle by a small random walk and rolls over
// to a fresh bucket when its ticks run out — exactly what a real trade feed
// would drive via UpsertLive.
func (m *model) stepLive() {
	delta := (m.rng.Float64() - 0.48) * 600
	m.live.Close += delta
	m.live.High = math.Max(m.live.High, m.live.Close)
	m.live.Low = math.Min(m.live.Low, m.live.Close)
	m.live.Volume += m.rng.Float64() * 2

	m.ticksLeft--
	if m.ticksLeft <= 0 {
		m.live = candlechart.Candle{
			Time:  m.live.Time + intervalMs,
			Open:  m.live.Close,
			High:  m.live.Close,
			Low:   m.live.Close,
			Close: m.live.Close,
		}
		m.ticksLeft = 6 + m.rng.Intn(8)
	}
}

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m model) View() tea.View {
	header := headerStyle.Render(fmt.Sprintf(" candlechart demo — live BTC/KRW (synthetic) — %.0f", m.live.Close))
	help := helpStyle.Render(" ←/→ select · pgup/pgdn scroll · +/- zoom · end live · v volume · p last-price · c colors · q quit")
	body := m.chart.View()
	v := tea.NewView(header + "\n" + body + "\n" + help)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// ---- static frame dump (no TTY) ----

func printFrames() {
	candles := synth(220, 95_000_000)

	c := candlechart.New(80, 24)
	c.SetCandles(candles, true)
	c.SetOverlays([]candlechart.Overlay{{Name: "SMA20", Color: lipgloss.Color("4"), Values: movingAverage(candles, 20)}})
	frame("80x24, default (bold single-column blocks) + SMA20 overlay", c)

	c.ZoomOut()
	frame("80x24, zoomed out one step (thin lines, packed)", c)

	c.ZoomIn()
	c.ZoomIn()
	c.ZoomIn()
	frame("80x24, zoomed all the way in (wide blocks)", c)

	c2 := candlechart.New(80, 24)
	c2.SetCandles(candles, true)
	c2.SetVolumePane(true)
	c2.SetLastPriceLine(true)
	c2.SetTimeAxis(true)
	frame("80x24, zoom=1 + volume pane + last-price line + time axis", c2)
}

func frame(title string, m candlechart.Model) {
	fmt.Printf("\n=== %s ===\n%s\n", title, m.View())
}
