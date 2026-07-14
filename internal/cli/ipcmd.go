// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/spf13/cobra"
)

// ipView renders the public-IP probe report for human output. probe.Report
// lives in the shared cli/probe package, which must not depend on the
// human-output toolkit, so the command wraps it here: embedding keeps the
// --json shape identical and implements textout.TextFormatter.
type ipView struct{ probe.Report }

func (v ipView) FormatText(w io.Writer) {
	ipv4, ipv6 := "", ""
	if v.IPv4 != nil {
		ipv4 = *v.IPv4
	}
	if v.IPv6 != nil {
		ipv6 = *v.IPv6
	}
	out := textout.KVBlock([][2]string{
		{"IPv4", ipv4},
		{"IPv6", ipv6},
	})
	// The allowlist guidance is part of the result, not stderr narration — the
	// --json document carries the same IPs structurally.
	out += "\n\n" + allowlistGuidance(v.Report)
	fmt.Fprint(w, out)
}

// runIP implements the `ip` command: report the public IP(s) Korbit sees, for
// the API-key IP allowlist.
func (rt *runtime) runIP(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q", args[0])
	}
	timeoutMs, err := rt.ipTimeout(cmd)
	if err != nil {
		return err
	}

	networks := rt.deps.netFamily.Networks()
	rep := probe.IPs(rt.deps.IPProbe, probe.ProdBaseURL, timeoutMs, networks)
	if !rep.Any() {
		return fmt.Errorf("could not determine your public IP over %s — check your network connection", probe.FamiliesLabel(networks))
	}
	return rt.Emit("ip", ipView{rep})
}

// ipTimeout returns the probe timeout, honoring --timeout when set.
func (rt *runtime) ipTimeout(cmd *cobra.Command) (int, error) {
	if cmd.Flags().Changed("timeout") {
		return parseRange(rt.timeout, "--timeout", 1, 600000)
	}
	return probe.DefaultTimeoutMs, nil
}

// allowlistGuidance tells the user exactly what to paste into the portal's IP
// allowlist; it is part of the `ip` result rendered on stdout (ipView.FormatText).
func allowlistGuidance(rep probe.Report) string {
	var l []string
	l = append(l, fmt.Sprintf("Add the following to your API key's IP allowlist at %s:", probe.PortalURL))
	if rep.IPv4 != nil {
		l = append(l, fmt.Sprintf("  %-24s (IPv4)", *rep.IPv4))
	}
	if rep.IPv6Prefix64 != nil {
		l = append(l, fmt.Sprintf("  %-24s (IPv6 /64 — covers address rotation within your prefix)", *rep.IPv6Prefix64))
	} else if rep.IPv6 != nil {
		l = append(l, fmt.Sprintf("  %-24s (IPv6)", *rep.IPv6))
	}
	l = append(l, "The allowlist accepts a full IPv4 address (a /32) and an IPv6 /64 or /128.")
	return strings.Join(l, "\n")
}
