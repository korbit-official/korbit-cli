// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"path/filepath"
	"testing"
)

// TestBotDBNoFsync: the db.* store honors the --no-fsync opt-in by opening with
// synchronous=OFF (0); the default keeps SQLite's FULL (2).
func TestBotDBNoFsync(t *testing.T) {
	for _, tc := range []struct {
		noFsync bool
		want    int
	}{{false, 2}, {true, 0}} {
		d := &botDB{path: filepath.Join(t.TempDir(), "bot.db"), noFsync: tc.noFsync}
		db, err := d.handle()
		if err != nil {
			t.Fatalf("handle(noFsync=%v): %v", tc.noFsync, err)
		}
		var sync int
		if err := db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("query synchronous: %v", err)
		}
		d.close()
		if sync != tc.want {
			t.Fatalf("noFsync=%v: synchronous = %d, want %d", tc.noFsync, sync, tc.want)
		}
	}
}
