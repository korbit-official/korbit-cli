// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package sandboxcmd implements the `sandbox …` builtin group: the lifecycle
// manager for the local Korbit API Sandbox. All sandbox business logic lives in
// internal/sandbox (+ internal/sandbox/deno); this layer only resolves config
// from the environment, dispatches, and produces results. The dispatch never
// writes config.json and the manager only ever pins loopback URLs onto the
// imported key (the production-config-safety invariants).
package sandboxcmd

import (
	"context"
	"errors"
	"os/exec"

	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/sandbox"
	"github.com/korbit-official/korbit-cli/internal/spec"
	"github.com/spf13/cobra"
)

// RealityReminder is the standing caveat shown on sandbox start/status: the
// sandbox is a mock, so passing against it does not prove the same code works in
// production. Verify against production and start small.
const RealityReminder = "Reminder: the sandbox is a mock — code that works here is not guaranteed to work in production. Always verify against production and start with small amounts."

// passthroughExit maps the result of a streamed sandbox subprocess (exec/license)
// to the CLI's exit handling. The bundle has already written its own stdout/stderr,
// so a non-zero bundle exit is forwarded as the CLI's exit code via clienv.ExitError
// (which carries a code but prints NO error envelope) — the user sees the bundle's
// own message, not a redundant "error: exit status N". A negative code (process
// killed by signal) falls back to ExitInternal. Any non-exit failure (runtime/bundle
// resolution, spawn error) is a real CLI error and surfaces normally.
func passthroughExit(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code < 0 {
			code = output.ExitInternal
		}
		return clienv.ExitError{Code: code}
	}
	return err
}

// Run is the thin dispatch for the `sandbox` builtin group.
func Run(cx *clienv.Cmd, c *spec.Command, cmd *cobra.Command, args []string) error {
	home, cfg, err := cx.LoadConfig()
	if err != nil {
		return err
	}
	cacheDir, err := sandboxCacheDir(cx)
	if err != nil {
		return err
	}
	km := cx.KeyManager(home, cfg)
	scfg := sandbox.Config{
		Home:        home,
		CacheDir:    cacheDir,
		URL:         cx.Getenv("KORBIT_CLI_SANDBOX_URL"),
		RuntimePref: cx.Getenv("KORBIT_CLI_SANDBOX_RUNTIME"),
	}
	deps := sandbox.Deps{
		Doer:        cx.Doer,
		Now:         cx.Now,
		Log:         func(s string) { cx.IO.Notef("%s", s) },
		Logger:      cx.Log,
		DenoURL:     cx.Getenv("KORBIT_CLI_DENO_URL"),
		DenoVersion: cx.Getenv("KORBIT_CLI_DENO_VERSION"),
		KeyManager:  km,
		// The short license notice prints to stderr at the top of `start`.
		BannerOut: cx.IO.Err,
	}

	switch c.Key() {
	case "sandbox start":
		return runStart(cx, c, cmd, args, scfg, deps)
	case "sandbox stop":
		if err := clienv.RequireNoArgs(args); err != nil {
			return err
		}
		m := sandbox.New(scfg, deps)
		res, err := m.Stop(context.Background())
		if err != nil {
			return err
		}
		return cx.Emit("sandbox stop", stopView{res})
	case "sandbox status":
		if err := clienv.RequireNoArgs(args); err != nil {
			return err
		}
		m := sandbox.New(scfg, deps)
		return cx.Emit("sandbox status", statusView{StatusResult: m.Status(context.Background()), Reminder: RealityReminder})
	case "sandbox update":
		if err := clienv.RequireNoArgs(args); err != nil {
			return err
		}
		if u := flagValue(cmd, "url"); u != "" {
			scfg.URL = u
		}
		m := sandbox.New(scfg, deps)
		res, err := m.Update(context.Background())
		if err != nil {
			return err
		}
		return cx.Emit("sandbox status", statusView{StatusResult: res, Reminder: RealityReminder})
	case "sandbox license":
		return runLicense(cx, cmd, args, scfg, deps)
	case "sandbox exec":
		return runExec(cx, args, scfg, deps)
	case "sandbox runtime status", "sandbox runtime install", "sandbox runtime update":
		return runRuntime(cx, c, args, scfg, deps)
	}
	return output.Usagef("unknown sandbox subcommand")
}

// sandboxCacheDir resolves the shared artifact cache via sandbox.ResolveCacheDir
// (KORBIT_CLI_SANDBOX_CACHE, else os.UserCacheDir()/korbit-cli), re-typing its
// failure as a config-class error (exit 4).
func sandboxCacheDir(cx *clienv.Cmd) (string, error) {
	dir, err := sandbox.ResolveCacheDir(cx.Getenv)
	if err != nil {
		return "", output.Configf("%s", err.Error())
	}
	return dir, nil
}

func runStart(cx *clienv.Cmd, c *spec.Command, cmd *cobra.Command, args []string, scfg sandbox.Config, deps sandbox.Deps) error {
	if err := clienv.RequireNoArgs(args); err != nil {
		return err
	}
	if cmd.Flags().Changed("port") {
		p, err := clienv.ParseRange(flagValue(cmd, "port"), "--port", 0, 65535)
		if err != nil {
			return err
		}
		scfg.Port = p
	}
	scfg.DB = flagValue(cmd, "db")
	scfg.KeyName = flagValue(cmd, "key-name")
	if r := flagValue(cmd, "runtime"); r != "" {
		scfg.RuntimePref = r
	}
	if u := flagValue(cmd, "url"); u != "" {
		scfg.URL = u
	}
	scfg.Reimport = cmd.Flags().Changed("reimport")
	scfg.SkipVersionCheck = cmd.Flags().Changed("skip-version-check")
	scfg.Paper = cmd.Flags().Changed("paper")
	scfg.AllPairs = cmd.Flags().Changed("all-pairs")
	scfg.Fresh = cmd.Flags().Changed("fresh")

	m := sandbox.New(scfg, deps)
	res, err := m.Start(context.Background())
	if err != nil {
		return err
	}
	return cx.Emit("sandbox start", startView{StartResult: res, Reminder: RealityReminder})
}

// runLicense prints the bundle's own terms — a shortcut for `sandbox exec
// license` that does NOT inject --db (license touches no database). The optional
// --lang is forwarded; with none the bundle follows the locale.
func runLicense(cx *clienv.Cmd, cmd *cobra.Command, args []string, scfg sandbox.Config, deps sandbox.Deps) error {
	if err := clienv.RequireNoArgs(args); err != nil {
		return err
	}
	var extra []string
	if l := flagValue(cmd, "lang"); l != "" {
		extra = []string{"--lang", l}
	}
	m := sandbox.New(scfg, deps)
	// Like exec, this streams the bundle's own stdout/stderr through — the terms
	// text is not part of the JSON output contract. A non-zero bundle exit is
	// forwarded as the CLI's exit code with no extra error envelope.
	return passthroughExit(m.License(context.Background(), extra, cx.IO.Out, cx.IO.Err))
}

func runExec(cx *clienv.Cmd, args []string, scfg sandbox.Config, deps sandbox.Deps) error {
	// DisableFlagParsing on this leaf means args are the verbatim tail, so exec
	// owns all argument handling and forwards everything to the bundle untouched.
	// A bare `sandbox exec` (no tail) prints this command's help — so --help/-h is
	// NOT intercepted and reaches the bundle like any other token.
	if len(args) == 0 {
		if h, ok := cx.CommandHelp("sandbox", "exec"); ok {
			_, _ = cx.IO.Out.Write([]byte(h + "\n"))
		}
		return clienv.ExitError{Code: output.ExitUsage}
	}
	// A leading `--` is the explicit pass-through boundary: drop it and forward the
	// rest verbatim. It is only needed to disambiguate (or once exec grows its own
	// flags); `sandbox exec --help` already forwards without it.
	if args[0] == "--" {
		args = args[1:]
		if len(args) == 0 {
			return output.Usagef("`%s sandbox exec --` needs a bundle subcommand, e.g. `%s sandbox exec set-balance --user 1 --currency btc --available 5`", progname.Name(), progname.Name())
		}
	}
	m := sandbox.New(scfg, deps)
	// exec streams the bundle's own stdout/stderr through — it is a pass-through,
	// not part of the JSON output contract. A non-zero bundle exit is forwarded as
	// the CLI's exit code with no extra error envelope (the output already shows).
	return passthroughExit(m.Exec(context.Background(), args, cx.IO.Out, cx.IO.Err))
}

// runRuntime handles the `sandbox runtime` subcommands. The subcommand is the
// command's last id segment (cobra has already routed it); a bare `sandbox
// runtime` is the group itself and never reaches here — it prints group help.
// All three forms render under the "sandbox runtime" output key.
func runRuntime(cx *clienv.Cmd, c *spec.Command, args []string, scfg sandbox.Config, deps sandbox.Deps) error {
	if err := clienv.RequireNoArgs(args); err != nil {
		return err
	}
	m := sandbox.New(scfg, deps)
	switch c.ID[len(c.ID)-1] {
	case "status":
		return cx.Emit("sandbox runtime", runtimeView{m.RuntimeStatus()})
	case "install":
		st, err := m.RuntimeInstall(context.Background())
		if err != nil {
			return err
		}
		return cx.Emit("sandbox runtime", runtimeView{st})
	case "update":
		st, err := m.RuntimeUpdate(context.Background())
		if err != nil {
			return err
		}
		return cx.Emit("sandbox runtime", runtimeView{st})
	}
	return output.Usagef("unknown sandbox runtime subcommand")
}

// flagValue returns a leaf's string flag value ("" if unset).
func flagValue(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}
