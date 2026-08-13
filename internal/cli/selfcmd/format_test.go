// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/selfupdate"
)

// TestDoctorViewStatesProblemsInline pins the contract that `self doctor`'s text
// output states each problem AND its fix on the diagnosis it belongs to, rather
// than a bare "(problem)" marker with the explanation detached elsewhere.
func TestDoctorViewStatesProblemsInline(t *testing.T) {
	r := &selfupdate.DoctorReport{
		RunningVersion: "dev",
		Executable:     "/home/u/.local/bin/korbit",
		ExecutableDir:  "/home/u/.local/bin",
		OnPath:         true,
		Problems: []selfupdate.DoctorProblem{
			{Field: selfupdate.FieldManaged, Message: "no install manifest — re-run the install one-liner to manage it"},
			{Field: selfupdate.FieldBinary, Message: "the installed binary is missing — re-run the install one-liner to recreate it"},
		},
	}
	var sb strings.Builder
	doctorView{r}.FormatText(&sb)
	out := sb.String()

	if strings.Contains(out, "(problem)") {
		t.Errorf("doctor text should not use a bare (problem) marker:\n%s", out)
	}
	// Every problem message, including its fix, must appear verbatim.
	for _, want := range []string{
		"no install manifest — re-run the install one-liner to manage it",
		"the installed binary is missing — re-run the install one-liner to recreate it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor text missing %q:\n%s", want, out)
		}
	}
	// The binary problem is stated under (after) the binary line, inline.
	if bin, prob := strings.Index(out, "binary:"), strings.Index(out, "the installed binary is missing"); bin < 0 || prob < bin {
		t.Errorf("binary problem should follow the binary line:\n%s", out)
	}
	// A healthy diagnosis (on PATH) gets no problem line under it.
	if strings.Contains(out, "on PATH:           yes\n    !") {
		t.Errorf("a healthy diagnosis should have no problem line:\n%s", out)
	}
}

// TestRenderAdditionShowsContextDiff pins the install PATH preview: context lines
// carry their real line numbers with no +/- marker, added lines are prefixed with
// "+", numbering continues across them, and a new file is labeled. Color is off
// (plain), so the assertions match the piped/CI rendering.
func TestRenderAdditionShowsContextDiff(t *testing.T) {
	col := colorize{enabled: false}
	add := selfupdate.PathAddition{
		Location: "/home/u/.zshrc",
		Context:  []selfupdate.DiffLine{{Num: 8, Text: "export EDITOR=vim"}, {Num: 9, Text: `eval "$(zoxide init zsh)"`}},
		Added: []selfupdate.DiffLine{
			{Num: 10, Text: ""},
			{Num: 11, Text: "# >>> korbit-cli >>>"},
			{Num: 12, Text: `export PATH="$HOME/.local/bin:$PATH"`},
			{Num: 13, Text: "# <<< korbit-cli <<<"},
		},
	}
	out := strings.Join(renderAddition(col, add, "/home/u"), "\n")

	// Home is abbreviated in the heading.
	if !strings.Contains(out, "add korbit-cli to PATH in ~/.zshrc:") {
		t.Errorf("missing/abbreviated heading:\n%s", out)
	}
	// Context lines: number, no + marker, original text.
	if !strings.Contains(out, "   8   export EDITOR=vim") {
		t.Errorf("context line not rendered with a bare (unmarked) gutter:\n%s", out)
	}
	// Added lines: number + "+ " + text; the export line is marked added.
	if !strings.Contains(out, `  12 + export PATH="$HOME/.local/bin:$PATH"`) {
		t.Errorf("added export line not rendered with a + marker:\n%s", out)
	}
	if !strings.Contains(out, "  11 + # >>> korbit-cli >>>") {
		t.Errorf("added begin marker not rendered:\n%s", out)
	}

	// A new file is labeled in the heading.
	newAdd := selfupdate.PathAddition{
		Location: "/home/u/.profile",
		NewFile:  true,
		Added:    []selfupdate.DiffLine{{Num: 1, Text: "# >>> korbit-cli >>>"}},
	}
	if !strings.Contains(strings.Join(renderAddition(col, newAdd, "/home/u"), "\n"), "~/.profile (new file):") {
		t.Error("a new file should be labeled (new file) in the heading")
	}
}

// TestUninstallViewGroupsSections pins the summary layout: Removed, Edited, and
// Kept each get their own section, edits are not mixed into Removed, and a kept
// PATH location is told to be edited (not removed) by hand. Home is abbreviated.
func TestUninstallViewGroupsSections(t *testing.T) {
	r := &selfupdate.UninstallResult{
		Removed:   []string{"/home/u/.local/bin/korbit", "/home/u/.korbit-cli/config.json"},
		Edited:    []string{"/home/u/.zshrc"},
		KeptPaths: []string{"/home/u/.profile"},
	}
	var sb strings.Builder
	uninstallView{result: r, home: "/home/u"}.FormatText(&sb)
	out := sb.String()
	if !strings.Contains(out, "Uninstalled korbit-cli.") {
		t.Errorf("expected the success header:\n%s", out)
	}
	if !strings.Contains(out, "Removed:") || !strings.Contains(out, "~/.local/bin/korbit") {
		t.Errorf("expected a Removed section with abbreviated paths:\n%s", out)
	}
	if !strings.Contains(out, "Edited (undid the installer's PATH change):") || !strings.Contains(out, "~/.zshrc") {
		t.Errorf("expected an Edited section listing the rc file:\n%s", out)
	}
	if !strings.Contains(out, "Kept:") || !strings.Contains(out, "~/.profile — edit it yourself") {
		t.Errorf("expected a Kept section telling the user to edit it themselves:\n%s", out)
	}
	// The edited file must NOT appear under Removed.
	removedBlock := out[strings.Index(out, "Removed:"):strings.Index(out, "Edited")]
	if strings.Contains(removedBlock, ".zshrc") {
		t.Errorf("edited rc file leaked into the Removed section:\n%s", out)
	}
}

// TestUninstallViewReportsFailures pins that things that could not be removed —
// e.g. a locked Windows binary — are surfaced under a "Could not remove" section
// with their reason, the header flags items left behind, and it is not mistaken
// for "nothing removed".
func TestUninstallViewReportsFailures(t *testing.T) {
	r := &selfupdate.UninstallResult{
		Removed: []string{`C:\Users\u\.korbit-cli\config.json`},
		Failed: []selfupdate.FailedRemoval{
			{Path: `C:\Users\u\AppData\Local\bin\korbit.exe`, Reason: "in use — delete it manually after this process exits"},
		},
	}
	var sb strings.Builder
	uninstallView{result: r, home: `C:\Users\u`}.FormatText(&sb)
	out := sb.String()
	if strings.Contains(out, "Nothing removed") {
		t.Errorf("a run with a failure is not 'nothing removed':\n%s", out)
	}
	if !strings.Contains(out, "left behind") {
		t.Errorf("header should flag items left behind:\n%s", out)
	}
	if !strings.Contains(out, "Could not remove (do this yourself):") || !strings.Contains(out, "korbit.exe") || !strings.Contains(out, "in use") {
		t.Errorf("expected a Could-not-remove section with the reason:\n%s", out)
	}
}

// TestUninstallViewNothingRemoved pins that declining everything says so, then
// still reports a kept PATH location.
func TestUninstallViewNothingRemoved(t *testing.T) {
	r := &selfupdate.UninstallResult{KeptPaths: []string{"/home/u/.zshrc"}}
	var sb strings.Builder
	uninstallView{result: r, home: "/home/u"}.FormatText(&sb)
	out := sb.String()
	if !strings.HasPrefix(out, "Nothing removed.") {
		t.Errorf("expected the 'Nothing removed.' header:\n%s", out)
	}
	if !strings.Contains(out, "~/.zshrc — edit it yourself") {
		t.Errorf("expected the kept PATH still reported:\n%s", out)
	}
}
