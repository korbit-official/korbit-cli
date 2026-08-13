// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"fmt"
)

// Diagnosis field names, one per line `self doctor` reports. A DoctorProblem
// carries the same tag as the line it belongs to, so the text view can print the
// problem + fix inline under its diagnosis and an agent can key off it.
const (
	FieldManaged = "managed" // the managed-install / manifest check
	FieldBinary  = "binary"  // the installed-binary check
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
	Managed          bool            `json:"managed"`
	Method           string          `json:"method,omitempty"`
	RunningVersion   string          `json:"runningVersion"`
	InstalledVersion string          `json:"installedVersion,omitempty"`
	RunningFrom      string          `json:"runningFrom"`
	Executable       string          `json:"executable"`
	ExecutableValid  bool            `json:"executableValid"`
	ExecutableDir    string          `json:"executableDir"`
	OnPath           bool            `json:"onPath"`
	Problems         []DoctorProblem `json:"problems"`
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

	// Installed-binary health.
	switch {
	case !fileExists(l.ExecutablePath()):
		r.add(FieldBinary, "the installed binary is missing — re-run the install one-liner to recreate it")
	case m.SHA256 != "" && !c.executableMatchesManifest(l, m):
		r.add(FieldBinary, "the installed binary doesn't match the recorded install — re-run the install one-liner to reinstall it")
		r.ExecutableValid = false
	default:
		r.ExecutableValid = fileExists(l.ExecutablePath())
	}

	if !r.OnPath {
		r.add(FieldPath, fmt.Sprintf("%s is not on your PATH — re-run the install one-liner, or add it manually", l.ExecutableDir()))
	}
	c.log().Debug("self doctor report", "ok", r.OK(), "managed", r.Managed, "executableValid", r.ExecutableValid, "onPath", r.OnPath, "problems", len(r.Problems))
	return r, nil
}

// executableMatchesManifest reports whether the installed binary's bytes match
// the sha256 the manifest recorded at install time.
func (c Config) executableMatchesManifest(l Layout, m Manifest) bool {
	pSum, err := sha256File(l.ExecutablePath())
	if err != nil {
		return false
	}
	return pSum == m.SHA256
}
