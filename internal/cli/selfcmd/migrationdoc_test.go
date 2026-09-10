// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicfg "github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/envalias"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox"
	"github.com/digitalx-official/digitalx-cli/internal/selfupdate"
)

// TestMigrationDocMatchesTheCode: MIGRATION.md is the user-facing contract for
// the two generations of names, and — since NOTHING in this CLI moves or renames
// a home, a cache, or a file inside them — it is also the only place the manual
// move is written down. A user follows it literally, by hand, so a name in it
// that the code no longer uses is a `mv` onto the wrong path.
//
// It checks tokens, not prose — a doc can always be wrong in ways no test
// catches — but it covers everything the reader types or looks for: both home
// directories, both spellings of every database and its sidecars, the
// environment variables, and the doctor diagnosis tags. The names are read from
// the SAME constants the code uses, so renaming one in its own package fails
// here rather than shipping with the doc naming the old one.
//
// It also pins that no paragraph claims a command moves or renames anything of
// yours.
func TestMigrationDocMatchesTheCode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "MIGRATION.md"))
	if err != nil {
		t.Fatalf("reading MIGRATION.md: %v", err)
	}
	doc := string(raw)
	// mustQuote requires a value to appear as a `code span`, so a token cannot be
	// satisfied by an unrelated sentence that happens to contain the word.
	mustQuote := func(what, value string) {
		t.Helper()
		if !strings.Contains(doc, "`"+value+"`") {
			t.Errorf("MIGRATION.md does not document the %s %q", what, value)
		}
	}

	for _, field := range selfupdate.Fields() {
		mustQuote("doctor diagnosis", field)
	}

	// The variable names the home- and cache-resolution lists are written in.
	for _, name := range []string{
		clicfg.EnvHome, selfupdate.LegacyEnvHome,
		sandbox.EnvCacheDir, legacyCacheEnv(t),
	} {
		mustQuote("environment variable", name)
	}

	// The two standard home directories, in the ~-relative form a user sees, plus
	// the bare directory name — because the file-layout rule is about the
	// directory's own name wherever that directory happens to be, including a home
	// pinned elsewhere.
	for _, dir := range []string{clicfg.DirName, clicfg.LegacyDirName} {
		mustQuote("home directory", "~/"+dir)
	}
	mustQuote("home directory name (the file-layout rule)", clicfg.LegacyDirName)

	// Every database a home can hold, in BOTH spellings, home-relative — read from
	// the same list `self doctor` diagnoses against, so an added or renamed
	// database cannot leave the hand-rename recipe naming the old name.
	for _, d := range homeDBNames() {
		mustQuote("home file name", d.Current)
		mustQuote("earlier home file name", d.Legacy)
	}

	// And the sidecars, since a database moved without its `-wal` loses whatever
	// had not been folded into it yet — the recipe has to name each one.
	for _, suffix := range append([]string{"-wal", "-shm"}, sandbox.DBCompanionSuffixes()...) {
		mustQuote("database sidecar suffix", suffix)
	}

	// No paragraph may say a command moves, migrates, or renames anything without
	// saying it does NOT. The scan is by paragraph, not by line: this document is
	// hard-wrapped, so a verb and its subject almost never share a line, and a
	// line-by-line check would pass on prose that says exactly the wrong thing.
	verbs := []string{"moves ", "migrates", "renames ", "relocates"}
	negations := []string{"no directory", "no file", "moves no", "renames no", "never", "nothing", "Nothing", "no command", "No command"}
	for _, para := range docParagraphs(doc) {
		if !mentionsACommand(para) {
			continue
		}
		for _, verb := range verbs {
			if !strings.Contains(para, verb) {
				continue
			}
			if !containsAny(para, negations) {
				t.Errorf("a MIGRATION.md paragraph says a command %q; nothing in this CLI does:\n%s", verb, para)
			}
		}
	}
}

// mentionsACommand reports whether a paragraph is about one of the verbs that
// touch an install, which are the ones a claim about moving data would attach to.
func mentionsACommand(para string) bool {
	return containsAny(para, []string{"self install", "self update", "self doctor", "self uninstall", "install one-liner"})
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// docParagraphs splits a hard-wrapped Markdown document into paragraphs —
// blank-line-separated blocks, joined onto one line each — so an assertion about
// a sentence is made against the whole sentence. A table row and a list item each
// stand alone, so a "never" in one cannot excuse a claim in the next.
func docParagraphs(doc string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trimmed, "|") {
			flush()
			out = append(out, trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			flush()
		}
		cur = append(cur, trimmed)
	}
	flush()
	return out
}

// legacyCacheEnv is the accepted older spelling of the cache variable, derived
// the way the CLI derives it so the test cannot drift from what is honored.
func legacyCacheEnv(t *testing.T) string {
	t.Helper()
	name, ok := envalias.LegacyName(sandbox.EnvCacheDir)
	if !ok {
		t.Fatalf("%s has no legacy spelling", sandbox.EnvCacheDir)
	}
	return name
}
