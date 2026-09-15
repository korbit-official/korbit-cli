// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package agentskill installs and inspects the bundled Digital X Agent Skill on
// disk. The skill content travels inside the binary as an embedded fs.FS (rooted
// at the skill directory, with SKILL.md at the top); this package writes it into
// an agent's skills directory and reports whether an on-disk copy matches the
// embedded one.
//
// It is deliberately frontend-agnostic — no cobra, no output contract, no
// program name. It takes an fs.FS plus target directories and returns plain data
// and errors; the cli layer resolves directories, dispatches, and formats (the
// same split the sandbox command group uses). Drift is detected by a content
// hash over the file set, so there is no version field to bump and no sidecar
// marker file to keep in sync: the embedded copy in the running binary is the
// single source of truth.
package agentskill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// SkillName is the directory name the skill installs under inside an agent's
// skills directory (…/skills/digitalx-cli). It matches the embedded source's
// root dir, and the frontmatter `name:` the skill declares.
const SkillName = "digitalx-cli"

// LegacySkillName is the other directory name an installed copy of THIS skill
// can carry: a binary that shipped the skill under that name wrote
// …/skills/korbit, and an agent that finds both loads two skills with the same
// triggers. Install replaces such a copy (only when Managed proves it is ours)
// and doctor reports one it finds.
const LegacySkillName = "korbit"

// SkillBinary is the command name the bundled skill tells agents to run. It is
// intentionally separate from the running executable name: the binary can be
// invoked through another filename such as "digitalx-cli", while the installed
// skill contains literal `digitalx ...` commands. Doctor and command metadata
// must describe the command the skill actually executes.
const SkillBinary = "digitalx"

// Agent is a target agent runtime whose skills directory we install into.
// SkillRel is the path of its skills directory relative to a root (the user's
// home for a user-wide install, the working directory for a project install) —
// e.g. {".claude","skills"}. ConfigRel is the agent's config root relative to
// the user's home, used only as a cheap "is this agent installed?" signal for
// doctor. Codex intentionally differs: its skills live in .agents/skills, while
// its app/config state lives under .codex.
type Agent struct {
	ID        string // stable identifier: "claude" | "codex"
	Name      string // display name
	SkillRel  []string
	ConfigRel []string
}

// Agents is the supported set, in display order.
var Agents = []Agent{
	{ID: "claude", Name: "Claude", SkillRel: []string{".claude", "skills"}, ConfigRel: []string{".claude"}},
	{ID: "codex", Name: "Codex", SkillRel: []string{".agents", "skills"}, ConfigRel: []string{".codex"}},
}

// AgentByID returns the agent with the given id.
func AgentByID(id string) (Agent, bool) {
	for _, a := range Agents {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// SkillDir is the absolute skill directory under root (…/skills/digitalx-cli).
func (a Agent) SkillDir(root string) string {
	return a.skillDirNamed(root, SkillName)
}

// LegacySkillDir is the absolute directory a copy under LegacySkillName would
// occupy under root (…/skills/korbit).
func (a Agent) LegacySkillDir(root string) string {
	return a.skillDirNamed(root, LegacySkillName)
}

func (a Agent) skillDirNamed(root, name string) string {
	// Build into a freshly sized slice rather than chained appends, so we never
	// risk aliasing a.SkillRel's backing array.
	parts := make([]string, 0, len(a.SkillRel)+2)
	parts = append(parts, root)
	parts = append(parts, a.SkillRel...)
	parts = append(parts, name)
	return filepath.Join(parts...)
}

// ConfigRoot is the agent's top-level config directory under root (e.g.
// root/.claude or root/.codex). Its presence is a cheap signal that the agent is
// installed, so doctor can stay quiet about an agent the user doesn't use.
func (a Agent) ConfigRoot(root string) string {
	parts := append([]string{root}, a.ConfigRel...)
	return filepath.Join(parts...)
}

// ContentHash is a stable digest over a skill file tree (the embedded source or
// an installed copy): every regular file's path, length, and bytes, in sorted
// path order. Two trees with identical files hash equal regardless of walk
// order or timestamps, so it is the drift check between the embedded copy and an
// on-disk one. Returned short (16 hex chars) — collision risk is irrelevant for
// "are these the same files".
func ContentHash(fsys fs.FS) (string, error) {
	paths, err := filesSorted(fsys)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// filesSorted returns every regular file path in fsys (slash-separated, relative
// to the root), sorted.
func filesSorted(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Outcome is the result of installing into one directory.
type Outcome struct {
	Dir    string   `json:"dir"`
	Action string   `json:"action"` // "created" | "updated" | "unchanged"
	Files  int      `json:"files"`
	Pruned []string `json:"pruned,omitempty"` // stale files removed (renamed/removed in a newer skill)
	Hash   string   `json:"contentHash"`
}

// Install writes the skill from src into dir (the skill directory, …/digitalx-cli),
// then prunes any file there that the embedded skill no longer ships — so a
// renamed or removed reference file from an older binary can't linger. It is
// idempotent: an on-disk copy already matching the embedded one is left
// untouched and reported as "unchanged" (no rewrite, no mtime churn).
func Install(src fs.FS, dir string) (Outcome, error) {
	embedded, err := filesSorted(src)
	if err != nil {
		return Outcome{}, err
	}
	wantHash, err := ContentHash(src)
	if err != nil {
		return Outcome{}, err
	}

	existedBefore := dirExists(dir)
	if existedBefore {
		if cur, err := DirHash(dir); err == nil && cur == wantHash {
			return Outcome{Dir: dir, Action: "unchanged", Files: len(embedded), Hash: wantHash}, nil
		}
	}

	want := make(map[string]bool, len(embedded))
	for _, p := range embedded {
		rel := filepath.FromSlash(p)
		want[rel] = true
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return Outcome{}, err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return Outcome{}, err
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return Outcome{}, err
		}
	}

	pruned, err := prune(dir, want)
	if err != nil {
		return Outcome{}, err
	}

	action := "created"
	if existedBefore {
		action = "updated"
	}
	return Outcome{Dir: dir, Action: action, Files: len(embedded), Pruned: pruned, Hash: wantHash}, nil
}

// prune removes every regular file under dir that isn't in want, then drops any
// directory left empty (except dir itself). Returns the slash-separated relative
// paths it removed, sorted.
func prune(dir string, want map[string]bool) ([]string, error) {
	var pruned []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if !want[rel] {
			if err := os.Remove(p); err != nil {
				return err
			}
			pruned = append(pruned, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	removeEmptyDirs(dir)
	sort.Strings(pruned)
	return pruned, nil
}

// removeEmptyDirs removes empty subdirectories of dir bottom-up (best-effort,
// never dir itself) so a pruned subtree leaves nothing behind.
func removeEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return nil
	})
	// Deepest first, so a parent becomes empty only after its children are gone.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, p := range dirs {
		if entries, err := os.ReadDir(p); err == nil && len(entries) == 0 {
			_ = os.Remove(p)
		}
	}
}

// DirHash is ContentHash over an installed copy on disk.
func DirHash(dir string) (string, error) {
	return ContentHash(os.DirFS(dir))
}

// InspectDir reports whether a skill is installed at dir and, if so, its content
// hash (for comparison against the embedded ContentHash). A missing directory is
// installed=false with no error.
func InspectDir(dir string) (installed bool, hash string, err error) {
	if !dirExists(dir) {
		return false, "", nil
	}
	h, err := DirHash(dir)
	if err != nil {
		return true, "", err
	}
	return true, h, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
