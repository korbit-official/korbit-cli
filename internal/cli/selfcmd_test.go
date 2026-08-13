// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/version"
)

// selfEnv points HOME/KORBIT_CLI_HOME at a temp dir with the binary dir absent
// from PATH, so the install exercises the "instructions" PATH branch.
func selfEnv(home string) map[string]string {
	return map[string]string{
		"HOME":                 home,
		"USERPROFILE":          home,
		"KORBIT_CLI_HOME":      filepath.Join(home, ".korbit-cli"),
		"PATH":                 "/usr/bin:/bin",
		"KORBIT_CLI_LOG_LEVEL": "off",
	}
}

// withVersion sets a non-dev version for the duration of a test (the self
// commands refuse a "dev" build). Tests run serially, so a plain set/restore is
// safe.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func newStub() *stubDoer { return &stubDoer{resp: resp(200, "{}", nil)} }

func TestSelfInstallDoctorAndRepair(t *testing.T) {
	withVersion(t, "v9.9.9")
	home := t.TempDir()
	env := selfEnv(home)

	// Decline PATH wiring so the install reports the instructions branch. An
	// injected confirm also keeps the flow off the real /dev/tty (which the
	// production confirm opens directly, bypassing stdin) — without it a `go test`
	// run in an interactive shell prompts the developer's own terminal.
	declinePath := func(string, bool) (bool, error) { return false, nil }

	// --- self install (the script's entry point), driven through cli.Execute ---
	out, _, code := runCLIInstallConfirm([]string{"self", "install", "--json"}, env, newStub(), declinePath)
	if code != 0 {
		t.Fatalf("self install exit=%d out=%s", code, out)
	}
	var inst struct {
		Version    string   `json:"version"`
		Executable string   `json:"executable"`
		Repaired   []string `json:"repaired"`
		Path       struct {
			Action string `json:"action"`
		} `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &inst); err != nil {
		t.Fatalf("install json: %v\n%s", err, out)
	}
	if inst.Version != "v9.9.9" {
		t.Errorf("installed version = %q", inst.Version)
	}
	if _, err := os.Stat(inst.Executable); err != nil {
		t.Errorf("binary not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env["KORBIT_CLI_HOME"], "install.json")); err != nil {
		t.Errorf("manifest not written: %v", err)
	}
	if inst.Path.Action != "instructions" {
		t.Errorf("path action = %q (want instructions; binary dir not on PATH)", inst.Path.Action)
	}
	if len(inst.Repaired) != 0 {
		t.Errorf("fresh install repaired = %v", inst.Repaired)
	}

	// --- self doctor: managed, but not on PATH → exit 4 ---
	dout, _, dcode := runCLI([]string{"self", "doctor", "--json"}, env, newStub())
	if dcode != 4 {
		t.Fatalf("self doctor exit=%d (want 4, not on PATH) out=%s", dcode, dout)
	}
	var doc struct {
		Managed bool `json:"managed"`
		OnPath  bool `json:"onPath"`
	}
	if err := json.Unmarshal([]byte(dout), &doc); err != nil {
		t.Fatalf("doctor json: %v\n%s", err, dout)
	}
	if !doc.Managed || doc.OnPath {
		t.Errorf("doctor = %+v (want managed, not on path)", doc)
	}

	// --- repair: delete the binary, re-run install → it is healed ---
	if err := os.Remove(inst.Executable); err != nil {
		t.Fatal(err)
	}
	rout, _, rcode := runCLIInstallConfirm([]string{"self", "install", "--json"}, env, newStub(), declinePath)
	if rcode != 0 {
		t.Fatalf("repair install exit=%d out=%s", rcode, rout)
	}
	var rep struct {
		Repaired []string `json:"repaired"`
	}
	_ = json.Unmarshal([]byte(rout), &rep)
	if _, err := os.Stat(inst.Executable); err != nil {
		t.Errorf("installed binary not recreated on re-run: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Repaired, " "), "binary") {
		t.Errorf("re-run repaired = %v (want a binary repair)", rep.Repaired)
	}
}

// TestSelfInstallHidden asserts `self install` is dispatchable but hidden — out
// of the machine catalog and the `self` group help, while the three visible
// subcommands are listed.
func TestSelfInstallHidden(t *testing.T) {
	home := t.TempDir()
	env := selfEnv(home)

	cout, _, _ := runCLI([]string{"commands", "--json"}, env, newStub())
	if strings.Contains(cout, `"self install"`) {
		t.Error("catalog must not list the hidden `self install`")
	}
	for _, want := range []string{`"self update"`, `"self uninstall"`, `"self doctor"`} {
		if !strings.Contains(cout, want) {
			t.Errorf("catalog missing %s", want)
		}
	}

	// The `self` group help lists the visible subcommands, not install. Match the
	// subcommand-line pattern (newline + indent + name) so the word "install"
	// inside "uninstall" / a summary doesn't false-positive.
	gout, _, _ := runCLI([]string{"self", "--help"}, env, newStub())
	if strings.Contains(gout, "\n  install") {
		t.Errorf("group help should not list the hidden install subcommand:\n%s", gout)
	}
	for _, want := range []string{"update", "uninstall", "doctor"} {
		if !strings.Contains(gout, want) {
			t.Errorf("group help missing %q", want)
		}
	}
}

// TestSelfUpdateRefusesDevBuild confirms a source (dev) build can't self update.
func TestSelfUpdateRefusesDevBuild(t *testing.T) {
	home := t.TempDir()
	_, errOut, code := runCLI([]string{"self", "update"}, selfEnv(home), newStub())
	if code != 4 {
		t.Fatalf("self update on dev build exit=%d (want 4) stderr=%s", code, errOut)
	}
}

// TestSelfUninstallRefusesJSON pins that `self uninstall` is interactive-only: a
// machine-output mode is a usage error (exit 2), decided before it touches the
// install — so an agent gets a clear refusal, not a half-done uninstall.
func TestSelfUninstallRefusesJSON(t *testing.T) {
	withVersion(t, "v9.9.9")
	home := t.TempDir()
	_, errOut, code := runCLI([]string{"self", "uninstall", "--json"}, selfEnv(home), newStub())
	if code != 2 {
		t.Fatalf("self uninstall --json exit=%d (want 2) stderr=%s", code, errOut)
	}
	// Assert on the JSON-specific message so this pins the --json gate (not the
	// confirmer-nil gate, whose message differs).
	if !strings.Contains(errOut, "--json/--compact") {
		t.Errorf("expected the --json/--compact usage error, got: %s", errOut)
	}
}

// TestSelfUninstallInjectedConfirmReachesOperation pins that an injected
// confirmer bypasses the TTY gate (no exit-2 refusal) and drives the uninstall
// operation itself — here refused with a provenance error (exit 4) because the
// test binary is not a managed install. It proves the confirm seam is wired.
func TestSelfUninstallInjectedConfirmReachesOperation(t *testing.T) {
	withVersion(t, "v9.9.9")
	home := t.TempDir()
	decline := func(string, bool) (bool, error) { return false, nil }
	_, errOut, code := runCLIConfirm([]string{"self", "uninstall"}, selfEnv(home), newStub(), decline)
	if code != 4 {
		t.Fatalf("self uninstall (injected confirm, unmanaged) exit=%d (want 4) stderr=%s", code, errOut)
	}
}

// TestSelfUninstallDoesNotDestroyBeforeManagedGate pins the safety invariant that
// the managed-install check runs BEFORE any question or key purge: on an
// unmanaged install, even a confirmer that accepts everything must not destroy
// the user's keys — the command refuses (exit 4) first.
func TestSelfUninstallDoesNotDestroyBeforeManagedGate(t *testing.T) {
	withVersion(t, "v9.9.9")
	home := t.TempDir()
	env := selfEnv(home)
	cliHome := env["KORBIT_CLI_HOME"]
	if err := os.MkdirAll(cliHome, 0o755); err != nil {
		t.Fatal(err)
	}
	m := keys.NewManager(cliHome, "file", func() int64 { return 1700000000000 }, nil)
	if _, err := m.Add("bot", "", ""); err != nil {
		t.Fatal(err)
	}

	acceptAll := func(string, bool) (bool, error) { return true, nil }
	_, _, code := runCLIConfirm([]string{"self", "uninstall"}, env, newStub(), acceptAll)
	if code != 4 {
		t.Fatalf("uninstall on an unmanaged install should refuse with exit 4, got %d", code)
	}
	// The key must survive — no purge may run before the managed gate.
	sums, err := keys.NewManager(cliHome, "file", func() int64 { return 1700000000000 }, nil).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Errorf("key was destroyed before the managed-install gate: %v", sums)
	}
}

// TestSelfUninstallDryRun pins that --dry-run runs the interactive flow (so it
// reflects the user's selections), reports what it WOULD do, and changes nothing
// — and that it works without a managed install (a plain build).
func TestSelfUninstallDryRun(t *testing.T) {
	withVersion(t, "v9.9.9")
	home := t.TempDir()
	env := selfEnv(home)
	cliHome := env["KORBIT_CLI_HOME"]
	if err := os.MkdirAll(cliHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliHome, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.NewManager(cliHome, "file", func() int64 { return 1700000000000 }, nil).Add("bot", "", ""); err != nil {
		t.Fatal(err)
	}
	zshrc := filepath.Join(home, ".zshrc")
	block := "# >>> korbit-cli >>>\nexport PATH=\"$HOME/.local/bin:$PATH\"\n# <<< korbit-cli <<<\n"
	if err := os.WriteFile(zshrc, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}

	acceptAll := func(string, bool) (bool, error) { return true, nil }
	out, _, code := runCLIConfirm([]string{"self", "uninstall", "--dry-run"}, env, newStub(), acceptAll)
	if code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, out)
	}
	for _, want := range []string{"Dry run — nothing was changed", "Would remove:", `API key "bot"`, "Would edit", ".zshrc"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	// It must not have touched anything.
	if _, err := os.Stat(filepath.Join(cliHome, "config.json")); err != nil {
		t.Errorf("dry-run removed config.json: %v", err)
	}
	if !strings.Contains(mustReadFile(t, zshrc), "# >>> korbit-cli >>>") {
		t.Error("dry-run edited .zshrc")
	}
}

func mustReadFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
