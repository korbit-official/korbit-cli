// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package selfcmd implements the `self install|update|uninstall|doctor`
// builtins: the CLI-facing layer over internal/selfupdate. It builds a
// selfupdate.Config from the invocation environment (the shared HTTP client,
// clock, program environment, and — for install — a /dev/tty PATH confirm),
// dispatches, and renders the result structs; all file/network mechanics live in
// internal/selfupdate.
package selfcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	clihome "github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/journal"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/keystore"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox"
	"github.com/digitalx-official/digitalx-cli/internal/selfupdate"
	"github.com/digitalx-official/digitalx-cli/internal/spec"
	"github.com/digitalx-official/digitalx-cli/internal/version"
	"github.com/spf13/cobra"
)

// Run dispatches the `self …` builtins.
func Run(cx *clienv.Cmd, c *spec.Command, cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q", args[0])
	}
	switch c.Key() {
	case "self install":
		return runInstall(cx)
	case "self update":
		return runUpdate(cx, cmd)
	case "self uninstall":
		return runUninstall(cx, cmd)
	case "self doctor":
		return runDoctor(cx, cmd)
	}
	return output.Usagef("unknown command %q", c.Key())
}

// config builds the selfupdate.Config for this invocation. The interactive PATH
// confirm (install only) is set by the caller afterward via cfg.PathConfirm.
func config(cx *clienv.Cmd) selfupdate.Config {
	return selfupdate.Config{
		Getenv:   cx.Getenv,
		Now:      cx.Now,
		Doer:     cx.Doer,
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Version:  version.Version,
		Repo:     selfupdate.DefaultRepo,
		Progress: cx.IO.Err,
		Logger:   cx.Log,
	}
}

// ---- install (hidden; run by the install script) ----

func runInstall(cx *clienv.Cmd) error {
	cfg := config(cx)
	col := detectColor(cx.Getenv)
	oh := osHome(cx.Getenv)
	// The install script pipes into `sh`, so stdin is the pipe; the confirm reads
	// the controlling terminal (/dev/tty) when there is one, else PATH wiring only
	// prints guidance. The confirm renders a diff preview of each PATH addition and
	// asks per file, defaulting to yes. An injected confirm (tests) takes
	// precedence, so the flow never reaches a real terminal in-process.
	tty := cx.InstallConfirm
	if tty == nil {
		tty = openTTYConfirm()
	}
	var confirm func(selfupdate.PathAddition) (bool, error)
	if tty != nil {
		binDir := abbrev(cfg.Layout().ExecutableDir(), oh)
		explained := false
		confirm = func(add selfupdate.PathAddition) (bool, error) {
			// State the why once, before the first prompt, so the user isn't confirming
			// bare diffs.
			if !explained {
				cx.IO.Note(fmt.Sprintf("To run `%s` from any shell, %s needs to be on your PATH — review each startup-file edit:", progname.Name(), binDir))
				explained = true
			}
			for _, ln := range renderAddition(col, add, oh) {
				cx.IO.Note(ln)
			}
			q := "Add this to " + abbrev(add.Location, oh) + "?"
			if isRegistryAddition(add) {
				q = "Add digitalx-cli to " + add.Location + "?"
			}
			return tty(q, true)
		}
	}

	// --dry-run runs the exact interactive flow (diff previews + per-file prompts)
	// but writes nothing, and skips the release-build gate so it can be exercised
	// from a source build — mirroring `self uninstall --dry-run`.
	if cx.Modes.DryRun {
		return reportInstallDryRun(cx, cfg, confirm, oh)
	}

	cfg.PathConfirm = confirm
	res, err := cfg.Install()
	if err != nil {
		return err
	}
	return cx.Emit("self install", installView{res})
}

// reportInstallDryRun runs the PATH-wiring flow read-only and reports what a real
// install would do — changing nothing. It mirrors the real decision exactly: with
// no terminal it reports "would print guidance, editing nothing" (never a false
// "would wire"); interactively it renders each addition's diff, asks per file, and
// splits into accepted vs left-unchanged.
func reportInstallDryRun(cx *clienv.Cmd, cfg selfupdate.Config, confirm func(selfupdate.PathAddition) (bool, error), oh string) error {
	l := cfg.Layout()
	cx.IO.Note(fmt.Sprintf("Install %s %s (dry run — nothing will be changed).", progname.Name(), version.Version))

	adds := cfg.PathAdditions()
	sections := []string{"Would place:\n  " + abbrev(l.ExecutablePath(), oh)}
	block := func(head string, locs []string) {
		lines := []string{head}
		for _, loc := range locs {
			lines = append(lines, "  "+abbrev(loc, oh))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	switch {
	case len(adds) == 0 && cfg.OnPath():
		sections = append(sections, "PATH:\n  "+l.ExecutableDir()+" is already on your PATH — nothing to wire")
	case len(adds) == 0:
		sections = append(sections, "PATH:\n  already wired in your startup files — restart your shell to pick it up")
	case confirm == nil:
		// A real non-interactive install writes nothing and only prints guidance, so
		// the dry run must not claim it would edit these files.
		var locs []string
		for _, a := range adds {
			locs = append(locs, a.Location)
		}
		block("PATH (no terminal to prompt — a real run would print guidance, editing nothing):", locs)
	default:
		var accepted, declined []string
		for _, add := range adds {
			ok, err := confirm(add)
			if err != nil {
				return err
			}
			if ok {
				accepted = append(accepted, add.Location)
			} else {
				declined = append(declined, add.Location)
			}
		}
		if len(accepted) > 0 {
			block("Would wire PATH in:", accepted)
		}
		if len(declined) > 0 {
			block("Would leave unchanged:", declined)
		}
	}
	return cx.IO.EmitText("Dry run — nothing was changed.\n\n" + strings.Join(sections, "\n\n"))
}

// renderAddition builds the diff preview lines for one pending PATH addition: a
// line-numbered block diff (green `+` additions above dim-numbered context) for a
// shell rc file, or the single added entry for the Windows User PATH. It mirrors
// renderEdit, which renders the removal side for uninstall.
func renderAddition(col colorize, a selfupdate.PathAddition, oh string) []string {
	if isRegistryAddition(a) {
		return []string{
			"",
			"add digitalx-cli to " + a.Location + ":",
			"  " + col.green("+ "+a.Added[0].Text),
		}
	}
	head := "add digitalx-cli to PATH in " + abbrev(a.Location, oh)
	if a.NewFile {
		head += " (new file)"
	}
	lines := []string{"", head + ":"}
	// The added block is appended after the context, so its last line carries the
	// largest number — size the gutter to it.
	width := 1
	if n := len(a.Added); n > 0 {
		width = len(fmt.Sprintf("%d", a.Added[n-1].Num))
	}
	num := func(n int) string { return col.dim(fmt.Sprintf("%*d", width, n)) }
	for _, dl := range a.Context {
		lines = append(lines, "  "+num(dl.Num)+"   "+dl.Text)
	}
	for _, dl := range a.Added {
		lines = append(lines, "  "+num(dl.Num)+" "+col.green("+ "+dl.Text))
	}
	return lines
}

// isRegistryAddition reports whether a PathAddition is the Windows User PATH entry
// (one added line, no file context or line number) rather than a shell rc file.
func isRegistryAddition(a selfupdate.PathAddition) bool {
	return len(a.Context) == 0 && len(a.Added) == 1 && a.Added[0].Num == 0
}

// ---- update ----

func runUpdate(cx *clienv.Cmd, cmd *cobra.Command) error {
	tag, _ := cmd.Flags().GetString("tag")
	// Bound the whole resolve+download+swap so a stalled connection to the release
	// host can't hang the command indefinitely (the shared HTTP client has no
	// timeout of its own); the release archives are tens of MB.
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	res, err := config(cx).Update(ctx, tag, cx.Modes.DryRun)
	if err != nil {
		return mapErr(err)
	}
	view := updateView{UpdateResult: res}
	// A real in-place update replaces the binary, whose embedded Agent Skill may
	// differ from the copies installed on disk — so attach a follow-up telling the
	// caller to refresh them. Only the just-installed binary can install its own
	// (correct) embedded skill, so this is always a suggestion to run `agent skill`
	// against the now-current binary, never an in-process reinstall (this process
	// still holds the pre-update embed). Only on an applied update: a --dry-run or
	// already-current run changed no binary, so there is nothing to refresh.
	if res.Updated {
		view.Skill = suggestSkillRefresh()
	}
	return cx.Emit("self update", view)
}

// updateTimeout bounds a `self update` run end to end (version resolution +
// archive download + in-place swap).
const updateTimeout = 5 * time.Minute

// suggestSkillRefresh is the bundled-skill follow-up to an applied update: run
// the now-current binary's `agent skill` commands to bring an installed copy
// into line with it. The command is the read-only doctor (it reports whether a
// copy is stale and the exact per-agent fix); the message names the install
// commands that apply it. It carries everything in struct fields, so a
// stdout-only / --json agent consumer can act on the result alone.
func suggestSkillRefresh() *skillResult {
	prog := progname.Name()
	return &skillResult{
		Suggested: true,
		Command:   prog + " agent skill doctor",
		Message: fmt.Sprintf("This update may bundle a newer Digital X Agent Skill. Run `%s agent skill doctor` to check your installed copy, then `%s agent skill install --claude` (or `--codex`) to refresh it.",
			prog, prog),
	}
}

// ---- uninstall ----

// uninstallPlan is everything a self uninstall could act on, gathered read-only
// so the interactive flow and --dry-run share one view of the install. The
// *Show fields are the existing paths to display; the others are the full
// candidate sets handed to the removal (which skips what's missing).
type uninstallPlan struct {
	binaryShow []string              // binary + manifest that exist (display)
	dataShow   []string              // config/keys/keystore/journal files that exist (display)
	keyNames   []string              // `API key "x" (backend)` lines (display)
	cacheShow  []string              // cache dirs that exist (display)
	edits      []selfupdate.PathEdit // pending PATH-undo edits, with diff previews

	dataPaths []string // config + journal (+sidecars) handed to Uninstall
	keyFiles  []string // keys.json + keystore.json handed to purgeKeys
	artifacts []string // all candidate cache dirs handed to Uninstall
}

// buildUninstallPlan resolves the candidate paths/edits read-only (no changes).
func buildUninstallPlan(cx *clienv.Cmd, su selfupdate.Config, home string) uninstallPlan {
	l := su.Layout()
	artifacts := []string{sandbox.StateDir(home)}
	if cacheDir, err := sandbox.ResolveCacheDir(cx.Getenv); err == nil {
		artifacts = append(artifacts, cacheDir)
	}
	return uninstallPlan{
		binaryShow: existing(append(append([]string{l.ExecutablePath()}, su.AliasPaths()...), l.ManifestPath())...),
		dataShow:   existing(append([]string{clihome.Path(home), keys.RegistryPath(home), keystore.FilePath(home), journal.DefaultPath(home)}, journal.LegacyPaths(home)...)...),
		keyNames:   keyDisplayNames(cx, home),
		cacheShow:  existingDirs(artifacts...),
		edits:      su.PathEdits(),
		// config + journal removed directly by Uninstall; the key registry + vault
		// are removed by purgeKeys AFTER it clears each key's material, so
		// keychain-backed keys are never orphaned by deleting keys.json first.
		// A home that predates the rename may still hold the journal under its
		// pre-rename name (it is adopted on the next open, but an uninstall may
		// come first), so the legacy file and its sidecars are removed too.
		dataPaths: append([]string{clihome.Path(home), journal.DefaultPath(home), journal.DefaultPath(home) + "-wal", journal.DefaultPath(home) + "-shm"}, journal.LegacyPaths(home)...),
		keyFiles:  []string{keys.RegistryPath(home), keystore.FilePath(home)},
		artifacts: artifacts,
	}
}

// runUninstall drives the interactive-only uninstall: it lists each item's files,
// then asks, then removes. It refuses to run non-interactively (--json/--compact
// or a non-terminal stdin → usage error), because the selection can only happen
// on a terminal. --dry-run runs the same interactive flow but performs no writes,
// reporting only what the selections would do (and skips the managed-install
// check so the flow can be exercised against a plain source build).
func runUninstall(cx *clienv.Cmd, _ *cobra.Command) error {
	home := clihome.Home(cx.Getenv)
	oh := osHome(cx.Getenv)
	su := config(cx)
	dry := cx.Modes.DryRun
	plan := buildUninstallPlan(cx, su, home)

	if cx.Modes.JSONMode {
		return output.Usagef("`%s self uninstall` is interactive and does not support --json/--compact", progname.Name())
	}
	ask := cx.Confirm
	if ask == nil {
		return output.Usagef("`%s self uninstall` is interactive and needs a terminal — run it directly in a terminal, not piped, redirected, or from an agent", progname.Name())
	}
	// Refuse a non-managed install BEFORE asking anything or clearing keys, so no
	// destructive work happens on an install this command can't own. Uninstall
	// re-checks this under its lock as a backstop. --dry-run skips this gate so the
	// flow can be exercised against a plain source build.
	if !dry {
		if err := su.AssertManaged(); err != nil {
			return mapErr(err)
		}
	}

	col := detectColor(cx.Getenv)
	if dry {
		cx.IO.Note("Uninstall digitalx-cli (dry run — nothing will be changed).")
	} else {
		cx.IO.Note("Uninstall digitalx-cli. Nothing is changed until you confirm each item.")
	}

	var removeBinary, removeData, removeCaches bool
	var err error
	if len(plan.binaryShow) > 0 {
		showList(cx, progname.Name()+" binary and install manifest", abbrevAll(plan.binaryShow, oh))
		if removeBinary, err = ask("Remove these?", true); err != nil {
			return err
		}
	}
	if len(plan.dataShow) > 0 || len(plan.keyNames) > 0 {
		showList(cx, "config, API keys, and action journal (deletes keys — cannot be undone)", append(abbrevAll(plan.dataShow, oh), plan.keyNames...))
		if removeData, err = ask("Remove these?", false); err != nil {
			return err
		}
	}
	if len(plan.cacheShow) > 0 {
		showList(cx, "regenerable caches (sandbox state + Deno runtime)", abbrevAll(plan.cacheShow, oh))
		if removeCaches, err = ask("Remove these?", false); err != nil {
			return err
		}
	}
	var editAccepted, keptPaths []string
	for _, e := range plan.edits {
		heading, lines := renderEdit(col, e, oh)
		cx.IO.Note("")
		cx.IO.Note(heading + ":")
		for _, ln := range lines {
			cx.IO.Note(ln)
		}
		q := "Apply this edit?"
		if isRegistryEdit(e) {
			q = "Remove this entry?"
		}
		ok, err := ask(q, false)
		if err != nil {
			return err
		}
		if ok {
			editAccepted = append(editAccepted, e.Location)
		} else {
			keptPaths = append(keptPaths, e.Location)
		}
	}

	// --dry-run: the flow above ran exactly as a real uninstall would, but we
	// perform no writes — just report what those selections would have done.
	if dry {
		return reportDryRun(cx, plan, removeBinary, removeData, removeCaches, editAccepted, keptPaths, oh)
	}

	// Key clearing and sandbox stop run INSIDE Uninstall, under its one lock, via
	// these callbacks — so a lock failure happens before any destruction and the
	// whole outcome comes back in one result that is never discarded.
	clearKeys := func() (rm, warn []string) {
		_, cfg, lerr := cx.LoadConfig()
		if lerr != nil {
			return nil, []string{fmt.Sprintf("could not load config to remove keys (%v) — your key files were left in place; remove keys with `%s key remove`", lerr, progname.Name())}
		}
		return purgeKeys(cx.KeyManager(home, cfg), plan.keyFiles)
	}
	stopSandbox := func() error {
		_, serr := sandbox.New(sandbox.Config{Home: home}, sandbox.Deps{Logger: cx.Log}).Stop(context.Background())
		return serr
	}

	res, err := su.Uninstall(selfupdate.UninstallOptions{
		RemoveBinary: removeBinary,
		RemoveData:   removeData,
		RemoveCaches: removeCaches,
		Artifacts:    plan.artifacts,
		DataPaths:    plan.dataPaths,
		EditPaths:    editAccepted,
		ClearKeys:    clearKeys,
		StopSandbox:  stopSandbox,
	})
	if err != nil {
		return mapErr(err)
	}
	res.KeptPaths = append(res.KeptPaths, keptPaths...)
	return cx.Emit("self uninstall", uninstallView{result: res, home: oh})
}

// reportDryRun prints what the selections made during the (dry) interactive flow
// would have removed/edited/kept, having changed nothing.
func reportDryRun(cx *clienv.Cmd, plan uninstallPlan, removeBinary, removeData, removeCaches bool, editAccepted, keptPaths []string, oh string) error {
	var would []string
	if removeBinary {
		would = append(would, abbrevAll(plan.binaryShow, oh)...)
	}
	if removeData {
		would = append(would, abbrevAll(plan.dataShow, oh)...)
		would = append(would, plan.keyNames...)
	}
	if removeCaches {
		would = append(would, abbrevAll(plan.cacheShow, oh)...)
	}

	var sections []string
	if len(would) > 0 {
		lines := []string{"Would remove:"}
		for _, w := range would {
			lines = append(lines, "  "+w)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(editAccepted) > 0 {
		lines := []string{"Would edit (undo the digitalx-cli PATH change):"}
		for _, loc := range editAccepted {
			lines = append(lines, "  "+abbrev(loc, oh))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(keptPaths) > 0 {
		lines := []string{"Would keep:"}
		for _, loc := range keptPaths {
			lines = append(lines, "  "+abbrev(loc, oh))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	out := "Dry run — nothing was changed."
	if len(sections) > 0 {
		out += "\n\n" + strings.Join(sections, "\n\n")
	} else {
		out += "\n\nNothing selected."
	}
	return cx.IO.EmitText(out)
}

// renderEdit builds the heading + diff lines for one pending PATH-undo edit: a
// line-numbered block diff for a shell rc file, or the single removed entry for
// the Windows User PATH.
func renderEdit(col colorize, e selfupdate.PathEdit, oh string) (heading string, lines []string) {
	if isRegistryEdit(e) {
		return "undo the installer's entry in " + e.Location, []string{"  " + col.red("- "+e.Lines[0].Text)}
	}
	heading = "undo the installer's PATH edit to " + abbrev(e.Location, oh)
	width := len(fmt.Sprintf("%d", e.Lines[len(e.Lines)-1].Num))
	for _, dl := range e.Lines {
		lines = append(lines, "  "+col.dim(fmt.Sprintf("%*d", width, dl.Num))+" "+col.red("- "+dl.Text))
	}
	return heading, lines
}

// isRegistryEdit reports whether a PathEdit is the Windows User PATH entry (one
// diff line, no file line number) rather than a shell rc file block.
func isRegistryEdit(e selfupdate.PathEdit) bool {
	return len(e.Lines) == 1 && e.Lines[0].Num == 0
}

// showList prints an item heading and its files, one per line, before the prompt.
func showList(cx *clienv.Cmd, heading string, items []string) {
	cx.IO.Note("")
	cx.IO.Note(heading + ":")
	for _, it := range items {
		cx.IO.Note("  " + it)
	}
}

// keyDisplayNames lists the configured keys as `API key "name" (backend)` lines
// for the removal preview, best-effort (empty if they can't be read).
func keyDisplayNames(cx *clienv.Cmd, home string) []string {
	_, cfg, err := cx.LoadConfig()
	if err != nil {
		return nil
	}
	sums, err := cx.KeyManager(home, cfg).List()
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range sums {
		out = append(out, fmt.Sprintf("API key %q (%s)", s.Name, s.Keystore))
	}
	return out
}

// osHome is the OS user home used to abbreviate displayed paths to ~.
func osHome(getenv func(string) string) string {
	if runtime.GOOS == "windows" {
		return getenv("USERPROFILE")
	}
	return getenv("HOME")
}

// abbrev shortens a path under the OS home to ~-relative form for display.
func abbrev(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// abbrevAll applies abbrev to each path.
func abbrevAll(paths []string, home string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = abbrev(p, home)
	}
	return out
}

// existing returns the subset of paths that exist (file, dir, or symlink).
//
// It lstats rather than stats, so a name is listed when the NAME is there,
// regardless of what it points at. An alias is a symlink onto the installed
// binary on unix; if that binary is already gone the symlink is dangling, and a
// stat-based check would hide it from the uninstall preview while uninstall
// still removed it — showing the user less than it does.
func existing(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// existingDirs returns the subset of paths that exist and are directories.
func existingDirs(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// purgeKeys removes every key's private material from its backend (file vault or
// OS keychain) and drops its registry record, then removes the key files in
// keyFiles — so a data purge leaves nothing behind, including keychain items that
// deleting keys.json alone would orphan. It returns a line per removed item and a
// warning per key it could not fully clear; it never fails the uninstall. If it
// cannot enumerate the keys, or a key's record could not be removed, it leaves
// the key files in place so what remains stays discoverable via `key remove`.
func purgeKeys(m *keys.Manager, keyFiles []string) (removed, warnings []string) {
	sums, err := m.List()
	if err != nil {
		return nil, []string{fmt.Sprintf("could not list keys to remove (%v) — your key files were left in place; remove keys with `%s key remove`", err, progname.Name())}
	}
	recordsCleared := true
	for _, s := range sums {
		rr, err := m.Remove(s.Name, true)
		if err != nil {
			// The registry record was NOT removed; keep the key files so it stays
			// discoverable.
			recordsCleared = false
			warnings = append(warnings, fmt.Sprintf("could not remove key %q (%v)", s.Name, err))
			continue
		}
		removed = append(removed, fmt.Sprintf("API key %q", s.Name))
		if !rr.SecretRemoved && rr.Warning != "" {
			// The record is gone but the secret stayed in its backend (e.g. keychain
			// unreachable) — surface it; deleting the files doesn't un-orphan it.
			warnings = append(warnings, rr.Warning)
		}
	}
	if !recordsCleared {
		warnings = append(warnings, fmt.Sprintf("left the key files in place because some keys could not be removed; remove them with `%s key remove`", progname.Name()))
		return removed, warnings
	}
	for _, p := range keyFiles {
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("could not remove %s (%v)", p, err))
		}
	}
	return removed, warnings
}

// ---- doctor ----

func runDoctor(cx *clienv.Cmd, _ *cobra.Command) error {
	rep, err := config(cx).Doctor()
	if err != nil {
		return err
	}
	if err := cx.Emit("self doctor", doctorView{rep}); err != nil {
		return err
	}
	if !rep.OK() {
		return clienv.ExitError{Code: output.ExitConfig}
	}
	return nil
}

// mapErr maps a selfupdate provenance refusal to a config-class error (exit 4)
// carrying its guidance, so an agent driving stdout/JSON gets the fix in-band.
func mapErr(err error) error {
	var pe *selfupdate.ProvenanceError
	if errors.As(err, &pe) {
		msg := pe.Message
		if pe.Guidance != "" {
			msg += " — " + pe.Guidance
		}
		return &output.ConfigError{Message: msg}
	}
	return err
}
