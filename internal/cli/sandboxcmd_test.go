// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The sandbox start/exec paths spawn a runtime and are covered end-to-end in
// internal/sandbox with a fake-runtime stub (and the env-gated live e2e). At the
// CLI layer the runtime-FREE paths are exercised here through Execute: status
// (pure inspection), stop (pidfile read), and runtime status (no network).

func sandboxEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"DIGITALX_CLI_HOME":          t.TempDir(),
		"DIGITALX_CLI_SANDBOX_CACHE": t.TempDir(),
	}
}

func TestSandboxStatusNoServer(t *testing.T) {
	out, errb, code := runCLI([]string{"sandbox", "status", "--json"}, sandboxEnv(t), nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	var st struct {
		BundleCached bool `json:"bundleCached"`
		Server       struct {
			Running bool `json:"running"`
		} `json:"server"`
		KeyImported bool   `json:"keyImported"`
		Runtime     string `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status json: %v (%s)", err, out)
	}
	if st.BundleCached || st.Server.Running || st.KeyImported {
		t.Errorf("fresh status should be empty: %+v", st)
	}
	if st.Runtime != "deno" && st.Runtime != "managed-deno" {
		t.Errorf("runtime should be deno or managed-deno, got %q", st.Runtime)
	}
}

func TestSandboxStatusHuman(t *testing.T) {
	out, errb, code := runCLI([]string{"sandbox", "status"}, sandboxEnv(t), nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, "bundle") || !strings.Contains(out, "server") {
		t.Errorf("human status missing fields:\n%s", out)
	}
}

func TestSandboxStopNothingRunning(t *testing.T) {
	out, errb, code := runCLI([]string{"sandbox", "stop", "--json"}, sandboxEnv(t), nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	var r struct {
		Stopped bool `json:"stopped"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stop json: %v", err)
	}
	if r.Stopped {
		t.Error("nothing should be stopped on a fresh home")
	}
}

func TestSandboxRuntimeStatusNoNetwork(t *testing.T) {
	out, errb, code := runCLI([]string{"sandbox", "runtime", "status", "--json"}, sandboxEnv(t), nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	var st struct {
		Installed     bool   `json:"installed"`
		PinnedVersion string `json:"pinnedVersion"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("runtime status json: %v (%s)", err, out)
	}
	if st.Installed {
		t.Error("nothing should be installed in a fresh cache")
	}
	if !strings.HasPrefix(st.PinnedVersion, "v") {
		t.Errorf("pinned version should be a vX.Y.Z tag, got %q", st.PinnedVersion)
	}
}

func TestSandboxRuntimeUnknownSubcommand(t *testing.T) {
	_, stderr, code := runCLI([]string{"sandbox", "runtime", "frobnicate"}, sandboxEnv(t), nil)
	if code != 2 {
		t.Errorf("an unknown runtime subcommand should be a usage error (exit 2), got %d", code)
	}
	if !strings.Contains(stderr, "sandbox runtime frobnicate") || !strings.Contains(stderr, "status, install, update") {
		t.Errorf("expected nested unknown-subcommand error listing the valid set, got: %s", stderr)
	}
}

// TestSandboxRuntimeBarePrintsHelp pins that the nested `sandbox runtime` group,
// invoked bare, prints its subcommand help (not an action) and exits 2 — so the
// status/install/update subcommands are discoverable.
func TestSandboxRuntimeBarePrintsHelp(t *testing.T) {
	stdout, stderr, code := runCLI([]string{"sandbox", "runtime"}, sandboxEnv(t), nil)
	if code != 2 {
		t.Fatalf("bare `sandbox runtime` should exit 2, got %d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("bare group must not emit an error, got stderr: %s", stderr)
	}
	for _, want := range []string{"sandbox runtime", "Subcommands:", "status", "install", "update"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("bare `sandbox runtime` help missing %q, got: %s", want, stdout)
		}
	}
}

// A bare `sandbox exec` (no tail) prints this command's help and exits 2 — the
// incomplete-command signal, shown helpfully (mirrors the bare-group/bare-root
// behavior). --help is NOT intercepted here; it forwards to the bundle.
func TestSandboxExecBarePrintsHelp(t *testing.T) {
	stdout, _, code := runCLI([]string{"sandbox", "exec"}, sandboxEnv(t), nil)
	if code != 2 {
		t.Errorf("bare `sandbox exec` should exit 2, got %d", code)
	}
	if !strings.Contains(stdout, "sandbox exec") || !strings.Contains(stdout, "Usage:") {
		t.Errorf("bare `sandbox exec` should print command help on stdout, got: %s", stdout)
	}
}

// `sandbox exec --` with nothing after the pass-through separator is a usage
// error (exit 2) — caught before any runtime resolution, so it's runtime-free.
func TestSandboxExecDashDashNeedsSubcommand(t *testing.T) {
	_, stderr, code := runCLI([]string{"sandbox", "exec", "--"}, sandboxEnv(t), nil)
	if code != 2 {
		t.Errorf("`sandbox exec --` should be a usage error (exit 2), got %d", code)
	}
	if !strings.Contains(stderr, "needs a bundle subcommand") {
		t.Errorf("expected needs-a-subcommand usage error, got: %s", stderr)
	}
}
