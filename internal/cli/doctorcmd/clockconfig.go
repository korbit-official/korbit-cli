// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package doctorcmd

import "runtime"

// clockConfigReport is the platform-neutral outcome of the opt-in OS time-sync
// diagnosis (`doctor --diagnose-clock`). Supported is false on a platform whose
// diagnosis isn't implemented yet; the renderer then emits a single "not
// supported" line. On a supported platform the per-OS diagnoseClockConfig fills
// Checks with ready-to-render lines using the same Check vocabulary as every
// other doctor check, so all platforms render identically in both the human
// checklist and the --json document.
//
// The per-OS split lives in clockconfig_<goos>.go behind build tags (Windows and
// Linux are implemented). To add macOS support, add clockconfig_darwin.go
// implementing diagnoseClockConfig and narrow clockconfig_other.go's build tag —
// nothing else changes.
type clockConfigReport struct {
	Supported bool
	Checks    []Check
}

// add appends one diagnostic line. The per-OS implementations build their
// report through this, so the field mapping to Check stays in one place.
func (r *clockConfigReport) add(name, status, detail, fix string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail, Fix: fix})
}

// doctorClockConfigCheck runs the OS time-sync configuration diagnosis and
// appends its findings via add. It is gated by --diagnose-clock (see
// diagnoseClockRequested): the default doctor run never reaches here, so it
// never inspects OS services, reads the registry, or spawns a subprocess.
//
// It is READ-ONLY on every platform — it never enables/starts a service or
// forces a resync; those are only ever offered as fix strings for the user to
// run in an elevated prompt. Its lines are OK/warn only (never CheckFail), so
// the opt-in flag can never change doctor's exit code — this is explanatory
// context for the skew the `clock skew` check already measured. timeoutMs
// bounds any external command the platform impl runs.
//
// clockConfirmedOK carries the `clock skew` check's verdict (true only when it
// measured the clock and found it within tolerance). A platform without its own
// synced flag uses it to gate a "force a resync" suggestion — pointless when the
// clock is already known good. Platforms that read an intrinsic synced state
// (Linux's NTPSynchronized) ignore it.
func doctorClockConfigCheck(add func(name, status, detail, fix string), timeoutMs int, clockConfirmedOK bool) {
	rep := diagnoseClockConfig(timeoutMs, clockConfirmedOK)
	if !rep.Supported {
		add("clock config", CheckWarn,
			"OS time-sync diagnosis isn't supported on "+runtime.GOOS+" yet",
			manualClockCheckHint(runtime.GOOS))
		return
	}
	for _, c := range rep.Checks {
		add(c.Name, c.Status, c.Detail, c.Fix)
	}
}

// manualClockCheckHint returns how to check the OS time-sync configuration by
// hand, chosen by GOOS so the user sees only their own platform's instruction
// (not a combined list). Used on platforms where doctor has no built-in
// diagnosis yet (Windows and Linux have one, so they never reach this).
func manualClockCheckHint(goos string) string {
	switch goos {
	case "darwin":
		return "check it in System Settings ▸ General ▸ Date & Time ▸ \"Set time and date automatically\""
	default:
		return "check that your platform's time-sync service is enabled and reaching an NTP server"
	}
}
