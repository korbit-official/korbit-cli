// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package setupui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// isQuit reports whether cmd is the quit command (it runs cmd, which for tea.Quit
// just yields a QuitMsg with no side effects).
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func neverSubmit(t *testing.T) func(string) ([]string, bool, error) {
	return func(string) ([]string, bool, error) {
		t.Helper()
		t.Fatal("Submit must not be called")
		return nil, false, nil
	}
}

func enter() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func escape() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEscape} }
func ctrlC() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl} }
func ctrlR() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl} }
func ctrlY() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl} }

func winSize(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

// longURL is a stand-in registration deep link — an obviously-fake example.com URL,
// shaped like the real one (long, space-less) so it must wrap at a normal terminal
// width and needs a large QR.
const longURL = "https://example.com/open-api/keys/new?public_key=MCowBQYDK2VwAyEAqrstuvwx0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcd&permissions=7&label=trading-bot&whitelist=203.0.113.7,2001:db8:1:2::/64"

// withSize applies a WindowSizeMsg and returns the updated model.
func withSize(m model, w, h int) model {
	mm, _ := m.Update(winSize(w, h))
	return mm.(model)
}

// TestInputIsFocused: the key-id input must be focused from the first frame.
// Init returns only a Cmd (value receiver), so it cannot set focus — a blurred
// textinput silently drops every keystroke.
func TestInputIsFocused(t *testing.T) {
	if m := newModel(Config{}); !m.input.Focused() {
		t.Fatal("the key-id input must be focused so it accepts keystrokes")
	}
}

// TestEmptyEnterIsRejected: Enter with no input shows a validation message and
// neither submits nor quits.
func TestEmptyEnterIsRejected(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	mm, cmd := m.Update(enter())
	got := mm.(model)
	if isQuit(cmd) {
		t.Fatal("empty Enter must not quit")
	}
	if got.phase != phaseInput {
		t.Fatalf("phase = %d, want phaseInput", got.phase)
	}
	if got.errText == "" {
		t.Fatal("expected a validation message")
	}
}

// TestCtrlCInterruptsAnytime: Ctrl-C quits and marks the model interrupted in any
// phase, including mid-bind (busy) — the caller then emits nothing.
func TestCtrlCInterruptsAnytime(t *testing.T) {
	for _, p := range []phase{phaseInput, phaseBusy} {
		m := newModel(Config{Submit: neverSubmit(t)})
		m.phase = p
		mm, cmd := m.Update(ctrlC())
		got := mm.(model)
		if !isQuit(cmd) {
			t.Fatalf("ctrl+c (phase %d) must quit", p)
		}
		if !got.interrupted {
			t.Fatalf("ctrl+c (phase %d) must mark interrupted", p)
		}
	}
}

// TestEscFinishesLaterOnlyWhileInputting: Esc quits (without interrupting) while
// inputting, and is ignored while busy (a bind may be in flight, so the
// finish-later document would be wrong).
func TestEscFinishesLaterOnlyWhileInputting(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	mm, cmd := m.Update(escape())
	got := mm.(model)
	if !isQuit(cmd) {
		t.Fatal("esc while inputting must quit")
	}
	if got.interrupted {
		t.Fatal("esc is finish-later, not an interrupt")
	}

	busy := newModel(Config{Submit: neverSubmit(t)})
	busy.phase = phaseBusy
	_, cmd2 := busy.Update(escape())
	if isQuit(cmd2) {
		t.Fatal("esc while busy must be ignored")
	}
}

// TestSubmitDoneQuits: a done result ends the session showing its lines.
func TestSubmitDoneQuits(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.phase = phaseBusy
	mm, cmd := m.Update(submitResultMsg{lines: []string{"health check: ok"}, done: true})
	got := mm.(model)
	if got.phase != phaseDone {
		t.Fatalf("phase = %d, want phaseDone", got.phase)
	}
	if !isQuit(cmd) {
		t.Fatal("a done result must quit")
	}
	if len(got.result) != 1 || got.result[0] != "health check: ok" {
		t.Fatalf("result = %v, want the submitted lines", got.result)
	}
}

// TestSubmitErrorRetries: a retryable error returns to the prompt with the
// message shown and does not quit.
func TestSubmitErrorRetries(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.phase = phaseBusy
	mm, cmd := m.Update(submitResultMsg{err: errors.New("bad id")})
	got := mm.(model)
	if isQuit(cmd) {
		t.Fatal("a retryable error must not quit")
	}
	if got.phase != phaseInput {
		t.Fatalf("phase = %d, want phaseInput", got.phase)
	}
	if got.errText != "bad id" {
		t.Fatalf("errText = %q, want the error message", got.errText)
	}
}

// TestPasteInsertsIntoInput: a bracketed-paste message (Cmd/Ctrl-V) must reach
// the text input — it arrives as its own message, not key presses, so the model
// has to forward it explicitly.
func TestPasteInsertsIntoInput(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	mm, _ := m.Update(tea.PasteMsg{Content: "KEYID-PASTED"})
	if got := mm.(model); !strings.Contains(got.input.Value(), "KEYID-PASTED") {
		t.Fatalf("paste must insert into the input, got %q", got.input.Value())
	}
}

// TestPasteIgnoredWhileBusy: a paste is ignored once a bind is in flight (like
// typing), so it can't mutate the input mid-submit.
func TestPasteIgnoredWhileBusy(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.phase = phaseBusy
	mm, _ := m.Update(tea.PasteMsg{Content: "X"})
	if got := mm.(model); got.input.Value() != "" {
		t.Fatalf("paste while busy must be ignored, got %q", got.input.Value())
	}
}

// TestClaimDoneQuits: a Done auto-claim update ends the session showing its
// result, exactly like a successful paste.
func TestClaimDoneQuits(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.claiming = true
	mm, cmd := m.Update(claimUpdateMsg{Done: true, Result: []string{"bound automatically"}})
	got := mm.(model)
	if got.phase != phaseDone {
		t.Fatalf("phase = %d, want phaseDone", got.phase)
	}
	if !isQuit(cmd) {
		t.Fatal("a Done claim update must quit")
	}
	if len(got.result) != 1 || got.result[0] != "bound automatically" {
		t.Fatalf("result = %v, want the claim result lines", got.result)
	}
}

// TestClaimStopKeepsPrompt: a Stop update stops the spinner/poll and shows the
// notice, but keeps the prompt open (does not quit) so manual paste still works.
func TestClaimStopKeepsPrompt(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.claiming = true
	mm, cmd := m.Update(claimUpdateMsg{Stop: true, Status: "collision — paste it"})
	got := mm.(model)
	if isQuit(cmd) {
		t.Fatal("a Stop update must not quit (manual paste remains)")
	}
	if got.claiming {
		t.Fatal("a Stop update must stop the live poll")
	}
	if got.status != "collision — paste it" {
		t.Fatalf("status = %q, want the stop notice", got.status)
	}
	if cmd != nil {
		t.Fatal("a Stop update must not keep draining the channel")
	}
}

// TestClaimStopPrefillsEmptyInput: a Stop carrying a Prefill id drops it into an
// empty input, so a fall-back after the id was retrieved doesn't make the user
// re-type it.
func TestClaimStopPrefillsEmptyInput(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.claiming = true
	mm, _ := m.Update(claimUpdateMsg{Stop: true, Status: "deactivated — paste it", Prefill: "KEYID-CLAIMED"})
	if got := mm.(model); got.input.Value() != "KEYID-CLAIMED" {
		t.Fatalf("Stop with Prefill must populate the empty input, got %q", got.input.Value())
	}
}

// TestClaimStopKeepsTypedInput: a Prefill must not clobber what the user already
// typed.
func TestClaimStopKeepsTypedInput(t *testing.T) {
	m := newModel(Config{Submit: neverSubmit(t)})
	m.claiming = true
	m.input.SetValue("TYPED-BY-USER")
	mm, _ := m.Update(claimUpdateMsg{Stop: true, Prefill: "KEYID-CLAIMED"})
	if got := mm.(model); got.input.Value() != "TYPED-BY-USER" {
		t.Fatalf("Prefill must not overwrite typed input, got %q", got.input.Value())
	}
}

// TestClaimStatusUpdateKeepsDraining: a non-terminal update updates the status
// line, stays in the input phase, and keeps draining (returns a re-arm cmd that
// reads the next update off the channel).
func TestClaimStatusUpdateKeepsDraining(t *testing.T) {
	// Preload the channel so the returned drain cmd resolves to the next update
	// instead of blocking (the cmd does a real <-ch; tea would run it off-loop).
	ch := make(chan ClaimUpdate, 1)
	ch <- ClaimUpdate{Status: "next"}
	m := newModel(Config{Submit: neverSubmit(t)})
	m.claiming = true
	m.claimCh = ch
	mm, cmd := m.Update(claimUpdateMsg{Status: "still waiting"})
	got := mm.(model)
	if !got.claiming {
		t.Fatal("a non-terminal update must keep the poll live")
	}
	if got.status != "still waiting" {
		t.Fatalf("status = %q, want the live status", got.status)
	}
	if cmd == nil {
		t.Fatal("a non-terminal update must keep draining the channel")
	}
	// The drain cmd re-arms by reading the next update (not a quit).
	if _, ok := cmd().(claimUpdateMsg); !ok {
		t.Fatal("the drain cmd must read the next claim update")
	}
}

// TestClaimStatusRendersWhileClaiming: the View shows the live status line while
// the poll is running.
func TestClaimStatusRendersWhileClaiming(t *testing.T) {
	m := newModel(Config{Prompt: "Paste the issued API key id"})
	m.claiming = true
	m.status = "Watching for your registered key"
	if v := m.View().Content; !strings.Contains(v, "Watching for your registered key") {
		t.Fatalf("expected the status line in the view, got:\n%s", v)
	}
}

// TestWaitingLeadsWhileClaiming: with auto-claim live, the waiting line is the
// primary call to action (rendered above the field), the key-id field is demoted
// to the dim fallback label, and the bold "paste this" prompt is NOT shown — so
// the user sees that waiting is the default and pasting the fallback.
func TestWaitingLeadsWhileClaiming(t *testing.T) {
	m := newModel(Config{Prompt: "Paste the issued API key id"})
	m.claiming = true
	m.input.SetValue("TESTID")
	v := withSize(m, 200, 40).View().Content // wide, so the waiting line stays one row
	const (
		waiting  = "Waiting for key registration"
		fallback = "Rather enter it yourself?"
	)
	if !strings.Contains(v, waiting) {
		t.Fatalf("expected the waiting line in the view, got:\n%s", v)
	}
	if !strings.Contains(v, fallback) {
		t.Fatalf("expected the demoted paste fallback label, got:\n%s", v)
	}
	if strings.Contains(v, "Paste the issued API key id") {
		t.Fatal("the bold paste prompt must be replaced by the fallback label while claiming")
	}
	// The waiting line leads: it precedes the fallback label, which precedes the field.
	iw, ifb, iin := strings.Index(v, waiting), strings.Index(v, fallback), strings.Index(v, "TESTID")
	if !(iw >= 0 && iw < ifb && ifb < iin) {
		t.Fatalf("expected waiting < fallback < input order, got %d, %d, %d", iw, ifb, iin)
	}
}

// TestPastePrimaryWhenNotClaiming: with no auto-claim poll, the screen keeps the
// paste-primary framing — the bold prompt, no waiting line, no fallback label.
func TestPastePrimaryWhenNotClaiming(t *testing.T) {
	v := withSize(newModel(Config{Prompt: "Paste the issued API key id"}), 200, 40).View().Content
	if !strings.Contains(v, "Paste the issued API key id") {
		t.Fatal("with no auto-claim, the bold paste prompt must be shown")
	}
	if strings.Contains(v, "Rather enter it yourself?") || strings.Contains(v, "Waiting for key registration") {
		t.Fatal("no waiting/fallback framing when not claiming")
	}
}

// TestStoppedClaimRevertsToPastePrompt: once the poll STOPS (conflict/hard error)
// the "just wait" path is dead, so the view reverts to the bold paste prompt —
// pasting is now the only way forward — and keeps the stop notice visible.
func TestStoppedClaimRevertsToPastePrompt(t *testing.T) {
	m := newModel(Config{Prompt: "Paste the issued API key id"})
	// The Stop handler clears claiming and sets a sticky status; mirror that state.
	m.claiming = false
	m.status = "collision — paste it"
	v := withSize(m, 200, 40).View().Content
	if !strings.Contains(v, "Paste the issued API key id") {
		t.Fatal("after a stop, the bold paste prompt must return")
	}
	if !strings.Contains(v, "collision — paste it") {
		t.Fatal("the stop notice must stay visible")
	}
	if strings.Contains(v, "Rather enter it yourself?") || strings.Contains(v, "Waiting for key registration") {
		t.Fatal("no waiting/fallback framing after the poll stops")
	}
}

// TestWaitingLineIsAccentStyled: the waiting line must render with the accent
// style (bold+green), not the dim hint tier. The presence/ordering assertions
// above would still pass if it were swapped back to the dim tier, so this pins the
// styling itself as the invariant.
func TestWaitingLineIsAccentStyled(t *testing.T) {
	const sentinel = "SENTINEL_STATUS"
	if waitStyle.Render(sentinel) == sentinel {
		t.Skip("terminal color disabled in this environment; styling not observable")
	}
	m := newModel(Config{Prompt: "Paste the issued API key id"})
	m.claiming = true
	m.status = sentinel // a custom status, so the assertion isn't coupled to the shipped copy
	v := withSize(m, 200, 40).View().Content
	if !strings.Contains(v, waitStyle.Render(sentinel)) {
		t.Fatalf("waiting line must be accent-styled (bold+green); view:\n%s", v)
	}
	if strings.Contains(v, hintStyle.Render(sentinel)) {
		t.Fatal("waiting line must not use the dim hint style")
	}
}

// TestWaitingWrapsAndIsNotClipped: a long auto-claim status wraps to width with a
// hanging indent under the spinner gutter, no visible line exceeds width, and the
// whole message survives the wrap — so it never clips on a narrow terminal.
func TestWaitingWrapsAndIsNotClipped(t *testing.T) {
	const width = 30
	const msg = "Waiting for key registration — finish it at the developers portal and setup completes automatically."
	block := renderWaiting("⠋", msg, width)
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		t.Fatalf("a long status must wrap at width %d, got %d line(s)", width, len(lines))
	}
	for _, ln := range lines {
		if w := ansi.StringWidth(ansi.Strip(ln)); w > width {
			t.Fatalf("wrapped line exceeds width %d (=%d): %q", width, w, ansi.Strip(ln))
		}
	}
	// Continuation lines hang-indent by exactly the two-cell spinner gutter (not 0,
	// not more), so wrapped text aligns under the first line's content rather than
	// under the spinner; the first line carries the spinner glyph.
	for i, ln := range lines {
		s := ansi.Strip(ln)
		if i == 0 {
			if !strings.HasPrefix(s, "⠋ ") {
				t.Fatalf("first line must start with the spinner gutter, got %q", s)
			}
			continue
		}
		if !strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "   ") {
			t.Fatalf("continuation line must indent by exactly two spaces, got %q", s)
		}
	}
	// Strip the styling, collapse the gutter/indent whitespace, and drop the spinner:
	// the words must reconstruct the original message exactly.
	got := strings.TrimPrefix(strings.Join(strings.Fields(ansi.Strip(block)), " "), "⠋ ")
	if got != msg {
		t.Fatalf("wrapped status must reconstruct:\n got %q\nwant %q", got, msg)
	}
}

// TestFailedPasteWhileClaimingShowsBoth: a paste that fails (submitResultMsg err)
// returns to the prompt while the poll is still live, so the view coherently shows
// BOTH the accent waiting line and the red retry error — neither path is dropped.
func TestFailedPasteWhileClaimingShowsBoth(t *testing.T) {
	m := newModel(Config{Prompt: "Paste the issued API key id"})
	m.claiming = true
	mm, _ := m.Update(submitResultMsg{err: errors.New("bad key id")})
	got := mm.(model)
	if got.phase != phaseInput {
		t.Fatalf("a failed submit must return to the input phase, got %v", got.phase)
	}
	if !got.claiming {
		t.Fatal("the background poll must stay live after a failed paste")
	}
	v := withSize(got, 200, 40).View().Content
	if !strings.Contains(v, "Rather enter it yourself?") {
		t.Fatal("the waiting/fallback framing must remain while claiming")
	}
	if !strings.Contains(v, "bad key id") {
		t.Fatal("the retry error must be shown")
	}
}

// TestClaimReserveRows: the inline-QR height reserve for the auto-claim waiting
// block is rows(block) − 2 — the block supplants the prompt row AND the sticky-
// status row that inputChrome already budgets, so only rows beyond those two are
// extra. An empty block (not claiming) reserves nothing. Reserving the full block
// height instead would hide the QR one row too early.
func TestClaimReserveRows(t *testing.T) {
	cases := []struct {
		name  string
		block string // hand-counted rows: an "⠋ …" waiting line, a blank, then the label
		want  int
	}{
		{"empty / not claiming", "", 0},
		{"one-row waiting (3 rows)", "⠋ status\n\nlabel", 1},
		{"two-row waiting (4 rows)", "⠋ status line one\n  line two\n\nlabel", 2},
		{"three-row waiting (5 rows)", "⠋ one\n  two\n  three\n\nlabel", 3},
	}
	for _, tc := range cases {
		if got := claimReserveRows(tc.block); got != tc.want {
			t.Errorf("%s: claimReserveRows = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// inputModel returns a sized, link-carrying model in the input phase — the state in
// which the link block, copy, and QR shortcuts are live.
func inputModel() model {
	m := newModel(Config{Prompt: "Paste the issued API key id", RegistrationURL: longURL})
	return withSize(m, 80, 40)
}

// TestLongURLWrapsAndIsNotClipped: the link block wraps across multiple lines, no
// visible line exceeds the target width, and the whole URL survives the wrap, so a
// long link is fully shown rather than clipped at the viewport edge. Asserted on
// renderLink directly, since the text-input widget below manages its own fixed width.
func TestLongURLWrapsAndIsNotClipped(t *testing.T) {
	const width = 48
	block := renderLink(longURL, width)
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		t.Fatalf("a %d-char URL must wrap at width %d, got %d line(s)", len(longURL), width, len(lines))
	}
	for _, ln := range lines {
		if w := ansi.StringWidth(ansi.Strip(ln)); w > width {
			t.Fatalf("wrapped line exceeds width %d (=%d): %q", width, w, ansi.Strip(ln))
		}
	}
	// Character wrap adds no spaces, so rejoining the stripped segments reconstructs
	// the full URL — nothing is dropped.
	if got := strings.ReplaceAll(ansi.Strip(block), "\n", ""); got != longURL {
		t.Fatalf("wrapped URL must reconstruct exactly:\n got %q\nwant %q", got, longURL)
	}
	// And the block must still appear (wrapped) in the live view.
	if v := withSize(newModel(Config{Prompt: "p", RegistrationURL: longURL}), width, 40).View().Content; !strings.Contains(v, "\x1b]8;;"+longURL) {
		t.Fatal("the wrapped link must render in the input view")
	}
}

// TestLinkHasOSC8Hyperlink: the link block carries an OSC 8 hyperlink whose target
// is the full URL, so a supporting terminal makes it clickable regardless of wrap.
func TestLinkHasOSC8Hyperlink(t *testing.T) {
	m := withSize(newModel(Config{Prompt: "p", RegistrationURL: longURL}), 48, 40)
	if v := m.View().Content; !strings.Contains(v, "\x1b]8;;"+longURL) {
		t.Fatalf("expected an OSC 8 hyperlink targeting the full URL")
	}
}

// TestNoLinkNoBlockOrShortcuts: with no registration link, the link block is absent
// and the footer omits the copy/QR shortcuts.
func TestNoLinkNoBlockOrShortcuts(t *testing.T) {
	m := withSize(newModel(Config{Prompt: "p"}), 80, 40)
	v := m.View().Content
	if strings.Contains(v, "\x1b]8;;") {
		t.Fatal("no link means no hyperlink block")
	}
	if strings.Contains(v, "copy link") || strings.Contains(v, "show QR") {
		t.Fatal("no link means no copy/QR shortcuts in the footer")
	}
	// ^R / ^Y are inert without a link.
	if mm, _ := m.Update(ctrlR()); mm.(model).showQR {
		t.Fatal("^R must not open the overlay without a link")
	}
	if _, cmd := m.Update(ctrlY()); cmd != nil {
		t.Fatal("^Y must be a no-op without a link")
	}
}

// TestCopyShortcutSetsClipboard: ^Y flags a copy confirmation and returns a command
// (OSC 52 clipboard write), from both the prompt and the QR overlay.
func TestCopyShortcutSetsClipboard(t *testing.T) {
	for _, overlay := range []bool{false, true} {
		m := inputModel()
		m.showQR = overlay
		mm, cmd := m.Update(ctrlY())
		got := mm.(model)
		if !got.copied {
			t.Fatalf("overlay=%v: ^Y must set the copied confirmation", overlay)
		}
		if cmd == nil {
			t.Fatalf("overlay=%v: ^Y must return a clipboard command", overlay)
		}
		if overlay && !got.showQR {
			t.Fatal("^Y in the overlay must not close it")
		}
	}
}

// TestCopiedClearsOnNextKey: the copied confirmation is transient — the next
// keypress clears it.
func TestCopiedClearsOnNextKey(t *testing.T) {
	m := inputModel()
	m.copied = true
	if got := mustModel(m.Update(escape())); got.copied {
		t.Fatal("a following keypress must clear the copied confirmation")
	}
}

// TestQROverlayTogglesAndCloses: ^R opens the overlay (rendering the scan caption),
// and ^R or Esc closes it back to the prompt without quitting.
func TestQROverlayTogglesAndCloses(t *testing.T) {
	m := inputModel()
	open := mustModel(m.Update(ctrlR()))
	if !open.showQR {
		t.Fatal("^R must open the QR overlay")
	}
	if v := open.View().Content; !strings.Contains(v, "Scan with your phone") {
		t.Fatalf("overlay view must show the scan caption; got:\n%s", ansi.Strip(v))
	}
	// Esc closes the overlay and must NOT quit (that's the prompt's finish-later).
	closed, cmd := open.Update(escape())
	if isQuit(cmd) {
		t.Fatal("Esc in the overlay must close it, not quit setup")
	}
	if closed.(model).showQR {
		t.Fatal("Esc must close the overlay")
	}
	// ^R toggles it shut too.
	if again := mustModel(open.Update(ctrlR())); again.showQR {
		t.Fatal("^R must toggle the overlay closed")
	}
}

// TestEnterClosesOverlay: Enter dismisses the QR overlay (like Esc/^R) without
// quitting setup or submitting.
func TestEnterClosesOverlay(t *testing.T) {
	m := inputModel()
	m.showQR = true
	got, cmd := m.Update(enter())
	if isQuit(cmd) {
		t.Fatal("Enter in the overlay must not quit or submit")
	}
	if got.(model).showQR {
		t.Fatal("Enter must close the overlay")
	}
}

// TestQRShownInlineWhenRoomy: on a large terminal the QR renders inline in the prompt
// view (alongside the paste field) without pressing ^R, the overlay flag stays off,
// and the footer drops the now-redundant "show QR" hint.
func TestQRShownInlineWhenRoomy(t *testing.T) {
	m := withSize(newModel(Config{Prompt: "Paste the issued API key id", RegistrationURL: longURL}), 120, 70)
	if m.showQR {
		t.Fatal("inline QR must not set the overlay flag")
	}
	v := m.View().Content
	if !strings.ContainsAny(v, "█▀▄") {
		t.Fatal("a roomy terminal must show the QR inline")
	}
	if !strings.Contains(v, "Paste the issued API key id") {
		t.Fatal("the paste prompt must stay visible alongside the inline QR")
	}
	if strings.Contains(v, "show QR") {
		t.Fatal("the footer must drop the ^R hint when the QR is already inline")
	}
}

// TestQRPrerenderedAtConstruction: the QR is rendered once at construction (not on
// the per-frame render path), so its cached fields are populated when there's a link
// and empty when there isn't.
func TestQRPrerenderedAtConstruction(t *testing.T) {
	m := newModel(Config{RegistrationURL: longURL})
	if m.qr == "" || m.qrW == 0 || m.qrH == 0 {
		t.Fatal("the QR must be rendered and measured once at construction")
	}
	if n := newModel(Config{}); n.qr != "" || n.qrW != 0 || n.qrH != 0 {
		t.Fatal("no link means no pre-rendered QR")
	}
}

// TestInlineQRDropsWhenRetryLinesCrowd: a terminal roomy enough to show the QR inline
// while idle must stop showing it inline once a non-terminal Submit adds retry
// guidance lines (m.result) that would otherwise push the QR off the top of the view.
func TestInlineQRDropsWhenRetryLinesCrowd(t *testing.T) {
	m := withSize(newModel(Config{Prompt: "Paste the issued API key id", RegistrationURL: longURL}), 120, 45)
	if !strings.ContainsAny(m.View().Content, "█▀▄") {
		t.Fatal("precondition: the QR should show inline while idle at this size")
	}
	m.result = make([]string, 20) // a tall retry-guidance block
	for i := range m.result {
		m.result[i] = "retry guidance line"
	}
	if strings.ContainsAny(m.View().Content, "█▀▄") {
		t.Fatal("the QR must step aside once retry lines would push it off-screen")
	}
}

// TestQRNotInlineWhenShort: on a short terminal the QR is not shown inline; the
// prompt shows the link and advertises ^R to open it.
func TestQRNotInlineWhenShort(t *testing.T) {
	m := withSize(newModel(Config{Prompt: "p", RegistrationURL: longURL}), 80, 24)
	v := m.View().Content
	if strings.ContainsAny(v, "█▀▄") {
		t.Fatal("a short terminal must not show the QR inline")
	}
	if !strings.Contains(v, "show QR") {
		t.Fatal("the footer must advertise ^R when the QR isn't inline")
	}
}

// TestQROverlayAutoClosesOnClaimDone: a Done claim update while the overlay is open
// completes setup — the overlay drops and the final result renders (scan-on-phone
// happy path).
func TestQROverlayAutoClosesOnClaimDone(t *testing.T) {
	m := inputModel()
	m.showQR = true
	m.claiming = true
	mm, cmd := m.Update(claimUpdateMsg{Done: true, Result: []string{"bound automatically"}})
	got := mm.(model)
	if got.showQR {
		t.Fatal("a Done claim must auto-close the overlay")
	}
	if got.phase != phaseDone || !isQuit(cmd) {
		t.Fatal("a Done claim must complete the session")
	}
}

// TestQROverlayAutoClosesOnClaimStop: a Stop claim while the overlay is open drops
// it back to the prompt so the fallback notice is visible.
func TestQROverlayAutoClosesOnClaimStop(t *testing.T) {
	m := inputModel()
	m.showQR = true
	m.claiming = true
	got := mustModel(m.Update(claimUpdateMsg{Stop: true, Status: "collision — paste it"}))
	if got.showQR {
		t.Fatal("a Stop claim must close the overlay")
	}
	if got.phase != phaseInput {
		t.Fatal("a Stop claim must return to the prompt")
	}
}

// TestQRTooSmallShowsMessageAndLink: when the terminal can't fit the QR, the overlay
// shows the too-small message and falls back to the link instead of a broken code.
func TestQRTooSmallShowsMessageAndLink(t *testing.T) {
	m := inputModel()
	m = withSize(m, 20, 8) // far smaller than a ~200-byte-URL QR needs
	m.showQR = true
	v := m.View().Content
	if !strings.Contains(v, "too small") {
		t.Fatalf("a too-small terminal must say so; got:\n%s", ansi.Strip(v))
	}
	if !strings.Contains(v, "\x1b]8;;"+longURL) {
		t.Fatal("the too-small overlay must fall back to the link")
	}
}

// TestQRRendersWhenItFits: a large terminal renders the half-block QR (module
// glyphs present, not the too-small message).
func TestQRRendersWhenItFits(t *testing.T) {
	m := inputModel()
	m = withSize(m, 120, 60)
	m.showQR = true
	v := m.View().Content
	if strings.Contains(v, "too small") {
		t.Fatalf("a large terminal must render the QR; got the too-small message:\n%s", ansi.Strip(v))
	}
	if !strings.ContainsAny(v, "█▀▄") {
		t.Fatal("expected half-block QR glyphs")
	}
}

// mustModel unwraps the (tea.Model, tea.Cmd) pair to the concrete model.
func mustModel(mm tea.Model, _ tea.Cmd) model { return mm.(model) }
