// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package agentskill

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func sampleSkill() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":                 {Data: []byte("# korbit\nfrontmatter\n")},
		"references/monitoring.md": {Data: []byte("monitoring\n")},
		"references/funding.md":    {Data: []byte("funding\n")},
	}
}

func TestContentHashStableAndSensitive(t *testing.T) {
	a := sampleSkill()
	h1, err := ContentHash(a)
	if err != nil {
		t.Fatal(err)
	}
	// Re-hashing the same tree is stable.
	h2, _ := ContentHash(sampleSkill())
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	// A content change moves the hash.
	b := sampleSkill()
	b["SKILL.md"] = &fstest.MapFile{Data: []byte("# korbit\nchanged\n")}
	if hb, _ := ContentHash(b); hb == h1 {
		t.Fatal("hash unchanged after content edit")
	}
	// An added file moves the hash.
	c := sampleSkill()
	c["references/new.md"] = &fstest.MapFile{Data: []byte("new\n")}
	if hc, _ := ContentHash(c); hc == h1 {
		t.Fatal("hash unchanged after adding a file")
	}
}

func TestInstallCreatesUpdatesAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills", "korbit")
	src := sampleSkill()

	out, err := Install(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != "created" {
		t.Fatalf("first install action = %q, want created", out.Action)
	}
	if out.Files != 3 {
		t.Fatalf("files = %d, want 3", out.Files)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "references", "monitoring.md")); string(got) != "monitoring\n" {
		t.Fatalf("monitoring.md content = %q", got)
	}

	// Re-install identical content → unchanged, on-disk hash matches embedded.
	out2, err := Install(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Action != "unchanged" {
		t.Fatalf("second install action = %q, want unchanged", out2.Action)
	}
	installed, hash, err := InspectDir(dir)
	if err != nil || !installed {
		t.Fatalf("InspectDir: installed=%v err=%v", installed, err)
	}
	if want, _ := ContentHash(src); hash != want {
		t.Fatalf("installed hash %s != embedded %s", hash, want)
	}

	// Changed content → updated.
	src["SKILL.md"] = &fstest.MapFile{Data: []byte("# korbit\nv2\n")}
	out3, err := Install(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out3.Action != "updated" {
		t.Fatalf("third install action = %q, want updated", out3.Action)
	}
}

func TestInstallPrunesStaleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "korbit")
	if _, err := Install(sampleSkill(), dir); err != nil {
		t.Fatal(err)
	}
	// A file left behind by an older skill version (renamed/removed since).
	stale := filepath.Join(dir, "references", "old.md")
	if err := os.WriteFile(stale, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := Install(sampleSkill(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Pruned) != 1 || out.Pruned[0] != "references/old.md" {
		t.Fatalf("pruned = %v, want [references/old.md]", out.Pruned)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file still present: %v", err)
	}
}

func TestInspectDirMissing(t *testing.T) {
	installed, hash, err := InspectDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if installed || hash != "" {
		t.Fatalf("missing dir: installed=%v hash=%q", installed, hash)
	}
}

func TestAgentDirHelpers(t *testing.T) {
	claude, ok := AgentByID("claude")
	if !ok {
		t.Fatal("claude agent not found")
	}
	codex, ok := AgentByID("codex")
	if !ok {
		t.Fatal("codex agent not found")
	}
	root := "/home/u"
	if got := claude.SkillDir(root); got != filepath.Join(root, ".claude", "skills", "korbit") {
		t.Fatalf("Claude SkillDir = %q", got)
	}
	if got := claude.ConfigRoot(root); got != filepath.Join(root, ".claude") {
		t.Fatalf("Claude ConfigRoot = %q", got)
	}
	if got := codex.SkillDir(root); got != filepath.Join(root, ".agents", "skills", "korbit") {
		t.Fatalf("Codex SkillDir = %q", got)
	}
	if got := codex.ConfigRoot(root); got != filepath.Join(root, ".codex") {
		t.Fatalf("Codex ConfigRoot = %q", got)
	}
}
