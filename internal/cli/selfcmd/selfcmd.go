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
	"slices"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/cli/monitorcmd"
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
		Getenv:      cx.Getenv,
		Now:         cx.Now,
		Doer:        cx.Doer,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Version:     version.Version,
		Repo:        selfupdate.DefaultRepo,
		HomeDBNames: homeDBNames(),
		Progress:    cx.IO.Err,
		Logger:      cx.Log,
	}
}

// homeDBNames describes the databases a CLI home holds, in both of the spellings
// a home can carry. `self doctor` uses it to report a home holding databases its
// own directory name says it will not read, and to name the rename the user has
// to perform to fix that.
//
// Each name belongs to its own package (internal/journal, the monitor command,
// internal/sandbox), and internal/selfupdate must not import any of them — it
// manages an install layout, not a journal or a sandbox — so the two meet here.
//
// When a NEW database is added under the CLI home, add it here too: a database
// missing from this list is one doctor cannot see stranded under the wrong
// filename, and one MIGRATION.md's rename table would not mention.
//
// The paths are home-relative with forward slashes — how a user reads them, and
// how MIGRATION.md writes them.
func homeDBNames() []selfupdate.HomeDBName {
	sandboxDir := filepath.Base(sandbox.StateDir(""))
	return []selfupdate.HomeDBName{
		{Current: journal.DefaultFileName, Legacy: journal.LegacyFileName},
		{Current: monitorcmd.BotDBDefaultName, Legacy: monitorcmd.LegacyBotDBFileName},
		{
			Current: sandboxDir + "/" + sandbox.DBDefaultName,
			Legacy:  sandboxDir + "/" + sandbox.LegacyDBFileName,
		},
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
	res, err := config(cx).Update(ctx, selfupdate.UpdateOptions{
		TargetVersion: tag,
		DryRun:        cx.Modes.DryRun,
	})
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

	dataPaths []string   // config + journal (+sidecars) handed to Uninstall
	homes     []string   // every CLI home this uninstall acts on, the one in use first
	keyPurges []keyPurge // per CLI home: clear every key, then remove its key files
	artifacts []string   // all candidate cache dirs handed to Uninstall
}

// keyPurge is one CLI home whose keys must be cleared from their backends before
// its key files are deleted. There is normally one; a machine that still carries
// a home under the earlier product's directory name has two, and each needs its
// own key manager built from its OWN config.json — the backend is a per-home
// setting, so purging a `"keystore": "keychain"` home through another home's
// file backend would delete its keys.json and leave its keychain items orphaned,
// where nothing can find them again.
type keyPurge struct {
	home  string
	files []string
}

// buildUninstallPlan resolves the candidate paths/edits read-only (no changes).
//
// It covers BOTH directory generations: an uninstall must leave nothing behind,
// and a machine still on the earlier directory name (or one carrying a leftover
// legacy home beside the current one) holds keys and a journal under the earlier
// product's directory names. Those paths are added only when they exist, so the
// usual single-home case is unchanged.
func buildUninstallPlan(cx *clienv.Cmd, su selfupdate.Config, home, oh string) uninstallPlan {
	l := su.Layout()
	artifacts := []string{sandbox.StateDir(home)}
	cands, cerr := sandbox.CacheDirs(cx.Getenv)
	if cerr == nil {
		artifacts = append(artifacts, cands.Current)
		if cands.Legacy != "" {
			artifacts = append(artifacts, cands.Legacy)
		}
	}
	homes := uninstallHomes(l, home)
	for _, h := range homes[1:] {
		artifacts = append(artifacts, sandbox.StateDir(h))
	}
	var dataPaths, dataShow []string
	var keyPurges []keyPurge
	for _, h := range homes {
		dataPaths = append(dataPaths, homeDataPaths(h)...)
		dataShow = append(dataShow, clihome.Path(h), keys.RegistryPath(h), keystore.FilePath(h))
		dataShow = append(dataShow, journal.Paths(h)...)
		dataShow = append(dataShow, monitorcmd.BotDBPaths(h)...)
		dataShow = append(dataShow, debugBundles(h)...)
		keyPurges = append(keyPurges, keyPurge{home: h, files: keyFilePaths(h)})
	}
	return uninstallPlan{
		binaryShow: existing(append(append([]string{l.ExecutablePath()}, su.AliasPaths()...), l.ManifestPath())...),
		dataShow:   existing(dataShow...),
		keyNames:   keyDisplayNames(cx, homes, oh),
		cacheShow:  existingDirs(artifacts...),
		edits:      su.PathEdits(),
		// config + journal removed directly by Uninstall; the key registry + vault
		// are removed by purgeKeys AFTER it clears each key's material, so
		// keychain-backed keys are never orphaned by deleting keys.json first.
		// Each home's files are listed under BOTH spellings — the one that home's
		// own directory name implies and the other one — so a home whose directory
		// was renamed without its files is still cleaned out completely.
		dataPaths: dataPaths,
		homes:     homes,
		keyPurges: keyPurges,
		artifacts: artifacts,
	}
}

// uninstallHomes lists every CLI home this uninstall acts on, the one in use
// FIRST (it owns the key backends the previews are built from), then any other
// standard location that exists on disk.
//
// All THREE candidates are considered, not just the legacy pair. The home in use
// may be the pinned one, or the legacy one, while a current-named home also sits
// on disk holding keys — and selfupdate's own pruneHome already removes all
// three once they are empty, so listing fewer here would leave files behind in a
// directory the same uninstall then tries to delete.
//
// A home that does not exist is skipped rather than listed: every consumer
// filters by existence anyway, and an absent home contributes nothing but noise
// to the previews.
func uninstallHomes(l selfupdate.Layout, inUse string) []string {
	homes := []string{inUse}
	for _, other := range []string{l.LegacyHomeDir(), l.CurrentHomeDir()} {
		if other == "" || slices.Contains(homes, other) || !dirIsPresent(other) {
			continue
		}
		homes = append(homes, other)
	}
	return homes
}

// homeDataPaths are the CLI-home data files an uninstall removes directly: the
// config, the action journal, the monitor bot database, and any `debug bundle`
// this home has collected. The key registry and vault are NOT here — purgeKeys
// removes them after clearing each key's material from its backend, and the
// sandbox state directory is removed whole as an artifact dir.
//
// Every database is listed under BOTH of its spellings (sidecars included), so a
// home whose directory was renamed without its files is cleaned out rather than
// left holding a stray file under the name it used to carry.
func homeDataPaths(home string) []string {
	paths := []string{clihome.Path(home)}
	paths = append(paths, journal.Paths(home)...)
	paths = append(paths, monitorcmd.BotDBPaths(home)...)
	return append(paths, debugBundles(home)...)
}

// debugBundles are the `debug bundle` files sitting in home. The bundle name
// carries a timestamp, so they can only be found by glob — and the earlier
// product-named spelling is matched too, since a bundle written under it is
// still the user's diagnostic file to remove.
//
// The patterns are the two names this CLI has ever written, not a wildcard like
// `*-debug-*.json`: these paths are DELETED, and the CLI home is a directory a
// user may keep their own notes in. A pattern broad enough to catch
// `run-debug-2.json` would take a file the CLI never created.
//
// A glob that cannot be read yields nothing: an uninstall does not fail over a
// bundle it could not enumerate.
func debugBundles(home string) []string {
	var out []string
	seen := map[string]bool{}
	for _, pattern := range []string{
		"debug-*.json",            // current
		"korbit-cli-debug-*.json", // the earlier name
	} {
		matches, err := filepath.Glob(filepath.Join(home, pattern))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// keyFilePaths are the key registry and vault files under one CLI home — what
// purgeKeys removes once it has cleared every key's material.
func keyFilePaths(home string) []string {
	return []string{keys.RegistryPath(home), keystore.FilePath(home)}
}

// dirIsPresent reports whether path exists and is a directory.
func dirIsPresent(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
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
	plan := buildUninstallPlan(cx, su, home, oh)

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
		for _, p := range plan.keyPurges {
			// Each home's keys are cleared through THAT home's config: the keystore
			// backend is a per-home setting, so using the in-use home's config for a
			// second home would purge a keychain-backed home through the file vault,
			// deleting its keys.json and orphaning its keychain items.
			cfg, lerr := clihome.Load(p.home, cx.Log)
			if lerr != nil {
				warn = append(warn, fmt.Sprintf("could not load %s to remove its keys (%v) — its key files were left in place; remove keys with `%s key remove`", clihome.Path(p.home), lerr, progname.Name()))
				continue
			}
			r, w := purgeKeys(cx.KeyManager(p.home, cfg), p.files)
			rm = append(rm, r...)
			warn = append(warn, w...)
		}
		return rm, warn
	}
	// Every home whose sandbox state dir is about to be removed must have its
	// sandbox stopped first — including a home under the earlier product's
	// directory name, which has its own state dir and can have its own live Deno
	// server. Deleting that directory under a running server leaves it writing to
	// a path nothing can find.
	stopSandbox := func() error {
		var firstErr error
		for _, h := range plan.homes {
			if _, serr := sandbox.New(sandbox.Config{Home: h}, sandbox.Deps{Logger: cx.Log, Doer: cx.Doer}).Stop(context.Background()); serr != nil && firstErr == nil {
				firstErr = serr
			}
		}
		return firstErr
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
func keyDisplayNames(cx *clienv.Cmd, homes []string, oh string) []string {
	var out []string
	for _, home := range homes {
		for _, s := range homeKeys(cx, home) {
			line := fmt.Sprintf("API key %q (%s)", s.Name, s.Keystore)
			// With two homes in play, the name alone is ambiguous — the same key
			// name can exist in both — and the user is confirming the deletion of
			// every one of them, so each must say which home it lives in.
			if len(homes) > 1 {
				line += " in " + abbrev(home, oh)
			}
			out = append(out, line)
		}
	}
	return out
}

// homeKeys lists one home's keys through that home's OWN config, so the backend
// each key is read from is the one it was written with. Best-effort: a home
// whose config or registry cannot be read contributes nothing to the preview.
func homeKeys(cx *clienv.Cmd, home string) []keys.Summary {
	cfg, err := clihome.Load(home, cx.Log)
	if err != nil {
		return nil
	}
	sums, err := cx.KeyManager(home, cfg).List()
	if err != nil {
		return nil
	}
	return sums
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
		if dirIsPresent(p) {
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
