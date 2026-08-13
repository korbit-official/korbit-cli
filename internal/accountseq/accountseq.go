// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package accountseq centralizes the accountSeq selection policy shared by
// REST operations and private WebSocket subscriptions.
package accountseq

import (
	"strconv"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

const (
	// APIName is the wire parameter name used by REST endpoints.
	APIName = "accountSeq"
	// FlagName is the CLI flag name used by endpoint commands.
	FlagName = "account-seq"
	// Main is the API's main sub-account sequence number.
	Main = 1
)

// Inputs are the shared resolution context for choosing an account sequence.
// Default is the caller's configured default (e.g. per-key metadata), when
// available. Empty values fall through to the main account.
type Inputs struct {
	Default string
	Param   cmdmeta.Param
	Label   string
}

// Param returns the shared accountSeq command-surface parameter.
func Param() cmdmeta.Param {
	one := Main
	return cmdmeta.Param{
		Flag: FlagName, API: APIName, Kind: cmdmeta.KindInt, Min: &one,
		Desc: "sub-account sequence number (default: the key's configured default, else 1/main)",
	}
}

// MainString returns the wire value for the main account.
func MainString() string { return strconv.Itoa(Main) }

// MainList returns a one-item accountSeqs list for private WebSocket
// subscriptions.
func MainList() []int { return []int{Main} }

// Resolve selects and validates a single accountSeq (REST). Explicit is the
// user-supplied value ("" when omitted); precedence: explicit → default → Main.
func Resolve(explicit string, in Inputs) (int, error) {
	p := in.Param
	if p.API == "" {
		p = Param()
	}
	label := in.Label
	if label == "" {
		label = p.API
		if p.Flag != "" {
			label = "--" + p.Flag
		}
	}
	raw := first(explicit, in.Default, MainString())
	v, err := cmdmeta.NormalizeValue(p, raw, label)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(v)
	return n, nil
}

// ParseList parses a comma-separated accountSeq selection (e.g. "1,2,3") into a
// validated, de-duplicated, order-preserving slice, each element checked through
// the shared Param (>= 1). A blank input returns (nil, nil) — the caller decides
// the default set. Order is preserved and duplicates are dropped keeping the
// first occurrence, because the first element is the one a multi-account session
// starts active on.
func ParseList(raw string, in Inputs) ([]int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	p := in.Param
	if p.API == "" {
		p = Param()
	}
	label := in.Label
	if label == "" {
		label = p.API
		if p.Flag != "" {
			label = "--" + p.Flag
		}
	}
	seen := map[int]bool{}
	out := make([]int, 0, strings.Count(raw, ",")+1)
	for _, field := range strings.Split(raw, ",") {
		// NormalizeValue rejects a blank field ("1,,2" / "1,") with a usage error,
		// so a stray comma is caught rather than silently skipped.
		v, err := cmdmeta.NormalizeValue(p, strings.TrimSpace(field), label)
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(v)
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// ResolveList selects accountSeqs for WebSocket subscriptions. Explicit is the
// user-supplied list (nil/empty when omitted); when empty, it resolves through
// the standard single-account precedence and wraps the result as a one-element
// list.
func ResolveList(explicit []int, in Inputs) ([]int, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}
	n, err := Resolve("", in)
	if err != nil {
		return nil, err
	}
	return []int{n}, nil
}

// Supports reports whether params include accountSeq and returns that Param.
func Supports(params []cmdmeta.Param) (cmdmeta.Param, bool) {
	for _, p := range params {
		if p.API == APIName {
			return p, true
		}
	}
	return cmdmeta.Param{}, false
}

// Ensure adds accountSeq to values when the command surface supports it and the
// caller did not supply it, resolving it through the standard precedence. The
// resolved value is written to values[APIName] — read it from there. The boolean
// reports whether the surface has an accountSeq parameter (false => nothing
// written).
func Ensure(params []cmdmeta.Param, values map[string]string, def string) (bool, error) {
	p, ok := Supports(params)
	if !ok {
		return false, nil
	}
	n, err := Resolve(values[APIName], Inputs{Default: def, Param: p})
	if err != nil {
		return true, err
	}
	values[APIName] = strconv.Itoa(n)
	return true, nil
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
