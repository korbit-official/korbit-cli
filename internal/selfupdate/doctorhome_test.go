// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/cli/monitorcmd"
	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/journal"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox"
)

// The CLI home diagnosis. Nothing in this CLI moves a home, renames one, or
// renames a file inside one, so doctor is the only place the two generations of
// names are spoken about at all — and it speaks about exactly two states, both
// of which mean the user has data the CLI is not reading. A home simply sitting
// under the earlier directory name is not one of them: it works as it is.

// testHomeDBNames is the same list the cli layer wires into Config.HomeDBNames
// (selfcmd.homeDBNames), built from the SAME constants the owning packages
// export — so a renamed database in journal / monitorcmd / sandbox shows up here
// rather than leaving these tests passing against names nothing uses.
func testHomeDBNames() []HomeDBName {
	dir := filepath.Base(sandbox.StateDir("")) + "/"
	return []HomeDBName{
		{Current: journal.DefaultFileName, Legacy: journal.LegacyFileName},
		{Current: monitorcmd.BotDBDefaultName, Legacy: monitorcmd.LegacyBotDBFileName},
		{Current: dir + sandbox.DBDefaultName, Legacy: dir + sandbox.LegacyDBFileName},
	}
}

// unpinnedConfig builds a Config rooted at a temp OS user home with NO home
// environment variable set, so Layout.Home() resolves by the directory-existence
// rule the home diagnosis is about. (testConfig pins KORBIT_CLI_HOME.)
func unpinnedConfig(t *testing.T, env map[string]string) Config {
	t.Helper()
	userHome := t.TempDir()
	return Config{
		Getenv: func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			switch k {
			case "HOME", "USERPROFILE":
				return userHome
			case "LOCALAPPDATA":
				return filepath.Join(userHome, "AppData", "Local")
			case "PATH":
				return "/usr/bin:/bin"
			}
			return ""
		},
		Now:      func() int64 { return 1700000000000 },
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Version:  "v1.0.0",
		Repo:     DefaultRepo,
		Progress: io.Discard,
	}
}

// seedHome creates a CLI home holding the named files (each with its own name as
// content, so a file is identifiable wherever it turns up).
func seedHome(t *testing.T, dir string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// installedDoctor gives c a managed install and returns its report, so a home
// test is about the homes rather than about an unmanaged binary.
func installedDoctor(t *testing.T, c Config) *DoctorReport {
	t.Helper()
	l := c.Layout()
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if err := c.installManifestOnly(l); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = l.ExecutablePath()
	r, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDoctorIsSilentAboutAHomeUnderTheEarlierName: a `~/.korbit-cli` holding
// everything the user has, with its earlier-named databases, is a fully
// supported state — the directory's own name is what selects those filenames
// (config.LegacyLayout), so every one of them is the RIGHT name there. Nothing
// moves it and nothing will, so doctor reports neither a problem nor a note
// about it: an install that works is not something to nag about once per run.
func TestDoctorIsSilentAboutAHomeUnderTheEarlierName(t *testing.T) {
	c := unpinnedConfig(t, nil)
	c.HomeDBNames = testHomeDBNames()
	l := c.Layout()
	seedHome(t, l.LegacyHomeDir(), "keys.json", "config.json",
		journal.LegacyFileName, "sandbox/"+sandbox.LegacyDBFileName)

	r := installedDoctor(t, c)
	if r.Home != l.LegacyHomeDir() {
		t.Fatalf("home = %q, want %q", r.Home, l.LegacyHomeDir())
	}
	if p := r.Problem(FieldHome); p != "" {
		t.Errorf("a single home under the earlier name is not a problem: %q", p)
	}
	for _, n := range r.Notes {
		if strings.Contains(n, l.LegacyHomeDir()) || strings.Contains(n, journal.LegacyFileName) {
			t.Errorf("doctor must say nothing about a home that works as it is: %q", n)
		}
	}
}

// TestDoctorProblemForTwoHomesThatBothHoldData: which home the CLI reads is
// decided by nothing the user can see (Layout.Home prefers the current name when
// that directory exists), so keys written to the other one are simply invisible.
// No rule can pick between them, and nothing here merges them, so it is reported
// as a problem naming which one is in use.
func TestDoctorProblemForTwoHomesThatBothHoldData(t *testing.T) {
	c := unpinnedConfig(t, nil)
	c.HomeDBNames = testHomeDBNames()
	l := c.Layout()
	seedHome(t, l.LegacyHomeDir(), "keys.json")
	seedHome(t, l.CurrentHomeDir(), "keys.json")

	r := installedDoctor(t, c)
	msg := r.Problem(FieldHome)
	if msg == "" {
		t.Fatalf("no `%s` problem for two homes holding data: %+v", FieldHome, r)
	}
	for _, want := range []string{l.LegacyHomeDir(), l.CurrentHomeDir(), "only " + l.CurrentHomeDir() + " is in use", "MIGRATION.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the problem does not mention %q: %q", want, msg)
		}
	}
	if r.OK() {
		t.Error("two CLI homes holding data must make doctor report needs-attention")
	}

	// A leftover directory holding nothing but this install's own bookkeeping
	// hides no data, so it is not reported at all.
	if err := os.Remove(filepath.Join(l.LegacyHomeDir(), "keys.json")); err != nil {
		t.Fatal(err)
	}
	if r := installedDoctor(t, c); r.Problem(FieldHome) != "" {
		t.Errorf("an empty leftover home is not a problem: %q", r.Problem(FieldHome))
	}
}

// TestDoctorProblemForLegacyFileNamesInACurrentHome: `korbit-cli.db` inside
// `~/.digitalx-cli` is the state a directory renamed without its files leaves
// behind. The home's own name says the current filenames are what it reads, so
// the CLI opens fresh empty databases beside a full set — the data is there and
// invisible, and no command fixes it, so the remedy is spelled out.
func TestDoctorProblemForLegacyFileNamesInACurrentHome(t *testing.T) {
	c := unpinnedConfig(t, nil)
	c.HomeDBNames = testHomeDBNames()
	l := c.Layout()
	seedHome(t, l.CurrentHomeDir(), "keys.json", journal.LegacyFileName)

	r := installedDoctor(t, c)
	msg := r.Problem(FieldHome)
	if msg == "" {
		t.Fatalf("no `%s` problem for a home holding earlier-named databases: %+v", FieldHome, r)
	}
	for _, want := range []string{
		l.CurrentHomeDir(), journal.LegacyFileName, journal.DefaultFileName,
		l.BinName(), l.LegacyBinName(), "MIGRATION.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the problem does not mention %q: %q", want, msg)
		}
	}
	if r.OK() {
		t.Error("data the CLI cannot see must fail the health check")
	}

	// The reverse is silent: the earlier names ARE the right ones in a home named
	// `.korbit-cli`, which is the whole compatibility rule.
	c2 := unpinnedConfig(t, nil)
	c2.HomeDBNames = testHomeDBNames()
	l2 := c2.Layout()
	seedHome(t, l2.LegacyHomeDir(), "keys.json", journal.DefaultFileName)
	if r := installedDoctor(t, c2); r.Problem(FieldHome) != "" {
		t.Errorf("a `%s` home holding a current-named file is not a problem: %q", config.LegacyDirName, r.Problem(FieldHome))
	}
}

// TestDoctorProblemForLegacyFileNamesInAPinnedHome: the same layout mismatch in
// a home the user pinned, where the second remedy applies — pointing the
// variable at a directory named `.korbit-cli` keeps the earlier filenames, which
// is what a still-installed pre-rename binary sharing that directory needs.
func TestDoctorProblemForLegacyFileNamesInAPinnedHome(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "cli-home")
	c := unpinnedConfig(t, map[string]string{config.EnvHome: pinned})
	c.HomeDBNames = testHomeDBNames()
	seedHome(t, pinned, "keys.json", "sandbox/"+sandbox.LegacyDBFileName)

	r := installedDoctor(t, c)
	msg := r.Problem(FieldHome)
	if msg == "" {
		t.Fatalf("no `%s` problem for a pinned home holding earlier-named databases: %+v", FieldHome, r)
	}
	for _, want := range []string{config.EnvHome, pinned, "sandbox/" + sandbox.LegacyDBFileName, config.LegacyDirName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the problem does not mention %q: %q", want, msg)
		}
	}

	// A pinned home whose basename IS `.korbit-cli` reads the earlier names, so
	// holding them is exactly right.
	legacyPinned := filepath.Join(t.TempDir(), config.LegacyDirName)
	c2 := unpinnedConfig(t, map[string]string{config.EnvHome: legacyPinned})
	c2.HomeDBNames = testHomeDBNames()
	seedHome(t, legacyPinned, "keys.json", journal.LegacyFileName)
	if r := installedDoctor(t, c2); r.Problem(FieldHome) != "" {
		t.Errorf("a pinned `%s` home is healthy: %q", config.LegacyDirName, r.Problem(FieldHome))
	}
}
