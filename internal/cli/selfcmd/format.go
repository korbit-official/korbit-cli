// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/selfupdate"
)

// The view types embed a selfupdate result so they marshal to the SAME JSON
// (encoding/json inlines an embedded struct's fields) while adding a FormatText
// for human mode. The emitter marshals verbatim under --json and calls
// FormatText otherwise.

type installView struct{ *selfupdate.InstallResult }

func (v installView) FormatText(w io.Writer) {
	fmt.Fprintf(w, "installed %s %s\n", progname.Name(), v.Version)
	fmt.Fprintf(w, "  binary:  %s", v.Executable)
	for _, r := range v.Repaired {
		fmt.Fprintf(w, "\n  repaired: %s", r)
	}
	writePath(w, v.Path)
	writeNext(w, v.Next)
}

// writeNext appends the follow-up steps a result carries, in the numbered shape
// the key/setup commands use for theirs.
func writeNext(w io.Writer, next []string) {
	if len(next) == 0 {
		return
	}
	fmt.Fprint(w, "\n\nNext step:")
	for i, n := range next {
		fmt.Fprintf(w, "\n  %d. %s", i+1, n)
	}
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
		fmt.Fprintf(w, "updated %s %s → %s", progname.Name(), v.PreviousVersion, v.LatestVersion)
		if v.SignatureCheck == selfupdate.SigDisabled {
			fmt.Fprint(w, " (release signature verification disabled — verified on TLS + SHA-256 only)")
		}
	case v.CheckedOnly && v.PreviousVersion == v.LatestVersion:
		fmt.Fprintf(w, "already up to date (%s) — dry run, nothing was changed", v.PreviousVersion)
	case v.CheckedOnly:
		fmt.Fprintf(w, "an update is available: %s → %s (run `%s self update` to install it)", v.PreviousVersion, v.LatestVersion, progname.Name())
	default:
		fmt.Fprintf(w, "already up to date (%s)", v.PreviousVersion)
	}
	// A layout repair happens with or without a new version, so it is reported on
	// its own line rather than folded into either headline. On a dry run the same
	// list is what a real run WOULD fix, so it must not read as done.
	label := "repaired"
	if v.CheckedOnly {
		label = "would repair"
	}
	for _, r := range v.LayoutRepaired {
		fmt.Fprintf(w, "\n  %s: %s", label, r)
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
		head = "Uninstalled digitalx-cli — some items were left behind (see below)."
	default:
		head = "Uninstalled digitalx-cli."
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
			lines = append(lines, "  "+abbrev(p, v.home)+" — edit it yourself to remove the digitalx-cli entry")
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

	b = append(b, fmt.Sprintf("%s self doctor — %s", progname.Name(), status))
	line("running version", r.RunningVersion)
	if r.InstalledVersion != "" {
		line("installed version", r.InstalledVersion)
	}
	line("managed install", yesNo(r.Managed))
	problemLine(selfupdate.FieldManaged)
	line("binary", r.Executable)
	problemLine(selfupdate.FieldBinary)
	for _, a := range r.Aliases {
		switch {
		case !a.Present:
			line("alias "+a.Name, "missing")
		case a.Target != "":
			line("alias "+a.Name, a.Path+" → "+a.Target+aliasMark(a))
		default:
			line("alias "+a.Name, a.Path+aliasMark(a))
		}
	}
	problemLine(selfupdate.FieldAlias)
	line("on PATH", yesNo(r.OnPath))
	problemLine(selfupdate.FieldPath)
	if r.Home != "" {
		line("CLI home", r.Home)
	}
	problemLine(selfupdate.FieldHome)
	// Any problem not tied to a shown diagnosis (defensive; none today).
	shown := map[string]bool{}
	for _, f := range selfupdate.Fields() {
		shown[f] = true
	}
	for _, p := range r.Problems {
		if !shown[p.Field] {
			b = append(b, "    ! "+p.Message)
		}
	}
	// Notes last: they are things that work as they are, so they must not read
	// as failures above the diagnoses that can be.
	for _, n := range r.Notes {
		b = append(b, "    - "+n)
	}
	fmt.Fprint(w, strings.Join(b, "\n"))
}

// aliasMark flags an alias that exists but does not run the primary binary, so
// the line reads as trouble rather than as a healthy path.
func aliasMark(a selfupdate.AliasStatus) string {
	if a.Valid {
		return ""
	}
	return " (not the installed binary)"
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
