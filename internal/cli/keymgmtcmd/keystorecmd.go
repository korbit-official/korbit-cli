// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package keymgmtcmd implements the key & keystore management commands —
// `setup`, the `key` group (add/bind/use/rename/remove/list/show/set-base-url/
// set-default-account-seq), and the `keystore` group (status/migrate/default).
// It runs against the clienv.Cmd seam, so the cli dispatches into it without it
// importing cli. The keystore results render their own human output via
// FormatText (textout.TextFormatter); the key/setup path additionally exports
// KeyContext + RunKeyCommand so the mcp server can drive the exact CLI
// key-creation path and capture its JSON document (see keycmd.go).
package keymgmtcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/keystore"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/spec"
	"github.com/spf13/cobra"
)

// RunKeystore handles the `keystore` builtin group (status, migrate, default).
// It is special-cased in dispatch like doctor/ip/logs because it manages the
// keystore backends themselves rather than signing a request with a key.
func RunKeystore(cx *clienv.Cmd, c *spec.Command, cmd *cobra.Command, args []string) error {
	home, cfg, err := cx.LoadConfig()
	if err != nil {
		return err
	}
	switch c.Key() {
	case "keystore status":
		if len(args) > 0 {
			return output.Usagef("unexpected argument %q", args[0])
		}
		return runKeystoreStatus(cx, home, cfg)
	case "keystore migrate":
		return runKeystoreMigrate(cx, home, cfg, cmd, args)
	case "keystore default":
		return runKeystoreDefault(cx, home, args)
	}
	return output.Usagef("unknown keystore subcommand")
}

type keystoreBackendStatus struct {
	Name string `json:"name"`
	// Default marks the backend newly created keys go to (config.json
	// `keystore`); it says nothing about where existing keys live.
	Default   bool   `json:"default"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"` // why it is unavailable, when applicable
	// KeyCount is how many registered keys store their private material here
	// (from each key's `keystore` field in keys.json).
	KeyCount int `json:"keyCount"`
}

type keystoreStatus struct {
	Home string `json:"home"`
	// Default is the backend for NEW keys only — each existing key carries its
	// own backend (see keys, and `key list`).
	Default  string                  `json:"default"`
	KeyCount int                     `json:"keyCount"`
	Backends []keystoreBackendStatus `json:"backends"`
}

func runKeystoreStatus(cx *clienv.Cmd, home string, cfg config.Config) error {
	km := cx.KeyManager(home, cfg)
	list, err := km.List()
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, s := range list {
		counts[s.Keystore]++
	}
	st := keystoreStatus{Home: home, Default: cfg.Keystore, KeyCount: len(list)}
	for _, b := range config.Backends {
		bs := keystoreBackendStatus{Name: b, Default: b == cfg.Keystore, Available: true, KeyCount: counts[b]}
		// Availability is a read-only probe (the keychain probe never prompts);
		// status does not read key material, so it can't trigger a Keychain dialog.
		if err := keystore.Available(b, cx.Log); err != nil {
			bs.Available = false
			bs.Detail = err.Error()
		}
		st.Backends = append(st.Backends, bs)
		delete(counts, b)
	}
	// Keys whose records name a backend this build doesn't know (written by a
	// newer CLI) still count somewhere — list those backends too, as unavailable.
	other := make([]string, 0, len(counts))
	for b := range counts {
		other = append(other, b)
	}
	sort.Strings(other)
	for _, b := range other {
		st.Backends = append(st.Backends, keystoreBackendStatus{
			Name: b, Available: false, KeyCount: counts[b],
			Detail: "not supported by this build",
		})
	}
	return cx.Emit("keystore status", st)
}

func runKeystoreMigrate(cx *clienv.Cmd, home string, cfg config.Config, cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return output.Usagef("`%s keystore migrate <backend> <key>...` needs a target backend (%s) and the key name(s) to move — or --all for every key", progname.Name(), strings.Join(config.Backends, ", "))
	}
	target := args[0]
	names := args[1:]
	if !config.ValidBackend(target) {
		return output.Usagef("unknown keystore backend %q — one of: %s", target, strings.Join(config.Backends, ", "))
	}
	all, _ := cmd.Flags().GetBool("all")
	switch {
	case all && len(names) > 0:
		return output.Usagef("pass key names or --all, not both")
	case !all && len(names) == 0:
		return output.Usagef("name the key(s) to migrate (e.g. `%s keystore migrate %s trading-bot`) or pass --all for every key", progname.Name(), target)
	}
	// Fail fast (before touching any key) if the target backend isn't usable here.
	if err := keystore.Available(target, cx.Log); err != nil {
		return err
	}
	keepSource, _ := cmd.Flags().GetBool("keep-source")

	km := cx.KeyManager(home, cfg)
	list, err := km.List()
	if err != nil {
		return err
	}
	byName := map[string]keys.Summary{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if all {
		for _, s := range list {
			names = append(names, s.Name)
		}
	} else {
		for _, n := range names {
			if _, ok := byName[n]; !ok {
				return output.Usagef("unknown key %q — run `%s key list`", n, progname.Name())
			}
		}
	}
	if len(names) == 0 {
		return cx.Emit("keystore migrate", migrateView{MigrateReport: keystore.MigrateReport{Target: target, KeptSource: keepSource}})
	}

	// Each key migrates from the backend its own record names; open every
	// distinct source once and probe its availability once. An unreachable
	// source switches its keys to recovery mode (salvage what already exists in
	// the target) — a source that is reachable but errors still fails loudly
	// inside Migrate.
	dst, err := keystore.ByName(target, home, cx.Log)
	if err != nil {
		return err
	}
	sources := map[string]keystore.Keystore{} // nil entry = a backend this build can't construct
	unavailable := map[string]bool{}
	items := make([]keystore.MigrateItem, 0, len(names))
	var skippedUnknown []string
	var warnings []string
	for _, n := range names {
		backend := byName[n].Keystore
		src, seen := sources[backend]
		if !seen {
			src, err = keystore.ByName(backend, home, cx.Log)
			if err != nil {
				// The key's recorded backend is unknown to this build (written by a
				// newer CLI). We can't read its material or safely re-point it, so we
				// must not touch it: under --all we skip it and migrate the rest (one
				// forward-version key must never brick the batch); when the user named
				// it explicitly, fail loudly for that key.
				if !all {
					return output.Configf("key %q is stored in the %q keystore, which this build does not support — migrate it with a newer digitalx-cli", n, backend)
				}
				src = nil
			} else if backend != target && keystore.Available(backend, cx.Log) != nil {
				unavailable[backend] = true
				warnings = append(warnings, fmt.Sprintf("the %s keystore is unavailable here — recovering any of its keys already present in the %s keystore", backend, target))
			}
			sources[backend] = src
		}
		if src == nil {
			skippedUnknown = append(skippedUnknown, n)
			continue
		}
		items = append(items, keystore.MigrateItem{Name: n, Source: src, SourceUnavailable: unavailable[backend]})
	}
	if len(skippedUnknown) > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d key(s) stored in a keystore this build does not support — migrate them with a newer digitalx-cli: %s",
			len(skippedUnknown), strings.Join(skippedUnknown, ", ")))
	}

	// commit re-points one key's record at the target — the engine calls it only
	// after that key's material is verified there.
	commit := func(name string) error { return km.SetKeystoreBackend(name, target) }
	rep, err := keystore.Migrate(items, dst, commit, keystore.MigrateOptions{KeepSource: keepSource}, cx.Log)
	if err != nil {
		return err
	}
	if len(rep.Missing) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d key(s) had no private material in either backend — re-import them: %s",
			len(rep.Missing), strings.Join(rep.Missing, ", ")))
	}
	warnings = append(warnings, rep.DeleteWarnings...)
	return cx.Emit("keystore migrate", migrateView{MigrateReport: rep, SkippedUnsupported: skippedUnknown, Warnings: warnings})
}

// runKeystoreDefault implements `keystore default <backend>`: set where newly
// created keys go. Existing keys are untouched — moving them is `keystore
// migrate`'s job.
func runKeystoreDefault(cx *clienv.Cmd, home string, args []string) error {
	if len(args) == 0 {
		return output.Usagef("`%s keystore default <backend>` needs a backend: %s", progname.Name(), strings.Join(config.Backends, ", "))
	}
	if len(args) > 1 {
		return output.Usagef("unexpected argument %q", args[1])
	}
	backend := args[0]
	if !config.ValidBackend(backend) {
		return output.Usagef("unknown keystore backend %q — one of: %s", backend, strings.Join(config.Backends, ", "))
	}
	// Refuse to point new keys at a backend that can't be used here.
	if err := keystore.Available(backend, cx.Log); err != nil {
		return err
	}
	if err := config.SetKeystore(home, backend, cx.Log); err != nil {
		return err
	}
	return cx.Emit("keystore default", keystoreDefaultResult{backend})
}

// ---- human output (textout.TextFormatter) ----

// FormatText renders the keystore-backend status table for human output; the
// --json path marshals the keystoreStatus struct.
func (st keystoreStatus) FormatText(w io.Writer) {
	var b strings.Builder
	fmt.Fprintf(&b, "default keystore for new keys: %s\n", st.Default)
	fmt.Fprintf(&b, "home: %s\n", st.Home)
	fmt.Fprintf(&b, "keys: %d\n\n", st.KeyCount)
	b.WriteString("backends:                    (* = default for new keys)\n")
	for _, bk := range st.Backends {
		marker := "  "
		if bk.Default {
			marker = "* "
		}
		status := "available"
		if !bk.Available {
			status = "unavailable"
			if bk.Detail != "" {
				status += " — " + bk.Detail
			}
		}
		fmt.Fprintf(&b, "%s%-9s %d key(s), %s\n", marker, bk.Name, bk.KeyCount, status)
	}
	fmt.Fprint(w, strings.TrimRight(b.String(), "\n"))
}

// migrateView wraps the lower-layer keystore.MigrateReport (which must not depend
// on the human-output toolkit) so the command package owns its rendering. Extra
// warning fields are CLI-owned result data: agents reading stdout only must still
// see skipped keys and follow-up actions.
type migrateView struct {
	keystore.MigrateReport
	SkippedUnsupported []string `json:"skippedUnsupported,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
}

func (v migrateView) FormatText(w io.Writer) {
	rep := v.MigrateReport
	moved := len(rep.Moved) + len(rep.Identical) + len(rep.Replaced)
	var b strings.Builder
	switch {
	case moved > 0:
		fmt.Fprintf(&b, "Migrated %d key(s) to the %s keystore.\n", moved, rep.Target)
	case len(rep.Recovered)+len(rep.Missing) > 0:
		// A recover-only run moves nothing — it just re-points records — so
		// "Migrated 0 key(s)" would read oddly.
		fmt.Fprintf(&b, "Re-pointed key record(s) at the %s keystore (no material needed moving).\n", rep.Target)
	case len(rep.AlreadyInTarget) > 0:
		fmt.Fprintf(&b, "Nothing to migrate — the named key(s) are already in the %s keystore.\n", rep.Target)
	default:
		fmt.Fprintf(&b, "No keys to migrate. New keys go to the %s keystore (change that with `%s keystore default <backend>`).\n", rep.Target, progname.Name())
	}
	line := func(label string, names []string) {
		if len(names) > 0 {
			fmt.Fprintf(&b, "  %-15s %s\n", label+":", strings.Join(names, ", "))
		}
	}
	line("moved", rep.Moved)
	line("replaced", rep.Replaced)
	line("identical", rep.Identical)
	line("recovered", rep.Recovered)
	line("missing", rep.Missing)
	line("skipped", v.SkippedUnsupported)
	line("already there", rep.AlreadyInTarget)
	if rep.KeptSource && moved > 0 {
		fmt.Fprintf(&b, "Kept the originals in their old keystore(s) (--keep-source).\n")
	}
	if moved+len(rep.Recovered)+len(rep.Missing) > 0 {
		fmt.Fprintf(&b, "Each migrated key's record now points at the %s keystore.", rep.Target)
	}
	if len(v.Warnings) > 0 {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("Warnings:")
		for _, w := range v.Warnings {
			fmt.Fprintf(&b, "\n  - %s", w)
		}
	}
	fmt.Fprint(w, strings.TrimRight(b.String(), "\n"))
}

// keystoreDefaultResult is the `keystore default` result (the new-key backend).
type keystoreDefaultResult struct {
	Default string `json:"default"`
}

func (r keystoreDefaultResult) FormatText(w io.Writer) {
	fmt.Fprintf(w, "New keys will be stored in the %s keystore. Existing keys are unchanged — move them with `%s keystore migrate %s --all` if you want them there too.", r.Default, progname.Name(), r.Default)
}
