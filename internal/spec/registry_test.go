// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"regexp"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

// The registry holds the BUILTIN commands only (setup/doctor/ip/key*/keystore*/
// monitor/mcp/logs/debug/commands/sandbox*); the endpoint command surface lives
// in the ops catalog. These guards cover the builtin surface: unique ids,
// kebab-case flags that never shadow a global, consistent boolean-ness, declared
// enums with self-validating defaults, examples resolving to their own command,
// and the depth limit. The endpoint-specific guards (path↔yaml match, retry-
// safety allowlist, response-hint match) belong to the ops catalog.

var kebab = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func globalFlagSet() map[string]bool {
	s := map[string]bool{"help": true}
	for _, g := range GlobalFlags {
		s[g.Flag] = true
	}
	return s
}

func TestUniqueCommandIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Registry {
		k := c.Key()
		if k == "" {
			t.Fatalf("command with bad id length: %v", c.ID)
		}
		if seen[k] {
			t.Fatalf("duplicate command id %q", k)
		}
		seen[k] = true
	}
}

func TestFlagsKebabAndNoGlobalShadow(t *testing.T) {
	globals := globalFlagSet()
	for _, c := range Registry {
		for _, p := range c.Params {
			if !kebab.MatchString(p.Flag) {
				t.Errorf("%s: flag --%s is not kebab-case", c.Key(), p.Flag)
			}
			if globals[p.Flag] {
				t.Errorf("%s: flag --%s shadows a global flag", c.Key(), p.Flag)
			}
		}
	}
}

func TestFlagBooleannessConsistent(t *testing.T) {
	isFlag := map[string]bool{}
	seen := map[string]bool{}
	for _, c := range Registry {
		for _, p := range c.Params {
			f := p.Kind == cmdmeta.KindFlag
			if seen[p.Flag] && isFlag[p.Flag] != f {
				t.Errorf("flag --%s used with inconsistent boolean-ness", p.Flag)
			}
			seen[p.Flag] = true
			isFlag[p.Flag] = f
		}
	}
}

func TestEnumsDeclaredAndDefaultsValidate(t *testing.T) {
	for _, c := range Registry {
		for _, p := range c.Params {
			if p.Kind == cmdmeta.KindEnum && len(p.EnumValues) == 0 {
				t.Errorf("%s: --%s is enum but declares no values", c.Key(), p.Flag)
			}
			if p.Default != "" && p.Kind == cmdmeta.KindEnum {
				ok := false
				for _, e := range p.EnumValues {
					if e == p.Default {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s: --%s default %q not in enum", c.Key(), p.Flag, p.Default)
				}
			}
		}
	}
}

func TestExamplesResolveToOwnCommand(t *testing.T) {
	for i := range Registry {
		c := &Registry[i]
		examples := append(append([]string{}, c.Examples...), c.ExperimentalExamples...)
		for _, ex := range examples {
			fields := strings.Fields(ex)
			if len(fields) == 0 || fields[0] != "{prog}" {
				t.Errorf("%s: example must start with the program-name placeholder `{prog}`: %q", c.Key(), ex)
				continue
			}
			var path []string
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "-") {
					break
				}
				path = append(path, f)
				if len(path) == 3 {
					break
				}
			}
			got := Find(path)
			if got == nil || got.Key() != c.Key() {
				t.Errorf("%s: example %q resolves to %v", c.Key(), ex, got)
			}
		}
	}
}

// TestCommandDepthAtMostThree pins the deliberate max command depth of three
// segments — Find/Key/Group and the cobra group builder handle one to three.
func TestCommandDepthAtMostThree(t *testing.T) {
	for _, c := range Registry {
		if len(c.ID) < 1 || len(c.ID) > 3 {
			t.Errorf("command %v: id depth must be 1, 2, or 3", c.ID)
		}
	}
}
