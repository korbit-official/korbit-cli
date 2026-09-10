// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"fmt"
	"slices"
)

// Diagnosis field names, one per line `self doctor` reports. A DoctorProblem
// carries the same tag as the line it belongs to, so the text view can print the
// problem + fix inline under its diagnosis and an agent can key off it.
const (
	FieldManaged = "managed" // the managed-install / manifest check
	FieldBinary  = "binary"  // the installed-binary check
	FieldAlias   = "alias"   // the alias-command-name check
	FieldPath    = "path"    // the on-PATH check
)

// DoctorProblem is one thing wrong with the install: which diagnosis it belongs
// to (Field) and a message that states the problem AND its fix.
type DoctorProblem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// DoctorReport is the read-only install-health report from `self doctor`. It
// diagnoses the same states self install repairs, but never mutates. Each
// Problem is attached to the diagnosis it concerns and states both what is wrong
// and how to fix it.
type DoctorReport struct {
	Managed          bool   `json:"managed"`
	Method           string `json:"method,omitempty"`
	RunningVersion   string `json:"runningVersion"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	RunningFrom      string `json:"runningFrom"`
	Executable       string `json:"executable"`
	ExecutableValid  bool   `json:"executableValid"`
	ExecutableDir    string `json:"executableDir"`
	OnPath           bool   `json:"onPath"`
	// LegacyLayout marks the shape where the binary on PATH carries only the
	// alias name (Layout.LegacyBinName) and the primary name is not on disk. It is
	// a HEALTHY install, not a problem: self update installs the primary name and
	// keeps the alias pointing at it.
	LegacyLayout bool `json:"legacyLayout,omitempty"`
	// Aliases is the state of each extra command name this install carries, in the
	// order the manifest records them.
	Aliases  []AliasStatus   `json:"aliases,omitempty"`
	Problems []DoctorProblem `json:"problems"`
}

// AliasStatus is one alias command name and where it currently points. Target is
// the symlink target when the alias is a symlink (unix) and empty when it is a
// plain copy (windows). Present is false when the manifest records the alias but
// nothing is there — a broken command the user must repair.
type AliasStatus struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Target  string `json:"target,omitempty"`
	Present bool   `json:"present"`
}

// OK reports whether the install is healthy (managed, executable valid, on PATH, no
// problems). The cli layer maps a false to a non-zero exit.
func (r DoctorReport) OK() bool {
	return r.Managed && r.ExecutableValid && r.OnPath && len(r.Problems) == 0
}

// Problem returns the message for the given diagnosis field, or "" if that
// diagnosis is healthy.
func (r DoctorReport) Problem(field string) string {
	for _, p := range r.Problems {
		if p.Field == field {
			return p.Message
		}
	}
	return ""
}

// add records a problem against a diagnosis field.
func (r *DoctorReport) add(field, message string) {
	r.Problems = append(r.Problems, DoctorProblem{Field: field, Message: message})
}

// Doctor inspects the install and reports its health without changing anything.
func (c Config) Doctor() (*DoctorReport, error) {
	l := c.Layout()
	exe, _ := c.runningExe()
	r := &DoctorReport{
		RunningVersion: c.Version,
		RunningFrom:    exe,
		Executable:     l.ExecutablePath(),
		ExecutableDir:  l.ExecutableDir(),
		OnPath:         c.effectivelyOnPath(l.ExecutableDir()),
		Problems:       []DoctorProblem{},
	}

	// The alias-only layout: the binary on PATH has just the alias name. The
	// binary checks then run against that path, since it IS this install's binary.
	r.LegacyLayout = l.isLegacyBinary(exe) && !l.isInstalledBinary(exe)
	if r.LegacyLayout {
		r.Executable = l.LegacyExecutablePath()
	}

	m, found, merr := loadManifest(l.ManifestPath())
	switch {
	case found && merr != nil:
		r.add(FieldManaged, "the install manifest is corrupt — re-run the install one-liner to rebuild it")
	case !found:
		r.add(FieldManaged, "no install manifest — this binary isn't a managed install (self update is unavailable); re-run the install one-liner to manage it")
	default:
		r.Managed = m.Method == MethodManagedScript
		r.Method = m.Method
		r.InstalledVersion = m.Version
	}

	// Installed-binary health, against whichever name this install's binary
	// currently carries (r.Executable).
	switch {
	case !fileExists(r.Executable):
		r.add(FieldBinary, "the installed binary is missing — re-run the install one-liner to recreate it")
	case m.SHA256 != "" && !fileHasSum(r.Executable, m.SHA256):
		r.add(FieldBinary, "the installed binary doesn't match the recorded install — re-run the install one-liner to reinstall it")
		r.ExecutableValid = false
	default:
		r.ExecutableValid = true
	}

	// Alias command names: each one the manifest records must still be there, or
	// that command is broken. An alias present on disk but not in the manifest is
	// reported too, so the report describes what is actually on PATH.
	r.Aliases = c.aliasStatuses(l, m)
	for _, a := range r.Aliases {
		if !a.Present {
			r.add(FieldAlias, fmt.Sprintf("the `%s` command is missing from %s — re-run the install one-liner to recreate it", a.Name, l.ExecutableDir()))
		}
	}

	if !r.OnPath {
		r.add(FieldPath, fmt.Sprintf("%s is not on your PATH — re-run the install one-liner, or add it manually", l.ExecutableDir()))
	}
	c.log().Debug("self doctor report", "ok", r.OK(), "managed", r.Managed, "executableValid", r.ExecutableValid, "onPath", r.OnPath, "legacyLayout", r.LegacyLayout, "aliases", len(r.Aliases), "problems", len(r.Problems))
	return r, nil
}

// fileHasSum reports whether the file at path has the given lowercase-hex
// sha256 — how doctor checks the installed binary against what the manifest
// recorded at install time.
func fileHasSum(path, want string) bool {
	got, err := sha256File(path)
	if err != nil {
		return false
	}
	return got == want
}

// aliasStatuses reports one entry per alias command name this install is
// expected to have (every name the manifest records) plus the managed alias name
// when it is on disk without being listed, so the report matches PATH. In the
// alias-only layout the alias name IS the binary, so it is not also an alias.
func (c Config) aliasStatuses(l Layout, m Manifest) []AliasStatus {
	names := append([]string(nil), m.Aliases...)
	unlisted := pathPresent(l.LegacyExecutablePath()) && !slices.Contains(names, l.LegacyBinName())
	if unlisted && fileExists(l.ExecutablePath()) {
		names = append(names, l.LegacyBinName())
	}
	var out []AliasStatus
	for _, name := range names {
		p := l.AliasPath(name)
		out = append(out, AliasStatus{
			Name:    name,
			Path:    p,
			Target:  symlinkTarget(p),
			Present: pathPresent(p),
		})
	}
	return out
}
