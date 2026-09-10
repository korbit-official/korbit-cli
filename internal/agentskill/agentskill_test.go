// Copyright (c) 2026 Digital X Co., Ltd.
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
		"SKILL.md":                 {Data: []byte("---\nname: digitalx-cli\ndescription: drives dgx-cli\n---\n# skill\n")},
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
	b["SKILL.md"] = &fstest.MapFile{Data: []byte("# skill\nchanged\n")}
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
	dir := filepath.Join(t.TempDir(), ".claude", "skills", SkillName)
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
	src["SKILL.md"] = &fstest.MapFile{Data: []byte("# skill\nv2\n")}
	out3, err := Install(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out3.Action != "updated" {
		t.Fatalf("third install action = %q, want updated", out3.Action)
	}
}

func TestInstallPrunesStaleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", SkillName)
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
	if got := claude.SkillDir(root); got != filepath.Join(root, ".claude", "skills", "digitalx-cli") {
		t.Fatalf("Claude SkillDir = %q", got)
	}
	if got := claude.LegacySkillDir(root); got != filepath.Join(root, ".claude", "skills", "korbit") {
		t.Fatalf("Claude LegacySkillDir = %q", got)
	}
	if got := claude.ConfigRoot(root); got != filepath.Join(root, ".claude") {
		t.Fatalf("Claude ConfigRoot = %q", got)
	}
	if got := codex.SkillDir(root); got != filepath.Join(root, ".agents", "skills", "digitalx-cli") {
		t.Fatalf("Codex SkillDir = %q", got)
	}
	if got := codex.LegacySkillDir(root); got != filepath.Join(root, ".agents", "skills", "korbit") {
		t.Fatalf("Codex LegacySkillDir = %q", got)
	}
	if got := codex.ConfigRoot(root); got != filepath.Join(root, ".codex") {
		t.Fatalf("Codex ConfigRoot = %q", got)
	}
}

// ourSkillMD is a SKILL.md carrying every proof Managed requires: one of our
// skill names and a command we drive in the frontmatter, and this CLI's
// repository URL in the body.
func ourSkillMD(name string) string {
	return "---\nname: " + name + "\ndescription: drives " + SkillBinary +
		"\n---\n# skill\n\nSource: https://" + managedRepoMarker + "\n"
}

// writeOurCopy materializes a complete copy of the skill (SKILL.md plus the
// exact reference set) under a fresh directory, then applies edits: a nil value
// deletes that path, a non-nil one replaces its content.
func writeOurCopy(t *testing.T, name string, edits map[string]*string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{skillFile: ourSkillMD(name)}
	for _, r := range managedReferences {
		files[guideRefDir+"/"+r] = "playbook\n"
	}
	for p, v := range edits {
		if v == nil {
			delete(files, p)
			continue
		}
		files[p] = *v
	}
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ptr(s string) *string { return &s }

// TestManagedRecognizesOurCopiesOnly is the ownership proof Install relies on
// before it removes a directory it did not write. A copy of THIS skill under
// either of its names is ours; a directory that satisfies only some of the three
// proofs is not, because the cost of a false positive is deleting someone's
// work.
func TestManagedRecognizesOurCopiesOnly(t *testing.T) {
	noRefs := map[string]*string{}
	for _, r := range managedReferences {
		noRefs[guideRefDir+"/"+r] = nil
	}

	cases := []struct {
		name  string
		skill string
		edits map[string]*string
		want  bool
	}{
		{name: "current name", skill: SkillName, want: true},
		{name: "legacy name", skill: LegacySkillName, want: true},
		{
			name:  "quoted frontmatter name",
			skill: LegacySkillName,
			edits: map[string]*string{skillFile: ptr("---\nname: \"korbit\"\ndescription: the korbit-cli tool\n---\nsee https://" + managedRepoMarker + "\n")},
			want:  true,
		},
		// The shape a hand-written skill about this CLI has: it names the tool in
		// its description, and nothing else. Naming the tool is not owning the
		// directory — this must survive untouched.
		{
			name:  "hand-written skill naming this CLI",
			skill: LegacySkillName,
			edits: mergeEdits(noRefs, map[string]*string{skillFile: ptr("---\nname: korbit\ndescription: through the korbit-cli tool\n---\nmy own notes\n")}),
			want:  false,
		},
		// Frontmatter alone, with our reference set absent.
		{
			name:  "frontmatter only, no references",
			skill: SkillName,
			edits: noRefs,
			want:  false,
		},
		// Frontmatter and references, but the body does not cite the repository.
		{
			name:  "body does not cite the repository",
			skill: SkillName,
			edits: map[string]*string{skillFile: ptr("---\nname: " + SkillName + "\ndescription: drives " + SkillBinary + "\n---\nno source link\n")},
			want:  false,
		},
		// A reference file we do not ship.
		{
			name:  "extra reference file",
			skill: SkillName,
			edits: map[string]*string{guideRefDir + "/mine.md": ptr("mine\n")},
			want:  false,
		},
		// One of ours missing.
		{
			name:  "reference file missing",
			skill: SkillName,
			edits: map[string]*string{guideRefDir + "/funding.md": nil},
			want:  false,
		},
		{
			name:  "another skill entirely",
			skill: "something-else",
			want:  false,
		},
		{
			name:  "no frontmatter",
			skill: SkillName,
			edits: map[string]*string{skillFile: ptr("# just markdown naming " + SkillBinary + " and https://" + managedRepoMarker + "\n")},
			want:  false,
		},
		{
			name:  "unterminated frontmatter",
			skill: SkillName,
			edits: map[string]*string{skillFile: ptr("---\nname: " + SkillName + "\ndescription: " + SkillBinary + "\nhttps://" + managedRepoMarker + "\n")},
			want:  false,
		},
		// An indented `name:` belongs to some other mapping, not the skill.
		{
			name:  "nested name key",
			skill: SkillName,
			edits: map[string]*string{skillFile: ptr("---\nmeta:\n  name: " + SkillName + "\ndescription: " + SkillBinary + "\n---\nhttps://" + managedRepoMarker + "\n")},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeOurCopy(t, tc.skill, tc.edits)
			if got := Managed(dir); got != tc.want {
				t.Fatalf("Managed = %v, want %v", got, tc.want)
			}
		})
	}
	// A directory with no SKILL.md at all, and a path that does not exist.
	if Managed(t.TempDir()) {
		t.Error("a directory with no SKILL.md is not ours")
	}
	if Managed(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing directory is not ours")
	}
}

func mergeEdits(a, b map[string]*string) map[string]*string {
	out := make(map[string]*string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TestManagedAcceptsAnInstalledCopy: whatever Install writes must satisfy the
// ownership proof — otherwise install could never clear a copy of its own skill
// sitting under the legacy directory name.
func TestManagedAcceptsAnInstalledCopy(t *testing.T) {
	src := fstest.MapFS{skillFile: {Data: []byte(ourSkillMD(SkillName))}}
	for _, r := range managedReferences {
		src[guideRefDir+"/"+r] = &fstest.MapFile{Data: []byte("playbook\n")}
	}
	dir := filepath.Join(t.TempDir(), "skills", LegacySkillName)
	if _, err := Install(src, dir); err != nil {
		t.Fatal(err)
	}
	if !Managed(dir) {
		t.Fatal("an installed copy of this skill must be recognized as ours")
	}
}
