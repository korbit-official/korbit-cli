// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keymgmtcmd

import (
	"fmt"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/cli/probe"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

// formatEndpointVerification renders the smoke-test result as a stderr note. When
// the WebSocket endpoint is unreachable it shows the exact command to pin a
// correct URL — the derived WebSocket host is the value most likely to be wrong.
func formatEndpointVerification(name, restURL string, v probe.EndpointVerification) string {
	mark := func(ok bool) string {
		if ok {
			return "ok"
		}
		return "FAILED"
	}
	var b strings.Builder
	b.WriteString("Endpoint check:\n")
	fmt.Fprintf(&b, "  REST  %s — %s (%s)\n", v.REST.URL, mark(v.REST.Reachable), v.REST.Detail)
	fmt.Fprintf(&b, "  WS    %s — %s (%s)", v.WS.URL, mark(v.WS.Reachable), v.WS.Detail)
	if !v.WS.Reachable {
		fmt.Fprintf(&b, "\nThe WebSocket endpoint is not reachable. If the derived URL is wrong, set it explicitly and re-run:\n  %s key set-base-url %s %s --ws-base-url <wss-url>", progname.Name(), name, restURL)
	}
	if !v.REST.Reachable {
		fmt.Fprintf(&b, "\nThe REST endpoint is not reachable — double-check the base URL:\n  %s key set-base-url %s <https-url>", progname.Name(), name)
	}
	return b.String()
}
