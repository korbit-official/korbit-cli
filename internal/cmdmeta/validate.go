// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cmdmeta

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/shopspring/decimal"
)

// The per-value validation engine over a Param: NormalizeValue validates and
// normalizes a raw flag/positional string, CoerceValue does the same for an
// already-typed value (the JSON/JS frontends). Money/quantity values stay
// strings end to end — never parsed to floats. Errors say what to do instead.
// The cross-field rules (which span several params of one operation) live with
// the operations that own them, not here.

var (
	symbolRE = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)
	intRE    = regexp.MustCompile(`^\d+$`)
	currRE   = regexp.MustCompile(`^[a-z0-9]+$`)
)

// NormalizeValue validates and normalizes one raw flag/positional value per its
// param, returning the value to send (API form). label is the user-facing name
// used in error messages, e.g. `--price`.
func NormalizeValue(p Param, raw, label string) (string, error) {
	value := strings.TrimSpace(raw)
	switch p.Kind {
	case KindFlag:
		return "true", nil

	case KindString:
		if value == "" {
			return "", output.Usagef("%s must not be empty", label)
		}
		if p.Pattern != nil && !p.Pattern.MatchString(value) {
			desc := p.PatternDesc
			if desc == "" {
				desc = "must match " + p.Pattern.String()
			}
			return "", output.Usagef(`%s %s (got %q)`, label, desc, raw)
		}
		return value, nil

	case KindEnum:
		for _, e := range p.EnumValues {
			if e == value {
				return value, nil
			}
		}
		for _, e := range p.EnumValues {
			if strings.EqualFold(e, value) {
				return e, nil
			}
		}
		return "", output.Usagef("%s must be one of: %s (got %q)", label, strings.Join(p.EnumValues, ", "), raw)

	case KindInt:
		if !intRE.MatchString(value) {
			return "", output.Usagef(`%s must be an integer (got %q)`, label, raw)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return "", output.Usagef(`%s must be an integer (got %q)`, label, raw)
		}
		if p.Min != nil && n < *p.Min {
			return "", output.Usagef("%s must be >= %d (got %s)", label, *p.Min, value)
		}
		if p.Max != nil && n > *p.Max {
			return "", output.Usagef("%s must be <= %d (got %s)", label, *p.Max, value)
		}
		return strconv.Itoa(n), nil

	case KindMs:
		if !intRE.MatchString(value) {
			return "", output.Usagef(`%s must be a unix timestamp in milliseconds (got %q)`, label, raw)
		}
		return value, nil

	case KindDuration:
		// Parse-only validation: reject a malformed duration and re-emit it
		// canonically. Range bounds for a duration are NOT carried in Param.Min/
		// Max (those are int, reserved for dimensionless counts) — the one live
		// consumer (monitor --duration) range-checks in the cli layer with
		// time.Duration literals. KindDuration is a client-side flag kind; the
		// returned string is never sent on the wire.
		d, ok := ParseDuration(value)
		if !ok {
			return "", output.Usagef(`%s must be a duration like "90s" — units: ms, s, m, h (got %q)`, label, raw)
		}
		return d.String(), nil

	case KindDecimal:
		d, ok := parsePlainDecimal(value)
		if !ok {
			return "", output.Usagef(
				`%s must be a plain positive decimal string like "0.001" — no sign, exponent, or commas (got %q)`,
				label, raw)
		}
		if !d.IsPositive() {
			return "", output.Usagef("%s must be greater than zero", label)
		}
		return value, nil

	case KindSymbol:
		v := strings.ToLower(value)
		if !symbolRE.MatchString(v) {
			return "", output.Usagef(`%s must be a trading pair like "btc_krw" (got %q)`, label, raw)
		}
		return v, nil

	case KindCurrency:
		v := strings.ToLower(value)
		if !currRE.MatchString(v) {
			return "", output.Usagef(`%s must be a currency code like "btc" (got %q)`, label, raw)
		}
		return v, nil

	case KindSymbols:
		parts := splitList(value)
		if len(parts) == 0 {
			return "", output.Usagef("%s must not be empty", label)
		}
		for _, p := range parts {
			if !symbolRE.MatchString(p) {
				return "", output.Usagef(`%s: %q is not a trading pair like "btc_krw"`, label, p)
			}
		}
		return strings.Join(parts, ","), nil

	case KindCSV:
		parts := splitList(value)
		if len(parts) == 0 {
			return "", output.Usagef("%s must not be empty", label)
		}
		for _, p := range parts {
			if !currRE.MatchString(p) {
				return "", output.Usagef(`%s: %q is not a currency code like "btc"`, label, p)
			}
		}
		return strings.Join(parts, ","), nil
	}
	return value, nil
}

// ParseDuration parses a Go duration string (time.ParseDuration — units ms, s,
// m, h, and combinations like "1h30m"). ok is false on a malformed string;
// callers apply their own range check.
func ParseDuration(value string) (d time.Duration, ok bool) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return d, true
}

func parsePlainDecimal(value string) (decimal.Decimal, bool) {
	if value == "" {
		return decimal.Decimal{}, false
	}
	seenDot := false
	beforeDot := 0
	afterDot := 0
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			if seenDot {
				afterDot++
			} else {
				beforeDot++
			}
		case r == '.' && !seenDot:
			seenDot = true
		default:
			return decimal.Decimal{}, false
		}
	}
	if beforeDot == 0 || (seenDot && afterDot == 0) {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(value)
	return d, err == nil
}

// CoerceValue applies the per-kind TYPE rules to an already-present, non-null
// value decoded to a Go any (bool | int64 | float64 | string | []any), and
// returns the raw string NormalizeValue then validates — exactly as a CLI flag
// string would be. It is the shared core behind the frontends whose inputs are
// typed values rather than already-string flags. Source-specific
// null/undefined detection stays in the caller; present=false means "treat as
// omitted" (a null/undefined value handled by the caller, or a false flag).
//
// It owns the money guardrail: a decimal given a number rather than a string is
// rejected HERE, before any request is built — so no frontend can let a float
// into price/qty/amount.
func CoerceValue(p Param, v any, label string) (raw string, present bool, err error) {
	switch p.Kind {
	case KindFlag:
		b, ok := v.(bool)
		if !ok {
			return "", false, output.Usagef("%s must be a boolean", label)
		}
		if !b {
			return "", false, nil
		}
		return "true", true, nil

	case KindDecimal:
		s, ok := v.(string)
		if !ok {
			return "", false, output.Usagef(`%s must be a decimal STRING like "0.001" — money and quantities stay strings end to end, never numbers`, label)
		}
		return s, true, nil

	case KindDuration:
		// A duration carries a unit, so it must be a string ("90s") — a bare
		// number is ambiguous and rejected here. NormalizeValue then parses it.
		s, ok := v.(string)
		if !ok {
			return "", false, output.Usagef(`%s must be a duration STRING like "90s" — units: ms, s, m, h`, label)
		}
		return s, true, nil

	case KindInt, KindMs:
		switch x := v.(type) {
		case string:
			return x, true, nil
		case int64:
			return strconv.FormatInt(x, 10), true, nil
		case float64:
			if x != float64(int64(x)) {
				return "", false, output.Usagef("%s must be an integer (got %v)", label, x)
			}
			return strconv.FormatInt(int64(x), 10), true, nil
		default:
			return "", false, output.Usagef("%s must be an integer", label)
		}

	case KindEnum:
		switch x := v.(type) {
		case string:
			return x, true, nil
		case int64:
			return strconv.FormatInt(x, 10), true, nil
		case float64:
			if x == float64(int64(x)) {
				return strconv.FormatInt(int64(x), 10), true, nil
			}
			return "", false, output.Usagef("%s must be one of the documented values (got %v)", label, x)
		default:
			return "", false, output.Usagef("%s must be a string", label)
		}

	case KindSymbols, KindCSV:
		switch x := v.(type) {
		case string:
			return x, true, nil
		case []any:
			var b strings.Builder
			for i, item := range x {
				s, ok := item.(string)
				if !ok {
					return "", false, output.Usagef("%s must be a string or an array of strings", label)
				}
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(s)
			}
			return b.String(), true, nil
		default:
			return "", false, output.Usagef("%s must be a string or an array of strings", label)
		}

	default: // KindString, KindSymbol, KindCurrency
		s, ok := v.(string)
		if !ok {
			return "", false, output.Usagef("%s must be a string", label)
		}
		return s, true, nil
	}
}

func splitList(value string) []string {
	var out []string
	for _, s := range strings.Split(strings.ToLower(value), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
