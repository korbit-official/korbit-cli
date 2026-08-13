// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// genPEM returns a fresh ED25519 private PEM for import-path tests.
func genPEM(t *testing.T) string {
	t.Helper()
	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return kp.PrivatePEM
}

func TestIsSandboxByAPIKeyPrefix(t *testing.T) {
	sandboxID := SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"
	realID := "live-key-1234"
	cases := []struct {
		name string
		id   *string
		want bool
	}{
		{"unbound", nil, false},
		{"sandbox", &sandboxID, true},
		{"real", &realID, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Record{APIKeyID: c.id}).IsSandbox(); got != c.want {
				t.Fatalf("IsSandbox = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAddBoundIsNeverDefaultOnFreshInstall(t *testing.T) {
	env := newEnv(t, "file")
	pem := genPEM(t)
	r, err := env.m.AddBound("sandbox", pem, SandboxAPIKeyPrefix+"ED25519_KEY_1", "file")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsDefault {
		t.Fatal("a sandbox key must never become the default, even as the first key")
	}
	def, _ := env.m.DefaultKeyName()
	if def != "" {
		t.Fatalf("default should remain unset, got %q", def)
	}
	// Resolve("") must NOT fall back to the sole (sandbox) key — explicit-only.
	if _, err := env.m.Resolve(""); err == nil {
		t.Fatal("Resolve(\"\") should error with only a sandbox key present (no default, no sole-key fallback)")
	}
	// But an explicit resolve of the sandbox key works.
	if _, err := env.m.Resolve("sandbox"); err != nil {
		t.Fatalf("explicit Resolve of the sandbox key should work: %v", err)
	}
}

func TestRealKeyAddedAfterSandboxBecomesDefault(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddBound("sandbox", genPEM(t), SandboxAPIKeyPrefix+"K1", "file"); err != nil {
		t.Fatal(err)
	}
	// A real key added afterwards must become the default even though it is the
	// second key (the sandbox key left DefaultKey nil).
	r, err := env.m.Add("real", "", "file")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsDefault {
		t.Fatal("the first non-sandbox key must become the default")
	}
	def, _ := env.m.DefaultKeyName()
	if def != "real" {
		t.Fatalf("default = %q, want \"real\"", def)
	}
}

func TestAddBoundRealKeyBecomesDefault(t *testing.T) {
	env := newEnv(t, "file")
	r, err := env.m.AddBound("real", genPEM(t), "live-key-1", "file")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsDefault {
		t.Fatal("a bound real key, added first, should become the default")
	}
}

func TestUseRefusesSandboxKey(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddBound("sandbox", genPEM(t), SandboxAPIKeyPrefix+"K1", "file"); err != nil {
		t.Fatal(err)
	}
	err := env.m.Use("sandbox")
	if err == nil || !strings.Contains(err.Error(), "sandbox key") {
		t.Fatalf("Use must refuse a sandbox key, got %v", err)
	}
	def, _ := env.m.DefaultKeyName()
	if def != "" {
		t.Fatalf("default must stay unset after a refused Use, got %q", def)
	}
}

func TestSummaryTagsSandbox(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddBound("sandbox", genPEM(t), SandboxAPIKeyPrefix+"K1", "file"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.Add("real", "", "file"); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("real", "live-1"); err != nil {
		t.Fatal(err)
	}
	list, err := env.m.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		switch s.Name {
		case "sandbox":
			if !s.IsSandbox {
				t.Error("sandbox key summary should report IsSandbox=true")
			}
		case "real":
			if s.IsSandbox {
				t.Error("real key summary should report IsSandbox=false")
			}
		}
	}
}

// A sandbox key (one bound to a SANDBOX_ api-key id) must carry the sandbox
// token in its local name, so selecting it always puts "sandbox" on the command
// line. The rule is enforced at every seam that attaches an id to a name.

func TestAddBoundRequiresSandboxToken(t *testing.T) {
	sb := SandboxAPIKeyPrefix + "K1"
	cases := []struct {
		name, id string
		wantErr  bool
	}{
		{"sandbox", sb, false},
		{"sandbox-test", sb, false},
		{"MySandboxKey", sb, false}, // case-insensitive
		{"prod", sb, true},          // sandbox id, name lacks the token
		{"test", sb, true},
		{"prod", "live-key-1", false}, // real id: any name is fine
	}
	for _, c := range cases {
		t.Run(c.name+"/"+c.id, func(t *testing.T) {
			env := newEnv(t, "file")
			_, err := env.m.AddBound(c.name, genPEM(t), c.id, "file")
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "sandbox") {
					t.Fatalf("AddBound(%q,%q) error = %v, want a sandbox-naming error", c.name, c.id, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("AddBound(%q,%q) unexpected error: %v", c.name, c.id, err)
			}
		})
	}
}

func TestBindRequiresSandboxToken(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("prod", genPEM(t), "file"); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("prod", SandboxAPIKeyPrefix+"K1"); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("Bind of a sandbox id onto %q must be refused, got %v", "prod", err)
	}
	// A real id binds fine under that name.
	if err := env.m.Bind("prod", "live-key-1"); err != nil {
		t.Fatalf("binding a real id should succeed: %v", err)
	}
	// A conforming name accepts the sandbox id.
	if _, err := env.m.Add("sandbox-x", genPEM(t), "file"); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("sandbox-x", SandboxAPIKeyPrefix+"K2"); err != nil {
		t.Fatalf("binding a sandbox id under a conforming name should succeed: %v", err)
	}
}

func TestRenameSandboxKeyMustKeepToken(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddBound("sandbox", genPEM(t), SandboxAPIKeyPrefix+"K1", "file"); err != nil {
		t.Fatal(err)
	}
	// Renaming a sandbox key to a name that drops the token is refused.
	if _, err := env.m.Rename("sandbox", "prod"); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("renaming a sandbox key to a non-conforming name must be refused, got %v", err)
	}
	// The original name still exists (rename failed before any mutation).
	if _, err := env.m.Show("sandbox"); err != nil {
		t.Fatalf("sandbox key should be untouched after the refused rename: %v", err)
	}
	// A conforming new name is allowed.
	if _, err := env.m.Rename("sandbox", "sandbox-2"); err != nil {
		t.Fatalf("renaming to a conforming name should succeed: %v", err)
	}
	// A real key renames to anything.
	if _, err := env.m.AddBound("real", genPEM(t), "live-key-1", "file"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.Rename("real", "anything"); err != nil {
		t.Fatalf("renaming a real key should be unrestricted: %v", err)
	}
}

func TestAssertSandboxKeyName(t *testing.T) {
	if err := AssertSandboxKeyName("sandbox"); err != nil {
		t.Errorf("\"sandbox\" should be accepted: %v", err)
	}
	if err := AssertSandboxKeyName("Sandbox-9"); err != nil {
		t.Errorf("case-insensitive token should be accepted: %v", err)
	}
	if err := AssertSandboxKeyName("prod"); err == nil {
		t.Error("a name without the token must be rejected")
	}
}

func TestAddBoundRejectsEmptyInputs(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddBound("k", "", "id-1", "file"); err == nil {
		t.Error("AddBound must require private material")
	}
	if _, err := env.m.AddBound("k", genPEM(t), "", "file"); err == nil {
		t.Error("AddBound must require an api-key id")
	}
}
