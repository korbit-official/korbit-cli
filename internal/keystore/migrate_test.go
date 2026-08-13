// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keystore"
	"github.com/korbit-official/korbit-cli/internal/keystore/keystoretest"
)

// recCommit returns a commit func that records the names it was called with.
func recCommit(names *[]string) func(string) error {
	return func(n string) error { *names = append(*names, n); return nil }
}

// items builds MigrateItems that all share one source backend.
func items(src keystore.Keystore, names ...string) []keystore.MigrateItem {
	out := make([]keystore.MigrateItem, 0, len(names))
	for _, n := range names {
		out = append(out, keystore.MigrateItem{Name: n, Source: src})
	}
	return out
}

func TestMigrateMovesAndDeletesSource(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	src.Seed("b", "pem-b")
	dst := keystoretest.New("keychain")

	var committed []string
	rep, err := keystore.Migrate(items(src, "a", "b"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(committed, []string{"a", "b"}) {
		t.Fatalf("committed = %v", committed)
	}
	if !reflect.DeepEqual(rep.Moved, []string{"a", "b"}) {
		t.Fatalf("Moved = %v", rep.Moved)
	}
	if v, _ := dst.Get("a"); v != "pem-a" {
		t.Fatalf("target missing a: %q", v)
	}
	if src.Has("a") || src.Has("b") {
		t.Fatal("source keys not deleted")
	}
}

func TestMigrateKeepSource(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")

	var committed []string
	rep, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{KeepSource: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.KeptSource {
		t.Fatal("KeptSource not reported")
	}
	if !src.Has("a") {
		t.Fatal("source key was deleted despite KeepSource")
	}
	if v, _ := dst.Get("a"); v != "pem-a" {
		t.Fatalf("target missing a: %q", v)
	}
}

func TestMigrateBuckets(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("moved", "m")
	src.Seed("same", "s")
	src.Seed("diff", "new")
	// "recovered" exists only in target; "gone" exists in neither.
	dst := keystoretest.New("keychain")
	dst.Seed("same", "s")
	dst.Seed("diff", "stale")
	dst.Seed("recovered", "r")

	var committed []string
	rep, err := keystore.Migrate(items(src, "moved", "same", "diff", "recovered", "gone"), dst,
		recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every processed key commits — including recovered/missing ones, whose
	// records must re-point at the target.
	if !reflect.DeepEqual(committed, []string{"moved", "same", "diff", "recovered", "gone"}) {
		t.Fatalf("committed = %v", committed)
	}
	if !reflect.DeepEqual(rep.Moved, []string{"moved"}) {
		t.Fatalf("Moved = %v", rep.Moved)
	}
	if !reflect.DeepEqual(rep.Identical, []string{"same"}) {
		t.Fatalf("Identical = %v", rep.Identical)
	}
	if !reflect.DeepEqual(rep.Replaced, []string{"diff"}) {
		t.Fatalf("Replaced = %v", rep.Replaced)
	}
	if !reflect.DeepEqual(rep.Recovered, []string{"recovered"}) {
		t.Fatalf("Recovered = %v", rep.Recovered)
	}
	if !reflect.DeepEqual(rep.Missing, []string{"gone"}) {
		t.Fatalf("Missing = %v", rep.Missing)
	}
	// The stale "diff" entry must now hold the authoritative source material.
	if v, _ := dst.Get("diff"); v != "new" {
		t.Fatalf("stale entry not overwritten: %q", v)
	}
	// "recovered" was absent from the source, so it must remain in the target.
	if v, _ := dst.Get("recovered"); v != "r" {
		t.Fatalf("recovered key disturbed: %q", v)
	}
	// Source-present keys are deleted; recovered/gone are not in the source.
	if src.Has("moved") || src.Has("same") || src.Has("diff") {
		t.Fatal("source-present keys not deleted")
	}
}

func TestMigrateMixedSources(t *testing.T) {
	// Keys come from different source backends in one run — each item carries
	// its own. "a" moves from file, "b" is already in the target backend.
	srcFile := keystoretest.New("file")
	srcFile.Seed("a", "pem-a")
	already := keystoretest.New("keychain")
	dst := keystoretest.New("keychain")
	dst.Seed("b", "pem-b")

	var committed []string
	rep, err := keystore.Migrate([]keystore.MigrateItem{
		{Name: "a", Source: srcFile},
		{Name: "b", Source: already},
	}, dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Moved, []string{"a"}) {
		t.Fatalf("Moved = %v", rep.Moved)
	}
	if !reflect.DeepEqual(rep.AlreadyInTarget, []string{"b"}) {
		t.Fatalf("AlreadyInTarget = %v", rep.AlreadyInTarget)
	}
	// Already-in-target keys need no commit (their record is already right).
	if !reflect.DeepEqual(committed, []string{"a"}) {
		t.Fatalf("committed = %v", committed)
	}
	if v, _ := dst.Get("b"); v != "pem-b" {
		t.Fatalf("already-in-target key disturbed: %q", v)
	}
}

func TestMigrateSourceUnavailableRecoversFromTarget(t *testing.T) {
	// The source backend can't be read at all (it would error if touched). The
	// keys live in the target; migration must recover them and never read or
	// delete the source.
	src := keystoretest.New("keychain")
	src.GetErr = func(string) error { return errors.New("no keyring on this host") }
	src.DeleteErr = func(string) error { return errors.New("no keyring on this host") }
	dst := keystoretest.New("file")
	dst.Seed("a", "pem-a")
	dst.Seed("b", "pem-b")

	its := items(src, "a", "b", "gone")
	for i := range its {
		its[i].SourceUnavailable = true
	}
	var committed []string
	rep, err := keystore.Migrate(its, dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err != nil {
		t.Fatalf("recovery should not fail: %v", err)
	}
	// Every key commits so its record re-points at the reachable target.
	if !reflect.DeepEqual(committed, []string{"a", "b", "gone"}) {
		t.Fatalf("committed = %v", committed)
	}
	if !reflect.DeepEqual(rep.Recovered, []string{"a", "b"}) {
		t.Fatalf("Recovered = %v", rep.Recovered)
	}
	// A key that exists only in the unreachable source can't be salvaged.
	if !reflect.DeepEqual(rep.Missing, []string{"gone"}) {
		t.Fatalf("Missing = %v", rep.Missing)
	}
	// Target keys are untouched.
	if v, _ := dst.Get("a"); v != "pem-a" {
		t.Fatalf("target key disturbed: %q", v)
	}
}

func TestMigrateSourceReadErrorAborts(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	src.GetErr = func(name string) error { return errors.New("source unreadable") }
	dst := keystoretest.New("keychain")

	var committed []string
	if _, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil); err == nil {
		t.Fatal("expected source read error")
	}
	if len(committed) != 0 {
		t.Fatal("commit must not run when a copy failed")
	}
	if dst.Has("a") {
		t.Fatal("target must be untouched on abort")
	}
}

func TestMigrateAbortKeepsEarlierKeysMigrated(t *testing.T) {
	// Per-key commits: when "b" fails, "a" — already copied, verified, and
	// committed — stays migrated. Only the in-flight key is rolled back, and the
	// error says a re-run continues.
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	src.Seed("b", "pem-b")
	dst := keystoretest.New("keychain")
	dst.SetErr = func(name string) error {
		if name == "b" {
			return errors.New("write denied")
		}
		return nil
	}

	var committed []string
	_, err := keystore.Migrate(items(src, "a", "b"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err == nil {
		t.Fatal("expected target write error")
	}
	if !reflect.DeepEqual(committed, []string{"a"}) {
		t.Fatalf("committed = %v", committed)
	}
	// "a" is durably migrated: in the target, gone from the source.
	if v, _ := dst.Get("a"); v != "pem-a" {
		t.Fatalf("migrated key lost: %q", v)
	}
	if src.Has("a") {
		t.Fatal("migrated key should be deleted from the source")
	}
	// "b" is untouched.
	if dst.Has("b") {
		t.Fatal("failed key must not be in the target")
	}
	if !src.Has("b") {
		t.Fatal("failed key must stay in the source")
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("error should point at re-running: %v", err)
	}
}

func TestMigrateTargetWriteErrorRollsBack(t *testing.T) {
	// The in-flight key had a pre-existing (stale) target entry; a failed copy
	// must restore exactly that entry.
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")
	dst.Seed("a", "old-a")
	calls := 0
	dst.SetErr = func(name string) error {
		calls++
		if calls == 1 {
			return errors.New("write denied") // forward copy fails; rollback Set succeeds
		}
		return nil
	}

	var committed []string
	if _, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil); err == nil {
		t.Fatal("expected target write error")
	}
	if len(committed) != 0 {
		t.Fatal("commit must not run after a failed copy")
	}
	if v, _ := dst.Get("a"); v != "old-a" {
		t.Fatalf("target 'a' not restored on rollback: %q", v)
	}
	if !src.Has("a") {
		t.Fatal("source must be untouched on rollback")
	}
}

func TestMigrateVerifyMismatchRollsBack(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")
	dst.Tamper = map[string]string{"a": "corrupted"} // Set stores wrong bytes

	var committed []string
	_, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err == nil {
		t.Fatal("expected verification failure")
	}
	if len(committed) != 0 {
		t.Fatal("commit must not run when verification failed")
	}
	if dst.Has("a") {
		t.Fatal("target entry must be rolled back after verify mismatch")
	}
	if !src.Has("a") {
		t.Fatal("source must be intact after verify mismatch")
	}
}

func TestMigrateVerifyReadErrorRollsBack(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")
	// Fail only the verification read (the 2nd Get of "a"): the 1st Get is the
	// pre-read, the 2nd is the post-Set verification.
	calls := 0
	dst.GetErr = func(name string) error {
		calls++
		if calls == 2 {
			return errors.New("read failed after write")
		}
		return nil
	}

	var committed []string
	if _, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil); err == nil {
		t.Fatal("expected verification read error")
	}
	if len(committed) != 0 {
		t.Fatal("commit must not run when verification read failed")
	}
}

func TestMigrateCommitErrorRollsBack(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")
	dst.Seed("a", "old-a")

	_, err := keystore.Migrate(items(src, "a"), dst, func(string) error {
		return errors.New("could not write keys.json")
	}, keystore.MigrateOptions{}, nil)
	if err == nil {
		t.Fatal("expected commit error")
	}
	// Commit failed, so this key's migration is a no-op: target restored, source kept.
	if v, _ := dst.Get("a"); v != "old-a" {
		t.Fatalf("target not rolled back after commit failure: %q", v)
	}
	if !src.Has("a") {
		t.Fatal("source must be intact after commit failure")
	}
}

func TestMigrateRollbackFailureIsReported(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	dst := keystoretest.New("keychain")
	dst.Seed("a", "old-a")
	dst.Tamper = map[string]string{"a": "corrupted"} // verify fails -> rollback
	setCalls := 0
	dst.SetErr = func(name string) error {
		setCalls++
		if setCalls >= 2 { // the rollback Set
			return errors.New("rollback write denied")
		}
		return nil
	}

	var committed []string
	_, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(committed) != 0 {
		t.Fatal("commit must not run")
	}
	// The error must surface that the target may hold partial data for the key
	// and that it is still safe in its source.
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "keychain keystore") {
		t.Fatalf("rollback failure not surfaced clearly: %v", err)
	}
	if !src.Has("a") {
		t.Fatal("source must be intact")
	}
}

func TestMigrateSourceDeleteErrorIsNonFatal(t *testing.T) {
	src := keystoretest.New("file")
	src.Seed("a", "pem-a")
	src.DeleteErr = func(name string) error { return errors.New("delete denied") }
	dst := keystoretest.New("keychain")

	var committed []string
	rep, err := keystore.Migrate(items(src, "a"), dst, recCommit(&committed), keystore.MigrateOptions{}, nil)
	if err != nil {
		t.Fatalf("source-delete failure after commit must not fail the migration: %v", err)
	}
	if len(committed) != 1 {
		t.Fatal("commit should have run")
	}
	if len(rep.DeleteWarnings) != 1 {
		t.Fatalf("expected one delete warning, got %v", rep.DeleteWarnings)
	}
	if v, _ := dst.Get("a"); v != "pem-a" {
		t.Fatalf("key must be in target: %q", v)
	}
}
