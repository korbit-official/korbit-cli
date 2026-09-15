// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/journal"
)

// Upgrading from a binary that shipped under the EARLIER product name takes TWO
// `korbit self update` runs: the code that creates the `digitalx` command lives
// in the binary being DOWNLOADED, and the process performing the download is the
// old one, which knows only its own command name. So the first run swaps
// `korbit` and stops there; the second — now executing the new code — repairs
// the layout. MIGRATION.md documents that two-step, and these tests pin both
// halves: what the first run leaves behind, and that the second run repairs
// exactly that state.

// oldBinaryUpdateOutcome reproduces on disk what the SHIPPED korbit-era update
// code leaves behind after it installs release newVersion: the binary at the
// `korbit` name holds the new bytes, the manifest names that path and that
// version, it records no aliases (the field did not exist), and there is no
// `digitalx` at all.
func oldBinaryUpdateOutcome(t *testing.T, l Layout, c Config, newVersion, newBytes string) {
	t.Helper()
	if err := os.MkdirAll(l.ExecutableDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	// The old code swaps its own name in place, and only that name.
	if err := os.WriteFile(l.LegacyExecutablePath(), []byte(newBytes), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(l.ExecutablePath())
	m := Manifest{
		Method:      MethodManagedScript,
		Executable:  l.LegacyExecutablePath(),
		Version:     newVersion,
		OS:          c.os(),
		Arch:        c.arch(),
		SHA256:      sha256Bytes([]byte(newBytes)),
		Repo:        c.repo(),
		InstalledAt: strconv.FormatInt(c.now(), 10),
	}
	if err := m.save(l.ManifestPath()); err != nil {
		t.Fatal(err)
	}
}

// TestFirstUpdateFromTheOldBinaryLeavesNoPrimaryCommand pins the FIRST run's
// outcome — the shape the old binary produces, and therefore the precondition
// the repair below starts from.
func TestFirstUpdateFromTheOldBinaryLeavesNoPrimaryCommand(t *testing.T) {
	c := testConfig(t.TempDir())
	c.Version = "v2.0.0"
	l := c.Layout()
	oldBinaryUpdateOutcome(t, l, c, "v2.0.0", "BINARY-v2")

	if pathPresent(l.ExecutablePath()) {
		t.Fatalf("the old update code cannot create %s — the code that does lives in the binary it downloaded", l.BinName())
	}
	if !fileExists(l.LegacyExecutablePath()) {
		t.Fatalf("the old update code swaps %s in place", l.LegacyBinName())
	}
	m, found, err := loadManifest(l.ManifestPath())
	if err != nil || !found {
		t.Fatalf("manifest: found=%v err=%v", found, err)
	}
	if m.Executable != l.LegacyExecutablePath() {
		t.Errorf("manifest executable = %q, want the alias path %q", m.Executable, l.LegacyExecutablePath())
	}
	if len(m.Aliases) != 0 {
		t.Errorf("manifest aliases = %v, want none (the old code did not write the field)", m.Aliases)
	}
	if m.Version != "v2.0.0" {
		t.Errorf("manifest version = %q, want the release the first run installed", m.Version)
	}

	// And that is precisely the state planLayout describes as needing the primary
	// command created — the link between this test and the one below.
	c.exeOverride = l.LegacyExecutablePath()
	plan := c.planLayout(l)
	if !plan.createPrimary {
		t.Fatalf("the first run's outcome must be a state the repair recognizes: %+v", plan)
	}
}

// TestSecondUpdateFromTheOldBinaryCreatesThePrimaryCommand: the second
// `korbit self update` runs the NEW code, finds itself already at the target
// version, and repairs the layout — which is where `digitalx` finally comes from.
// No new release is needed, and none is downloaded.
func TestSecondUpdateFromTheOldBinaryCreatesThePrimaryCommand(t *testing.T) {
	c := testConfig(t.TempDir())
	c.Version = "v2.0.0"
	l := c.Layout()
	oldBinaryUpdateOutcome(t, l, c, "v2.0.0", "BINARY-v2")
	c.exeOverride = l.LegacyExecutablePath()
	// The release resolves to the version already running: nothing to download.
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName()}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Errorf("nothing was updated, only repaired: %+v", res)
	}
	if len(res.LayoutRepaired) == 0 {
		t.Fatalf("expected the repair to be reported: %+v", res)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v2" {
		t.Errorf("%s = %q, want the running binary's current bytes", l.BinName(), got)
	}
	assertAlias(t, l, "BINARY-v2")
	m, _, _ := loadManifest(l.ManifestPath())
	if m.Executable != l.ExecutablePath() {
		t.Errorf("manifest executable = %q, want the primary %q", m.Executable, l.ExecutablePath())
	}
	if !slices.Contains(m.Aliases, l.LegacyBinName()) {
		t.Errorf("manifest aliases = %v, want %q", m.Aliases, l.LegacyBinName())
	}
	if want := sha256Bytes([]byte("BINARY-v2")); m.SHA256 != want {
		t.Errorf("manifest sha256 = %q, want the installed binary's %q", m.SHA256, want)
	}
}

// TestRepairNeverBlessesAStalePrimary: the running binary carries the alias name
// and its bytes ARE the manifest's, while a file at the primary name hashes
// differently — a stale copy an interrupted repair left
// behind. Adopting it would make `digitalx` a version behind while every check
// called it healthy, so it is recreated from the running verified bytes.
//
// Two consecutive repairs are run because the failure this guards against is
// self-reinforcing: a repair that recorded the stale hash would make the second
// run agree with it.
func TestRepairNeverBlessesAStalePrimary(t *testing.T) {
	c := testConfig(t.TempDir())
	l := c.Layout()
	// A healthy install, then made to look like the alias-only layout: the running
	// binary is a real file at the alias name, recorded in the manifest.
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
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
	// A STALE file at the primary name, of a version this install never recorded.
	if err := os.WriteFile(l.ExecutablePath(), []byte("BINARY-v0-STALE"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.LegacyExecutablePath()
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}

	for i := 1; i <= 2; i++ {
		res, err := c.Update(context.Background(), UpdateOptions{})
		if err != nil {
			t.Fatalf("repair %d: %v", i, err)
		}
		if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
			t.Fatalf("repair %d: %s = %q, want the verified running bytes", i, l.BinName(), got)
		}
		if got := mustContent(t, l.LegacyExecutablePath()); got != "BINARY-v1" {
			t.Fatalf("repair %d: %s = %q, want the verified bytes", i, l.LegacyBinName(), got)
		}
		got, _, _ := loadManifest(l.ManifestPath())
		if want := sha256Bytes([]byte("BINARY-v1")); got.SHA256 != want {
			t.Fatalf("repair %d: manifest sha256 = %q, want the verified %q — the stale primary was blessed", i, got.SHA256, want)
		}
		if i == 1 && len(res.LayoutRepaired) == 0 {
			t.Fatalf("repair 1 reported no fix: %+v", res)
		}
		// After the first repair the alias is a symlink onto the primary; run the
		// second repair from the primary path, as a real invocation would.
		c.exeOverride = l.ExecutablePath()
	}
}

// TestDoctorReportsAStalePrimary: the same state doctor must not call healthy.
// The `digitalx` command exists, so nothing looks wrong — which is exactly why it
// is a problem and not a note, and why it must be reported by the same predicate
// the repair uses.
func TestDoctorReportsAStalePrimary(t *testing.T) {
	c := testConfig(t.TempDir())
	l := c.Layout()
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(l.ExecutablePath(), []byte("BINARY-v0-STALE"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.LegacyExecutablePath()

	r, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if msg := r.Problem(FieldBinary); msg == "" {
		t.Fatalf("doctor called a stale `%s` healthy: %+v", l.BinName(), r)
	}
	if r.OK() {
		t.Error("a stale primary command must fail the health check")
	}
	// And it must not ALSO tell the user to add a command that is sitting there.
	for _, note := range r.Notes {
		if containsSubstr([]string{note}, "to add the `"+l.BinName()+"` command") {
			t.Errorf("contradictory note beside the problem: %q", note)
		}
	}
}

// TestDoctorProblemForATrivialHomeInUse: the CLI is
// reading an empty home right now while the other one holds the keys. That is
// not ambiguity to note — it is a definite "your keys are invisible", so it fails
// the health check, and MIGRATION.md's doctor section says so.
func TestDoctorProblemForATrivialHomeInUse(t *testing.T) {
	c := unpinnedConfig(t, nil)
	c.HomeDBNames = testHomeDBNames()
	l := c.Layout()
	seedHome(t, l.LegacyHomeDir(), "keys.json", "config.json")
	seedHome(t, l.CurrentHomeDir(), "self.lock")
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	// Give it a managed install without touching either home, so the report is
	// about the two homes rather than about anything this setup changed.
	if err := c.installManifestOnly(l); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()

	r, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	msg := r.Problem(FieldHome)
	if msg == "" {
		t.Fatalf("no `%s` problem for a trivial home in use beside a home holding data: %+v", FieldHome, r)
	}
	if !containsSubstr([]string{msg}, l.LegacyHomeDir()) || !containsSubstr([]string{msg}, l.CurrentHomeDir()) {
		t.Errorf("the problem must name both homes: %q", msg)
	}
	// And it must say WHICH way round it is. "Two homes exist, one of them is
	// invisible" understates this state: the home being read holds nothing, so
	// the keys are definitely not visible, not merely maybe.
	if !containsSubstr([]string{msg}, "holds no keys") {
		t.Errorf("the problem does not state that the home in use is the empty one: %q", msg)
	}
	if r.OK() {
		t.Error("the health check must fail while the CLI reads an empty home")
	}
}

// installManifestOnly writes just the manifest for l, so a doctor test can
// exercise the home diagnosis on a managed install without a full Install
// touching the two homes it is about.
func (c Config) installManifestOnly(l Layout) error {
	if err := os.MkdirAll(l.Home(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(l.ExecutableDir(), 0o755); err != nil {
		return err
	}
	src, err := c.runningExe()
	if err != nil {
		return err
	}
	if err := copyFileAtomic(l.ExecutablePath(), src, 0o755); err != nil {
		return err
	}
	sum, err := sha256File(l.ExecutablePath())
	if err != nil {
		return err
	}
	_, err = c.reconcileManifest(l, sum, nil)
	return err
}

// TestDoctorReportsAStrayFileNameLayoutOnlyForTheHomeInUse: the file-layout
// diagnosis is about the home this CLI actually reads. A directory at the OTHER
// standard location is not one any command of this install touches, so its file
// names are not this report's business — that it holds data at all is what
// diagnoseHome says, and saying more would be advice about somebody else's
// directory.
func TestDoctorReportsAStrayFileNameLayoutOnlyForTheHomeInUse(t *testing.T) {
	c := unpinnedConfig(t, nil)
	c.HomeDBNames = testHomeDBNames()
	l := c.Layout()
	seedHome(t, l.CurrentHomeDir(), "keys.json")
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	// An earlier-named database in a home whose directory name says the current
	// names are what belongs there — the home in use, so it is reported.
	stray := filepath.Join(l.CurrentHomeDir(), journal.LegacyFileName)
	if err := os.WriteFile(stray, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()

	r, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Problem(FieldHome); !strings.Contains(p, journal.LegacyFileName) {
		t.Fatalf("the `%s` problem does not name the stray file: %q", FieldHome, p)
	}

	// The same file inside the home NOT in use is silent.
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	seedHome(t, l.LegacyHomeDir(), journal.LegacyFileName)
	r, err = c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Problem(FieldHome); strings.Contains(p, journal.LegacyFileName) {
		t.Errorf("doctor reports file names inside a home it does not read: %q", p)
	}
}
