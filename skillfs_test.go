// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/agentskill"
)

// The bundled skill and the code that installs it must agree. The skill system
// keys a skill on its directory name and its frontmatter `name:`, and the skill
// body tells the agent which command to shell out to — so all three have to
// match agentskill's constants, and none of them can be checked by the content
// hash (which only proves an installed copy equals the embedded one).

// skillFiles reads every file of the embedded skill, keyed by its path.
func skillFiles(t *testing.T) map[string]string {
	t.Helper()
	src := skillSource()
	out := map[string]string{}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the embedded skill is empty")
	}
	return out
}

// TestEmbeddedSkillDirMatchesSkillName: the directory the skill is embedded from
// is the directory it installs to, so `agent skill install` writes a skill whose
// frontmatter name matches its own directory name (what the agent keys on).
func TestEmbeddedSkillDirMatchesSkillName(t *testing.T) {
	if _, err := fs.Stat(embeddedSkills, filepath.ToSlash(filepath.Join("skills", agentskill.SkillName))); err != nil {
		t.Fatalf("the embedded skill must live at skills/%s: %v", agentskill.SkillName, err)
	}
}

// TestEmbeddedSkillFrontmatterName pins the frontmatter `name:` to SkillName.
func TestEmbeddedSkillFrontmatterName(t *testing.T) {
	skill := skillFiles(t)["SKILL.md"]
	want := "\nname: " + agentskill.SkillName + "\n"
	if !strings.Contains(skill, want) {
		head := skill
		if len(head) > 200 {
			head = head[:200]
		}
		t.Fatalf("SKILL.md frontmatter must declare %q:\n%s", "name: "+agentskill.SkillName, head)
	}
}

// TestEmbeddedSkillIsRecognizedAsOurs: an installed copy of the skill we ship
// must satisfy the ownership proof — that proof is what lets install clear a
// copy of this same skill sitting under its legacy directory name.
func TestEmbeddedSkillIsRecognizedAsOurs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", agentskill.LegacySkillName)
	if _, err := agentskill.Install(skillSource(), dir); err != nil {
		t.Fatal(err)
	}
	if !agentskill.Managed(dir) {
		t.Fatal("the skill this binary ships must be recognized as ours")
	}
}

// TestEmbeddedSkillInvokesSkillBinary: every command example in the skill runs
// the command the CLI reports as the skill's binary. A stale invocation is a
// live defect — the agent would shell out to a command a fresh install does not
// place on PATH.
func TestEmbeddedSkillInvokesSkillBinary(t *testing.T) {
	// An invocation is the legacy name at the start of a line (a shell example)
	// or straight after a backtick (an inline command), followed by a word.
	legacy := regexp.MustCompile("(?m)(?:^|`)" + regexp.QuoteMeta(agentskill.LegacySkillName) + `[ \t]+[a-z-]`)
	files := skillFiles(t)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if m := legacy.FindString(files[p]); m != "" {
			t.Errorf("%s invokes %q (%q) — the skill must invoke %q; name the other command in prose, not as a command example",
				p, agentskill.LegacySkillName, strings.TrimSpace(m), agentskill.SkillBinary)
		}
	}
	// And the command it does invoke is the one doctor checks for on PATH.
	if !strings.Contains(files["SKILL.md"], agentskill.SkillBinary+" ") {
		t.Errorf("SKILL.md must invoke %q", agentskill.SkillBinary)
	}
}

// maxSkillDescription is the Agent Skills limit on a skill's frontmatter
// description. The description is the whole trigger surface — an agent decides
// whether to load the skill from it — so an over-long one is not a cosmetic
// problem: the skill can be rejected or truncated, and the triggers past the cut
// stop working.
const maxSkillDescription = 1024

// TestEmbeddedSkillDescriptionFitsTheLimit measures the description the way YAML
// hands it to the skill loader (a folded scalar: line breaks become spaces), not
// the raw block.
func TestEmbeddedSkillDescriptionFitsTheLimit(t *testing.T) {
	desc, ok := foldedDescription(skillFiles(t)["SKILL.md"])
	if !ok {
		t.Fatal("SKILL.md frontmatter has no folded `description: >-` block")
	}
	if n := len([]rune(desc)); n > maxSkillDescription {
		t.Errorf("description is %d characters, over the %d limit — trim it:\n%s", n, maxSkillDescription, desc)
	}
	// A description that lost its trigger words would pass the limit and fail the
	// job. These are the names a user can call the exchange and the tool by — the
	// exchange's earlier name included, because a user still types it and the
	// trigger has to match what they type, not what the product is called now.
	for _, trigger := range []string{"Digital X", "디지털엑스", "Korbit", "코빗", "dgx-cli", agentskill.SkillName} {
		if !strings.Contains(desc, trigger) {
			t.Errorf("the description must still trigger on %q", trigger)
		}
	}
}

// foldedDescription returns the frontmatter's `description: >-` block folded to
// one line, the form the skill loader sees.
func foldedDescription(skill string) (string, bool) {
	lines := strings.Split(skill, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "description: >-" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var parts []string
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "  ") { // the block ends at the next key or the closing ---
			break
		}
		parts = append(parts, strings.TrimSpace(l))
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " "), true
}
