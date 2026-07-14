// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package agentskill

import (
	"strings"
	"testing"
	"testing/fstest"
)

func guideSkill() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":                 {Data: []byte("---\nname: korbit\ndescription: x\n---\n\n# Operating Korbit\n\nbody\n")},
		"references/monitoring.md": {Data: []byte("# Monitoring\n\nplaybook\n")},
		"references/funding.md":    {Data: []byte("# Funding\n\nplaybook\n")},
	}
}

func TestGuideTopics(t *testing.T) {
	topics, err := GuideTopics(guideSkill())
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, one per references/<topic>.md; the overview is not listed.
	if got, want := strings.Join(topics, ","), "funding,monitoring"; got != want {
		t.Fatalf("topics = %q, want %q", got, want)
	}
	if _, err := GuideTopics(nil); err == nil {
		t.Fatal("nil fsys should error")
	}
}

func TestGuideContentOverviewStripsFrontmatter(t *testing.T) {
	body, err := GuideContent(guideSkill(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(body, "---") || strings.Contains(body, "name: korbit") {
		t.Fatalf("frontmatter not stripped:\n%s", body)
	}
	if !strings.HasPrefix(body, "# Operating Korbit") {
		t.Fatalf("body should start at the heading:\n%s", body)
	}
}

func TestGuideContentTopic(t *testing.T) {
	body, err := GuideContent(guideSkill(), "monitoring")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "# Monitoring") {
		t.Fatalf("unexpected topic body:\n%s", body)
	}
}

func TestStripFrontmatter(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"none", "# Title\n\nbody\n", "# Title\n\nbody\n"},
		{"normal", "---\nname: x\n---\n\n# Title\nbody\n", "# Title\nbody\n"},
		{"crlf", "---\r\nname: x\r\n---\r\n\r\n# Title\r\n", "# Title\r\n"},
		{"unterminated", "---\nname: x\nno close\n", "---\nname: x\nno close\n"},
		{"dashes-in-body", "---\nname: x\n---\n\nbody\n\n---\n\nmore\n", "body\n\n---\n\nmore\n"},
		{"empty", "", ""},
		{"only-opening-dashes", "---\n", "---\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripFrontmatter(c.in); got != c.want {
				t.Errorf("stripFrontmatter(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGuideContentRejectsUnknownAndEscapes(t *testing.T) {
	// Unknown topic: a recoverable error naming the valid topics.
	_, err := GuideContent(guideSkill(), "nope")
	if err == nil || !strings.Contains(err.Error(), "monitoring") {
		t.Fatalf("unknown topic error = %v, want one listing valid topics", err)
	}
	// A path separator or "." must not escape the references directory; it is
	// treated as an unknown topic, never resolved to a file.
	for _, bad := range []string{"../SKILL", "sub/x", "monitoring.md", "."} {
		if _, err := GuideContent(guideSkill(), bad); err == nil {
			t.Fatalf("topic %q should be rejected", bad)
		}
	}
	// nil fsys is an error, not a panic.
	if _, err := GuideContent(nil, ""); err == nil {
		t.Fatal("nil fsys should error")
	}
}
