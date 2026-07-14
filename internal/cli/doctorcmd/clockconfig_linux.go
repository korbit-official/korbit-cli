// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package doctorcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// timedatectlMaxMs caps the `timedatectl show` subprocess. The query is local and
// fast, but a wedged timedated shouldn't stall doctor.
const timedatectlMaxMs = 5000

// timedatectlPath resolves the systemd time CLI. It prefers the canonical
// absolute path (systemd installs its tools under /usr/bin; /bin is a symlink to
// it on merged-/usr systems) so a `timedatectl` planted earlier on PATH can't be
// run in its place. It falls back to a bare PATH lookup for unusual layouts —
// which, when the binary is genuinely absent (e.g. Alpine/non-systemd), yields an
// exec.ErrNotFound that the caller reports as "not available".
func timedatectlPath() string {
	for _, p := range []string{"/usr/bin/timedatectl", "/bin/timedatectl"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "timedatectl"
}

// diagnoseClockConfig reports the Linux system clock's time-sync state via
// `timedatectl show`, whose `Key=Value` output is stable and locale-independent
// (unlike the localized `timedatectl status`), so nothing here parses translated
// text. Read-only. On a non-systemd host (no `timedatectl`) it warns with the
// chrony/ntpd fallbacks rather than failing — leaving the check to the user.
//
// clockConfirmedOK (the `clock skew` verdict) is unused here: NTPSynchronized is
// an intrinsic synced flag, so this diagnosis doesn't fall back to the skew check.
func diagnoseClockConfig(timeoutMs int, clockConfirmedOK bool) clockConfigReport {
	rep := clockConfigReport{Supported: true}
	if timeoutMs <= 0 || timeoutMs > timedatectlMaxMs {
		timeoutMs = timedatectlMaxMs
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	out, err := exec.CommandContext(ctx, timedatectlPath(), "show").Output()
	if err != nil {
		detail := "could not query systemd time sync via `timedatectl show`: " + err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			detail = "`timedatectl` is not available — this host may not use systemd"
		}
		rep.add("clock config", CheckWarn, detail,
			"check your NTP daemon directly: `chronyc tracking` (chrony) or `ntpq -p` (ntpd)")
		return rep
	}
	evaluateTimedatectl(&rep, parseTimedatectlShow(string(out)))
	return rep
}
