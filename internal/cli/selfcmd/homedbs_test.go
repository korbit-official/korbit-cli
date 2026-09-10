// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/cli/monitorcmd"
	"github.com/digitalx-official/digitalx-cli/internal/journal"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox"
	"github.com/digitalx-official/digitalx-cli/internal/selfupdate"
)

// TestHomeDBNamesCoverEveryDatabaseUnderTheHome: this list is what `self doctor`
// checks a CLI home against, so it can report a home holding databases its own
// directory name says it will not read — and it is the list MIGRATION.md's
// rename table is pinned to. A database missing from it is one a user could be
// left unable to see, with no diagnosis and no documented rename.
//
// The assertion is against the OWNING packages' own constants, so adding a
// database under the home without adding it here fails, and renaming one in its
// own package cannot leave this list pointing at a name nothing uses.
func TestHomeDBNamesCoverEveryDatabaseUnderTheHome(t *testing.T) {
	sandboxDir := filepath.Base(sandbox.StateDir("")) + "/"
	want := map[string]string{
		journal.DefaultFileName:            journal.LegacyFileName,
		monitorcmd.BotDBDefaultName:        monitorcmd.LegacyBotDBFileName,
		sandboxDir + sandbox.DBDefaultName: sandboxDir + sandbox.LegacyDBFileName,
	}

	got := homeDBNames()
	if len(got) != len(want) {
		t.Fatalf("homeDBNames() has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for _, d := range got {
		legacy, ok := want[d.Current]
		if !ok {
			t.Errorf("homeDBNames() lists an unexpected database %q", d.Current)
			continue
		}
		if d.Legacy != legacy {
			t.Errorf("%s: earlier name = %q, want %q", d.Current, d.Legacy, legacy)
		}
		// The two spellings must differ — a pair that does not is a database the
		// layout rule cannot distinguish, so nothing could ever be reported about it.
		if d.Current == d.Legacy {
			t.Errorf("%s: the two spellings are identical", d.Current)
		}
		// Home-relative, forward slashes: the way a user reads them and the way
		// MIGRATION.md writes them.
		if strings.Contains(d.Current, `\`) || strings.Contains(d.Legacy, `\`) {
			t.Errorf("%s / %s: paths must use forward slashes", d.Current, d.Legacy)
		}
	}
}

// TestDebugBundlesMatchesOnlyTheNamesThisCLIWrites: `debug bundle` names carry a
// timestamp, so an uninstall can only find them by glob — and it must find the
// ones written under the earlier name too, since they are the user's diagnostic
// files either way.
//
// The other half matters just as much: these paths are DELETED, and the CLI home
// is a directory a user may keep their own notes in. So the patterns are the two
// names this CLI has ever written, not a wildcard — a `*-debug-*.json` would
// take `run-debug-2.json` with it, a file the CLI never created.
func TestDebugBundlesMatchesOnlyTheNamesThisCLIWrites(t *testing.T) {
	home := t.TempDir()
	writeFile := func(name string) string {
		t.Helper()
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := []string{
		writeFile("debug-1700000000000.json"),            // current
		writeFile("korbit-cli-debug-1698000000000.json"), // the earlier name
	}
	// Files the CLI never wrote, which an over-broad glob would swallow.
	notOurs := []string{
		writeFile("config.json"),
		writeFile("run-debug-2.json"),
		writeFile("my-debug-notes.json"),
		writeFile("debug.json"),
	}

	got := debugBundles(home)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("debugBundles = %v, want it to include %q", got, w)
		}
	}
	for _, other := range notOurs {
		if slices.Contains(got, other) {
			t.Errorf("debugBundles would delete %q, which this CLI never wrote", other)
		}
	}
	// No path may appear twice — an uninstall would report the same removal twice.
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("debugBundles listed %q twice", p)
		}
		seen[p] = true
	}
}

// TestUninstallHomesCoversAllThreeLocations: the home in use may be the pinned
// or the legacy one while a current-named home also sits on disk holding keys.
// selfupdate's pruneHome removes all three once they are empty, so an uninstall
// that listed fewer would clear one directory's files and then try to delete
// another that still holds data.
func TestUninstallHomesCoversAllThreeLocations(t *testing.T) {
	userHome := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HOME", "USERPROFILE":
			return userHome
		}
		return ""
	}
	legacy := filepath.Join(userHome, ".korbit-cli")
	current := filepath.Join(userHome, ".digitalx-cli")
	pinned := filepath.Join(t.TempDir(), "pinned")
	for _, d := range []string{legacy, current, pinned} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	l := selfupdate.Config{Getenv: getenv, GOOS: runtime.GOOS}.Layout()

	for _, tc := range []struct {
		name  string
		inUse string
		want  []string
	}{
		{name: "pinned elsewhere", inUse: pinned, want: []string{pinned, legacy, current}},
		{name: "the legacy home is in use", inUse: legacy, want: []string{legacy, current}},
		{name: "the current home is in use", inUse: current, want: []string{current, legacy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uninstallHomes(l, tc.inUse)
			if len(got) != len(tc.want) {
				t.Fatalf("uninstallHomes = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("uninstallHomes = %v, want %v", got, tc.want)
				}
			}
		})
	}

	// A standard location that does not exist is not listed — an absent home
	// contributes nothing but noise to the previews.
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	if got := uninstallHomes(l, current); len(got) != 1 || got[0] != current {
		t.Errorf("uninstallHomes = %v, want just the home in use", got)
	}
}
