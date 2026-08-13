// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package uikit

import "testing"

func TestGroupThousands(t *testing.T) {
	cases := map[string]string{
		// >8 integer digits with a multi-digit fraction: cap to one, mark the drop.
		"9621139961.931951158765": "9,621,139,961.9…",
		"962113995.79818":         "962,113,995.7…",
		"123456789.12345":         "123,456,789.1…",
		// >8 integer digits but nothing to trim: no ellipsis.
		"123456789":   "123,456,789",
		"123456789.0": "123,456,789.0",
		"123456789.5": "123,456,789.5",
		// ≤8 integer digits: full precision kept.
		"12345678.9012": "12,345,678.9012",
		"1000":          "1,000",
		"999":           "999",
		"0.5":           "0.5", // small values keep their fraction
		"-1234.5":       "-1,234.5",
		"+1234.5":       "+1,234.5",
		// Negatives still trip the cap on their integer digits alone.
		"-9621139961.93": "-9,621,139,961.9…",
		"":               "",
		".5":             ".5",      // no whole part: leave it alone
		"12a34.5":        "12a34.5", // not a plain decimal: unchanged
		"abc":            "abc",
	}
	for in, want := range cases {
		if got := GroupThousands(in); got != want {
			t.Errorf("GroupThousands(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupExact(t *testing.T) {
	cases := map[string]string{
		// GroupExact never caps: every fractional digit survives.
		"9621139961.931951158765": "9,621,139,961.931951158765",
		"0.5":                     "0.5",
		"-1234.5":                 "-1,234.5",
		"1000":                    "1,000",
		"":                        "",
		".5":                      ".5",
		"abc":                     "abc",
	}
	for in, want := range cases {
		if got := GroupExact(in); got != want {
			t.Errorf("GroupExact(%q) = %q, want %q", in, got, want)
		}
	}
}

// The pad/clip helpers measure display cells, not runes: a Korean glyph is
// one rune but two cells, and every result must land on exactly the asked
// width (lipgloss.Width-consistent) or the table columns shear.
func TestPadClipDisplayWidth(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		// ASCII fast path — behavior identical to the pre-width-aware helpers.
		{"padleft ascii fits", PadLeft("42", 5), "   42"},
		{"padleft ascii clips left", PadLeft("123456", 4), "…456"},
		{"padright ascii fits", PadRight("ab", 4), "ab  "},
		{"padright ascii clips", PadRight("abcdef", 4), "abc…"},
		{"cliptail ascii", ClipTail("abcdef", 4), "abc…"},
		{"cliptail ascii fits", ClipTail("abc", 4), "abc"},
		{"padleft w1", PadLeft("abc", 1), "c"},
		{"cliptail w1", ClipTail("abc", 1), "a"},
		{"cliptail w1 wide leading glyph", ClipTail("가나", 1), "…"},

		// Wide glyphs: 입/출/금/액 are two cells each.
		{"padright wide fits", PadRight("입출금", 8), "입출금  "},
		{"padright wide exact", PadRight("입출금", 6), "입출금"},
		{"padright wide clips", PadRight("입출금액", 5), "입출…"},
		{"cliptail wide clips", ClipTail("입출금액", 5), "입출…"},
		// Dropping a two-cell glyph can strand a spare cell: pad it so the
		// result is still exactly w cells.
		{"cliptail wide spare cell", ClipTail("가나다라", 6), "가나 …"},
		{"padleft wide fits", PadLeft("가나다", 7), " 가나다"},
		{"padleft wide clips", PadLeft("가나다라", 5), "…다라"},
		{"padleft wide spare cell", PadLeft("가나다라", 6), "… 다라"},
		{"mixed ascii wide", PadRight("금액 krw", 9), "금액 krw "},
		{"padleft wide w1", PadLeft("가나", 1), " "},

		// PadCenter: even split, odd leftover cell to the right, and clip-on-overflow.
		{"padcenter ascii even", PadCenter("ab", 6), "  ab  "},
		{"padcenter ascii odd", PadCenter("bid", 10), "   bid    "},
		{"padcenter wide fits", PadCenter("입출금", 8), " 입출금 "},
		{"padcenter wide exact", PadCenter("가격", 4), "가격"},
		{"padcenter wide clips", PadCenter("입출금액", 5), "입출…"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}
