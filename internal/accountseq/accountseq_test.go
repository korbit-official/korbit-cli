// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package accountseq

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		in       Inputs
		want     int
	}{
		{"explicit wins", "2", Inputs{Default: "3"}, 2},
		{"default fills omission", "", Inputs{Default: "3"}, 3},
		{"main fallback", "", Inputs{}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.explicit, tt.in)
			if err != nil {
				t.Fatalf("Resolve() err=%v", err)
			}
			if got != tt.want {
				t.Fatalf("Resolve() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveValidatesAccountSeq(t *testing.T) {
	_, err := Resolve("0", Inputs{Label: "--account-seq"})
	if err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Fatalf("Resolve() err=%v, want min-bound usage error", err)
	}
}

func TestResolveList(t *testing.T) {
	t.Run("explicit passthrough", func(t *testing.T) {
		got, err := ResolveList([]int{2, 3}, Inputs{})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 2 || got[0] != 2 || got[1] != 3 {
			t.Fatalf("got %v, want [2 3]", got)
		}
	})
	t.Run("nil falls through to Resolve", func(t *testing.T) {
		got, err := ResolveList(nil, Inputs{Default: "5"})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 1 || got[0] != 5 {
			t.Fatalf("got %v, want [5]", got)
		}
	})
	t.Run("empty falls through to main", func(t *testing.T) {
		got, err := ResolveList(nil, Inputs{})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("got %v, want [1]", got)
		}
	})
}

func TestParseList(t *testing.T) {
	t.Run("blank is nil", func(t *testing.T) {
		got, err := ParseList("  ", Inputs{})
		if err != nil || got != nil {
			t.Fatalf("ParseList(blank) = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("single value", func(t *testing.T) {
		got, err := ParseList("2", Inputs{})
		if err != nil || len(got) != 1 || got[0] != 2 {
			t.Fatalf("ParseList(\"2\") = %v, %v", got, err)
		}
	})
	t.Run("order preserved, deduped", func(t *testing.T) {
		got, err := ParseList("3, 1 ,3,2,1", Inputs{})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
			t.Fatalf("got %v, want [3 1 2] (first-occurrence order)", got)
		}
	})
	t.Run("rejects a bad element", func(t *testing.T) {
		if _, err := ParseList("1,0,2", Inputs{Label: "--account-seq"}); err == nil ||
			!strings.Contains(err.Error(), "must be >= 1") {
			t.Fatalf("ParseList bad element err=%v, want min-bound usage error", err)
		}
	})
	t.Run("rejects a blank field", func(t *testing.T) {
		if _, err := ParseList("1,,2", Inputs{Label: "--account-seq"}); err == nil {
			t.Fatalf("ParseList(\"1,,2\") err=nil, want a usage error for the empty field")
		}
	})
}

func TestEnsureOnlyWhenSupported(t *testing.T) {
	values := map[string]string{}
	if ok, err := Ensure(nil, values, "2"); err != nil || ok {
		t.Fatalf("Ensure() unsupported = ok %v err %v", ok, err)
	}
	if _, ok := values[APIName]; ok {
		t.Fatalf("unsupported Ensure must not add %s", APIName)
	}

	ok, err := Ensure([]cmdmeta.Param{Param()}, values, "2")
	if err != nil || !ok {
		t.Fatalf("Ensure() supported = ok %v err %v", ok, err)
	}
	if values[APIName] != "2" {
		t.Fatalf("Ensure() values=%v, want accountSeq 2", values)
	}
}

// TestAccountSeqIsOptionalAtSurface documents the user-facing contract:
// accountSeq is OPTIONAL for CLI flags, MCP tool args, and bot JS params.
// The frontend fills it via Ensure before calling the ops layer, which
// requires it (panics on absence). This test validates Param().Required==false
// and that Ensure fills the default when the user omits it.
func TestAccountSeqIsOptionalAtSurface(t *testing.T) {
	params := []cmdmeta.Param{Param()}

	p := Param()
	if p.Required {
		t.Fatal("accountSeq Param must not be Required — it is optional at the user surface")
	}

	values := map[string]string{"symbol": "btc_krw"}
	applied, err := Ensure(params, values, "")
	if err != nil {
		t.Fatalf("Ensure err=%v", err)
	}
	if !applied {
		t.Fatal("Ensure must apply when accountSeq param is in the surface")
	}
	if values[APIName] != MainString() {
		t.Fatalf("Ensure must default to main account: got values[accountSeq]=%q", values[APIName])
	}

	values2 := map[string]string{"symbol": "btc_krw", APIName: "3"}
	if _, err := Ensure(params, values2, ""); err != nil {
		t.Fatalf("Ensure explicit err=%v", err)
	}
	if values2[APIName] != "3" {
		t.Fatalf("Ensure must honor explicit accountSeq: got %q", values2[APIName])
	}
}
