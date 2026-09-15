// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The alias contract, end to end. An install may carry a second command name in
// the install dir that runs the same binary; on unix that is a relative symlink
// onto the primary. These tests pin the four states that matter: a fresh install
// creates none; an install/update that finds one (or is running as one) adopts
// it; a healthy adopted install keeps it current; and uninstall takes both away.

// installLegacyOnly builds the alias-only layout: the managed binary on PATH
// carries ONLY the alias name and the primary name is not on disk, with a
// manifest that records the alias path as the executable. It returns a Config
// whose running binary IS that alias-named file.
func installLegacyOnly(t *testing.T, content string) (Config, Layout) {
	t.Helper()
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, content)
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	// Rename the primary to the alias name, and point the manifest at it: what an
	// install placed before the primary name existed looks like on disk.
	if err := os.Rename(l.ExecutablePath(), l.LegacyExecutablePath()); err != nil {
		t.Fatal(err)
	}
	m, _, err := loadManifest(l.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	m.Executable = l.LegacyExecutablePath()
	m.Aliases = nil
	if err := m.save(l.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.LegacyExecutablePath()
	return c, l
}

// assertAlias checks that the alias name is a symlink pointing at the primary
// binary's bare name and that reading through it yields want.
func assertAlias(t *testing.T, l Layout, want string) {
	t.Helper()
	target, err := os.Readlink(l.LegacyExecutablePath())
	if err != nil {
		t.Fatalf("alias is not a symlink: %v", err)
	}
	if target != l.BinName() {
		t.Errorf("alias target = %q, want the relative name %q", target, l.BinName())
	}
	if got := mustContent(t, l.LegacyExecutablePath()); got != want {
		t.Errorf("reading through the alias gave %q, want %q", got, want)
	}
}

// TestLayoutNames pins the primary and alias filenames per OS — the asset
// extraction (extractBinary by exact basename) and the installers agree on them.
func TestLayoutNames(t *testing.T) {
	for _, tc := range []struct{ goos, bin, legacy string }{
		{"linux", "digitalx", "korbit"},
		{"darwin", "digitalx", "korbit"},
		{"windows", "digitalx.exe", "korbit.exe"},
	} {
		l := Layout{getenv: func(string) string { return "" }, goos: tc.goos}
		if got := l.BinName(); got != tc.bin {
			t.Errorf("%s BinName = %q, want %q", tc.goos, got, tc.bin)
		}
		if got := l.LegacyBinName(); got != tc.legacy {
			t.Errorf("%s LegacyBinName = %q, want %q", tc.goos, got, tc.legacy)
		}
	}
}

// TestAssetNameFallback pins the archive name self update asks for when the
// release publishes no manifest to name its own (see resolveTarget).
func TestAssetNameFallback(t *testing.T) {
	c := Config{GOOS: "linux", GOARCH: "arm64"}
	if got := c.assetName(); got != "digitalx-cli_linux_arm64.tar.gz" {
		t.Errorf("assetName = %q", got)
	}
	c = Config{GOOS: "windows", GOARCH: "amd64"}
	if got := c.assetName(); got != "digitalx-cli_windows_amd64.zip" {
		t.Errorf("assetName = %q", got)
	}
}

// TestFreshInstallCreatesNoAlias: a machine with nothing installed gets the
// primary binary alone — no alias file, and nothing recorded in the manifest.
func TestFreshInstallCreatesNoAlias(t *testing.T) {
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	if pathPresent(l.LegacyExecutablePath()) {
		t.Error("a fresh install must not create an alias")
	}
	if len(res.Aliases) != 0 {
		t.Errorf("result aliases = %v, want none", res.Aliases)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if len(m.Aliases) != 0 {
		t.Errorf("manifest aliases = %v, want none", m.Aliases)
	}
}

// TestInstallAdoptsLegacyLayout: running `self install` while the binary on PATH
// carries only the alias name places the primary AND turns the alias into a
// symlink onto it, so both command names keep working and the manifest records
// the alias.
func TestInstallAdoptsLegacyLayout(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("primary binary = %q", got)
	}
	assertAlias(t, l, "BINARY-v1")
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v, want %q", res.Aliases, l.LegacyBinName())
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if !slices.Contains(m.Aliases, l.LegacyBinName()) {
		t.Errorf("manifest aliases = %v, want %q", m.Aliases, l.LegacyBinName())
	}
	if m.Executable != l.ExecutablePath() {
		t.Errorf("manifest executable = %q, want the primary %q", m.Executable, l.ExecutablePath())
	}
}

// TestUpdateAdoptsLegacyLayout: an install whose only binary is the alias name
// updates through the normal path, and comes out with the new version at the
// primary name plus the alias pointing at it. Both command names then run the
// version just installed.
func TestUpdateAdoptsLegacyLayout(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatalf("update from the alias-only layout: %v", err)
	}
	if !res.Updated {
		t.Fatal("update reported nothing applied")
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v2" {
		t.Errorf("primary binary = %q, want BINARY-v2", got)
	}
	assertAlias(t, l, "BINARY-v2")
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v", res.Aliases)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if !slices.Contains(m.Aliases, l.LegacyBinName()) {
		t.Errorf("manifest aliases = %v", m.Aliases)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
}

// TestUpdateKeepsAdoptedAliasCurrent: an install that already carries the alias
// keeps it after a further update — the symlink needs no rewrite, so the alias
// resolves to the new bytes on its own.
func TestUpdateKeepsAdoptedAliasCurrent(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // adopt: primary + alias
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()

	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v3")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v3.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}
	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated {
		t.Fatal("update reported nothing applied")
	}
	assertAlias(t, l, "BINARY-v3")
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v", res.Aliases)
	}
}

// TestAssertManagedAcceptsAliasPaths: the provenance guard recognizes the
// running binary under either name — the alias-only layout (a real file at the
// alias name), and the adopted layout reached through the alias symlink.
func TestAssertManagedAcceptsAliasPaths(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if err := c.AssertManaged(); err != nil {
		t.Fatalf("alias-only layout rejected: %v", err)
	}
	if _, err := c.Install(); err != nil { // adopt
		t.Fatal(err)
	}
	// Running via the alias symlink: resolveExecutable resolves it to the primary,
	// which is what the production path hands the guard.
	resolved, err := filepath.EvalSymlinks(l.LegacyExecutablePath())
	if err != nil {
		t.Fatal(err)
	}
	c.exeOverride = resolved
	if err := c.AssertManaged(); err != nil {
		t.Fatalf("adopted install reached through the alias rejected: %v", err)
	}
	// A binary somewhere else entirely is still refused.
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if err := c.AssertManaged(); err == nil {
		t.Error("a binary outside the install dir must not be treated as managed")
	}
}

// TestUninstallRemovesAliasAndPrimary: uninstall clears every command name it
// put on PATH, alias first, so nothing dangling is left behind.
func TestUninstallRemovesAliasAndPrimary(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // adopt
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()

	res, err := c.Uninstall(UninstallOptions{RemoveBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	if pathPresent(l.ExecutablePath()) {
		t.Error("the primary binary was left behind")
	}
	if pathPresent(l.LegacyExecutablePath()) {
		t.Error("the alias was left behind")
	}
	if !slices.Contains(res.Removed, l.LegacyExecutablePath()) {
		t.Errorf("removed = %v, want the alias listed", res.Removed)
	}
	if !slices.Contains(res.Removed, l.ExecutablePath()) {
		t.Errorf("removed = %v, want the primary listed", res.Removed)
	}
}

// TestDoctorReportsAliasStates covers the three things doctor must say about the
// alias: the alias-only layout is healthy and labeled, an adopted alias is
// reported with its symlink target, and an alias the manifest promises but that
// is gone is a problem.
func TestDoctorReportsAliasStates(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")

	// 1. Alias-only layout: healthy, flagged, and the binary line points at the
	//    alias-named file rather than a primary that isn't there.
	rep, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.LegacyLayout {
		t.Error("the alias-only layout was not reported")
	}
	if rep.Executable != l.LegacyExecutablePath() {
		t.Errorf("executable = %q, want %q", rep.Executable, l.LegacyExecutablePath())
	}
	if !rep.OK() && rep.Problem(FieldBinary) != "" {
		t.Errorf("the alias-only layout must not be a binary problem: %q", rep.Problem(FieldBinary))
	}

	// 2. Adopted: the alias is reported with its symlink target.
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()
	rep, err = c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if rep.LegacyLayout {
		t.Error("an adopted install must not be reported as the alias-only layout")
	}
	if len(rep.Aliases) != 1 || rep.Aliases[0].Name != l.LegacyBinName() {
		t.Fatalf("aliases = %+v", rep.Aliases)
	}
	if !rep.Aliases[0].Present || rep.Aliases[0].Target != l.BinName() {
		t.Errorf("alias status = %+v, want present with target %q", rep.Aliases[0], l.BinName())
	}
	if p := rep.Problem(FieldAlias); p != "" {
		t.Errorf("a healthy alias must not be a problem: %q", p)
	}

	// 3. The alias the manifest promises is deleted: a problem naming the command.
	if err := os.Remove(l.LegacyExecutablePath()); err != nil {
		t.Fatal(err)
	}
	rep, err = c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if p := rep.Problem(FieldAlias); !strings.Contains(p, l.LegacyBinName()) {
		t.Errorf("missing-alias problem = %q", p)
	}
	if rep.OK() {
		t.Error("a missing alias must make doctor report needs-attention")
	}
}

// TestSweepLeftoversClearsBothSwapNames pins that an interrupted swap of either
// command name is cleaned up, along with any hidden temp file.
func TestSweepLeftoversClearsBothSwapNames(t *testing.T) {
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	leftovers := []string{
		"." + l.BinName() + ".new",
		"." + l.BinName() + ".old",
		"." + l.LegacyBinName() + ".new",
		"." + l.LegacyBinName() + ".old",
		".korbit-abc.tmp",
		".digitalx-abc.tmp",
	}
	for _, name := range leftovers {
		if err := os.WriteFile(filepath.Join(l.ExecutableDir(), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(res.Repaired, "leftover") {
		t.Errorf("repaired = %v, want a leftover sweep", res.Repaired)
	}
	for _, name := range leftovers {
		if pathPresent(filepath.Join(l.ExecutableDir(), name)) {
			t.Errorf("%s was not swept", name)
		}
	}
	if !fileExists(l.ExecutablePath()) {
		t.Error("the sweep must never remove the installed binary")
	}
}

// ---- ownership ----

// TestForeignFileAtAliasNameIsLeftAlone is the safety property: the install dir
// is a plain directory on the user's PATH, so a `korbit` in it may be their own
// wrapper script. Install must not overwrite it, uninstall must not delete it,
// and the user must be told rather than left guessing why the alias is absent.
func TestForeignFileAtAliasNameIsLeftAlone(t *testing.T) {
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // fresh install: primary only
		t.Fatal(err)
	}
	l := c.Layout()

	// The user drops their own wrapper at the alias name.
	const foreign = "#!/bin/sh\n# my own wrapper\nexec something-else \"$@\"\n"
	if err := os.WriteFile(l.LegacyExecutablePath(), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustContent(t, l.LegacyExecutablePath()); got != foreign {
		t.Errorf("a file we do not own was overwritten:\n%s", got)
	}
	if len(res.Aliases) != 0 {
		t.Errorf("result aliases = %v, want none (the name is not ours)", res.Aliases)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if len(m.Aliases) != 0 {
		t.Errorf("manifest aliases = %v, want none", m.Aliases)
	}
	if !containsSubstr(res.Warnings, "not a managed install") {
		t.Errorf("warnings = %v, want one naming the unowned file", res.Warnings)
	}

	// And uninstall leaves it where it is.
	c.exeOverride = l.ExecutablePath()
	ures, err := c.Uninstall(UninstallOptions{RemoveBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pathPresent(l.LegacyExecutablePath()) {
		t.Fatal("uninstall deleted a file this install does not own")
	}
	if got := mustContent(t, l.LegacyExecutablePath()); got != foreign {
		t.Errorf("uninstall rewrote a file we do not own:\n%s", got)
	}
	if slices.Contains(ures.Removed, l.LegacyExecutablePath()) {
		t.Errorf("removed = %v, must not list the unowned file", ures.Removed)
	}
}

// TestOwnsLegacyAliasProofs pins each of the four independent proofs of
// ownership on its own, so none of them silently stops working behind the
// others, and pins that a plain foreign file satisfies none of them.
func TestOwnsLegacyAliasProofs(t *testing.T) {
	// setup returns a config whose install dir holds a primary binary and a file
	// at the alias name with the given content, plus the layout.
	setup := func(t *testing.T, aliasContent string) (Config, Layout) {
		t.Helper()
		c := testConfig(t.TempDir())
		c.exeOverride = writeFakeBinary(t, "BINARY-v1")
		if _, err := c.Install(); err != nil {
			t.Fatal(err)
		}
		l := c.Layout()
		if err := os.WriteFile(l.LegacyExecutablePath(), []byte(aliasContent), 0o755); err != nil {
			t.Fatal(err)
		}
		c.exeOverride = l.ExecutablePath()
		return c, l
	}

	t.Run("manifest lists the alias", func(t *testing.T) {
		c, l := setup(t, "WHATEVER")
		exe, _ := c.runningExe()
		if !c.ownsLegacyAlias(l, Manifest{Aliases: []string{l.LegacyBinName()}}, exe) {
			t.Error("a manifest-listed alias must be recognized as ours")
		}
	})

	t.Run("manifest executable is the alias path", func(t *testing.T) {
		c, l := setup(t, "WHATEVER")
		exe, _ := c.runningExe()
		if !c.ownsLegacyAlias(l, Manifest{Executable: l.LegacyExecutablePath()}, exe) {
			t.Error("the manifest's own executable path must be recognized as ours")
		}
	})

	t.Run("running as the alias", func(t *testing.T) {
		c, l := setup(t, "WHATEVER")
		c.exeOverride = l.LegacyExecutablePath()
		exe, _ := c.runningExe()
		if !c.ownsLegacyAlias(l, Manifest{}, exe) {
			t.Error("the running binary's own path must be recognized as ours")
		}
	})

	t.Run("bytes match the manifest sha256", func(t *testing.T) {
		c, l := setup(t, "BINARY-v1")
		sum, err := sha256File(l.LegacyExecutablePath())
		if err != nil {
			t.Fatal(err)
		}
		exe, _ := c.runningExe()
		if !c.ownsLegacyAlias(l, Manifest{SHA256: sum}, exe) {
			t.Error("bytes matching the recorded install must be recognized as ours")
		}
	})

	t.Run("a foreign file satisfies nothing", func(t *testing.T) {
		c, l := setup(t, "#!/bin/sh\nexec something-else\n")
		// A manifest fully populated for the PRIMARY install, so only a genuinely
		// foreign alias file can fail every proof.
		m, _, _ := loadManifest(l.ManifestPath())
		if m.SHA256 == "" || m.Executable != l.ExecutablePath() {
			t.Fatalf("test setup: manifest = %+v", m)
		}
		exe, _ := c.runningExe()
		if c.ownsLegacyAlias(l, m, exe) {
			t.Error("a foreign file must not be treated as ours")
		}
	})
}

// TestUpdateSweepsSwapLeftovers pins that self update clears the swap/scratch
// files an in-place replacement leaves behind — Install is not the only command
// that has to tidy up, and on windows the alias copy goes through the same swap.
func TestUpdateSweepsSwapLeftovers(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // adopt: primary + alias
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()

	leftovers := []string{
		"." + l.BinName() + ".old",
		"." + l.LegacyBinName() + ".old",
		".digitalx-stale.tmp",
	}
	for _, name := range leftovers {
		if err := os.WriteFile(filepath.Join(l.ExecutableDir(), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range leftovers {
		if pathPresent(filepath.Join(l.ExecutableDir(), name)) {
			t.Errorf("%s survived the update", name)
		}
	}
	// The sweep must never take the binaries with it.
	if !fileExists(l.ExecutablePath()) {
		t.Error("the primary binary was swept")
	}
	assertAlias(t, l, "BINARY-v2")
}

// TestWindowsAliasIsARefreshedCopy drives the windows branch on this host via
// Config.GOOS: there the alias is a second COPY (an unprivileged install has no
// usable file symlink), written through minio/selfupdate so a target that is
// itself running is renamed aside rather than refused. It pins both halves — the
// alias ends up holding the primary's bytes, and the .old swap file that rename
// leaves behind is cleaned up rather than left in the install dir.
func TestWindowsAliasIsARefreshedCopy(t *testing.T) {
	c := testConfig(t.TempDir())
	c.GOOS = "windows"
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	if l.BinName() != "digitalx.exe" || l.LegacyBinName() != "korbit.exe" {
		t.Fatalf("windows layout not in effect: %q / %q", l.BinName(), l.LegacyBinName())
	}

	// A stale alias copy of an older version, recorded in the manifest as ours.
	if err := os.WriteFile(l.LegacyExecutablePath(), []byte("BINARY-OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	m.Aliases = []string{l.LegacyBinName()}
	if err := m.save(l.ManifestPath()); err != nil {
		t.Fatal(err)
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v, want %q", res.Aliases, l.LegacyBinName())
	}
	// A real copy, not a symlink, holding the primary's bytes.
	if target := symlinkTarget(l.LegacyExecutablePath()); target != "" {
		t.Errorf("the windows alias must be a copy, not a symlink to %q", target)
	}
	if got := mustContent(t, l.LegacyExecutablePath()); got != "BINARY-v1" {
		t.Errorf("alias copy = %q, want the primary's bytes", got)
	}
	// minio renames the outgoing file to .<bin>.old; the sweep must clear it.
	if swap := filepath.Join(l.ExecutableDir(), "."+l.LegacyBinName()+".old"); pathPresent(swap) {
		t.Errorf("%s was left in the install dir", swap)
	}
}

// TestUpdateRepairsTheAliasOnlyLayoutWhenAlreadyCurrent: the install that most
// needs the primary command name is the one that has none — an install placed
// under the alias name that already updated itself to the latest version. It
// must not have to wait for a release that may never come, so an already-current
// update creates the primary from the running binary's own bytes and points the
// alias at it, reporting a repair rather than an update.
func TestUpdateRepairsTheAliasOnlyLayoutWhenAlreadyCurrent(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	// The release resolves to the version already running: nothing to download.
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Errorf("a layout repair must not report an update: %+v", res)
	}
	if len(res.LayoutRepaired) == 0 {
		t.Fatalf("expected layoutRepaired to describe the fix, got %+v", res)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("primary binary = %q, want the running binary's bytes", got)
	}
	assertAlias(t, l, "BINARY-v1")
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v, want %q", res.Aliases, l.LegacyBinName())
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if m.Executable != l.ExecutablePath() {
		t.Errorf("manifest executable = %q, want the primary %q", m.Executable, l.ExecutablePath())
	}
	if !slices.Contains(m.Aliases, l.LegacyBinName()) {
		t.Errorf("manifest aliases = %v, want %q", m.Aliases, l.LegacyBinName())
	}
	if sum, err := sha256File(l.ExecutablePath()); err != nil || m.SHA256 != sum {
		t.Errorf("manifest sha256 = %q, want the primary's %q (%v)", m.SHA256, sum, err)
	}

	// Running it again changes nothing and reports no repair — the layout is
	// correct, so this must be idempotent rather than rewriting on every check.
	c.exeOverride = l.ExecutablePath()
	again, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.LayoutRepaired) != 0 {
		t.Errorf("a healthy layout must need no repair: %v", again.LayoutRepaired)
	}
}

// TestUpdateRecreatesADeletedAlias: an alias deleted from under a healthy
// install is a broken command, and no new release is needed to put it back.
func TestUpdateRecreatesADeletedAlias(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // adopt: primary + alias
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()
	if err := os.Remove(l.LegacyExecutablePath()); err != nil {
		t.Fatal(err)
	}
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertAlias(t, l, "BINARY-v1")
	if len(res.LayoutRepaired) == 0 {
		t.Errorf("expected the recreated alias to be reported: %+v", res)
	}
}

// TestWindowsAliasIsRecreatedWhenMissing drives the windows branch: the copy
// mechanism must be able to CREATE the alias, not only refresh one. minio's
// Apply starts by renaming the existing target aside, so an absent alias needs
// the plain atomic create instead — without it a deleted `korbit.exe` could
// never come back.
func TestWindowsAliasIsRecreatedWhenMissing(t *testing.T) {
	c := testConfig(t.TempDir())
	c.GOOS = "windows"
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	// An install that owns the alias name, with the file itself gone.
	m, _, _ := loadManifest(l.ManifestPath())
	m.Aliases = []string{l.LegacyBinName()}
	if err := m.save(l.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	if pathPresent(l.LegacyExecutablePath()) {
		t.Fatal("the alias should not exist yet")
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v, want the recreated %q", res.Aliases, l.LegacyBinName())
	}
	if got := mustContent(t, l.LegacyExecutablePath()); got != "BINARY-v1" {
		t.Errorf("recreated alias = %q, want the primary's bytes", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("recreating an absent alias must not warn: %v", res.Warnings)
	}
}

// TestFailedAliasWriteStaysInTheManifest: the manifest records the names this
// install OWNS, so an alias whose write failed is retried by the next run and
// reported as broken in the meantime. Recording only successful writes would
// make one transient failure silently drop the command forever.
func TestFailedAliasWriteStaysInTheManifest(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	if _, err := c.Install(); err != nil { // adopt: primary + alias
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()
	// Make the alias name unwritable by putting a non-empty directory there: the
	// rename that installs the symlink cannot replace it.
	if err := os.Remove(l.LegacyExecutablePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.LegacyExecutablePath(), "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(res.Aliases, l.LegacyBinName()) {
		t.Errorf("result aliases = %v, must not claim an alias that was not written", res.Aliases)
	}
	if !containsSubstr(res.Warnings, "could not keep the") {
		t.Errorf("warnings = %v, want one naming the failed alias", res.Warnings)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if !slices.Contains(m.Aliases, l.LegacyBinName()) {
		t.Errorf("manifest aliases = %v, want the owned name kept for the next retry", m.Aliases)
	}
	// And doctor reports the command as broken rather than silently forgetting it.
	rep, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if p := rep.Problem(FieldAlias); p == "" {
		t.Error("doctor should report the alias that could not be written")
	}
}

// TestDoctorChecksAliasIdentity: an alias that EXISTS but does not run the
// installed binary is the dangerous case — the command works, so nothing looks
// wrong, and it silently runs another binary or a version behind. Presence is
// therefore not enough; the link target and a copy's bytes are both checked.
func TestDoctorChecksAliasIdentity(t *testing.T) {
	t.Run("symlink to the wrong target", func(t *testing.T) {
		c, l := installLegacyOnly(t, "BINARY-v1")
		if _, err := c.Install(); err != nil {
			t.Fatal(err)
		}
		c.exeOverride = l.ExecutablePath()
		// Healthy first.
		rep, err := c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Aliases) != 1 || !rep.Aliases[0].Valid {
			t.Fatalf("a correct alias must be valid: %+v", rep.Aliases)
		}
		if p := rep.Problem(FieldAlias); p != "" {
			t.Errorf("a correct alias must not be a problem: %q", p)
		}

		// Repoint it at something else entirely.
		other := filepath.Join(l.ExecutableDir(), "something-else")
		if err := os.WriteFile(other, []byte("NOT-OURS"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(l.LegacyExecutablePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("something-else", l.LegacyExecutablePath()); err != nil {
			t.Fatal(err)
		}
		rep, err = c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Aliases) != 1 || rep.Aliases[0].Valid {
			t.Fatalf("an alias pointing elsewhere must not be valid: %+v", rep.Aliases)
		}
		p := rep.Problem(FieldAlias)
		if !strings.Contains(p, "something-else") || !strings.Contains(p, "self update") {
			t.Errorf("alias problem = %q, want the wrong target and the fixing verb", p)
		}
		if rep.OK() {
			t.Error("an alias running the wrong binary must make doctor report needs-attention")
		}

		// And the verb the problem names actually heals it, with no new release.
		c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}
		ures, err := c.Update(context.Background(), UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ures.LayoutRepaired) == 0 {
			t.Errorf("expected the repointed alias to be reported: %+v", ures)
		}
		assertAlias(t, l, "BINARY-v1")
		rep, err = c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Aliases) != 1 || !rep.Aliases[0].Valid {
			t.Fatalf("the alias must be valid after the repair: %+v", rep.Aliases)
		}
		if p := rep.Problem(FieldAlias); p != "" {
			t.Errorf("doctor still reports an alias problem after the fix: %q", p)
		}
	})

	t.Run("stale windows copy", func(t *testing.T) {
		c := testConfig(t.TempDir())
		c.GOOS = "windows"
		c.exeOverride = writeFakeBinary(t, "BINARY-v1")
		if _, err := c.Install(); err != nil {
			t.Fatal(err)
		}
		l := c.Layout()
		// An alias copy of an older build, recorded as ours.
		if err := os.WriteFile(l.LegacyExecutablePath(), []byte("BINARY-OLD"), 0o755); err != nil {
			t.Fatal(err)
		}
		m, _, _ := loadManifest(l.ManifestPath())
		m.Aliases = []string{l.LegacyBinName()}
		if err := m.save(l.ManifestPath()); err != nil {
			t.Fatal(err)
		}
		c.exeOverride = l.ExecutablePath()

		rep, err := c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Aliases) != 1 || rep.Aliases[0].Valid {
			t.Fatalf("a stale copy must not be valid: %+v", rep.Aliases)
		}
		if p := rep.Problem(FieldAlias); !strings.Contains(p, "stale copy") {
			t.Errorf("alias problem = %q, want it named as a stale copy", p)
		}

		// `self update` is the verb the problem names, so it must be the verb that
		// heals it — on an install that is already current, with no release to
		// download. Without that, the stale copy runs an old version forever while
		// doctor complains about it.
		c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}
		ures, err := c.Update(context.Background(), UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if ures.Updated {
			t.Errorf("no version changed, so updated must stay false: %+v", ures)
		}
		if len(ures.LayoutRepaired) == 0 {
			t.Errorf("expected the refreshed copy to be reported: %+v", ures)
		}
		if got := mustContent(t, l.LegacyExecutablePath()); got != "BINARY-v1" {
			t.Errorf("alias copy = %q, want the primary's bytes", got)
		}
		rep, err = c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Aliases) != 1 || !rep.Aliases[0].Valid {
			t.Fatalf("a refreshed copy must be valid: %+v", rep.Aliases)
		}
		if p := rep.Problem(FieldAlias); p != "" {
			t.Errorf("doctor still reports an alias problem after the fix: %q", p)
		}

		// A now-valid copy must not be re-swapped on every later check.
		again, err := c.Update(context.Background(), UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(again.LayoutRepaired) != 0 {
			t.Errorf("a healthy alias must need no repair: %v", again.LayoutRepaired)
		}
	})
}

// TestManifestAliasNamesAreValidated: every alias name in the manifest becomes a
// path that install overwrites and uninstall DELETES, and the manifest is a
// plain file any process can rewrite. So only the one managed name is ever
// honored — a name that escapes the install dir is ignored, not resolved.
func TestManifestAliasNamesAreValidated(t *testing.T) {
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	// A traversal that would resolve to the CLI home's config file, plus an
	// unrelated command name.
	escape := filepath.Join("..", "config.json")
	m, _, _ := loadManifest(l.ManifestPath())
	m.Aliases = []string{escape, "someone-elses-tool", l.LegacyBinName()}
	if err := m.save(l.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	// A real file where the traversal would land, so a deletion would be visible.
	victim := l.AliasPath(escape)
	if err := os.WriteFile(victim, []byte("MINE"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := c.aliasPaths(l, m); len(got) != 1 || got[0] != l.LegacyExecutablePath() {
		t.Fatalf("aliasPaths = %v, want only %q", got, l.LegacyExecutablePath())
	}
	for _, st := range c.aliasStatuses(l, m, c.exeOverride) {
		if st.Name != l.LegacyBinName() {
			t.Errorf("aliasStatuses reported an unmanaged name %q", st.Name)
		}
	}
	// And an uninstall that removes the binary leaves the traversal target alone.
	if _, err := c.Uninstall(UninstallOptions{RemoveBinary: true}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "MINE" {
		t.Fatalf("the traversal target was touched: %q, %v", b, err)
	}
}

// TestDoctorNotesAreNotProblems: the states that work but could be tidier are
// reported as notes and leave the install healthy — an install carrying the
// earlier product's names is supported, not broken, so `self doctor` must not
// start failing for it.
func TestDoctorNotesAreNotProblems(t *testing.T) {
	t.Run("alias-only layout", func(t *testing.T) {
		c, l := installLegacyOnly(t, "BINARY-v1")
		rep, err := c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if !rep.LegacyLayout {
			t.Error("the alias-only layout was not reported")
		}
		if !containsSubstr(rep.Notes, "self update") {
			t.Errorf("notes = %v, want one naming the verb that adds %q", rep.Notes, l.BinName())
		}
		if p := rep.Problem(FieldBinary); p != "" {
			t.Errorf("the alias-only layout must not be a problem: %q", p)
		}
	})

	t.Run("unmanaged file at the alias name", func(t *testing.T) {
		c := testConfig(t.TempDir())
		c.exeOverride = writeFakeBinary(t, "BINARY-v1")
		if _, err := c.Install(); err != nil {
			t.Fatal(err)
		}
		l := c.Layout()
		if err := os.WriteFile(l.LegacyExecutablePath(), []byte("#!/bin/sh\nexec other\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		c.exeOverride = l.ExecutablePath()

		rep, err := c.Doctor()
		if err != nil {
			t.Fatal(err)
		}
		if !containsSubstr(rep.Notes, "not a file this install manages") {
			t.Errorf("notes = %v, want one naming the unmanaged file", rep.Notes)
		}
		if len(rep.Aliases) != 0 {
			t.Errorf("an unowned file must not be reported as our alias: %+v", rep.Aliases)
		}
		if p := rep.Problem(FieldAlias); p != "" {
			t.Errorf("someone else's file is not an alias problem: %q", p)
		}
	})
}

// TestUpdateDryRunOnACurrentInstallWritesNothing: `self update --dry-run` is a
// read-only question, and being already current does not make it a write. It
// reports the layout fixes a real run WOULD make, with CheckedOnly set, and
// leaves every byte and timestamp in the install dir and the home alone.
func TestUpdateDryRunOnACurrentInstallWritesNothing(t *testing.T) {
	c, l := installLegacyOnly(t, "BINARY-v1")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}

	before := snapshotTree(t, l.ExecutableDir(), l.Home())
	res, err := c.Update(context.Background(), UpdateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.CheckedOnly {
		t.Errorf("a dry run must report checkedOnly: %+v", res)
	}
	if res.Updated {
		t.Errorf("a dry run must not report an update: %+v", res)
	}
	// The alias-only layout needs the primary created, so the dry run must SAY so.
	if len(res.LayoutRepaired) == 0 {
		t.Errorf("expected the pending fixes to be reported: %+v", res)
	}
	if !containsSubstr(res.LayoutRepaired, l.BinName()) {
		t.Errorf("layoutRepaired = %v, want the missing %q named", res.LayoutRepaired, l.BinName())
	}
	// And nothing may have moved on disk.
	if pathPresent(l.ExecutablePath()) {
		t.Error("a dry run created the primary binary")
	}
	if diff := treeDiff(before, snapshotTree(t, l.ExecutableDir(), l.Home())); diff != "" {
		t.Errorf("a dry run changed the install:\n%s", diff)
	}

	// The real run then does what the dry run described.
	res, err = c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckedOnly {
		t.Errorf("a real run must not report checkedOnly: %+v", res)
	}
	if !fileExists(l.ExecutablePath()) {
		t.Error("the real run did not create the primary binary")
	}
}

// snapshotTree records the name, size, mode, and content hash of every entry
// under the given directories, so a test can assert that an operation changed
// nothing at all rather than only checking the files it thought to name.
func snapshotTree(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // an absent dir is a legitimate state to snapshot
			}
			fi, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			desc := fmt.Sprintf("mode=%s", fi.Mode())
			if fi.Mode().IsRegular() {
				sum, herr := sha256File(path)
				if herr != nil {
					sum = "unreadable"
				}
				desc += fmt.Sprintf(" size=%d sha=%s", fi.Size(), sum)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				desc += " -> " + symlinkTarget(path)
			}
			out[path] = desc
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// treeDiff describes how two snapshots differ, or "" when they are identical.
func treeDiff(before, after map[string]string) string {
	var lines []string
	for path, was := range before {
		switch now, ok := after[path]; {
		case !ok:
			lines = append(lines, "removed: "+path)
		case now != was:
			lines = append(lines, fmt.Sprintf("changed: %s (%s -> %s)", path, was, now))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			lines = append(lines, "created: "+path)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
