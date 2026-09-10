// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package agentskill

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// This file answers one question: is the skill directory at this path a copy of
// OUR skill, or somebody else's skill that happens to sit at a name we also use?
// Install needs the answer before it REMOVES a directory it did not just write
// (…/skills/korbit, the legacy skill name), and doctor needs it to report that
// directory honestly.
//
// The content hash cannot answer it: a copy of any other version of the skill
// hashes differently from the embedded one by design — that is exactly the
// "stale" case. So the proof is a conjunction of three independent facts about
// the directory, ALL required, because the consequence of a false positive is
// deleting someone's work:
//
//  1. SKILL.md declares one of our skill names in its frontmatter (the field the
//     skill system itself keys on) AND names a command this skill drives;
//  2. its body carries one of this CLI's repository URLs, which every
//     revision of the skill has cited as where its source lives;
//  3. the directory holds exactly our reference set and nothing else.
//
// Any one of these alone is writable by hand — a skill that mentions this CLI's
// name in its description is not thereby ours — so a directory that satisfies
// only some of them is reported as foreign and left exactly as it is.

// skillFile is the skill's entry document, the only file the proof reads.
const skillFile = "SKILL.md"

// managedNames are the frontmatter `name:` values our own copies carry.
var managedNames = []string{SkillName, LegacySkillName}

// managedMarkers are the tool names this skill drives; every copy of it names
// one of them in its frontmatter (the description says which command to run).
// The marker is deliberately NOT the skill's own name — that would make any
// skill at one of our directory names pass.
var managedMarkers = []string{SkillBinary, "korbit-cli"}

// managedRepoMarkers are the repository URLs a copy of this skill cites in its
// body as where the tool's source lives. Every revision of the skill carries one
// of them, so the presence of any is a fact about the file's provenance rather
// than about its subject matter — and both must keep counting, since a copy
// citing the second URL is exactly the one Install has to recognise in order to
// replace it.
var managedRepoMarkers = []string{
	"github.com/digitalx-official/digitalx-cli",
	"github.com/korbit-official/korbit-cli",
}

// managedReferences is the exact set of files our skill ships in its references
// directory (Install prunes anything else, so an installed copy holds precisely
// these). A directory with a different set is not one of our copies. Adding a
// reference file to the skill means adding it here — the invariant test over the
// embedded skill fails until it is.
var managedReferences = []string{"debugging.md", "funding.md", "monitoring.md", "sandbox.md"}

// Managed reports whether dir holds a copy of this CLI's bundled skill — all
// three proofs described above. Anything that cannot be read, or that fails any
// one of them, reports false with no error: "not ours" is an answer, not a
// failure. Only a true here licenses removing a directory this run did not
// write.
func Managed(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, skillFile))
	if err != nil {
		return false
	}
	return managedFrontmatter(string(b)) && managedBody(string(b)) && managedReferenceSet(dir)
}

// managedFrontmatter is proof 1: the frontmatter declares one of our skill names
// and names a command this skill drives. It is the weakest of the three (a
// hand-written skill can say both), so it never stands alone.
func managedFrontmatter(skill string) bool {
	fm, _, ok := splitFrontmatter(skill)
	if !ok {
		return false
	}
	if !anyContains(fm, managedMarkers) {
		return false
	}
	name, ok := frontmatterName(fm)
	if !ok {
		return false
	}
	return slices.Contains(managedNames, name)
}

// managedBody is proof 2: the body cites one of this CLI's repository URLs.
func managedBody(skill string) bool {
	_, body, ok := splitFrontmatter(skill)
	if !ok {
		return false
	}
	return anyContains(body, managedRepoMarkers)
}

// managedReferenceSet is proof 3: the references directory holds exactly the
// files our skill ships — no extras, nothing missing, no subdirectories.
func managedReferenceSet(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, guideRefDir))
	if err != nil {
		return false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			return false
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return slices.Equal(names, managedReferences)
}

// splitFrontmatter splits markdown into its leading YAML frontmatter block
// ("---" … "---") and the body after it, reporting false when there is no such
// block or it is left unterminated (in which case both halves are empty and the
// caller keeps the input as-is).
func splitFrontmatter(s string) (fm, body string, ok bool) {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// frontmatterName reads the `name:` scalar out of a frontmatter block. Only a
// top-level (unindented) key counts, so a `name:` nested inside some other
// mapping cannot pose as the skill's own, and the value is unquoted so both
// `name: korbit` and `name: "korbit"` read the same.
func frontmatterName(fm string) (string, bool) {
	for _, line := range strings.Split(fm, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "name:")
		if !ok {
			continue
		}
		v := strings.TrimSpace(strings.TrimRight(rest, "\r"))
		v = strings.Trim(v, `"'`)
		return v, v != ""
	}
	return "", false
}

// anyContains reports whether s contains any of subs.
func anyContains(s string, subs []string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}
