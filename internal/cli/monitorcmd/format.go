// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package monitorcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/cli/textout"
)

// FormatText renders the --dry-run plan for human output (textout.TextFormatter);
// the --json path marshals the Plan struct. Empty script rows (where/init/on) and
// the private URL when public-only are dropped by KVBlock's skip-empty rule.
func (p Plan) FormatText(w io.Writer) {
	fmt.Fprint(w, "DRY RUN — monitor plan (no connection opened)\n")
	fmt.Fprint(w, textout.IndentLines(textout.KVBlock([][2]string{
		{"REST base", p.RESTBaseURL},
		{"WS public", p.PublicURL},
		{"WS private", p.PrivateURL},
		{"auth", textout.YesNo(p.Auth)},
		{"backfill", textout.YesNo(p.Backfill)},
		{"where", textout.OneLine(p.Where, 80)},
		{"init", textout.OneLine(p.Init, 80)},
		{"on", textout.OneLine(p.On, 80)},
	}), "  "))
	if len(p.Subscriptions) > 0 {
		fmt.Fprint(w, "\n  subscriptions:")
		for _, s := range p.Subscriptions {
			syms := ""
			if len(s.Symbols) > 0 {
				syms = " " + strings.Join(s.Symbols, ",")
			}
			extra := ""
			if len(s.Intervals) > 0 {
				extra = " intervals=" + strings.Join(s.Intervals, ",")
				if s.History > 0 {
					extra += fmt.Sprintf(" history=%d", s.History)
				}
			}
			if s.Implicit {
				extra += " (feeds candles; its own lines are not emitted)"
			}
			fmt.Fprintf(w, "\n    %s%s%s", s.Channel, syms, extra)
		}
	}
	if p.Note != "" {
		fmt.Fprintf(w, "\n  note: %s", p.Note)
	}
}
