// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

// Diagnosis field names, one per line `self doctor` reports. A DoctorProblem
// carries the same tag as the line it belongs to, so the text view can print the
// problem + fix inline under its diagnosis and an agent can key off it.
const (
	FieldManaged = "managed" // the managed-install / manifest check
	FieldBinary  = "binary"  // the installed-binary check
	FieldAlias   = "alias"   // the alias-command-name check
	FieldPath    = "path"    // the on-PATH check
	FieldHome    = "home"    // the CLI-home directory check
)

// Fields lists every diagnosis tag doctor reports against, in report order, so
// a renderer covers them all without repeating the list and the user-facing
// contract (MIGRATION.md) can be checked against it.
func Fields() []string {
	return []string{FieldManaged, FieldBinary, FieldAlias, FieldPath, FieldHome}
}

// HomeDBName is one database a CLI home holds, as the home-relative path it
// takes under each of the two filename layouts: `journal.db` and
// `korbit-cli.db`, `sandbox/sandbox.db` and `sandbox/korbit-sandbox.db`. Both
// are written with forward slashes, the way a user reads them.
//
// Which spelling a home reads is decided by the home's own directory name alone
// (config.LegacyLayout), so a home holding the OTHER spelling holds data nothing
// in it will open — the one file-layout state doctor reports. The names live
// with their owners (internal/journal, internal/sandbox, the monitor command),
// which this package must not import, so the cli layer supplies them through
// Config.HomeDBNames.
type HomeDBName struct {
	Current string
	Legacy  string
}

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
	// keeps the alias pointing at it, on any run — a new release is not needed
	// (see repairLayout).
	LegacyLayout bool `json:"legacyLayout,omitempty"`
	// Home is the CLI home in use; LegacyHome is the home an installation made
	// under the earlier product name carries, reported only when it exists.
	Home       string `json:"home"`
	LegacyHome string `json:"legacyHome,omitempty"`
	// Aliases is the state of each extra command name this install carries, in the
	// order the manifest records them.
	Aliases  []AliasStatus   `json:"aliases,omitempty"`
	Problems []DoctorProblem `json:"problems"`
	// Notes are informational observations that are NOT problems: a state that
	// works as it is, but that a verb would tidy up (an install running only
	// under the alias name, an unmanaged file at the alias name). They never
	// affect OK(), so `self doctor` still exits 0.
	Notes []string `json:"notes,omitempty"`
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
	// Valid reports whether the alias actually runs the primary binary: on unix
	// its symlink resolves to it, and for a plain copy its bytes are the ones the
	// manifest recorded. A present-but-invalid alias is the dangerous case — the
	// command exists, so nothing looks wrong, and it silently runs some other
	// binary or a version behind.
	Valid bool `json:"valid"`
}

// OK reports whether the install is healthy (managed, executable valid, on PATH, no
// problems). The cli layer maps a false to a non-zero exit. Notes are excluded by
// design: they describe supported states, not faults.
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

// note records an informational observation (see DoctorReport.Notes).
func (r *DoctorReport) note(message string) {
	r.Notes = append(r.Notes, message)
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
		Home:           l.Home(),
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

	// The running binary carries the ALIAS name and matches the manifest, while a
	// file at the PRIMARY name does not (primaryIsStale). The `dgx-cli` command —
	// the name the docs, the installers, and every example use — then runs
	// something this install did not place, and nothing about it looks wrong from
	// outside. `self update` recreates it from the running verified bytes rather
	// than adopting it, which is the same predicate this reports it by.
	if r.LegacyLayout && c.primaryIsStale(l, m, exe) {
		r.add(FieldBinary, fmt.Sprintf("the `%s` command at %s doesn't match the recorded install while the running `%s` does — run `%s self update` to recreate it from the running binary",
			l.BinName(), l.ExecutablePath(), l.LegacyBinName(), progname.Name()))
	}

	// Alias command names: each one the manifest records must still be there AND
	// actually run the primary binary, or that command is broken. An alias present
	// on disk but not in the manifest is reported too, so the report describes
	// what is actually on PATH.
	r.Aliases = c.aliasStatuses(l, m, exe)
	for _, a := range r.Aliases {
		switch {
		case !a.Present:
			r.add(FieldAlias, fmt.Sprintf("the `%s` command is missing from %s — run `%s self update` to recreate it", a.Name, l.ExecutableDir(), progname.Name()))
		case !a.Valid && a.Target != "":
			r.add(FieldAlias, fmt.Sprintf("the `%s` command points at %s, not `%s` — run `%s self update` to repoint it", a.Name, a.Target, l.BinName(), progname.Name()))
		case !a.Valid:
			r.add(FieldAlias, fmt.Sprintf("the `%s` command is a stale copy of another version — run `%s self update` to refresh it", a.Name, progname.Name()))
		}
	}

	if !r.OnPath {
		r.add(FieldPath, fmt.Sprintf("%s is not on your PATH — re-run the install one-liner, or add it manually", l.ExecutableDir()))
	}

	c.diagnoseLayout(l, m, exe, r)
	c.diagnoseHome(l, r)

	c.log().Debug("self doctor report", "ok", r.OK(), "managed", r.Managed, "executableValid", r.ExecutableValid, "onPath", r.OnPath, "legacyLayout", r.LegacyLayout, "aliases", len(r.Aliases), "problems", len(r.Problems), "notes", len(r.Notes))
	return r, nil
}

// diagnoseLayout notes the two command-name states that work but are worth
// tidying: an install whose only binary carries the alias name (the primary
// command is simply absent until an update places it), and a file at the alias
// name that this install cannot prove is its own.
func (c Config) diagnoseLayout(l Layout, m Manifest, exe string, r *DoctorReport) {
	if r.LegacyLayout {
		// Only when the primary command is genuinely ABSENT. A primary that exists
		// but is stale is a problem, not a note, and the binary check above has
		// already reported it — telling the user to "add" a command that is sitting
		// right there would contradict it.
		if !fileExists(l.ExecutablePath()) {
			r.note(fmt.Sprintf("this install runs as `%s`; run `%s self update` to add the `%s` command and keep `%s` as an alias for it",
				l.LegacyBinName(), progname.Name(), l.BinName(), l.LegacyBinName()))
		}
		return
	}
	if pathPresent(l.LegacyExecutablePath()) && !c.ownsLegacyAlias(l, m, exe) {
		r.note(fmt.Sprintf("%s is not a file this install manages, so it is left alone — if it is an older install, run `%s self update` with it; otherwise nothing needs doing",
			l.LegacyExecutablePath(), l.LegacyBinName()))
	}
}

// diagnoseHome reports on the CLI home directory. Exactly two situations are
// reported, and both mean data the user has is data the CLI is not reading:
//
//   - Both standard homes exist and the one NOT in use holds data. Which one the
//     CLI reads depends on nothing the user can see (Layout.Home prefers the
//     current name when it is there), so keys written to one are invisible to
//     the other. It is worse still when the home in use holds nothing at all:
//     that is not ambiguity but a certainty, and the message says which way
//     round it is.
//   - The home in use holds databases under the OTHER filename layout
//     (diagnoseHomeFileNames).
//
// Everything else about the two generations of names is silent: a home under the
// earlier directory name is a fully supported state. Both remedies are the
// user's to carry out, and MIGRATION.md spells them out.
func (c Config) diagnoseHome(l Layout, r *DoctorReport) {
	legacy, current := l.LegacyHomeDir(), l.CurrentHomeDir()
	if dirExists(legacy) {
		r.LegacyHome = legacy
	}
	if env := l.HomeEnvVar(); env != "" {
		if env == LegacyEnvHome {
			r.note(fmt.Sprintf("%s pins your CLI home; %s is the current name for that variable, and both are honored", LegacyEnvHome, config.EnvHome))
		}
		// A pinned home is the only home in play: the two standard locations are
		// not what this CLI reads, so a directory sitting at either of them is none
		// of this install's business.
		c.diagnoseHomeFileNames(l, r)
		return
	}
	c.diagnoseHomeFileNames(l, r)
	if !dirExists(legacy) || !dirExists(current) {
		return // one home, wherever it is: nothing is ambiguous
	}
	inUse, other := l.Home(), legacy
	if inUse == legacy {
		other = current
	}
	if homeIsTrivial(other) {
		return // the other one holds no keys, config, or journal — nothing is hidden in it
	}
	if homeIsTrivial(inUse) {
		r.add(FieldHome, fmt.Sprintf("the CLI home in use (%s) holds no keys, config, or journal while %s does — your keys are in a home the CLI stopped reading; see MIGRATION.md to move that data yourself, or remove the empty directory so the other one is used",
			inUse, other))
		return
	}
	r.add(FieldHome, fmt.Sprintf("two CLI homes exist (%s and %s) — only %s is in use, so keys or config in the other are invisible; see MIGRATION.md to move or remove one of them yourself",
		inUse, other, inUse))
}

// homeIsTrivial reports whether dir holds nothing but this install's own
// bookkeeping — the manifest, lock files, and empty directories — so a home that
// looks occupied is not mistaken for one holding keys, config, or a journal. An
// unreadable directory is never trivial.
//
// A SYMLINK is never trivial either, whatever it points at: what matters is
// whether the user has data reachable through that name, and a link's target is
// data this diagnosis cannot vouch for.
func homeIsTrivial(dir string) bool {
	if isSymlink(dir) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		switch {
		case e.IsDir():
			if !dirIsEmpty(filepath.Join(dir, e.Name())) {
				return false
			}
		case e.Name() == manifestFileName:
		case filepath.Ext(e.Name()) == ".lock":
		default:
			return false
		}
	}
	return true
}

// dirIsEmpty reports whether dir has no entries at all.
func dirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// isSymlink reports whether path is itself a symlink. It Lstats deliberately:
// dirExists and every other check here follow links, and a home reached through
// a link is not a directory whose contents this report can speak for.
func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// diagnoseHomeFileNames reports the home IN USE holding databases under the
// EARLIER filename layout when its own directory name says it reads the current
// one — `korbit-cli.db` inside `~/.digitalx-cli`, or inside any pinned directory
// not named `.korbit-cli`. That is a problem, not a note: those files hold
// orders, keys' bookkeeping, and a journal, and the CLI running right now opens
// none of them — it starts empty databases beside them instead.
//
// It reaches that state by a directory being renamed without its files, or by a
// home being copied under a new name; the remedy is the user's, and is spelled
// out in the message. A pinned home gets a second remedy: pointing the variable
// at a directory named `.korbit-cli` keeps the earlier layout.
//
// The reverse state is silent — in a home named `.korbit-cli` the earlier
// spellings ARE the right ones. Only the home in use is examined; diagnoseHome
// already reports that the other standard location holds data.
func (c Config) diagnoseHomeFileNames(l Layout, r *DoctorReport) {
	home := l.Home()
	if home == "" || !dirExists(home) || config.LegacyLayout(home) {
		return
	}
	stray := c.strayLegacyDBs(home)
	if len(stray) == 0 {
		return
	}
	msg := fmt.Sprintf(
		"%s holds %s under the earlier file names — a home whose directory is not named `%s` does not read them, so that data is invisible to the CLI. "+
			"Nothing renames files in your home for you: stop every process using that directory under either command name (`%s` and `%s`), then rename %s, each with its -wal/-shm and other sidecars (see MIGRATION.md)",
		home, strings.Join(stray, ", "), config.LegacyDirName,
		l.BinName(), l.LegacyBinName(), c.manualRenameSteps())
	if env := l.HomeEnvVar(); env != "" {
		msg += fmt.Sprintf("; or point %s at a directory named `%s` to keep the earlier layout", env, config.LegacyDirName)
	}
	r.add(FieldHome, msg)
}

// strayLegacyDBs lists the databases inside home that carry the earlier
// filenames — the spelling a home not named `.korbit-cli` never opens. It is
// presence-only: nothing is opened, so the report stays read-only and cheap.
func (c Config) strayLegacyDBs(home string) []string {
	var out []string
	for _, d := range c.HomeDBNames {
		if pathPresent(filepath.Join(home, filepath.FromSlash(d.Legacy))) {
			out = append(out, d.Legacy)
		}
	}
	return out
}

// manualRenameSteps spells out the rename the user has to perform by hand. It
// names every pair rather than one example, because a home that carries one
// earlier name usually carries all of them.
func (c Config) manualRenameSteps() string {
	var pairs []string
	for _, d := range c.HomeDBNames {
		pairs = append(pairs, fmt.Sprintf("`%s` → `%s`", d.Legacy, d.Current))
	}
	return strings.Join(pairs, ", ")
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
// expected to have (every name the manifest records that this package manages —
// see managedAliasNames) plus the managed alias name when it is on disk without
// being listed but provably ours, so the report matches PATH. In the alias-only
// layout the alias name IS the binary, so it is not also an alias; a file at
// that name this install cannot prove is its own is not an alias either, and is
// reported as an unmanaged file instead (diagnoseLayout).
func (c Config) aliasStatuses(l Layout, m Manifest, runningExe string) []AliasStatus {
	names := c.managedAliasNames(l, m.Aliases)
	unlisted := pathPresent(l.LegacyExecutablePath()) && !slices.Contains(names, l.LegacyBinName())
	if unlisted && fileExists(l.ExecutablePath()) && c.ownsLegacyAlias(l, m, runningExe) {
		names = append(names, l.LegacyBinName())
	}
	var out []AliasStatus
	for _, name := range names {
		p := l.AliasPath(name)
		st := AliasStatus{
			Name:    name,
			Path:    p,
			Target:  symlinkTarget(p),
			Present: pathPresent(p),
		}
		st.Valid = st.Present && c.aliasRunsPrimary(l, m, p, st.Target)
		out = append(out, st)
	}
	return out
}

// aliasRunsPrimary reports whether the alias at path actually runs the primary
// binary — the thing an alias exists to do, and the thing mere presence does not
// prove. Both mechanisms are checked the way each is written (see alias.go), and
// each check is exactly as strong as what it can establish cheaply:
//
//   - a SYMLINK must NAME the primary (the bare relative name this package
//     writes), or resolve to it. Naming it is accepted without following the
//     link, so the primary must also exist for the pair to be sound — a link
//     onto a deleted binary is a command that fails, not one that works.
//     Anything else is a link at this install's command name pointing somewhere
//     this install did not put it.
//   - a plain COPY (windows, or a hand-made copy on unix) must hash to the
//     sha256 the manifest recorded for the installed binary. A copy left behind
//     by an interrupted refresh is a working command that runs a version
//     BEHIND — invisible without the hash.
//
// With no recorded sha256 there is nothing to compare a copy against, so it
// passes uncompared rather than being called invalid on a guess.
func (c Config) aliasRunsPrimary(l Layout, m Manifest, path, target string) bool {
	if target != "" {
		if !fileExists(l.ExecutablePath()) {
			return false // a link onto a binary that is not there runs nothing
		}
		if target == l.BinName() {
			return true
		}
		return resolveOrClean(path) == resolveOrClean(l.ExecutablePath())
	}
	if m.SHA256 == "" {
		return true
	}
	return fileHasSum(path, m.SHA256)
}
