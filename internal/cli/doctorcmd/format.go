// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package doctorcmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FormatText renders the doctor report as a ✓/⚠/✗ checklist for human output,
// satisfying textout.TextFormatter so the emitter renders it from these typed
// fields (the --json path marshals the same struct). The report carries the
// resolved endpoint(s) and home so a pass against the sandbox or a per-key host
// is never mistaken for production.
func (r Report) FormatText(w io.Writer) {
	if r.OK {
		fmt.Fprint(w, "Korbit CLI doctor: healthy")
	} else {
		fmt.Fprint(w, "Korbit CLI doctor: problems found")
	}
	if r.Key != "" {
		fmt.Fprint(w, " (key "+strconv.Quote(r.Key)+")")
	}
	var ctxLine []string
	if r.BaseURL != "" {
		ctxLine = append(ctxLine, "endpoint: "+r.BaseURL)
	}
	if r.WSBaseURL != "" {
		ctxLine = append(ctxLine, "ws: "+r.WSBaseURL)
	}
	if r.Home != "" {
		ctxLine = append(ctxLine, "home: "+r.Home)
	}
	if len(ctxLine) > 0 {
		fmt.Fprint(w, "\n  "+strings.Join(ctxLine, "   "))
	}
	for _, c := range r.Checks {
		mark := "?"
		switch c.Status {
		case CheckOK:
			mark = "✓"
		case CheckWarn:
			mark = "⚠"
		case CheckFail:
			mark = "✗"
		}
		fmt.Fprintf(w, "\n  %s %s: %s", mark, c.Name, c.Detail)
		// Secondary detail (e.g. the raw transport error behind a "(network error)"
		// summary) on its own indented line, so the summary stays terse.
		if c.Context != "" {
			fmt.Fprintf(w, "\n      %s", c.Context)
		}
		// An OK check's guidance is advisory ("note"), not a remedy ("fix") — a
		// "fix:" on a ✓ line reads as if something were broken.
		if c.Fix != "" {
			label := "fix"
			if c.Status == CheckOK {
				label = "note"
			}
			fmt.Fprintf(w, "\n      %s: %s", label, c.Fix)
		}
	}
}
