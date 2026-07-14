// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/selfupdate"
)

// The view types embed a selfupdate result so they marshal to the SAME JSON
// (encoding/json inlines an embedded struct's fields) while adding a FormatText
// for human mode. The emitter marshals verbatim under --json and calls
// FormatText otherwise.

type installView struct{ *selfupdate.InstallResult }

func (v installView) FormatText(w io.Writer) {
	fmt.Fprintf(w, "installed korbit %s\n", v.Version)
	fmt.Fprintf(w, "  binary:  %s", v.Executable)
	for _, r := range v.Repaired {
		fmt.Fprintf(w, "\n  repaired: %s", r)
	}
	writePath(w, v.Path)
}

// updateView adds a Skill follow-up alongside the embedded UpdateResult's own
// fields (an embedded struct inlines its JSON fields, so the wire document is the
// UpdateResult plus a "skill" object). Skill is nil unless an applied update had
// a skill follow-up to report.
type updateView struct {
	*selfupdate.UpdateResult
	Skill *skillResult `json:"skill,omitempty"`
}

// skillResult is the bundled-skill follow-up to an applied self update, for both
// the human summary and the --json document. An agent driving stdout reads
// Suggested/Command to learn it should refresh its installed skill copy against
// the now-current binary.
type skillResult struct {
	// Suggested marks that an installed skill copy should be refreshed manually.
	Suggested bool `json:"suggested,omitempty"`
	// Message is the human-readable suggestion.
	Message string `json:"message,omitempty"`
	// Command is the command to run to check/refresh the skill.
	Command string `json:"command,omitempty"`
}

func (v updateView) FormatText(w io.Writer) {
	switch {
	case v.Updated:
		fmt.Fprintf(w, "updated korbit %s → %s", v.PreviousVersion, v.LatestVersion)
		if v.SignatureCheck == selfupdate.SigDisabled {
			fmt.Fprint(w, " (release signature verification disabled — verified on TLS + SHA-256 only)")
		}
	case v.CheckedOnly:
		fmt.Fprintf(w, "an update is available: %s → %s (run `%s self update` to install it)", v.PreviousVersion, v.LatestVersion, progname.Name())
	default:
		fmt.Fprintf(w, "already up to date (%s)", v.PreviousVersion)
	}
	if v.Skill != nil && v.Skill.Message != "" {
		fmt.Fprintf(w, "\n%s", v.Skill.Message)
	}
}

type uninstallView struct {
	result *selfupdate.UninstallResult
	home   string // OS home, for abbreviating displayed paths to ~
}

func (v uninstallView) FormatText(w io.Writer) {
	r := v.result
	nothingDone := len(r.Removed) == 0 && len(r.Edited) == 0 && len(r.Failed) == 0

	var head string
	switch {
	case nothingDone:
		head = "Nothing removed."
	case len(r.Failed) > 0:
		head = "Uninstalled korbit-cli — some items were left behind (see below)."
	default:
		head = "Uninstalled korbit-cli."
	}

	var sections []string
	section := func(lines []string) {
		if len(lines) > 0 {
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}

	if len(r.Removed) > 0 {
		lines := []string{"Removed:"}
		for _, p := range r.Removed {
			lines = append(lines, "  "+abbrev(p, v.home))
		}
		section(lines)
	}
	if len(r.Edited) > 0 {
		lines := []string{"Edited (undid the installer's PATH change):"}
		for _, p := range r.Edited {
			lines = append(lines, "  "+abbrev(p, v.home))
		}
		section(lines)
	}
	if len(r.Failed) > 0 {
		lines := []string{"Could not remove (do this yourself):"}
		for _, f := range r.Failed {
			lines = append(lines, "  "+abbrev(f.Path, v.home)+"  — "+f.Reason)
		}
		section(lines)
	}
	if len(r.KeptPaths) > 0 {
		lines := []string{"Kept:"}
		for _, p := range r.KeptPaths {
			lines = append(lines, "  "+abbrev(p, v.home)+" — edit it yourself to remove the korbit-cli entry")
		}
		section(lines)
	}
	if len(r.Warnings) > 0 {
		var lines []string
		for _, warn := range r.Warnings {
			lines = append(lines, "warning: "+warn)
		}
		section(lines)
	}

	out := head
	if len(sections) > 0 {
		out += "\n\n" + strings.Join(sections, "\n\n")
	}
	fmt.Fprint(w, out)
}

type doctorView struct{ *selfupdate.DoctorReport }

func (v doctorView) FormatText(w io.Writer) {
	r := v.DoctorReport
	status := "ok"
	if !r.OK() {
		status = "needs attention"
	}
	var b []string
	line := func(label, value string) { b = append(b, fmt.Sprintf("  %-19s%s", label+":", value)) }
	// problemLine appends the problem + fix for a diagnosis right under it, so each
	// line explains its own trouble instead of a bare marker.
	problemLine := func(field string) {
		if p := r.Problem(field); p != "" {
			b = append(b, "    ! "+p)
		}
	}

	b = append(b, fmt.Sprintf("korbit self doctor — %s", status))
	line("running version", r.RunningVersion)
	if r.InstalledVersion != "" {
		line("installed version", r.InstalledVersion)
	}
	line("managed install", yesNo(r.Managed))
	problemLine(selfupdate.FieldManaged)
	line("binary", r.Executable)
	problemLine(selfupdate.FieldBinary)
	line("on PATH", yesNo(r.OnPath))
	problemLine(selfupdate.FieldPath)
	// Any problem not tied to a shown diagnosis (defensive; none today).
	for _, p := range r.Problems {
		if p.Field != selfupdate.FieldManaged && p.Field != selfupdate.FieldBinary && p.Field != selfupdate.FieldPath {
			b = append(b, "    ! "+p.Message)
		}
	}
	fmt.Fprint(w, strings.Join(b, "\n"))
}

// writePath appends the PATH outcome to an install summary.
func writePath(w io.Writer, p selfupdate.PathResult) {
	switch p.Action {
	case "already-on-path":
		fmt.Fprintf(w, "\n  PATH:    %s is already on your PATH", p.Dir)
	case "edited-shell-rc":
		// Empty Files means the block was already in the startup files (nothing
		// written this run) — it just needs a fresh shell, so don't claim "added".
		if len(p.Files) == 0 {
			fmt.Fprintf(w, "\n  PATH:    %s is already wired in your shell profile", p.Dir)
		} else {
			fmt.Fprintf(w, "\n  PATH:    added %s to your shell profile", p.Dir)
		}
		if p.Hint != "" {
			fmt.Fprintf(w, "\n           %s", p.Hint)
		}
	case "added-user-path":
		fmt.Fprintf(w, "\n  PATH:    added %s to your User PATH", p.Dir)
		if p.Hint != "" {
			fmt.Fprintf(w, "\n           %s", p.Hint)
		}
	default: // instructions
		fmt.Fprintf(w, "\n  PATH:    %s is NOT on your PATH", p.Dir)
		if p.Hint != "" {
			fmt.Fprintf(w, "\n           %s", p.Hint)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
