// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// seedLegacyState writes a sandbox database (and the companions named) under the
// legacy file name in a fresh home, returning the home.
func seedLegacyState(t *testing.T, companions ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := StateDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, LegacyDBFileName)
	if err := os.WriteFile(legacy, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range companions {
		if err := os.WriteFile(legacy+suffix, []byte(suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writePidfile(t *testing.T, path string, pid, port int) {
	t.Helper()
	body := fmt.Sprintf(`{"pid":%d,"port":%d}`, pid, port)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDBPathAdoptsLegacyDatabase: with no sandbox running, the database written
// under the earlier product name is moved onto the current name, companions
// included — balances, orders, and trades survive the rename.
func TestDBPathAdoptsLegacyDatabase(t *testing.T) {
	home := seedLegacyState(t, "-wal", ".market-snapshot.json")
	m := New(Config{Home: home}, Deps{})

	got := m.dbPath()
	want := filepath.Join(StateDir(home), DBFileName)
	if got != want {
		t.Fatalf("dbPath = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "db" {
		t.Fatalf("adopted database = %q, %v", b, err)
	}
	for _, suffix := range []string{"-wal", ".market-snapshot.json"} {
		if b, err := os.ReadFile(want + suffix); err != nil || string(b) != suffix {
			t.Fatalf("companion %s = %q, %v", suffix, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(StateDir(home), LegacyDBFileName)); !os.IsNotExist(err) {
		t.Fatalf("legacy database still present: %v", err)
	}
}

// TestDBPathAdoptsPastAStalePidfile: only a pidfile naming a LIVE process holds
// adoption back. A pidfile left behind by a crashed run — unparseable, or naming
// no process at all — must not strand the database on the legacy name forever.
func TestDBPathAdoptsPastAStalePidfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no process", `{"pid":0,"port":0}`},
		{"malformed", "not json"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := seedLegacyState(t)
			legacy := filepath.Join(StateDir(home), LegacyDBFileName)
			if err := os.WriteFile(legacy+"-pid", []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m := New(Config{Home: home}, Deps{})
			if got, want := m.dbPath(), filepath.Join(StateDir(home), DBFileName); got != want {
				t.Fatalf("dbPath = %q, want %q", got, want)
			}
		})
	}
}

// answeringSandbox starts a loopback server that answers the readiness probe
// (/v2/time) and returns the port it bound.
func answeringSandbox(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestDBPathKeepsLegacyNameWhileRunning: a live sandbox is still serving the
// legacy database — its pid is alive AND its port answers. Renaming it out from
// under that server would leave it writing to a path nothing can find, so this
// process keeps using the legacy name, and every path derived from it (pidfile,
// market snapshot) follows.
func TestDBPathKeepsLegacyNameWhileRunning(t *testing.T) {
	home := seedLegacyState(t)
	legacy := filepath.Join(StateDir(home), LegacyDBFileName)
	writePidfile(t, legacy+"-pid", os.Getpid(), answeringSandbox(t))

	m := New(Config{Home: home}, Deps{})
	if got := m.dbPath(); got != legacy {
		t.Fatalf("dbPath = %q, want the legacy name %q while a sandbox is running", got, legacy)
	}
	if got, want := m.pidfilePath(), legacy+"-pid"; got != want {
		t.Fatalf("pidfilePath = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy database was moved while in use: %v", err)
	}
}

// TestDBPathAdoptsWhenThePortDoesNotAnswer: a pidfile whose pid is live but
// whose port is silent is not a running sandbox — the pid may simply have been
// recycled by an unrelated process, and on Windows a resolvable pid is weak
// evidence on its own. Without the port check such a pidfile would pin the
// database to the legacy name forever.
func TestDBPathAdoptsWhenThePortDoesNotAnswer(t *testing.T) {
	home := seedLegacyState(t)
	legacy := filepath.Join(StateDir(home), LegacyDBFileName)
	// A port nothing is listening on, paired with this (very much alive) process.
	writePidfile(t, legacy+"-pid", os.Getpid(), closedPort(t))

	m := New(Config{Home: home}, Deps{})
	if got, want := m.dbPath(), filepath.Join(StateDir(home), DBFileName); got != want {
		t.Fatalf("dbPath = %q, want %q", got, want)
	}
}

// closedPort returns a loopback port that was bound and then released, so a
// connection to it is refused rather than hanging.
func closedPort(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestDBPathPrefersAnExistingCurrentDatabase: once adopted (or freshly created),
// the current name wins and a leftover legacy file is never consulted.
func TestDBPathPrefersAnExistingCurrentDatabase(t *testing.T) {
	home := seedLegacyState(t)
	current := filepath.Join(StateDir(home), DBFileName)
	if err := os.WriteFile(current, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Home: home}, Deps{})
	if got := m.dbPath(); got != current {
		t.Fatalf("dbPath = %q, want %q", got, current)
	}
	if b, _ := os.ReadFile(current); string(b) != "current" {
		t.Fatalf("current database overwritten: %q", b)
	}
}

// TestDBPathHonorsTheExplicitOverride: --db bypasses adoption entirely.
func TestDBPathExplicitOverride(t *testing.T) {
	home := seedLegacyState(t)
	explicit := filepath.Join(t.TempDir(), "elsewhere.db")
	m := New(Config{Home: home, DB: explicit}, Deps{})
	if got := m.dbPath(); got != explicit {
		t.Fatalf("dbPath = %q, want %q", got, explicit)
	}
	if _, err := os.Stat(filepath.Join(StateDir(home), LegacyDBFileName)); err != nil {
		t.Fatalf("legacy database disturbed by an explicit --db: %v", err)
	}
}
