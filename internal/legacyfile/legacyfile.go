// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package legacyfile adopts a file (and its companion sidecars) that an
// installation carries under an older name, moving it to the name the CLI uses
// now so the data keeps being read instead of being silently replaced by an
// empty file beside it.
package legacyfile

import (
	"errors"
	"io/fs"
	"os"
)

// Adopt renames legacy to current when current does not exist and legacy does,
// then renames each existing companion (legacy+suffix -> current+suffix). A
// SQLite database's -wal/-shm sidecars are companions in this sense: moving the
// database without them would strand the committed-but-uncheckpointed tail of
// the write-ahead log.
//
// adopted reports whether the PRIMARY rename happened. The two return values
// together tell a caller which path to open:
//
//	adopted=false, err=nil   nothing to do — current already exists, or legacy
//	                         does not. Open current.
//	adopted=true,  err=nil   fully moved. Open current.
//	adopted=false, err!=nil  the primary rename failed (on Windows, renaming
//	                         over or out from under an open file does). Open
//	                         LEGACY: creating an empty file under the current
//	                         name would make the next run see current and never
//	                         retry, orphaning the data for good.
//	adopted=true,  err!=nil  the database moved but a companion did not. Open
//	                         current — legacy no longer exists — and warn naming
//	                         the stranded companion.
//
// Adoption is never destructive: an existing current file is left untouched and
// legacy is not consulted.
func Adopt(current, legacy string, companionSuffixes ...string) (adopted bool, err error) {
	if current == "" || legacy == "" || current == legacy {
		return false, nil
	}
	switch _, err := os.Lstat(current); {
	case err == nil:
		return false, nil // already on the current name
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	// A stat failure that is NOT "absent" (a permission error on the directory,
	// say) must not read as "nothing to adopt" — reporting it keeps the caller
	// on the legacy name instead of quietly starting an empty database.
	switch _, err := os.Lstat(legacy); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil // nothing to adopt
	case err != nil:
		return false, err
	}
	if err := os.Rename(legacy, current); err != nil {
		return false, err
	}
	var firstErr error
	for _, suffix := range companionSuffixes {
		from, to := legacy+suffix, current+suffix
		if _, err := os.Lstat(from); err != nil {
			continue
		}
		if err := os.Rename(from, to); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return true, firstErr
}
