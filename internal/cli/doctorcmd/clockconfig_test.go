// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package doctorcmd

import (
	"strings"
	"testing"
)

func TestParseTimedatectlShow(t *testing.T) {
	m := parseTimedatectlShow("Timezone=Asia/Seoul\nNTP=yes\n\n  NTPSynchronized=yes  \nno-equals line\nTimeUSec=Mon 2026-07-07 10:00:00 KST\n")
	if m["Timezone"] != "Asia/Seoul" || m["NTP"] != "yes" || m["NTPSynchronized"] != "yes" {
		t.Fatalf("parsed wrong: %#v", m)
	}
	// A value may itself contain '=' (split on the first only).
	if m["TimeUSec"] != "Mon 2026-07-07 10:00:00 KST" {
		t.Errorf("value split wrong: %q", m["TimeUSec"])
	}
	if _, ok := m["no-equals line"]; ok {
		t.Errorf("lines without '=' must be skipped")
	}
}

func TestEvaluateTimedatectl(t *testing.T) {
	cases := []struct {
		name       string
		show       string
		wantStatus string
		wantSubstr string // must appear in detail+fix
	}{
		{"synced via timesyncd", "NTP=yes\nNTPSynchronized=yes\nCanNTP=yes\n", CheckOK, "synchronized"},
		{"synced by other daemon", "NTP=no\nNTPSynchronized=yes\n", CheckOK, "another NTP daemon"},
		{"ntp disabled", "NTP=no\nNTPSynchronized=no\nCanNTP=yes\n", CheckWarn, "set-ntp true"},
		{"ntp disabled, no service", "NTP=no\nNTPSynchronized=no\nCanNTP=no\n", CheckWarn, "no systemd-timesyncd"},
		{"enabled, not yet synced", "NTP=yes\nNTPSynchronized=no\n", CheckWarn, "not synchronized yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rep clockConfigReport
			evaluateTimedatectl(&rep, parseTimedatectlShow(c.show))
			if len(rep.Checks) != 1 {
				t.Fatalf("want exactly 1 check, got %d: %#v", len(rep.Checks), rep.Checks)
			}
			ch := rep.Checks[0]
			if ch.Status != c.wantStatus {
				t.Errorf("status = %q, want %q", ch.Status, c.wantStatus)
			}
			// Never fatal: the OS-config diagnosis must not flip doctor's exit code.
			if ch.Status == CheckFail {
				t.Errorf("clock-config checks must never be CheckFail")
			}
			if !strings.Contains(ch.Detail+" "+ch.Fix, c.wantSubstr) {
				t.Errorf("detail=%q fix=%q missing %q", ch.Detail, ch.Fix, c.wantSubstr)
			}
		})
	}
}
