// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	legacyBotDB  = "korbit-bot.db"
	currentBotDB = "digitalx-bot.db"
)

// seedLegacyBotDB runs one monitor with an explicit --db pointed at the LEGACY
// bot-database name, leaving a real SQLite file with a row in it.
func seedLegacyBotDB(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, legacyBotDB)
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1", "--db", path,
			"--init", `await db.exec("CREATE TABLE IF NOT EXISTS t (n INTEGER)"); await db.exec("INSERT INTO t (n) VALUES (7)")`,
			"--on", `""`},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("seeding the legacy bot db: exit=%d (stderr %q)", code, errb)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("legacy bot db not created: %v", err)
	}
	return path
}

// TestMonitorPlainStreamingLeavesTheBotDBAlone: a run with no script never opens
// the bot database, so it must not rename one either. Resolving the default path
// eagerly would have every `monitor --ticker` run touch it.
func TestMonitorPlainStreamingLeavesTheBotDBAlone(t *testing.T) {
	home := t.TempDir()
	legacy := seedLegacyBotDB(t, home)

	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1"},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("a plain streaming run renamed the bot database: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, currentBotDB)); !os.IsNotExist(err) {
		t.Fatalf("a plain streaming run created a bot database: %v", err)
	}
}

// TestMonitorExplicitDBLeavesTheDefaultAlone: --db is used verbatim, so the
// DEFAULT bot database is never resolved and never renamed.
func TestMonitorExplicitDBLeavesTheDefaultAlone(t *testing.T) {
	home := t.TempDir()
	legacy := seedLegacyBotDB(t, home)
	explicit := filepath.Join(t.TempDir(), "mybot.db")

	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1", "--db", explicit,
			"--init", `await db.exec("CREATE TABLE IF NOT EXISTS u (n INTEGER)")`, "--on", `""`},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("an explicit --db renamed the default bot database: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, currentBotDB)); !os.IsNotExist(err) {
		t.Fatalf("an explicit --db created the default bot database: %v", err)
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("the --db file should exist at %s: %v", explicit, err)
	}
}

// TestMonitorDefaultDBAdoptsTheLegacyName: a script run with no --db resolves the
// default path, which adopts the database left under the earlier product name —
// the script's own tables and rows come with it.
func TestMonitorDefaultDBAdoptsTheLegacyName(t *testing.T) {
	home := t.TempDir()
	legacy := seedLegacyBotDB(t, home)

	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1",
			"--on", `if (ev.type === "data") { var r = await db.get("SELECT n FROM t"); console.error("n=" + r.n) }`},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "n=7") {
		t.Fatalf("the adopted database should carry the seeded row: %q", errb)
	}
	if _, err := os.Stat(filepath.Join(home, currentBotDB)); err != nil {
		t.Fatalf("bot database not adopted onto the current name: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy bot database still present: %v", err)
	}
}
