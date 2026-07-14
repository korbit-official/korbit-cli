// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package setupui is the interactive terminal front-end for the `setup` command.
// On a TTY, instead of printing a registration link and exiting, setup stays
// live: it shows the link (width-wrapped and clickable, with ^Y to copy it and ^R
// to show it as a scannable QR code), prompts for the API key id the developers
// portal issues, and — once the user pastes it — binds the key and runs the health
// check in the same session.
//
// This package is purely the UI. It knows nothing about keys, signing, or the
// Korbit API: the caller passes the guidance lines and registration URL to display
// and a Submit callback that performs the bind + health check and returns the lines.
// That keeps the whole interactive layer isolated behind two function types, so
// the non-interactive setup path (and the MCP server, which drives setup with a
// buffer-backed IO) never touches Bubble Tea.
package setupui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/mdp/qrterminal/v3"
)

// ErrInterrupted is returned by Run when the user aborts with Ctrl-C. The caller
// treats it as a deliberate cancel — emit nothing (mirroring the non-interactive
// SIGINT abort, which prints no result) and exit 0. Any key already bound stays;
// re-running setup is idempotent.
var ErrInterrupted = errors.New("setup canceled")

// Config drives one interactive setup session.
type Config struct {
	// In/Out are the terminal streams the program runs on. setup renders the UI
	// on stderr (Out) and keeps stdout for the final result document, so the
	// machine-readable contract holds even in interactive mode.
	In  io.Reader
	Out io.Writer
	// Intro is the static guidance shown above the prompt — the lead-in prose, the
	// public key fallback, and what to paste. Rendered one entry per line, each
	// hard-wrapped to the terminal width so a long line is never clipped.
	Intro []string
	// RegistrationURL is the developers-portal deep link the user opens to register
	// the generated key. When non-empty it is rendered as a dedicated block right
	// below Intro: width-wrapped so the (space-less, often 200+ char) URL is never
	// truncated and stays copyable, and wrapped in an OSC 8 hyperlink so terminals
	// that support it make it clickable regardless of how it wraps. It also enables
	// the ^Y "copy link" and ^R "show QR" shortcuts. Empty when no link could be
	// built (Intro then carries the manual public-key fallback and no block, copy,
	// or QR is offered).
	RegistrationURL string
	// Prompt labels the input field (e.g. "Paste the issued API key id").
	Prompt string
	// Submit binds the entered token (the issued API key id) and runs the health
	// check. It performs network I/O, so the model runs it off the UI loop behind
	// a spinner. It returns the lines to display, done=true when setup is
	// complete (the session then ends), or a non-nil error to show as a retryable
	// message while keeping the prompt open.
	Submit func(token string) (result []string, done bool, err error)

	// AutoClaim, when non-nil, runs concurrently with the paste prompt: a
	// background poll that detects the registered key, binds it, and runs the
	// health check without the user pasting anything. The model starts it once
	// (passing a context canceled when the session ends, so the poll stops on
	// paste/Esc/Ctrl-C), renders each ClaimUpdate.Status as a live line beneath
	// the prompt, and on a terminal update either ends the session showing Result
	// (Done — the key was found, bound, and checked) or stops the poll and keeps
	// the prompt open for manual paste (Stop — a conflict or hard error; Status
	// says why). nil disables auto-claim (a pipe, no TTY, the MCP server).
	AutoClaim func(ctx context.Context) <-chan ClaimUpdate

	// programOpts are appended to the Bubble Tea options (test seam).
	programOpts []tea.ProgramOption
}

// ClaimUpdate is one message from the background auto-claim poll. A non-terminal
// update carries only Status (the live line to show). A terminal update sets
// exactly one of Done (success: Result holds the lines to display before the
// session ends) or Stop (polling stopped without a claim; the prompt stays open
// and Status explains why).
type ClaimUpdate struct {
	Status  string
	Result  []string
	Done    bool
	Stop    bool
	Prefill string
}

// Run starts the interactive session and blocks until it exits. A deliberate
// quit (the user pressed Esc/Ctrl-C without finishing) returns nil — whether
// setup was completed is observed by the caller through its Submit closure, not
// through this return value. Only a Bubble Tea runtime failure is returned as an
// error.
func Run(cfg Config) error {
	// The poll's lifetime is the dialog's lifetime: cancel on any exit (paste
	// success, Esc, Ctrl-C, or a Bubble Tea fault) so the background goroutine
	// stops promptly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newModel(cfg)
	if cfg.AutoClaim != nil {
		m.claimCh = cfg.AutoClaim(ctx)
		m.claiming = m.claimCh != nil
	}
	opts := append([]tea.ProgramOption{tea.WithOutput(cfg.Out), tea.WithInput(cfg.In)}, cfg.programOpts...)
	fm, err := tea.NewProgram(m, opts...).Run()
	if err != nil {
		return err
	}
	if m, ok := fm.(model); ok && m.interrupted {
		return ErrInterrupted
	}
	return nil
}

type phase int

const (
	phaseInput phase = iota // waiting for the user to type the key id
	phaseBusy               // Submit is running (binding + health check)
	phaseDone               // setup completed; final view shown, quitting
)

type model struct {
	cfg   Config
	input textinput.Model
	spin  spinner.Model
	phase phase

	errText     string   // retryable validation/bind message, shown under the prompt
	result      []string // lines returned by Submit (health check etc.)
	interrupted bool     // the user pressed Ctrl-C — abort, emit nothing

	claimCh  <-chan ClaimUpdate // background auto-claim updates; nil when disabled
	claiming bool               // the auto-claim poll is live (drives the status line + spinner)
	status   string             // latest auto-claim status line, shown beneath the prompt

	width, height int  // terminal size (from WindowSizeMsg); 0 until the first arrives
	showQR        bool // the ^R QR overlay is open (renders full-viewport instead of the prompt)
	copied        bool // transient "link copied" confirmation, cleared on the next keypress

	// qr is the QR code of RegistrationURL, rendered once at construction (it depends
	// only on the URL, which is fixed for the session) and reused by both the inline
	// and overlay paths — so the encoder never runs on the per-frame render path. Empty
	// when there is no link; qrW/qrH are its measured display size.
	qr       string
	qrW, qrH int
}

// fallbackWidth/fallbackHeight are used until the first WindowSizeMsg arrives, so
// wrapping and the QR fit-check have sane defaults on the first frame.
const (
	fallbackWidth  = 80
	fallbackHeight = 24
)

func (m model) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return fallbackWidth
}

func (m model) viewHeight() int {
	if m.height > 0 {
		return m.height
	}
	return fallbackHeight
}

// submitResultMsg carries the outcome of a Submit call back onto the UI loop.
type submitResultMsg struct {
	lines []string
	done  bool
	err   error
}

// claimUpdateMsg delivers one ClaimUpdate from the auto-claim channel onto the UI
// loop; claimClosedMsg signals the channel closed without a terminal update.
type (
	claimUpdateMsg ClaimUpdate
	claimClosedMsg struct{}
)

func newModel(cfg Config) model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "KEY_ID"
	ti.CharLimit = 128
	ti.SetWidth(48)
	// Focus here, on the addressable local, so the stored input accepts keys from
	// the first frame. Focusing in Init instead would mutate a throwaway copy (Init
	// has a value receiver and returns only a Cmd), leaving the input blurred — and
	// a blurred textinput drops every keystroke.
	ti.Focus()
	m := model{
		cfg:   cfg,
		input: ti,
		spin:  spinner.New(spinner.WithSpinner(spinner.Dot)),
		phase: phaseInput,
	}
	// Render the QR once here, not per frame: the URL is fixed for the session, so
	// re-encoding it on every keystroke/blink/tick would be wasted work on the hot
	// render path.
	if cfg.RegistrationURL != "" {
		lines := strings.Split(strings.TrimRight(renderQR(cfg.RegistrationURL), "\n"), "\n")
		m.qr = strings.Join(lines, "\n")
		m.qrW, m.qrH = blockSize(lines)
	}
	return m
}

// Init kicks off the cursor blink for the already-focused input and, when an
// auto-claim poll is wired, starts draining it and spinning its status line.
func (m model) Init() tea.Cmd {
	if m.claiming {
		return tea.Batch(textinput.Blink, m.waitClaim(), m.spin.Tick)
	}
	return textinput.Blink
}

// waitClaim blocks on the next auto-claim update off the UI loop and delivers it
// as a message (claimClosedMsg once the channel closes).
func (m model) waitClaim() tea.Cmd {
	ch := m.claimCh
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return claimClosedMsg{}
		}
		return claimUpdateMsg(u)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Track the terminal size so the URL block wraps to width (never clipped) and
		// the QR overlay knows whether it fits. Recomputed on every resize, so a QR
		// that doesn't fit a small window renders once it's enlarged and ^R is pressed.
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		key := msg.String()
		// Any keypress clears a stale "copied" confirmation (a ^Y below re-sets it).
		m.copied = false
		if key == "ctrl+c" {
			// Hard interrupt, accepted at any time (including mid-bind and while the QR
			// overlay is open). Run reports it as ErrInterrupted and the caller emits
			// nothing, the same as the non-interactive SIGINT abort.
			m.interrupted = true
			return m, tea.Quit
		}
		// While the QR overlay is open it owns the keyboard: Enter, Esc, or ^R close it
		// back to the prompt (Esc/Enter here mean "close overlay", NOT the prompt's
		// "finish later"/"submit"), ^Y copies the link without closing, and every other
		// key is ignored so a stray press while lining up the phone camera doesn't
		// dismiss it. The background poll keeps draining through the claimUpdateMsg
		// cases regardless, so a claim still completes (or falls back) while the QR is up.
		if m.showQR {
			switch key {
			case "enter", "esc", "ctrl+r":
				m.showQR = false
			case "ctrl+y":
				m.copied = true
				return m, tea.SetClipboard(m.cfg.RegistrationURL)
			}
			return m, nil
		}
		switch key {
		case "esc":
			// "Finish later" — only before a key id is submitted: the generated key
			// stays in the keystore and the caller emits the resume guidance. Ignored
			// once Submit is in flight, because the key may already be bound and the
			// resume document would then be wrong; Ctrl-C is the way out at that point.
			if m.phase == phaseInput {
				return m, tea.Quit
			}
			return m, nil
		case "ctrl+y":
			// Copy the registration link to the system clipboard via OSC 52 (works over
			// SSH, needs no external binary). No-op when there is no link to copy.
			if m.cfg.RegistrationURL != "" {
				m.copied = true
				return m, tea.SetClipboard(m.cfg.RegistrationURL)
			}
			return m, nil
		case "ctrl+r":
			// Open the scannable-QR overlay — only while waiting for the id and only
			// when there is a link to encode.
			if m.cfg.RegistrationURL != "" && m.phase == phaseInput {
				m.showQR = true
			}
			return m, nil
		}
		if m.phase != phaseInput {
			return m, nil // ignore typing while busy or done
		}
		if key == "enter" {
			token := strings.TrimSpace(m.input.Value())
			if token == "" {
				m.errText = i18n.T("Enter the issued key id, or press Esc to finish later.")
				return m, nil
			}
			m.phase = phaseBusy
			m.errText = ""
			m.input.Blur()
			return m, tea.Batch(m.runSubmit(token), m.spin.Tick)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.PasteMsg:
		// Bracketed paste (e.g. Cmd/Ctrl-V) arrives as its own message, not key
		// presses — forward it to the input so pasting the key id works.
		if m.phase != phaseInput {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		// Spin while binding (phaseBusy) or while the auto-claim poll is live; stop
		// once neither is active.
		if m.phase != phaseBusy && !m.claiming {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case claimUpdateMsg:
		switch {
		case msg.Done:
			// The poll found, bound, and checked the key — end the session showing
			// its result, exactly like a successful paste. Drop any open QR overlay so
			// the final result renders (this is the scan-on-phone happy path: the
			// overlay auto-closes the moment the key is claimed).
			m.showQR = false
			m.result = msg.Result
			m.phase = phaseDone
			return m, tea.Quit
		case msg.Stop:
			// Conflict or hard error: polling stopped, but manual paste stays open.
			// Close the QR overlay too so the fallback notice and any pre-filled id are
			// visible at the prompt.
			m.showQR = false
			m.claiming = false
			if msg.Status != "" {
				m.status = msg.Status
			}
			// If the id was already retrieved before the fall-back, drop it into an
			// empty input so the user needn't re-find it — just review and confirm.
			if msg.Prefill != "" && strings.TrimSpace(m.input.Value()) == "" {
				m.input.SetValue(msg.Prefill)
				m.input.CursorEnd()
			}
			return m, nil // stop draining
		default:
			if msg.Status != "" {
				m.status = msg.Status
			}
			return m, m.waitClaim() // keep draining
		}

	case claimClosedMsg:
		m.claiming = false
		return m, nil

	case submitResultMsg:
		if msg.err != nil {
			m.phase = phaseInput
			m.errText = msg.err.Error()
			return m, m.input.Focus()
		}
		m.result = msg.lines
		if msg.done {
			m.phase = phaseDone
			return m, tea.Quit
		}
		// Not done and no error: show the lines and let the user try again.
		m.phase = phaseInput
		return m, m.input.Focus()
	}
	return m, nil
}

func (m model) runSubmit(token string) tea.Cmd {
	return func() tea.Msg {
		lines, done, err := m.cfg.Submit(token)
		return submitResultMsg{lines: lines, done: done, err: err}
	}
}

var (
	promptStyle = lipgloss.NewStyle().Bold(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m model) View() tea.View {
	// The QR overlay takes over the whole viewport (see qrView); ^R/Esc close it.
	if m.showQR {
		return tea.NewView(m.qrView())
	}

	w := m.viewWidth()
	var b strings.Builder

	// Header: the intro guidance, then (while awaiting the id) the registration link
	// as its own width-wrapped + hyperlinked block right below the intro lead-in.
	// Rendering the link here — rather than as a plain line in Intro — keeps the long,
	// space-less URL wrapped to the viewport instead of clipped by the cell renderer.
	var header strings.Builder
	for _, line := range m.cfg.Intro {
		header.WriteString(wrapProse(line, w))
		header.WriteByte('\n')
	}
	awaiting := m.cfg.RegistrationURL != "" && m.phase == phaseInput
	if awaiting {
		header.WriteString(renderLink(m.cfg.RegistrationURL, w))
		header.WriteByte('\n')
	}
	b.WriteString(header.String())

	// On a terminal roomy enough for the whole layout, show the QR inline so it can be
	// scanned right away without pressing ^R; otherwise the ^R overlay (and its footer
	// hint) remains the way to see it. Recomputed each frame, so a resize flips it.
	inlineQR := false
	if awaiting {
		// Count what renders around the QR: the header above it, plus the variable
		// Submit-retry result lines that render below it in the phaseInput branch (a
		// non-terminal Submit leaves those set while awaiting stays true). Without the
		// result term a borderline-height terminal would green-light the QR and then
		// scroll it off the top once the retry lines pushed past the viewport.
		contentLines := countLines(header.String()) + linesRendered(m.result)
		if qr, qrW, qrH := m.qrCode(); m.inlineQRFits(contentLines, qrW, qrH) {
			inlineQR = true
			b.WriteByte('\n')
			b.WriteString(hintStyle.Render(i18n.T("Or scan this QR code with your phone:")))
			b.WriteByte('\n')
			b.WriteString(qr)
			b.WriteByte('\n')
		}
	}

	switch m.phase {
	case phaseBusy:
		b.WriteByte('\n')
		b.WriteString(m.spin.View())
		b.WriteString(" " + i18n.T("Binding the key and running a health check…") + "\n")
	case phaseDone:
		for _, line := range m.result {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	default: // phaseInput
		// Lines from a non-terminal Submit attempt, if any.
		for _, line := range m.result {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		if m.cfg.Prompt != "" {
			b.WriteString(promptStyle.Render(m.cfg.Prompt))
			b.WriteByte('\n')
		}
		b.WriteString(m.input.View())
		b.WriteByte('\n')
		if m.errText != "" {
			b.WriteString(errStyle.Render(m.errText))
			b.WriteByte('\n')
		}
		if m.copied {
			b.WriteString(hintStyle.Render(i18n.T("✓ Link copied to clipboard")))
			b.WriteByte('\n')
		}
		// Auto-claim status: a spinning live line while the poll runs, or a sticky
		// notice once it has stopped (conflict/hard error). Empty when no poll.
		if m.claiming {
			status := m.status
			if status == "" {
				status = i18n.T("Watching for your registered key — it'll bind automatically when you finish.")
			}
			b.WriteString(hintStyle.Render(m.spin.View() + " " + status))
			b.WriteByte('\n')
		} else if m.status != "" {
			b.WriteString(hintStyle.Render(m.status))
			b.WriteByte('\n')
		}
		b.WriteString(hintStyle.Render(m.footerHints(inlineQR)))
		b.WriteByte('\n')
	}

	return tea.NewView(b.String())
}

// footerHints is the one-line key legend under the prompt. The ^Y/^R shortcuts are
// advertised only when there is a link to copy or encode, and ^R ("show QR") is
// dropped once the QR is already shown inline.
func (m model) footerHints(inlineQR bool) string {
	if m.cfg.RegistrationURL == "" {
		return i18n.T("Enter to confirm · Esc to finish later")
	}
	if inlineQR {
		return i18n.T("Enter confirm · ^Y copy link · Esc finish later")
	}
	return i18n.T("Enter confirm · ^Y copy link · ^R show QR · Esc finish later")
}

// qrView renders the full-viewport QR overlay: a scannable QR of the registration
// link, or — when the terminal is too small to show it — a message plus the link as
// a fallback. The fit-check uses the live terminal size, so enlarging the window
// and pressing ^R again renders the code.
func (m model) qrView() string {
	w, h := m.viewWidth(), m.viewHeight()
	var b strings.Builder
	b.WriteString(promptStyle.Render(i18n.T("Scan with your phone to open the pre-filled registration form")))
	b.WriteString("\n\n")

	qr, qrW, qrH := m.qrCode()
	// Reserve rows for the caption above and the hint/footer below the code.
	const chrome = 6
	if qrW > w || qrH+chrome > h {
		fmt.Fprintf(&b, "%s\n\n", errStyle.Render(i18n.T(
			"Your terminal is too small to show the QR code (it needs about %d×%d; this window is %d×%d).", qrW, qrH+chrome, w, h)))
		b.WriteString(hintStyle.Render(i18n.T("Enlarge the window and press ^R again, or use the link:")))
		b.WriteByte('\n')
		b.WriteString(renderLink(m.cfg.RegistrationURL, w))
		b.WriteByte('\n')
	} else {
		b.WriteString(qr)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if m.copied {
		b.WriteString(hintStyle.Render(i18n.T("✓ Link copied to clipboard")))
		b.WriteByte('\n')
	}
	b.WriteString(hintStyle.Render(i18n.T("^Y copy link · Enter/Esc/^R to close")))
	b.WriteByte('\n')
	return b.String()
}

// qrCode returns the QR code rendered once at construction and its measured display
// size — width in terminal cells, height in rows. Shared by the inline QR and the ^R
// overlay so both measure identically without re-encoding.
func (m model) qrCode() (qr string, w, h int) {
	return m.qr, m.qrW, m.qrH
}

// inlineQRFits reports whether the QR fits inline in the prompt view. contentLines is
// the caller's count of the variable rows around the QR (the intro+link header above
// it and any Submit-retry result lines below it); inputChrome covers the fixed rest.
func (m model) inlineQRFits(contentLines, qrW, qrH int) bool {
	// The QR's own caption + blank (2), the base prompt region (blank + prompt + input
	// + footer = 4), and headroom for the optional copied/status/error lines (3).
	const inputChrome = 9
	return qrW <= m.viewWidth() && contentLines+qrH+inputChrome <= m.viewHeight()
}

// countLines returns the number of terminal rows a rendered block occupies (each
// line is newline-terminated, so this counts the trailing newlines).
func countLines(s string) int { return strings.Count(s, "\n") }

// linesRendered counts the terminal rows a slice of view lines occupies, allowing for
// entries that themselves contain embedded newlines.
func linesRendered(ss []string) int {
	n := 0
	for _, s := range ss {
		n += strings.Count(s, "\n") + 1
	}
	return n
}

// wrapProse word-wraps a guidance line to width so a long sentence is not clipped by
// the cell renderer. Any hard newlines already in the line are preserved.
func wrapProse(s string, width int) string {
	if width < 1 {
		width = fallbackWidth
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		out = append(out, ansi.Wrap(ln, width, " -"))
	}
	return strings.Join(out, "\n")
}

// renderLink renders the registration URL as a width-wrapped, OSC 8 hyperlinked
// block. Character-level wrapping (the URL has no spaces) guarantees the whole link
// is visible and copyable instead of clipped at the viewport edge; the hyperlink is
// re-emitted per visual line so the link stays one clickable target even on
// terminals that reset link state at a newline.
func renderLink(url string, width int) string {
	if width < 8 {
		width = 8
	}
	linkStyle := lipgloss.NewStyle().Hyperlink(url)
	var lines []string
	for _, ln := range strings.Split(ansi.Hardwrap(url, width, false), "\n") {
		lines = append(lines, linkStyle.Render(ln))
	}
	return strings.Join(lines, "\n")
}

// renderQR encodes s as a half-block QR code string. Half-blocks pack two vertical
// modules per character row, the densest portable rendering; the glyphs carry no
// ANSI color, and QR readers auto-detect polarity, so the same output scans on both
// light and dark terminals.
func renderQR(s string) string {
	var b strings.Builder
	qrterminal.GenerateWithConfig(s, qrterminal.Config{
		Level:      qrterminal.L,
		Writer:     &b,
		HalfBlocks: true,
		QuietZone:  3,
	})
	return b.String()
}

// blockSize returns the display width (widest line in terminal cells, not bytes) and
// the line count of a rendered multi-line block.
func blockSize(lines []string) (w, h int) {
	for _, ln := range lines {
		if cw := ansi.StringWidth(ln); cw > w {
			w = cw
		}
	}
	return w, len(lines)
}
