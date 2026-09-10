// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/fslock"
)

// InstallResult is what `self install` reports: the version installed, the
// binary path it settled, what it had to repair (empty on a clean first install),
// and the PATH outcome. Everything the caller needs is here — nothing important
// is only on stderr.
type InstallResult struct {
	Version    string     `json:"version"`
	Executable string     `json:"executable"`
	SHA256     string     `json:"sha256"`
	Repaired   []string   `json:"repaired"` // healed states (always present; [] when clean)
	Path       PathResult `json:"path"`
	Warnings   []string   `json:"warnings,omitempty"`
	// Aliases are the extra command names now pointing at the installed binary
	// (see alias.go). Absent on a fresh install, which creates none.
	Aliases []string `json:"aliases,omitempty"`
}

// Install copies the running binary to the stable PATH location, wires PATH, and
// writes the manifest. It is RECONCILING, not linear: re-running it converges a
// broken install to healthy (a deleted binary, a lost PATH entry, a corrupt
// manifest, a leftover temp file, …) and tolerates any subset already correct. It
// is the binary side of the `curl … | sh` repair path, so it assumes nothing
// about the prior on-disk state. Because the install script is version-pinned,
// Install records THIS binary's version.
func (c Config) Install() (*InstallResult, error) {
	c.log().Debug("self install starting", "version", c.Version)
	if err := c.requireReleaseBuild(); err != nil {
		c.log().Debug("self install refused: development build", "version", c.Version)
		return nil, err
	}
	src, err := c.runningExe()
	if err != nil {
		return nil, fmt.Errorf("locating the running binary: %w", err)
	}
	l := c.Layout()
	// Serialize against a concurrent install/update.
	if err := os.MkdirAll(l.Home(), 0o700); err != nil {
		return nil, err
	}
	unlock, err := fslock.Lock(l.LockPath())
	if err != nil {
		return nil, err
	}
	defer unlock()

	res := &InstallResult{
		Version:    c.Version,
		Executable: l.ExecutablePath(),
		Repaired:   []string{},
	}
	srcSum, err := sha256File(src)
	if err != nil {
		return nil, err
	}
	res.SHA256 = srcSum

	// 1. Place the running binary at the stable PATH location (a copy, not a symlink).
	repaired, err := c.reconcileExecutable(l, src, srcSum)
	if err != nil {
		return nil, err
	}
	res.Repaired = append(res.Repaired, repaired...)

	// 2. Keep every alias command name pointing at the binary just placed. Only
	// names this install OWNS are written (alias.go), so a fresh install gets the
	// primary binary alone and a stranger's file at that name is left untouched.
	// The manifest read here is the PRIOR one — step 4 rewrites it below — which
	// is what records an alias an earlier run adopted.
	prevManifest, _, _ := loadManifest(l.ManifestPath())
	wanted, ownWarnings := c.aliasesToKeep(l, prevManifest, src)
	aliases, aliasWarnings := c.syncAliases(l, wanted)
	res.Aliases = aliases
	res.Warnings = append(res.Warnings, ownWarnings...)
	res.Warnings = append(res.Warnings, aliasWarnings...)

	// 3. Wire PATH (idempotent; prompts on a TTY, else prints guidance).
	pr, err := c.wirePath(l.ExecutableDir())
	if err != nil {
		return nil, err
	}
	res.Path = pr

	// 4. Write/rebuild the manifest for this version.
	repairedManifest, err := c.reconcileManifest(l, srcSum, aliases)
	if err != nil {
		return nil, err
	}
	if repairedManifest != "" {
		res.Repaired = append(res.Repaired, repairedManifest)
	}

	// 5. Sweep leftover temp / swap files from an interrupted run.
	if swept := c.sweepLeftovers(l); swept {
		res.Repaired = append(res.Repaired, "removed leftover temp files")
	}
	c.log().Debug("self install complete", "version", res.Version, "executable", res.Executable, "sha256", res.SHA256, "repaired", res.Repaired, "pathAction", res.Path.Action)
	return res, nil
}

// reconcileExecutable ensures the installed binary on PATH is this binary: it
// (re)creates the bin dir and copies src over the installed binary when the
// installed binary is missing, unreadable, or its bytes differ. It returns the
// repairs performed. Repairs are reported only when a manifest already existed
// (a first install's setup is not a repair) AND — for a byte mismatch — only when
// the manifest records THIS same version, since a differing version is a normal
// upgrade/reinstall, not corruption.
func (c Config) reconcileExecutable(l Layout, src, wantSum string) ([]string, error) {
	var repaired []string
	prev, hadManifest, _ := loadManifest(l.ManifestPath())
	if !dirExists(l.ExecutableDir()) {
		if err := os.MkdirAll(l.ExecutableDir(), 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", l.ExecutableDir(), err)
		}
		if hadManifest {
			repaired = append(repaired, "created the bin directory")
		}
	}
	ptr := l.ExecutablePath()
	switch {
	case resolveOrClean(src) == resolveOrClean(ptr):
		// Running from the installed location already (e.g. a manual re-run); nothing
		// to copy. Symlinks are resolved on both sides (src is the already-resolved
		// running binary) so a symlinked home doesn't force a needless self-copy.
	case !fileExists(ptr):
		if err := copyFileAtomic(ptr, src, 0o755); err != nil {
			return nil, fmt.Errorf("creating the installed binary: %w", err)
		}
		c.log().Debug("copied binary to PATH location", "dst", ptr)
		if hadManifest {
			repaired = append(repaired, "recreated the missing binary")
		}
	default:
		have, err := sha256File(ptr)
		if err != nil || have != wantSum {
			if err := copyFileAtomic(ptr, src, 0o755); err != nil {
				return nil, fmt.Errorf("installing the binary to %s: %w", l.ExecutablePath(), err)
			}
			c.log().Debug("replaced binary at PATH location", "dst", ptr)
			// A byte mismatch is corruption only when the manifest claims this exact
			// version is already installed; a different version is a normal upgrade.
			if hadManifest && prev.Version == c.Version {
				repaired = append(repaired, "replaced the stale binary")
			}
		}
	}
	return repaired, nil
}

// reconcileManifest rebuilds the manifest for the version just installed, so a
// missing or corrupt manifest is regenerated rather than blocking the install. It
// returns the repair note when it had to rebuild a corrupt one.
func (c Config) reconcileManifest(l Layout, sum string, aliases []string) (repaired string, err error) {
	_, found, perr := loadManifest(l.ManifestPath())
	if found && perr != nil {
		repaired = "rebuilt the corrupt manifest"
	}
	m := Manifest{
		Method:      MethodManagedScript,
		Executable:  l.ExecutablePath(),
		Version:     c.Version,
		OS:          c.os(),
		Arch:        c.arch(),
		SHA256:      sum,
		Repo:        c.repo(),
		InstalledAt: strconv.FormatInt(c.now(), 10),
		Aliases:     aliases,
	}
	if err := m.save(l.ManifestPath()); err != nil {
		return "", err
	}
	return repaired, nil
}

// sweepLeftovers best-effort removes temp/swap files an interrupted install or
// update left in the bin dir or home: any hidden *.tmp scratch file (which
// covers this package's own tmpPattern and any earlier one), and minio's
// .<bin>.new / .<bin>.old swap files for the primary binary AND for each alias
// name, since an alias is written through the same swap on windows. It never
// removes the installed binary, an alias, or the manifest.
func (c Config) sweepLeftovers(l Layout) bool {
	swaps := map[string]bool{}
	for _, bin := range []string{l.BinName(), l.LegacyBinName()} {
		swaps["."+bin+".new"] = true
		swaps["."+bin+".old"] = true
	}
	swept := false
	dirs := []string{l.ExecutableDir(), l.Home()}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			isTmp := filepath.Ext(name) == ".tmp" && len(name) > 0 && name[0] == '.'
			if isTmp || swaps[name] {
				if os.Remove(filepath.Join(d, name)) == nil {
					swept = true
				}
			}
		}
	}
	return swept
}
