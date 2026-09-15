// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package agentskillcmd implements the `agent skill …` builtins: install the
// bundled Agent Skill onto disk for each supported agent, and a doctor that
// reports install/drift/PATH health. The skill content travels as an embedded
// fs.FS handed in by the cli; all on-disk logic lives in internal/agentskill —
// this layer only resolves directories, dispatches, and produces results.
package agentskillcmd

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	goruntime "runtime"

	"github.com/digitalx-official/digitalx-cli/internal/agentskill"
	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/spec"
	"github.com/digitalx-official/digitalx-cli/internal/version"
	"github.com/spf13/cobra"
)

// Run dispatches the `agent skill …` builtins. src is the embedded skill
// content (nil only on a build that didn't wire it).
func Run(cx *clienv.Cmd, src fs.FS, c *spec.Command, cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q", args[0])
	}
	if src == nil {
		return fmt.Errorf("internal: the bundled Agent Skill is unavailable in this build")
	}
	switch c.Key() {
	case "agent skill install":
		return runInstall(cx, cmd, src)
	case "agent skill doctor":
		return runDoctor(cx, src)
	}
	return output.Usagef("unknown command %q", c.Key())
}

// ---- install ----

type skillInstallResult struct {
	Skill       string            `json:"skill"`
	CliVersion  string            `json:"cliVersion"`
	ContentHash string            `json:"contentHash"`
	Scope       string            `json:"scope"` // "user" | "project"
	Installs    []skillInstallOne `json:"installs"`
	Warnings    []string          `json:"warnings,omitempty"`
}

type skillInstallOne struct {
	Agent   string   `json:"agent"`
	AgentID string   `json:"agentId"`
	Dir     string   `json:"dir"`
	Action  string   `json:"action"` // "created" | "updated" | "unchanged"
	Files   int      `json:"files"`
	Pruned  []string `json:"pruned,omitempty"`
	// LegacyRemoved is a directory holding this same skill under
	// agentskill.LegacySkillName that install deleted after writing Dir, so the
	// agent is left with exactly one copy of it.
	LegacyRemoved string `json:"legacyRemoved,omitempty"`
}

func runInstall(cx *clienv.Cmd, cmd *cobra.Command, src fs.FS) error {
	all, _ := cmd.Flags().GetBool("all")
	project, _ := cmd.Flags().GetBool("project")

	var selected []agentskill.Agent
	if all {
		selected = agentskill.Agents
	} else {
		for _, a := range agentskill.Agents {
			if v, _ := cmd.Flags().GetBool(a.ID); v {
				selected = append(selected, a)
			}
		}
	}
	if len(selected) == 0 {
		return output.Usagef("choose where to install: --claude, --codex, or --all (add --project for a repo-scoped install)")
	}

	root, scope, err := skillRoot(cx, project)
	if err != nil {
		return err
	}

	res := skillInstallResult{Skill: agentskill.SkillName, CliVersion: version.Version, Scope: scope}
	for _, a := range selected {
		out, err := agentskill.Install(src, a.SkillDir(root))
		if err != nil {
			return fmt.Errorf("installing the skill for %s: %w", a.Name, err)
		}
		res.ContentHash = out.Hash
		one := skillInstallOne{
			Agent: a.Name, AgentID: a.ID, Dir: out.Dir,
			Action: out.Action, Files: out.Files, Pruned: out.Pruned,
		}
		// Clear a copy of this same skill sitting under the legacy name, so the
		// agent never loads two skills with identical triggers. It happens only
		// AFTER the new copy is on disk (so a failure above leaves the working
		// one alone) and only when the ownership proof passes — a skill someone
		// else wrote at that name is reported and left exactly as it is.
		if legacy := a.LegacySkillDir(root); legacy != out.Dir && isDir(legacy) {
			if !agentskill.Managed(legacy) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s holds a skill this CLI did not install, so it was left alone — if it duplicates this skill's triggers, remove it yourself", legacy))
			} else if err := os.RemoveAll(legacy); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not remove the copy of this skill at %s: %v — delete it yourself, or the agent loads two skills with the same triggers", legacy, err))
			} else {
				one.LegacyRemoved = legacy
			}
		}
		res.Installs = append(res.Installs, one)
	}

	// The skill shells out to the public command; if it isn't on PATH the agent
	// can't run it. Carry that warning in the result so stdout-only consumers see
	// the follow-up needed after a successful install.
	if _, err := exec.LookPath(agentskill.SkillBinary); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%q isn't on your PATH yet — the skill runs it as a shell command. Add it to PATH, then run `%s agent skill doctor`.", agentskill.SkillBinary, progname.Name()))
	}
	return cx.Emit("agent skill install", res)
}

// skillRoot returns the directory the skills tree is rooted under: the working
// directory for a --project install, otherwise the user's home.
func skillRoot(cx *clienv.Cmd, project bool) (root, scope string, err error) {
	if project {
		wd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		return wd, "project", nil
	}
	h, err := agentHome(cx.Getenv)
	return h, "user", err
}

// agentHome resolves the user's home directory, honoring the injected Getenv
// (so tests can point it at a temp dir) before falling back to os.UserHomeDir.
func agentHome(getenv func(string) string) (string, error) {
	return agentHomeFor(getenv, goruntime.GOOS)
}

// agentHomeFor resolves the home directory the way os.UserHomeDir does —
// USERPROFILE on Windows, HOME elsewhere — but through the injected getenv so
// tests can redirect it. Querying the OS-canonical variable matters on Windows:
// a shell like Git Bash may export a unix-style HOME (e.g. /c/Users/name) that
// filepath.Join would mangle, whereas USERPROFILE is the native path. goos is a
// parameter so both branches are testable on one OS.
func agentHomeFor(getenv func(string) string, goos string) (string, error) {
	envVar := "HOME"
	if goos == "windows" {
		envVar = "USERPROFILE"
	}
	if h := getenv(envVar); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// ---- doctor ----

type skillDoctorReport struct {
	Skill        string              `json:"skill"`
	CliVersion   string              `json:"cliVersion"`
	Binary       string              `json:"binary"`      // command name the skill shells out to ("digitalx")
	InvokedAs    string              `json:"invokedAs"`   // name this executable was actually run as
	NameMatches  bool                `json:"nameMatches"` // InvokedAs == Binary
	NameFix      string              `json:"nameFix,omitempty"`
	EmbeddedHash string              `json:"embeddedHash"`
	OnPath       bool                `json:"onPath"`
	PathResolved string              `json:"pathResolved,omitempty"`
	PathFix      string              `json:"pathFix,omitempty"`
	Targets      []skillDoctorTarget `json:"targets"`
}

type skillDoctorTarget struct {
	Agent         string `json:"agent"`
	AgentID       string `json:"agentId"`
	Dir           string `json:"dir"`
	AgentDetected bool   `json:"agentDetected"`
	Installed     bool   `json:"installed"`
	InstalledHash string `json:"installedHash,omitempty"`
	Status        string `json:"status"` // "ok" | "stale" | "missing" | "n/a"
	Fix           string `json:"fix,omitempty"`
	// A directory under agentskill.LegacySkillName, present alongside (or
	// instead of) Dir. LegacyStatus is "managed" when it holds this same skill —
	// a re-install clears it — or "foreign" when it holds someone else's, which
	// this CLI never touches.
	LegacyDir    string `json:"legacyDir,omitempty"`
	LegacyStatus string `json:"legacyStatus,omitempty"` // "managed" | "foreign"
	LegacyNote   string `json:"legacyNote,omitempty"`
}

func runDoctor(cx *clienv.Cmd, src fs.FS) error {
	embeddedHash, err := agentskill.ContentHash(src)
	if err != nil {
		return err
	}
	home, err := agentHome(cx.Getenv)
	if err != nil {
		return err
	}

	rep := skillDoctorReport{
		Skill: agentskill.SkillName, CliVersion: version.Version,
		Binary: agentskill.SkillBinary, EmbeddedHash: embeddedHash,
		InvokedAs: progname.Name(),
	}
	// Check 1: this executable is itself named the command the skill invokes.
	// `go install` produces a "digitalx-cli" binary, but the installed skill runs
	// literal `digitalx …` commands (agentskill.SkillBinary) — so a mismatch is
	// the usual reason that command isn't found below, and the fix is to expose
	// this binary under that name.
	rep.NameMatches = rep.InvokedAs == agentskill.SkillBinary
	if !rep.NameMatches {
		self := rep.InvokedAs // fallback if the executable path can't be resolved
		if exe, err := os.Executable(); err == nil && exe != "" {
			self = exe
		}
		rep.NameFix = fmt.Sprintf("put a %q on your PATH — rename or symlink this binary (e.g. `ln -s %s <dir-on-PATH>/%s`)", agentskill.SkillBinary, self, agentskill.SkillBinary)
	}
	// Check 2: that command actually resolves on PATH (what the skill needs).
	if p, err := exec.LookPath(agentskill.SkillBinary); err == nil {
		rep.OnPath, rep.PathResolved = true, p
	} else {
		rep.PathFix = fmt.Sprintf("put %q on your PATH — the skill runs it as a shell command", agentskill.SkillBinary)
	}

	anyInstalled, anyStale, anyLegacy := false, false, false
	for _, a := range agentskill.Agents {
		dir := a.SkillDir(home)
		t := skillDoctorTarget{Agent: a.Name, AgentID: a.ID, Dir: dir, AgentDetected: isDir(a.ConfigRoot(home))}
		installed, hash, err := agentskill.InspectDir(dir)
		if err != nil {
			return err
		}
		switch {
		case installed:
			anyInstalled = true
			t.Installed, t.InstalledHash = true, hash
			if hash == embeddedHash {
				t.Status = "ok"
			} else {
				t.Status = "stale"
				t.Fix = fmt.Sprintf("run `%s agent skill install --%s` to refresh it to this version", progname.Name(), a.ID)
				anyStale = true
			}
		case t.AgentDetected:
			t.Status = "missing"
			t.Fix = fmt.Sprintf("run `%s agent skill install --%s`", progname.Name(), a.ID)
		default:
			t.Status = "n/a" // agent not detected on this machine
		}
		// A copy under the legacy name is a live skill for the agent: reported
		// whether or not the current-name one is installed, because two copies
		// carry the same triggers. It counts toward the exit code only for an
		// agent that is actually on this machine — a leftover directory for an
		// agent the user does not run is worth naming, not worth failing over.
		if legacy := a.LegacySkillDir(home); legacy != dir && isDir(legacy) {
			t.LegacyDir = legacy
			if agentskill.Managed(legacy) {
				t.LegacyStatus = "managed"
				also := ""
				if installed {
					also = "also " // both copies are on disk, with the same triggers
				}
				t.LegacyNote = fmt.Sprintf("%sinstalled under the legacy name %q (%s) — run `%s agent skill install --%s` to refresh it as %q and remove that copy",
					also, agentskill.LegacySkillName, legacy, progname.Name(), a.ID, agentskill.SkillName)
				if t.Status != "n/a" {
					anyLegacy = true
				}
			} else {
				t.LegacyStatus = "foreign"
				t.LegacyNote = fmt.Sprintf("a skill this CLI did not install sits at %s; it is left alone", legacy)
			}
		}
		rep.Targets = append(rep.Targets, t)
	}

	// rep implements textout.TextFormatter (see format.go): the emitter renders
	// the checklist in human mode and marshals the same struct in --json mode.
	if err := cx.Emit("agent skill doctor", rep); err != nil {
		return err
	}
	// A problem worth fixing: a stale install, or a skill installed for a local
	// agent while the binary it shells out to isn't reachable on PATH.
	if anyStale || anyLegacy || (anyInstalled && !rep.OnPath) {
		return clienv.ExitError{Code: output.ExitConfig}
	}
	return nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
