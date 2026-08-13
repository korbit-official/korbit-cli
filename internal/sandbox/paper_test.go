// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
)

func TestInitDBArgsPaper(t *testing.T) {
	plain := New(Config{Home: t.TempDir()}, Deps{}).initDBArgs()
	if strings.Contains(strings.Join(plain, " "), "--source") {
		t.Errorf("plain init-db args should not carry --source: %v", plain)
	}
	paper := New(Config{Home: t.TempDir(), Paper: true}, Deps{}).initDBArgs()
	if !strings.Contains(strings.Join(paper, " "), "--source live") {
		t.Errorf("paper init-db args should carry --source live: %v", paper)
	}
	if strings.Contains(strings.Join(paper, " "), "--mode korbit-api") {
		t.Errorf("paper-only init-db args should not carry --mode korbit-api (fixtures pairs only): %v", paper)
	}

	// --all-pairs seeds from the live production snapshot (--mode korbit-api) and
	// points the bundle at the snapshot cache, independent of --paper.
	allWalk := New(Config{Home: t.TempDir(), AllPairs: true}, Deps{})
	allWalkArgs := allWalk.initDBArgs()
	if joined := strings.Join(allWalkArgs, " "); !strings.Contains(joined, "--mode korbit-api") || strings.Contains(joined, "--source") {
		t.Errorf("--all-pairs (no --paper) should carry --mode korbit-api and no --source: %v", allWalkArgs)
	}
	if !strings.Contains(strings.Join(allWalkArgs, " "), "--market-cache "+allWalk.marketCachePath()) {
		t.Errorf("--all-pairs should carry --market-cache at the beside-db path: %v", allWalkArgs)
	}

	// --all-pairs + --paper: every launched pair, mirrored live, cached.
	allPaper := New(Config{Home: t.TempDir(), AllPairs: true, Paper: true}, Deps{}).initDBArgs()
	joined := strings.Join(allPaper, " ")
	if !strings.Contains(joined, "--mode korbit-api") || !strings.Contains(joined, "--source live") || !strings.Contains(joined, "--market-cache") {
		t.Errorf("--all-pairs --paper should carry --mode korbit-api --source live --market-cache: %v", allPaper)
	}

	// The cache path is NOT one of the sidecars freshen deletes, so it survives --fresh.
	cache := allWalk.marketCachePath()
	for _, sidecar := range []string{allWalk.dbPath(), allWalk.dbPath() + "-wal", allWalk.dbPath() + "-shm", allWalk.pidfilePath()} {
		if cache == sidecar {
			t.Errorf("market cache path %q must differ from freshen-deleted sidecar %q", cache, sidecar)
		}
	}
}

func TestNonPaperPairs(t *testing.T) {
	cases := []struct {
		name  string
		pairs []statusPair
		want  []string
	}{
		{"no pairs", nil, nil},
		{"all live", []statusPair{{"btc_krw", "live"}, {"eth_krw", "live"}}, nil},
		{"mixed", []statusPair{{"btc_krw", "live"}, {"doge_krw", "walk"}}, []string{"doge_krw"}},
		{"unknown source counts as non-paper", []statusPair{{"btc_krw", ""}}, []string{"btc_krw"}},
	}
	for _, c := range cases {
		got := nonPaperPairs(statusDoc{Markets: statusMarkets{Pairs: c.pairs}})
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: nonPaperPairs = %v, want %v", c.name, got, c.want)
		}
	}
}

// paperHarness builds a Manager over the fake runtime with the given status
// pairs, returning it with the db path.
func paperHarness(t *testing.T, pairs ...statusPair) (*Manager, string) {
	t.Helper()
	return paperHarnessMarkets(t, statusMarkets{Pairs: pairs})
}

// paperHarnessMarkets is paperHarness taking the full markets document, so a
// test can also set initializedSource.
func paperHarnessMarkets(t *testing.T, markets statusMarkets) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	apiKey := keys.SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	dbPath := filepath.Join(home, "sandbox", "korbit-sandbox.db")
	denoBin := fakeDenoMarkets(t, dbPath+"-pid", kp.PrivatePEM, apiKey, markets)
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	m := New(Config{Home: home, CacheDir: t.TempDir(), RuntimePref: "deno", Paper: true}, Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(filepath.Dir(denoBin), name), nil },
		KeyManager: km,
	})
	return m, dbPath
}

// A --paper start against an existing database whose pairs are not in live mode
// must refuse (before any spawn) and name both ways out.
func TestStartPaperRefusesExistingWalkDB(t *testing.T) {
	m, dbPath := paperHarness(t, statusPair{Symbol: "btc_krw", MarketSource: "walk"})
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := m.Start(context.Background())
	if err == nil {
		t.Fatal("Start --paper on a walk-mode db should refuse")
	}
	for _, want := range []string{"btc_krw", "set-market", "--source live", "--fresh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// A db initialized with `init-db --source live` is a paper db even when some
// pairs stayed on the walk (production reported them non-launched at init):
// a --paper start must accept it and name the non-mirrored pairs in the result.
func TestStartPaperAcceptsLiveInitializedPartialDB(t *testing.T) {
	m, dbPath := paperHarnessMarkets(t, statusMarkets{
		InitializedSource: "live",
		Pairs: []statusPair{
			{Symbol: "btc_krw", MarketSource: "live"},
			{Symbol: "doge_krw", MarketSource: "walk"},
		},
	})
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start --paper on a live-initialized partial db should succeed: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background()) })
	if !res.Paper {
		t.Error("Paper should be true for a live-initialized db")
	}
	if strings.Join(res.WalkPairs, ",") != "doge_krw" {
		t.Errorf("WalkPairs = %v, want [doge_krw]", res.WalkPairs)
	}
	if res.PairCount != 2 {
		t.Errorf("PairCount = %d, want 2", res.PairCount)
	}
}

// --fresh deletes an existing database (and its sidecars) and starts a new one
// — combined with Paper it is the mode switch, succeeding exactly where the
// non-fresh --paper start refuses.
func TestStartFreshRecreatesDB(t *testing.T) {
	m, dbPath := paperHarness(t, statusPair{Symbol: "btc_krw", MarketSource: "live"})
	m.cfg.Fresh = true
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.WriteFile(p, []byte("OLD"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start --fresh: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background()) })
	if !res.Recreated {
		t.Error("Recreated should be true when a database was deleted")
	}
	if !res.Paper {
		t.Error("the fresh --paper start should report paper mode")
	}
	// The fake bundle's init-db truncates the db file: the OLD content is gone.
	if raw, rerr := os.ReadFile(dbPath); rerr != nil || string(raw) == "OLD" {
		t.Errorf("db should be recreated: err=%v content=%q", rerr, raw)
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("sidecar %s should be deleted", p)
		}
	}
}

// --fresh stops a running server for the database before deleting it.
func TestStartFreshStopsRunningServer(t *testing.T) {
	m, _ := paperHarness(t, statusPair{Symbol: "btc_krw", MarketSource: "live"})
	m.cfg.Paper = false
	first, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	m.cfg.Fresh = true
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start --fresh over a running server: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background()) })
	if !res.Recreated {
		t.Error("Recreated should be true")
	}
	if res.PID == first.PID {
		t.Error("a fresh start should have spawned a new server process")
	}
	if processAlive(first.PID) {
		t.Errorf("the first server (pid %d) should have been stopped", first.PID)
	}
}

// --fresh with nothing to delete is a plain start (Recreated stays false).
func TestFreshenNoDatabase(t *testing.T) {
	m := New(Config{Home: t.TempDir(), Fresh: true}, Deps{})
	recreated, err := m.freshen(context.Background())
	if err != nil {
		t.Fatalf("freshen: %v", err)
	}
	if recreated {
		t.Error("recreated should be false with no database present")
	}
}

// A fresh --paper start initializes the db (init-db --source live) and reports
// the db's actual paper mode from the bundle's status.
func TestStartPaperFreshDBReportsPaper(t *testing.T) {
	m, _ := paperHarness(t,
		statusPair{Symbol: "btc_krw", MarketSource: "live"},
		statusPair{Symbol: "eth_krw", MarketSource: "live"},
	)
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !res.Paper {
		t.Error("StartResult.Paper should be true when every pair is live")
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background()) })
}

// Without --paper the start proceeds regardless of the db's mode, and Paper in
// the result reports the ACTUAL mode (false here: a walk pair exists).
func TestStartWithoutPaperReportsActualMode(t *testing.T) {
	m, _ := paperHarness(t, statusPair{Symbol: "btc_krw", MarketSource: "walk"})
	m.cfg.Paper = false
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Paper {
		t.Error("StartResult.Paper should be false when a pair uses the walk")
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background()) })
}
