// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestBotDBNameFollowsTheHomeDirectoryName: the directory decides the filename.
// A home under the earlier product's directory name keeps the earlier file name,
// so an older `korbit` binary sharing that directory opens the same database;
// every other home uses the service-neutral official name.
func TestBotDBNameFollowsTheHomeDirectoryName(t *testing.T) {
	for _, tc := range []struct {
		home string
		want string
	}{
		{filepath.Join("/home/u", ".digitalx-cli"), BotDBDefaultName},
		{filepath.Join("/home/u", ".korbit-cli"), LegacyBotDBFileName},
		{filepath.Join("/srv", "agent-home"), BotDBDefaultName},
	} {
		if got := botDBFileName(tc.home); got != tc.want {
			t.Errorf("botDBFileName(%q) = %q, want %q", tc.home, got, tc.want)
		}
		if got, want := botDBPath(tc.home), filepath.Join(tc.home, tc.want); got != want {
			t.Errorf("botDBPath(%q) = %q, want %q", tc.home, got, want)
		}
	}
}

// TestBotDBPaths pins the list a caller removing the CLI's data uses. Both
// spellings are listed so a home whose directory was renamed without its files
// is cleaned out completely.
func TestBotDBPaths(t *testing.T) {
	for _, home := range []string{
		filepath.Join("/home", "u", ".digitalx-cli"),
		filepath.Join("/home", "u", ".korbit-cli"),
	} {
		var want []string
		for _, name := range []string{BotDBDefaultName, LegacyBotDBFileName} {
			db := filepath.Join(home, name)
			want = append(want, db, db+"-wal", db+"-shm")
		}
		if got := BotDBPaths(home); !slices.Equal(got, want) {
			t.Errorf("BotDBPaths(%q) = %v, want %v", home, got, want)
		}
	}
}
