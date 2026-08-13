// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"fmt"
	"log/slog"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// MigrateItem is one key to migrate: its name and the backend its registry
// record currently points at. Keys may come from different source backends in
// a single run (each key carries its own).
type MigrateItem struct {
	Name string
	// Source is the backend currently recorded for this key.
	Source Keystore
	// SourceUnavailable signals that this key's source backend can't be reached
	// at all here — e.g. its record points at the OS keyring on a headless host
	// that has none. In that mode Migrate never reads from or deletes in the
	// source for this key; it can only salvage material that already exists in
	// the target (the recovery path that `doctor` points at when a key's backend
	// is unavailable). A key present only in the unreachable source is reported
	// Missing. This is distinct from a source that is *reachable but errors*
	// (e.g. a corrupt keystore.json), which must still fail loudly — the caller
	// sets this flag only from a real backend-availability probe, never from a
	// per-key read error.
	SourceUnavailable bool
}

// MigrateOptions tunes a Migrate call.
type MigrateOptions struct {
	// KeepSource leaves the migrated keys in their source backends instead of
	// deleting them once they are safely in the target.
	KeepSource bool
}

// MigrateReport summarizes what a migration did, per key. The buckets are
// mutually exclusive; a name lands in exactly one. It carries no secret
// material — only key names.
type MigrateReport struct {
	Target     string `json:"target"`
	KeptSource bool   `json:"keptSource"`
	// Moved: present in the source, copied into the target (which had no prior
	// entry) and removed from the source unless KeepSource.
	Moved []string `json:"moved"`
	// Identical: the target already held byte-identical material; re-copied and
	// removed from the source unless KeepSource.
	Identical []string `json:"identical"`
	// Replaced: the target held a *different* (stale) entry under this name; it
	// was overwritten with the authoritative source material.
	Replaced []string `json:"replaced"`
	// Recovered: absent from the source but already present in the target — left
	// in place, record re-pointed. This is the orphan-recovery path (the record
	// pointed at an empty or unreachable backend while the material sat in the
	// target one).
	Recovered []string `json:"recovered"`
	// Missing: absent from both backends — the key's private material is gone;
	// re-import it. A warning, not a failure: the record is still re-pointed at
	// the target so a later re-import lands there.
	Missing []string `json:"missing"`
	// AlreadyInTarget: the key's record already points at the target backend —
	// nothing to do. Makes re-running a partially-failed migration a clean
	// continue instead of an error.
	AlreadyInTarget []string `json:"alreadyInTarget"`
	// DeleteWarnings records source-deletion failures that happened *after* a
	// key's migration already committed (so they don't undo a valid migration).
	DeleteWarnings []string `json:"deleteWarnings,omitempty"`
}

// committed counts every key whose commit ran (all buckets except
// AlreadyInTarget land after a commit).
func (r *MigrateReport) committed() int {
	return len(r.Moved) + len(r.Identical) + len(r.Replaced) + len(r.Recovered) + len(r.Missing)
}

// Migrate moves each item's key material into dst and re-points its registry
// record there via commit(name). The per-key ordering is the safety contract:
//
//  1. Copy the key into the target and read it back to VERIFY it (a copy that
//     doesn't round-trip aborts).
//  2. Only after the copy verifies, call commit(name) — the caller stamps the
//     key's record in keys.json with the target backend. This is that key's
//     point of no return.
//  3. Only after commit succeeds, delete the original from the source.
//
// Any failure rolls the in-flight key's target entry back to its exact prior
// state and aborts the run — that key is untouched, while keys committed
// before it stay migrated (each key is individually consistent; re-running
// continues, since committed keys then report AlreadyInTarget). A
// source-delete failure after commit is non-fatal (the key is already safe in
// the target) and is reported in DeleteWarnings.
//
// Items whose source backend already equals the target are bucketed
// AlreadyInTarget and skipped.
func Migrate(items []MigrateItem, dst Keystore, commit func(name string) error, opts MigrateOptions, log *slog.Logger) (MigrateReport, error) {
	l := logging.Or(log)
	rep := MigrateReport{Target: dst.Backend(), KeptSource: opts.KeepSource}
	l.Debug("migrate: starting", "target", dst.Backend(), "keys", len(items), "keepSource", opts.KeepSource)

	// fail rolls the in-flight key's target entry back to its prior state and
	// aborts. If the rollback itself fails (a double fault — rare), the error
	// says so, so the user isn't left with a silently half-modified target.
	// Committed keys are durable by design; the error tells the user a re-run
	// continues from where it stopped.
	fail := func(name string, priorVal string, priorExisted, written bool, err error) (MigrateReport, error) {
		// We roll back whenever the forward dst.Set was attempted (written), even
		// if that Set itself failed: a backend may have partially written, so
		// restoring the captured prior value (or deleting a net-new entry) is the
		// safe move regardless. An atomic backend makes the restore a no-op.
		if written {
			l.Warn("migrate: failed, rolling back in-flight key", "key", name, "target", dst.Backend(), "priorExisted", priorExisted, "err", err)
			var rbErr error
			if priorExisted {
				rbErr = dst.Set(name, priorVal)
			} else {
				rbErr = dst.Delete(name)
			}
			if rbErr != nil {
				err = output.Configf(
					"%v — and rolling back key %q in the %s keystore failed, which may now hold partial data for it (the key is still safe in its source keystore; re-run the migration once the target is reachable)",
					err, name, dst.Backend())
			}
		}
		if n := rep.committed(); n > 0 {
			err = output.Configf(
				"%v — %d key(s) migrated before this failure are already in the %s keystore; re-run the command to migrate the rest", err, n, dst.Backend())
		}
		return MigrateReport{}, err
	}

	for _, it := range items {
		if it.Source.Backend() == dst.Backend() {
			l.Debug("migrate: already in target", "key", it.Name, "backend", dst.Backend())
			rep.AlreadyInTarget = append(rep.AlreadyInTarget, it.Name)
			continue
		}
		l.Debug("migrate: key step begin", "key", it.Name, "source", it.Source.Backend(), "target", dst.Backend(), "sourceUnavailable", it.SourceUnavailable)

		// When this key's source backend is unavailable we cannot (and must not
		// try to) read it; leave sv empty so the only material we can use is
		// whatever the target already holds. When it is available, a read error is
		// a genuine failure (e.g. corrupt keystore.json) and aborts loudly.
		var sv string
		if !it.SourceUnavailable {
			v, err := it.Source.Get(it.Name)
			if err != nil {
				return fail(it.Name, "", false, false, err)
			}
			sv = v
		}
		tv, err := dst.Get(it.Name)
		if err != nil {
			return fail(it.Name, "", false, false, err)
		}

		if sv == "" {
			// Nothing to move from the source: re-point the record at the target so
			// the key is usable (Recovered) or at least lands future material there
			// (Missing — a re-import then writes to the target).
			l.Debug("migrate: no source material, committing record re-point", "key", it.Name, "target", dst.Backend(), "targetHasMaterial", tv != "")
			if err := commit(it.Name); err != nil {
				return fail(it.Name, "", false, false, err)
			}
			if tv != "" {
				l.Info("migrated key (recovered from target)", "key", it.Name, "target", dst.Backend())
				rep.Recovered = append(rep.Recovered, it.Name)
			} else {
				l.Warn("migrated key has no material in either backend (re-import needed)", "key", it.Name, "target", dst.Backend())
				rep.Missing = append(rep.Missing, it.Name)
			}
			continue
		}

		// The source holds authoritative material: copy it into the target and
		// verify the round-trip before committing the record to it.
		l.Debug("migrate: copying material into target", "key", it.Name, "target", dst.Backend())
		if err := dst.Set(it.Name, sv); err != nil {
			return fail(it.Name, tv, tv != "", true, err)
		}
		got, err := dst.Get(it.Name)
		if err != nil {
			return fail(it.Name, tv, tv != "", true, err)
		}
		if got != sv {
			return fail(it.Name, tv, tv != "", true, output.Configf(
				"verification failed: key %q did not read back correctly from the %s keystore after copying — no changes were made to it", it.Name, dst.Backend()))
		}
		l.Debug("migrate: read-back verified, committing record", "key", it.Name, "target", dst.Backend())
		if err := commit(it.Name); err != nil {
			return fail(it.Name, tv, tv != "", true, err)
		}

		// The record now points at the target and the material is verified there.
		// Removing the original is best-effort: a failure here cannot invalidate a
		// migration that already committed, so it's reported, not returned.
		if !opts.KeepSource && !it.SourceUnavailable {
			l.Debug("migrate: deleting from source", "key", it.Name, "source", it.Source.Backend())
			if err := it.Source.Delete(it.Name); err != nil {
				l.Warn("migrate: source delete failed (key already safe in target)", "key", it.Name, "source", it.Source.Backend(), "err", err)
				rep.DeleteWarnings = append(rep.DeleteWarnings,
					fmt.Sprintf("could not remove %q from the old %s keystore: %v", it.Name, it.Source.Backend(), err))
			}
		}
		switch {
		case tv == "":
			rep.Moved = append(rep.Moved, it.Name)
		case tv == sv:
			rep.Identical = append(rep.Identical, it.Name)
		default:
			rep.Replaced = append(rep.Replaced, it.Name)
		}
		l.Info("migrated key", "key", it.Name, "source", it.Source.Backend(), "target", dst.Backend())
	}
	l.Debug("migrate: complete", "target", dst.Backend(), "committed", rep.committed())
	return rep, nil
}
