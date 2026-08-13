// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// TestReleaseTargetsMatchGoreleaser pins releaseTargets to the platforms the
// release actually builds. Drift is silent and expensive in one direction: a
// target added to .goreleaser.yaml but not here ships an archive whose notices
// file omits modules that platform links, which is a license violation and only
// shows up in an audit.
func TestReleaseTargetsMatchGoreleaser(t *testing.T) {
	src, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := goreleaserTargets(t, string(src))
	if got := key(releaseTargets); got != key(want) {
		t.Errorf("releaseTargets is out of date with .goreleaser.yaml\n got: %s\nwant: %s", got, key(want))
	}
}

func key(targets []target) string {
	var s []string
	for _, t := range targets {
		s = append(s, t.String())
	}
	sort.Strings(s)
	return strings.Join(s, " ")
}

// goreleaserTargets reads the build matrix out of .goreleaser.yaml: the goos ×
// goarch cross product minus the `ignore` pairs. It scans rather than parses
// YAML (the module has no YAML dependency, and adding one for a test would put
// it in front of every contributor). The scan is deliberately strict — an
// unexpected shape fails the test rather than silently matching less.
func goreleaserTargets(t *testing.T, src string) []target {
	var goos, goarch []string
	var ignored []target
	var pending target
	list := "" // which sequence the following "- item" lines belong to

	for _, line := range buildsBlock(t, src) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		switch trimmed {
		case "goos:", "goarch:", "ignore:":
			list = strings.TrimSuffix(trimmed, ":")
			continue
		}
		item, isItem := strings.CutPrefix(trimmed, "- ")
		switch {
		case list == "goos" && isItem:
			goos = append(goos, item)
		case list == "goarch" && isItem:
			goarch = append(goarch, item)
		case list == "ignore" && isItem && strings.HasPrefix(item, "goos:"):
			pending = target{goos: strings.TrimSpace(strings.TrimPrefix(item, "goos:"))}
		case list == "ignore" && strings.HasPrefix(trimmed, "goarch:"):
			pending.goarch = strings.TrimSpace(strings.TrimPrefix(trimmed, "goarch:"))
			if pending.goos == "" || pending.goarch == "" {
				t.Fatalf("incomplete ignore entry near %q", trimmed)
			}
			ignored = append(ignored, pending)
			pending = target{}
		default:
			list = "" // any other key ends the sequence
		}
	}
	if len(goos) == 0 || len(goarch) == 0 {
		t.Fatalf("no build matrix found in .goreleaser.yaml (goos=%v goarch=%v)", goos, goarch)
	}

	skip := map[target]bool{}
	for _, ig := range ignored {
		skip[ig] = true
	}
	var out []target
	for _, sys := range goos {
		for _, arch := range goarch {
			if tg := (target{sys, arch}); !skip[tg] {
				out = append(out, tg)
			}
		}
	}
	return out
}

// buildsBlock returns the lines under the top-level `builds:` key, so keys that
// repeat elsewhere in the file (archives' `format_overrides`, the hooks' GOOS
// env) cannot be mistaken for part of the matrix.
func buildsBlock(t *testing.T, src string) []string {
	lines := strings.Split(src, "\n")
	start := -1
	for i, line := range lines {
		if line == "builds:" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatal("no top-level `builds:` key in .goreleaser.yaml")
	}
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		return lines[start:i] // next top-level key
	}
	return lines[start:]
}
