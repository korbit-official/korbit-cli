// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package agentskillcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

// FormatText renders the install summary for human output (textout.TextFormatter);
// the --json path marshals the same struct.
func (r skillInstallResult) FormatText(w io.Writer) {
	fmt.Fprintf(w, "Installed the %q Agent Skill (%s, %s scope):", r.Skill, r.CliVersion, r.Scope)
	for _, o := range r.Installs {
		fmt.Fprintf(w, "\n  %s %s  →  %s  (%d files)", actionMark(o.Action), o.Agent, o.Dir, o.Files)
		if len(o.Pruned) > 0 {
			fmt.Fprintf(w, "\n      removed stale files: %s", strings.Join(o.Pruned, ", "))
		}
		if o.LegacyRemoved != "" {
			fmt.Fprintf(w, "\n      removed the copy at %s (same skill, legacy name)", o.LegacyRemoved)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprint(w, "\n\nWarnings:")
		for _, warn := range r.Warnings {
			fmt.Fprintf(w, "\n  - %s", warn)
		}
	}
	fmt.Fprintf(w, "\n\nVerify with `%s agent skill doctor`.", progname.Name())
}

// actionMark renders an install action as a glyph: + created, ↻ updated, = unchanged.
func actionMark(action string) string {
	switch action {
	case "created":
		return "+"
	case "updated":
		return "↻"
	default:
		return "="
	}
}

// FormatText renders the skill-doctor checklist for human output
// (textout.TextFormatter); the --json path marshals the same struct.
func (r skillDoctorReport) FormatText(w io.Writer) {
	fmt.Fprintf(w, "%q Agent Skill — %s %s", r.Skill, r.Binary, r.CliVersion)
	if r.NameMatches {
		fmt.Fprintf(w, "\n  ✓ running as %q (the command the skill invokes)", r.Binary)
	} else {
		fmt.Fprintf(w, "\n  ✗ running as %q, but the skill invokes %q", r.InvokedAs, r.Binary)
		if r.NameFix != "" {
			fmt.Fprintf(w, "\n      fix: %s", r.NameFix)
		}
	}
	if r.OnPath {
		fmt.Fprintf(w, "\n  ✓ %q on PATH (%s)", r.Binary, r.PathResolved)
	} else {
		fmt.Fprintf(w, "\n  ✗ %q not on PATH", r.Binary)
		if r.PathFix != "" {
			fmt.Fprintf(w, "\n      fix: %s", r.PathFix)
		}
	}
	for _, t := range r.Targets {
		mark, suffix := "•", ""
		switch t.Status {
		case "ok":
			mark, suffix = "✓", "up to date"
		case "stale":
			mark, suffix = "⚠", "installed but stale"
		case "missing":
			mark, suffix = "•", "not installed"
		case "n/a":
			mark, suffix = "·", "agent not detected"
		}
		fmt.Fprintf(w, "\n  %s %s — %s", mark, t.Agent, suffix)
		if t.Installed {
			fmt.Fprintf(w, "\n      %s", t.Dir)
		}
		if t.Fix != "" {
			fmt.Fprintf(w, "\n      fix: %s", t.Fix)
		}
		if t.LegacyNote != "" {
			mark := "⚠"
			if t.LegacyStatus == "foreign" {
				mark = "·"
			}
			fmt.Fprintf(w, "\n      %s %s", mark, t.LegacyNote)
		}
	}
}
