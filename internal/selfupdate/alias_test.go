// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
		{"linux", "dgx-cli", "korbit"},
		{"darwin", "dgx-cli", "korbit"},
		{"windows", "dgx-cli.exe", "korbit.exe"},
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

// TestAssetNameIsPrimary pins the release asset self update asks for. The
// legacy korbit_<os>_<arch> set exists on the release for older updaters; this
// code must never request it.
func TestAssetNameIsPrimary(t *testing.T) {
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

	res, err := c.Update(context.Background(), "", false)
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
	res, err := c.Update(context.Background(), "", false)
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
		".dgx-cli-abc.tmp",
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
		".dgx-cli-stale.tmp",
	}
	for _, name := range leftovers {
		if err := os.WriteFile(filepath.Join(l.ExecutableDir(), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}
	if _, err := c.Update(context.Background(), "", false); err != nil {
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
	if l.BinName() != "dgx-cli.exe" || l.LegacyBinName() != "korbit.exe" {
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
