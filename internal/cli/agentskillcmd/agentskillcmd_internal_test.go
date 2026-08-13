// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package agentskillcmd

import "testing"

func TestAgentHomeForPerOS(t *testing.T) {
	// On Windows the native home is USERPROFILE; a unix-style HOME (e.g. exported
	// by Git Bash, like /c/Users/name) must NOT win, or filepath.Join would mangle
	// it into \c\Users\name. On other OSes HOME wins.
	env := map[string]string{"HOME": "/c/Users/unixy", "USERPROFILE": `C:\Users\native`}
	get := func(k string) string { return env[k] }

	if h, err := agentHomeFor(get, "windows"); err != nil || h != `C:\Users\native` {
		t.Fatalf("windows home = %q, err=%v; want USERPROFILE", h, err)
	}
	if h, err := agentHomeFor(get, "linux"); err != nil || h != "/c/Users/unixy" {
		t.Fatalf("linux home = %q, err=%v; want HOME", h, err)
	}
	if h, err := agentHomeFor(get, "darwin"); err != nil || h != "/c/Users/unixy" {
		t.Fatalf("darwin home = %q, err=%v; want HOME", h, err)
	}
}
