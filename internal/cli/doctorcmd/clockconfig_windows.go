// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package doctorcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows OS time-sync diagnosis. It reports three signals about the
// Windows Time service (W32Time), the one that keeps the system clock synced:
//
//  1. service state + start type — via the Service Control Manager API. Numeric
//     enums, so the reading is LOCALE-INDEPENDENT (no text parsing) and needs no
//     administrator rights (query-only access), and no subprocess.
//  2. configured NTP source — read straight from the registry (Type/NtpServer).
//     Also locale-independent (the values are invariant tokens) and needs no
//     admin rights or subprocess.
//  3. live sync status — from `w32tm /query /status`, shown VERBATIM. w32tm has
//     no locale-invariant output mode and its field labels are translated, so
//     rather than parse them doctor surfaces the raw report in the OS display
//     language. Read-only (`/query`), unprivileged, run under a bounded timeout.
//
// Everything here is read-only: doctor never enables/starts the service or
// forces a resync. The fixes it names (`sc config`, `net start`, `w32tm
// /config`, `w32tm /resync /force`) DO need an elevated prompt, so each fix
// says so.

const w32TimeService = "W32Time"

// resyncForce is the elevated command that forces an immediate time resync.
// `/force` is undocumented (it is absent from `w32tm /?`) but real: it overrides
// the large-offset guard, so the resync still steps a clock whose skew exceeds
// the phase-correction limit — the case a bare `w32tm /resync` refuses with "the
// computer did not resync because the required time change was too big", i.e.
// the very clock doctor is diagnosing. Keep `/force`; do not "correct" it back
// to a bare `/resync`.
const resyncForce = "`w32tm /resync /force`"

// w32tmStatusMaxMs caps the `w32tm /query /status` subprocess. The query is
// local and normally sub-second, but a wedged Time service could hang it, so it
// never runs longer than this even when --timeout is larger.
const w32tmStatusMaxMs = 5000

// w32tmPath resolves the Windows Time CLI. It prefers the absolute
// %SystemRoot%\System32\w32tm.exe so a `w32tm.exe` planted earlier on PATH can't
// be run in its place (and so the spawned binary is unambiguously the
// Microsoft-signed system one). It falls back to the bare name — resolved via
// PATH + PATHEXT — when SystemRoot is unset or the absolute path isn't present,
// which also covers a 32-bit process whose System32 view is redirected to
// SysWOW64.
func w32tmPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "w32tm.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "w32tm"
}

func diagnoseClockConfig(timeoutMs int, clockConfirmedOK bool) clockConfigReport {
	rep := clockConfigReport{Supported: true}
	running, determined := diagnoseW32TimeService(&rep)
	diagnoseW32TimeSource(&rep)
	// `w32tm /query /status` fails outright when the service isn't running, so
	// don't run it once the SCM check has told us it's stopped — surface a clear
	// "skipped" line instead of a failed-command error. When the state couldn't be
	// determined, still attempt it (its own error path handles a failure).
	if determined && !running {
		rep.add("clock sync status", CheckWarn,
			"not checking the live sync status — the W32Time service isn't running (`w32tm /query /status` needs it running)",
			"start the service (see the `clock service` fix above), then re-run `"+progname.Name()+" doctor --diagnose-clock`")
	} else {
		diagnoseW32TimeStatus(&rep, timeoutMs, clockConfirmedOK)
	}
	return rep
}

// diagnoseW32TimeService reports whether W32Time is running and how it is set to
// start, entirely via the Service Control Manager (no subprocess, no admin). It
// returns (running, determined): determined is false when the state couldn't be
// read, so the caller doesn't wrongly claim the service is stopped. It never
// emits CheckFail: a stopped service is not proof the clock is wrong (on a
// standalone PC W32Time is often trigger-started on demand), and the `clock
// skew` check is the authority on whether the clock is actually off. Disabled is
// the one strong signal, so it warns with the enable-it fix.
func diagnoseW32TimeService(rep *clockConfigReport) (running, determined bool) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		rep.add("clock service", CheckWarn,
			"could not open the service manager to check the Windows Time service: "+err.Error(),
			"check it by hand: run `sc query w32time`")
		return false, false
	}
	defer windows.CloseServiceHandle(scm)

	name, err := windows.UTF16PtrFromString(w32TimeService)
	if err != nil {
		rep.add("clock service", CheckWarn, "could not check the Windows Time service: "+err.Error(), "check it by hand: run `sc query w32time`")
		return false, false
	}
	svc, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		rep.add("clock service", CheckWarn,
			"could not open the Windows Time (W32Time) service: "+err.Error(),
			"check it by hand: run `sc query w32time`")
		return false, false
	}
	defer windows.CloseServiceHandle(svc)

	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(svc, &st); err != nil {
		rep.add("clock service", CheckWarn, "could not query the W32Time service state: "+err.Error(), "check it by hand: run `sc query w32time`")
		return false, false
	}
	startType, haveStart := queryStartType(svc)
	running = st.CurrentState == windows.SERVICE_RUNNING
	disabled := haveStart && startType == windows.SERVICE_DISABLED

	// Fixes are commands the user runs in an elevated (Administrator) prompt.
	const enableFix = "in an elevated prompt: `sc config w32time start= auto`, then `net start w32time`, then " + resyncForce

	switch {
	case running && disabled:
		rep.add("clock service", CheckWarn,
			"the Windows Time service (W32Time) is running now but its start type is Disabled — it won't come back after a reboot",
			"keep it enabled across reboots (elevated): `sc config w32time start= auto`")
	case running:
		detail := "the Windows Time service (W32Time) is running"
		if haveStart {
			detail += " (start type: " + startTypeLabel(startType) + ")"
		}
		rep.add("clock service", CheckOK, detail, "")
	case disabled:
		rep.add("clock service", CheckWarn,
			"the Windows Time service (W32Time) is "+stateLabel(st.CurrentState)+" and its start type is Disabled — this machine is not keeping its clock in sync",
			enableFix)
	default:
		rep.add("clock service", CheckWarn,
			"the Windows Time service (W32Time) is "+stateLabel(st.CurrentState)+" — on a standalone PC it is often started on demand, so this alone may be fine; rely on the `clock skew` check",
			"to force a sync now (elevated): "+resyncForce+"; to keep it always running: `sc config w32time start= auto` then `net start w32time`")
	}
	return running, true
}

// queryStartType reads the service's configured start type via QueryServiceConfig,
// which needs a caller-sized buffer: call once with none to learn the size, then
// again with it. Returns ok=false (rather than a misleading zero, which is a
// real start type — Boot) if the config can't be read.
func queryStartType(svc windows.Handle) (startType uint32, ok bool) {
	var needed uint32
	err := windows.QueryServiceConfig(svc, nil, 0, &needed)
	if err != nil && err != windows.ERROR_INSUFFICIENT_BUFFER {
		return 0, false
	}
	if needed == 0 {
		return 0, false
	}
	buf := make([]byte, needed)
	cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(svc, cfg, needed, &needed); err != nil {
		return 0, false
	}
	return cfg.StartType, true
}

func startTypeLabel(t uint32) string {
	switch t {
	case windows.SERVICE_BOOT_START:
		return "Boot"
	case windows.SERVICE_SYSTEM_START:
		return "System"
	case windows.SERVICE_AUTO_START:
		return "Automatic"
	case windows.SERVICE_DEMAND_START:
		return "Manual"
	case windows.SERVICE_DISABLED:
		return "Disabled"
	default:
		return "unknown"
	}
}

func stateLabel(s uint32) string {
	switch s {
	case windows.SERVICE_STOPPED:
		return "stopped"
	case windows.SERVICE_START_PENDING:
		return "starting"
	case windows.SERVICE_STOP_PENDING:
		return "stopping"
	case windows.SERVICE_RUNNING:
		return "running"
	case windows.SERVICE_PAUSED:
		return "paused"
	default:
		return "in an unknown state"
	}
}

// diagnoseW32TimeSource reports the configured sync source, read straight from
// the registry (no subprocess, no admin). Type is an invariant token
// (NTP/NT5DS/NoSync/AllSync); NtpServer is the peer list. NoSync means the clock
// free-runs — the strong "not syncing" signal.
func diagnoseW32TimeSource(rep *clockConfigReport) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\W32Time\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		rep.add("clock source", CheckWarn,
			"could not read the Windows Time configuration from the registry: "+err.Error(),
			"check it by hand: run `w32tm /query /source` and `w32tm /query /configuration`")
		return
	}
	defer k.Close()
	typ, _, _ := k.GetStringValue("Type")
	server, _, _ := k.GetStringValue("NtpServer")

	// Point a not-syncing machine at a public NTP server; the reconfigure needs
	// an elevated prompt.
	const configFix = "point it at an NTP server in an elevated prompt: `w32tm /config /manualpeerlist:\"time.windows.com\" /syncfromflags:manual /update`, then `net stop w32time && net start w32time`, then " + resyncForce

	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "NOSYNC":
		rep.add("clock source", CheckWarn,
			"the Windows Time service is configured NOT to synchronize (Type=NoSync) — the clock free-runs",
			configFix)
	case "NTP":
		detail := "syncs from an NTP server (Type=NTP)"
		if p := firstPeer(server); p != "" {
			detail += ": " + p
		}
		rep.add("clock source", CheckOK, detail, "")
	case "NT5DS":
		rep.add("clock source", CheckOK, "syncs from the Windows domain time hierarchy (Type=NT5DS)", "")
	case "ALLSYNC":
		detail := "syncs from NTP and/or the domain hierarchy (Type=AllSync)"
		if p := firstPeer(server); p != "" {
			detail += "; NTP peer: " + p
		}
		rep.add("clock source", CheckOK, detail, "")
	case "":
		rep.add("clock source", CheckWarn,
			"the Windows Time sync type is not set in the registry",
			configFix)
	default:
		detail := "Windows Time sync type: " + typ
		if p := firstPeer(server); p != "" {
			detail += "; peer: " + p
		}
		rep.add("clock source", CheckOK, detail, "")
	}
}

// firstPeer extracts the first peer host from an NtpServer value, which is a
// space-separated list of "host,flags" entries (e.g. `time.windows.com,0x9`).
func firstPeer(ntpServer string) string {
	f := strings.Fields(ntpServer)
	if len(f) == 0 {
		return ""
	}
	host := f[0]
	if i := strings.IndexByte(host, ','); i >= 0 {
		host = host[:i]
	}
	return host
}

// diagnoseW32TimeStatus runs `w32tm /query /status` (read-only, unprivileged)
// and surfaces its output VERBATIM. w32tm's labels are localized and it has no
// machine-readable mode, so doctor shows the raw report in the OS display
// language rather than parse translated labels. Bounded by a short timeout so a
// wedged service can't hang doctor. The output is decoded from the console code
// page (see decodeConsoleBytes) — w32tm emits its localized text in the machine's
// code page (e.g. CP949 on a Korean Windows), not UTF-8.
func diagnoseW32TimeStatus(rep *clockConfigReport, timeoutMs int, clockConfirmedOK bool) {
	if timeoutMs <= 0 || timeoutMs > w32tmStatusMaxMs {
		timeoutMs = w32tmStatusMaxMs
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	out, err := exec.CommandContext(ctx, w32tmPath(), "/query", "/status").CombinedOutput()
	text := strings.TrimRight(decodeConsoleBytes(out), "\r\n \t")
	if err != nil {
		detail := "could not read the live sync status via `w32tm /query /status`: " + err.Error()
		if line := firstLine(text); line != "" {
			// w32tm's own error line (e.g. "The service has not been started.")
			// is localized but informative; pass it through.
			detail += " — " + line
		}
		rep.add("clock sync status", CheckWarn, detail,
			"run `w32tm /query /status` yourself; if it reports the service is not started, start it (elevated): `net start w32time`")
		return
	}
	// The service running is not proof the clock is synced — it can be running
	// yet far off (a stale or never-completed sync). doctor can't read that from
	// the verbatim status (the labels are localized and unparsed), so it leans on
	// the `clock skew` check's verdict: only when that check did NOT confirm a
	// good clock (drift, or it couldn't measure) does it name the forced resync as
	// a note. When the clock is already confirmed within tolerance the note would
	// just be noise, so it's omitted.
	note := ""
	if !clockConfirmedOK {
		note = "if it shows the clock is off or hasn't synced (e.g. an old or unspecified last sync time), force a sync in an elevated prompt: " + resyncForce
	}
	rep.add("clock sync status", CheckOK,
		"live status from `w32tm /query /status` (shown in the OS language):\n"+indentLines(text),
		note)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// decodeConsoleBytes converts bytes printed by a Windows console tool (here
// w32tm) into a Go UTF-8 string. Such tools emit localized text in the machine's
// console/OEM code page (e.g. CP949 on a Korean Windows), NOT UTF-8, so reading
// the bytes as UTF-8 yields U+FFFD replacement characters. It decodes via the
// active code page instead (MultiByteToWideChar → UTF-16 → UTF-8). Forcing the
// child to UTF-8 (chcp 65001 / SetConsoleOutputCP) is unreliable for piped output
// and has global side effects, so decode-on-read is the robust choice. Pure ASCII
// (or an already-UTF-8 code page) round-trips unchanged; on any failure it falls
// back to the raw bytes.
func decodeConsoleBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	cp := consoleOutputCodePage()
	if cp == cpUTF8 {
		return string(b)
	}
	n, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return string(b)
	}
	w := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), &w[0], n); err != nil {
		return string(b)
	}
	return windows.UTF16ToString(w)
}

// cpUTF8 is the Windows code-page identifier for UTF-8.
const cpUTF8 = 65001

// consoleOutputCodePage is the code page w32tm's output is encoded in: the
// console output code page when this process has a console (the interactive case
// — the common one), else the ANSI code page as a fallback (which equals the OEM
// code page on the CJK locales where this decoding matters most).
func consoleOutputCodePage() uint32 {
	if cp, err := windows.GetConsoleOutputCP(); err == nil && cp != 0 {
		return cp
	}
	if cp := windows.GetACP(); cp != 0 {
		return cp
	}
	return cpUTF8
}

// indentLines indents each line so the multi-line verbatim block reads as a
// nested detail under the checklist's "  ⚠ clock sync status:" line.
func indentLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "      " + strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}
