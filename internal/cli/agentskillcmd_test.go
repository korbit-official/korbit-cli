// Copyright (c) 2026 Digital X Co., Ltd.
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

	"github.com/digitalx-official/digitalx-cli/internal/cli"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

func skillFixture() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":                 {Data: []byte("---\nname: digitalx-cli\ndescription: drives dgx-cli\n---\n# skill\n")},
		"references/monitoring.md": {Data: []byte("monitoring\n")},
	}
}

// legacySkillFixture is a complete copy of THIS skill under the legacy skill
// name — frontmatter name, the tool it drives, the repository URL in the body,
// and the exact reference set — i.e. everything agentskill.Managed requires
// before install is allowed to remove it.
func legacySkillFixture() fstest.MapFS {
	fs := fstest.MapFS{
		"SKILL.md": {Data: []byte("---\nname: korbit\ndescription: through the korbit-cli tool\n---\n" +
			"# skill\n\nSource: https://github.com/digitalx-official/digitalx-cli\n")},
	}
	for _, r := range []string{"debugging.md", "funding.md", "monitoring.md", "sandbox.md"} {
		fs["references/"+r] = &fstest.MapFile{Data: []byte("playbook\n")}
	}
	return fs
}

// handWrittenSkillFixture is a skill somebody wrote themselves that happens to
// sit at one of our directory names AND names this CLI in its description — the
// shape closest to ours that is still not ours. It must never be removed.
func handWrittenSkillFixture() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md": {Data: []byte("---\nname: korbit\ndescription: through the korbit-cli tool\n---\nmy own trading notes\n")},
	}
}

// writeSkillDir materializes a skill fixture on disk at dir.
func writeSkillDir(t *testing.T, dir string, src fstest.MapFS) {
	t.Helper()
	for name, f := range src {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runSkillCLI drives the agent-skill commands with an injected embedded skill
// and a temp HOME, so installs land under the supported agent skill dirs.
func runSkillCLI(args []string, home string, src fs.FS) (string, string, int) {
	env := map[string]string{
		"HOME":              home,
		"USERPROFILE":       home, // windows
		"DIGITALX_CLI_HOME": filepath.Join(home, ".digitalx-cli"),
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
	if res.Skill != "digitalx-cli" || res.Scope != "user" || len(res.Installs) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	in := res.Installs[0]
	if in.Agent != "Claude" || in.Action != "created" || in.Files != 2 {
		t.Fatalf("install outcome: %+v", in)
	}
	want := filepath.Join(home, ".claude", "skills", "digitalx-cli")
	if in.Dir != want {
		t.Fatalf("dir = %q, want %q", in.Dir, want)
	}
	if b, err := os.ReadFile(filepath.Join(want, "SKILL.md")); err != nil || !strings.Contains(string(b), "name: digitalx-cli") {
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
		if _, err := os.Stat(filepath.Join(home, base, "skills", "digitalx-cli", "SKILL.md")); err != nil {
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
	if _, err := os.Stat(filepath.Join(proj, ".agents", "skills", "digitalx-cli", "SKILL.md")); err != nil {
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
	tampered := filepath.Join(home, ".claude", "skills", "digitalx-cli", "SKILL.md")
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

func TestAgentSkillDoctorChecksSkillCommandEvenWhenInvokedUnderAnotherName(t *testing.T) {
	home := t.TempDir()
	path := t.TempDir()
	t.Setenv("PATH", path)
	// Restore whatever the process name was, rather than assuming the default:
	// hard-coding a name here silently rewrites the default for every test that
	// runs after this one.
	prevProgname := progname.Name()
	progname.Set("digitalx-cli")
	t.Cleanup(func() { progname.Set(prevProgname) })

	if err := os.WriteFile(filepath.Join(path, "digitalx-cli"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	if code != 4 {
		t.Fatalf("doctor exit=%d, want 4 (the skill's command is missing on PATH)", code)
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
	if rep.Binary != "dgx-cli" || rep.OnPath || !strings.Contains(rep.PathFix, `"dgx-cli"`) {
		t.Fatalf("doctor checked wrong binary: %+v", rep)
	}
	// The executable's own invoked name is checked and flagged: it's "digitalx-cli",
	// not the "dgx-cli" the skill shells out to, and the fix names both.
	if rep.InvokedAs != "digitalx-cli" || rep.NameMatches {
		t.Fatalf("name check wrong: invokedAs=%q nameMatches=%v", rep.InvokedAs, rep.NameMatches)
	}
	if !strings.Contains(rep.NameFix, "ln -s") || !strings.Contains(rep.NameFix, "/dgx-cli") {
		t.Fatalf("nameFix should suggest symlinking a %q onto PATH: %q", "dgx-cli", rep.NameFix)
	}
}

// A binary invoked under a different name is only a real problem when the
// "dgx-cli" command the skill calls isn't reachable. If a correctly-named
// "dgx-cli" IS on PATH, the skill works: doctor reports the name mismatch
// informationally (nameMatches=false) but exits 0.
func TestAgentSkillDoctorNameMismatchIsHealthyWhenSkillCommandOnPath(t *testing.T) {
	home := t.TempDir()
	path := t.TempDir()
	t.Setenv("PATH", path)
	// Restore whatever the process name was, rather than assuming the default:
	// hard-coding a name here silently rewrites the default for every test that
	// runs after this one.
	prevProgname := progname.Name()
	progname.Set("digitalx-cli")
	t.Cleanup(func() { progname.Set(prevProgname) })

	// A proper "dgx-cli" command exists on PATH.
	if err := os.WriteFile(filepath.Join(path, "dgx-cli"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude"}, home, skillFixture()); code != 0 {
		t.Fatalf("install exit=%d", code)
	}
	out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("doctor exit=%d, want 0 (the skill's command is reachable on PATH)\n%s", code, out)
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
	if !strings.Contains(out, "Installed the \"digitalx-cli\" Agent Skill") || !strings.Contains(out, "Claude") {
		t.Fatalf("human output unexpected:\n%s", out)
	}
}

// TestAgentSkillInstallClearsLegacyNamedCopy: an agent that has this skill
// installed under the legacy directory name must be left with exactly one copy
// of it — two directories carry the same triggers, so the old one goes as soon
// as the new one is safely written.
func TestAgentSkillInstallClearsLegacyNamedCopy(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".claude", "skills", "korbit")
	writeSkillDir(t, legacy, legacySkillFixture())

	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var res struct {
		Installs []struct{ Dir, LegacyRemoved string } `json:"installs"`
		Warnings []string                              `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(res.Installs) != 1 || res.Installs[0].LegacyRemoved != legacy {
		t.Fatalf("legacyRemoved not reported: %+v", res.Installs)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, legacy) {
			t.Fatalf("removing our own copy is not a warning: %v", res.Warnings)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy skill dir still present: %v", err)
	}
	// The new copy is on disk.
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "digitalx-cli", "SKILL.md")); err != nil {
		t.Fatalf("skill not installed under the current name: %v", err)
	}
}

// TestAgentSkillInstallLeavesForeignSkillAlone: a skill somebody else wrote
// that happens to sit at the legacy directory name is never deleted — install
// says so and moves on.
func TestAgentSkillInstallLeavesForeignSkillAlone(t *testing.T) {
	home := t.TempDir()
	foreign := filepath.Join(home, ".claude", "skills", "korbit")
	writeSkillDir(t, foreign, handWrittenSkillFixture())

	out, _, code := runSkillCLI([]string{"agent", "skill", "install", "--claude", "--json"}, home, skillFixture())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var res struct {
		Installs []struct{ LegacyRemoved string } `json:"installs"`
		Warnings []string                         `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if res.Installs[0].LegacyRemoved != "" {
		t.Fatalf("a foreign skill must not be reported as removed: %+v", res.Installs)
	}
	if b, err := os.ReadFile(filepath.Join(foreign, "SKILL.md")); err != nil || !strings.Contains(string(b), "my own trading notes") {
		t.Fatalf("a foreign skill must be left untouched: %v %q", err, b)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, foreign) {
			found = true
		}
	}
	if !found {
		t.Fatalf("install should report the directory it left alone: %v", res.Warnings)
	}
}

// TestAgentSkillDoctorReportsLegacyNamedCopy: doctor names a legacy-named copy
// of this skill, points at the install that clears it, and exits 4 — an agent
// loading two copies of the same skill is a problem worth fixing. A foreign
// skill at that name is reported informationally instead.
func TestAgentSkillDoctorReportsLegacyNamedCopy(t *testing.T) {
	path := t.TempDir()
	t.Setenv("PATH", path)
	if err := os.WriteFile(filepath.Join(path, "dgx-cli"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	type target struct{ AgentID, Status, LegacyDir, LegacyStatus, LegacyNote string }
	report := func(t *testing.T, home string) (map[string]target, int) {
		t.Helper()
		out, _, code := runSkillCLI([]string{"agent", "skill", "doctor", "--json"}, home, skillFixture())
		var rep struct {
			Targets []target `json:"targets"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		byID := map[string]target{}
		for _, tg := range rep.Targets {
			byID[tg.AgentID] = tg
		}
		return byID, code
	}

	t.Run("ours", func(t *testing.T) {
		home := t.TempDir()
		legacy := filepath.Join(home, ".claude", "skills", "korbit")
		writeSkillDir(t, legacy, legacySkillFixture())
		byID, code := report(t, home)
		if code != 4 {
			t.Fatalf("doctor exit=%d, want 4 (a legacy-named copy is live)", code)
		}
		tg := byID["claude"]
		if tg.LegacyDir != legacy || tg.LegacyStatus != "managed" {
			t.Fatalf("legacy copy not reported: %+v", tg)
		}
		if !strings.Contains(tg.LegacyNote, "agent skill install") {
			t.Fatalf("legacy note should name the fix: %q", tg.LegacyNote)
		}
	})

	// An agent that isn't on this machine: Codex's skills dir holds a legacy
	// copy, but ~/.codex doesn't exist, so the target is "n/a". Report the
	// directory, don't fail the run — there is no agent loading it.
	t.Run("agent not present", func(t *testing.T) {
		home := t.TempDir()
		legacy := filepath.Join(home, ".agents", "skills", "korbit")
		writeSkillDir(t, legacy, legacySkillFixture())
		byID, code := report(t, home)
		if code != 0 {
			t.Fatalf("doctor exit=%d, want 0 (the agent is not installed here)", code)
		}
		tg := byID["codex"]
		if tg.Status != "n/a" {
			t.Fatalf("codex status = %q, want n/a", tg.Status)
		}
		if tg.LegacyDir != legacy || tg.LegacyStatus != "managed" || tg.LegacyNote == "" {
			t.Fatalf("the legacy copy must still be reported: %+v", tg)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		home := t.TempDir()
		foreign := filepath.Join(home, ".claude", "skills", "korbit")
		writeSkillDir(t, foreign, handWrittenSkillFixture())
		byID, code := report(t, home)
		if code != 0 {
			t.Fatalf("doctor exit=%d, want 0 (someone else's skill is not our problem)", code)
		}
		if tg := byID["claude"]; tg.LegacyStatus != "foreign" || tg.LegacyDir != foreign {
			t.Fatalf("foreign copy not reported: %+v", tg)
		}
	})
}
