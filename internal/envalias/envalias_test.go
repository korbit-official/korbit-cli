// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package envalias

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		key  string
		want string
	}{
		{
			name: "canonical name",
			vars: map[string]string{"DIGITALX_CLI_KEY": "new"},
			key:  "DIGITALX_CLI_KEY",
			want: "new",
		},
		{
			name: "legacy fallback",
			vars: map[string]string{"KORBIT_CLI_KEY": "old"},
			key:  "DIGITALX_CLI_KEY",
			want: "old",
		},
		{
			name: "canonical wins over legacy",
			vars: map[string]string{"DIGITALX_CLI_KEY": "new", "KORBIT_CLI_KEY": "old"},
			key:  "DIGITALX_CLI_KEY",
			want: "new",
		},
		{
			name: "empty canonical falls back",
			vars: map[string]string{"DIGITALX_CLI_KEY": "", "KORBIT_CLI_KEY": "old"},
			key:  "DIGITALX_CLI_KEY",
			want: "old",
		},
		{
			name: "neither set",
			vars: map[string]string{},
			key:  "DIGITALX_CLI_KEY",
			want: "",
		},
		{
			name: "unprefixed name has no fallback",
			vars: map[string]string{"KORBIT_SANDBOX_MODE": "paper"},
			key:  "KORBIT_SANDBOX_MODE",
			want: "paper",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Lookup(env(tc.vars), tc.key); got != tc.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestLookupNilGetenv(t *testing.T) {
	if got := Lookup(nil, "DIGITALX_CLI_KEY"); got != "" {
		t.Fatalf("Lookup(nil) = %q, want empty", got)
	}
}
