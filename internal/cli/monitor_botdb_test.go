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

// TestMonitorDefaultDBIsTheHomesOwnName: a script run with no --db opens the
// database the HOME's directory name implies and renames nothing. This home is
// not the earlier product's directory, so a database beside it under the other
// spelling is neither read nor moved — its seeded row is invisible, and a fresh
// database is created under the official name.
func TestMonitorDefaultDBIsTheHomesOwnName(t *testing.T) {
	home := t.TempDir()
	other := filepath.Join(home, "korbit-bot.db")

	// Seed the other spelling through an explicit --db, so it holds a real
	// SQLite file with a row in it.
	_, errb, code := runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1", "--db", other,
			"--init", `await db.exec("CREATE TABLE IF NOT EXISTS t (n INTEGER)"); await db.exec("INSERT INTO t (n) VALUES (7)")`,
			"--on", `""`},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("seeding the other bot db: exit=%d (stderr %q)", code, errb)
	}

	_, errb, code = runMonitorCLI(
		[]string{"monitor", "--symbols", "btc_krw", "--ticker", "--json", "--max-events", "1",
			"--init", `await db.exec("CREATE TABLE IF NOT EXISTS t (n INTEGER)")`,
			"--on", `if (ev.type === "data") { var r = await db.get("SELECT count(*) AS c FROM t"); console.error("c=" + r.c) }`},
		map[string]string{"DIGITALX_CLI_HOME": home},
		failingDoer(), dialFrames(tickerFrame("100")))
	if code != 0 {
		t.Fatalf("exit=%d (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "c=0") {
		t.Fatalf("the run should have opened a fresh database, not the other spelling: %q", errb)
	}
	if _, err := os.Stat(filepath.Join(home, "bot.db")); err != nil {
		t.Fatalf("bot database not created under the home's own name: %v", err)
	}
	if b, err := os.ReadFile(other); err != nil || len(b) == 0 {
		t.Fatalf("the other spelling was moved or emptied: %v", err)
	}
}
