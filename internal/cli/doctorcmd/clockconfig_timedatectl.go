// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package doctorcmd

import "strings"

// parseTimedatectlShow parses `timedatectl show` output: stable, locale-independent
// `Key=Value` lines (a D-Bus property dump), unlike the localized `timedatectl
// status`. It lives in a portable (untagged) file — with the exec kept in the
// linux-tagged file — so the parse+evaluate logic is unit-testable on any host.
func parseTimedatectlShow(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// evaluateTimedatectl turns parsed `timedatectl show` properties into one
// `clock sync` check. NTPSynchronized is the kernel's own "clock is disciplined"
// flag, so it is authoritative even when a non-systemd daemon (chrony/ntpd) does
// the syncing; NTP reflects whether systemd's network time sync is enabled. Only
// OK/warn, never CheckFail — the `clock skew` check remains the authority on
// whether the clock is actually off.
func evaluateTimedatectl(rep *clockConfigReport, p map[string]string) {
	synced := p["NTPSynchronized"] == "yes"
	ntpEnabled := p["NTP"] == "yes"
	canNTP, haveCanNTP := p["CanNTP"]

	switch {
	case synced:
		detail := "the system clock is synchronized (NTPSynchronized=yes)"
		if !ntpEnabled {
			detail += "; systemd network time sync is off, so another NTP daemon (e.g. chrony) is disciplining the clock"
		}
		rep.add("clock sync", CheckOK, detail, "")
	case !ntpEnabled:
		detail := "network time synchronization is disabled (NTP=no) — the system clock is not being kept in sync"
		if haveCanNTP && canNTP == "no" {
			detail += "; no systemd-timesyncd service is available here"
		}
		rep.add("clock sync", CheckWarn, detail,
			"enable it: `sudo timedatectl set-ntp true` (or configure chrony/ntpd if this host uses one)")
	default:
		rep.add("clock sync", CheckWarn,
			"network time synchronization is enabled but the clock is not synchronized yet (NTPSynchronized=no) — it may be starting up or unable to reach a time server",
			"check it: `timedatectl timesync-status`; verify the host can reach its NTP server")
	}
}
