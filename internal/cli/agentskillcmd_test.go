// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/progname"
)

func skillFixture() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":                 {Data: []byte("---\nname: korbit\n---\n# skill\n")},
		"references/monitoring.md": {Data: []byte("monitoring\n")},
	}
}

// runSkillCLI drives the agent-skill commands with an injected embedded skill
// and a temp HOME, so installs land under the supported agent skill dirs.
func runSkillCLI(args []string, home string, src fs.FS) (string, string, int) {
	env := map[string]string{
		"HOME":            home,
		"USERPROFILE":     home, // windows
		"KORBIT_CLI_HOME": filepath.Join(home, ".korbit-cli"),
	}
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:  func(k string) string { return env[k] },
		Stdout:  &out,
		Stderr:  &errb,
		Now:     func() int64 { return 1700000000000 },
		SkillFS: src,
	})
	return out.String(), errb.String(), code
}

func TestAgentSkillInstallUserScope(t *testing.T) {
	home := t.TempDir()
	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var res struct {
		Skill    string `json:"skill"`
		Scope    string `json:"scope"`
		Installs []struct {
			Agent, Dir, Action string
			Files              int
		} `json:"installs"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if res.Skill != "korbit" || res.Scope != "user" || len(res.Installs) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	in := res.Installs[0]
	if in.Agent != "Claude" || in.Action != "created" || in.Files != 2 {
		t.Fatalf("install outcome: %+v", in)
	}
	want := filepath.Join(home, ".claude", "skills", "korbit")
	if in.Dir != want {
		t.Fatalf("dir = %q, want %q", in.Dir, want)
	}
	if b, err := os.ReadFile(filepath.Join(want, "SKILL.md")); err != nil || !strings.Contains(string(b), "name: korbit") {
		t.Fatalf("SKILL.md not written: %v %q", err, b)
	}
}

func TestAgentSkillInstallAll(t *testing.T) {
	home := t.TempDir()
	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--all", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var res struct {
		Installs []struct{ AgentID string } `json:"installs"`
	}
	_ = json.Unmarshal([]byte(out), &res)
	if len(res.Installs) != 2 {
		t.Fatalf("want 2 installs, got %d", len(res.Installs))
	}
	for _, base := range []string{".claude", ".agents"} {
		if _, err := os.Stat(filepath.Join(home, base, "skills", "korbit", "SKILL.md")); err != nil {
			t.Fatalf("%s skill not installed: %v", base, err)
		}
	}
}

func TestAgentSkillInstallPathWarningIsInResult(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	out, errb, code := runSkillCLI([]string{"agent", "skill", "install", "--claude", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s stderr=%s", code, out, errb)
	}
	var res struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "PATH") {
		t.Fatalf("PATH warning must be carried in stdout: %+v (%s)", res.Warnings, out)
	}
	if strings.TrimSpace(errb) != "" {
		t.Fatalf("PATH warning must not be duplicated on stderr: %s", errb)
	}
}

func TestAgentSkillInstallNoTargetIsUsageError(t *testing.T) {
	_, _, code := runSkillCLI([]string{"agent", "skill", "install"}, t.TempDir(), skillFixture())
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (usage)", code)
	}
}

func TestAgentSkillInstallProjectScope(t *testing.T) {
	// --project installs under the working directory; run it from a temp cwd.
	proj := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--codex", "--project", "--json"}, t.TempDir(), skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(proj, ".agents", "skills", "korbit", "SKILL.md")); err != nil {
		t.Fatalf("project skill not installed: %v", err)
	}
}

func TestAgentSkillDoctorReportsStatuses(t *testing.T) {
	home := t.TempDir()
	// Install for Claude only; Codex isn't present at all.
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	out, _, _ := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	var rep struct {
		EmbeddedHash string `json:"embeddedHash"`
		Targets      []struct {
			AgentID, Status, InstalledHash string
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	byID := map[string]string{}
	for _, tg := range rep.Targets {
		byID[tg.AgentID] = tg.Status
	}
	if byID["claude"] != "ok" {
		t.Fatalf("claude status = %q, want ok", byID["claude"])
	}
	if byID["codex"] != "n/a" {
		t.Fatalf("codex status = %q, want n/a (not detected)", byID["codex"])
	}
}

func TestAgentSkillDoctorFlagsStaleInstall(t *testing.T) {
	home := t.TempDir()
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	// Tamper with the installed copy so its hash no longer matches the embedded skill.
	tampered := filepath.Join(home, ".claude", "skills", "korbit", "SKILL.md")
	if err := os.WriteFile(tampered, []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	if code != 4 {
		t.Fatalf("doctor exit=%d, want 4 (stale)", code)
	}
	var rep struct {
		Targets []struct{ AgentID, Status string } `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	var claudeStatus string
	for _, tg := range rep.Targets {
		if tg.AgentID == "claude" {
			claudeStatus = tg.Status
		}
	}
	if claudeStatus != "stale" {
		t.Fatalf("claude status = %q, want stale", claudeStatus)
	}
}

func TestAgentSkillDoctorChecksKorbitCommandEvenWhenInvokedUnderAnotherName(t *testing.T) {
	home := t.TempDir()
	path := t.TempDir()
	t.Setenv("PATH", path)
	progname.Set("korbit-cli")
	t.Cleanup(func() { progname.Set("korbit") })

	if err := os.WriteFile(filepath.Join(path, "korbit-cli"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	if code != 4 {
		t.Fatalf("doctor exit=%d, want 4 (korbit missing on PATH)", code)
	}
	var rep struct {
		Binary      string `json:"binary"`
		InvokedAs   string `json:"invokedAs"`
		NameMatches bool   `json:"nameMatches"`
		NameFix     string `json:"nameFix"`
		OnPath      bool   `json:"onPath"`
		PathFix     string `json:"pathFix"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if rep.Binary != "korbit" || rep.OnPath || !strings.Contains(rep.PathFix, `"korbit"`) {
		t.Fatalf("doctor checked wrong binary: %+v", rep)
	}
	// The executable's own invoked name is checked and flagged: it's "korbit-cli",
	// not the "korbit" the skill shells out to, and the fix names both.
	if rep.InvokedAs != "korbit-cli" || rep.NameMatches {
		t.Fatalf("name check wrong: invokedAs=%q nameMatches=%v", rep.InvokedAs, rep.NameMatches)
	}
	if !strings.Contains(rep.NameFix, "ln -s") || !strings.Contains(rep.NameFix, "/korbit") {
		t.Fatalf("nameFix should suggest symlinking a %q onto PATH: %q", "korbit", rep.NameFix)
	}
}

// A binary invoked under a different name is only a real problem when the
// "korbit" command the skill calls isn't reachable. If a correctly-named
// "korbit" IS on PATH, the skill works: doctor reports the name mismatch
// informationally (nameMatches=false) but exits 0.
func TestAgentSkillDoctorNameMismatchIsHealthyWhenKorbitOnPath(t *testing.T) {
	home := t.TempDir()
	path := t.TempDir()
	t.Setenv("PATH", path)
	progname.Set("korbit-cli")
	t.Cleanup(func() { progname.Set("korbit") })

	// A proper "korbit" command exists on PATH.
	if err := os.WriteFile(filepath.Join(path, "korbit"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("doctor exit=%d, want 0 (korbit reachable on PATH)\n%s", code, out)
	}
	var rep struct {
		NameMatches bool `json:"nameMatches"`
		OnPath      bool `json:"onPath"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if rep.NameMatches || !rep.OnPath {
		t.Fatalf("want nameMatches=false, onPath=true; got %+v", rep)
	}
}

func TestAgentSkillInstallHumanOutput(t *testing.T) {
	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, t.TempDir(), skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "Installed the \"korbit\" Agent Skill") || !strings.Contains(out, "Claude") {
		t.Fatalf("human output unexpected:\n%s", out)
	}
}
