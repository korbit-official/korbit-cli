// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package agentskill

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

// This file reads the skill body for callers that surface its guidance in place
// rather than installing it to disk — chiefly the MCP guide tool, which
// is how an MCP-only client (Claude Desktop via the .mcpb Desktop Extension)
// reaches the same guidance Claude Code gets from the installed skill. It reads
// the same embedded tree Install writes, so the two can never drift.
//
// Layout: SKILL.md is the overview and task router; each references/<topic>.md is
// a focused playbook. Topics are discovered from the references directory, so
// adding a reference file adds a guide topic with no code change — the same
// single-source-of-truth rule the content hash enforces for installs.
const (
	guideOverviewFile = "SKILL.md"
	guideRefDir       = "references"
)

// GuideTopics returns the focused-playbook topic names available in the skill,
// sorted — one per references/<topic>.md. The overview (SKILL.md) is always
// available via the empty topic and is not listed here. A nil fsys (no skill
// embedded in this build) is an error.
func GuideTopics(fsys fs.FS) ([]string, error) {
	if fsys == nil {
		return nil, fmt.Errorf("no skill content embedded in this build")
	}
	entries, err := fs.ReadDir(fsys, guideRefDir)
	if err != nil {
		return nil, err
	}
	var topics []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasSuffix(name, ".md") {
			topics = append(topics, strings.TrimSuffix(name, ".md"))
		}
	}
	sort.Strings(topics)
	return topics, nil
}

// GuideContent returns the readable guide text for a topic: the empty topic is
// the SKILL.md overview with its skill-system YAML frontmatter stripped (that
// block is trigger metadata for the skill loader, not guidance), and any other
// topic t is references/<t>.md verbatim. The topic must be one that GuideTopics
// advertises — anything else is an error naming the valid topics, so the caller
// (or the model) can recover. Validating against the discovered set (rather than
// constructing a path from the raw input) keeps "advertised == served" true by
// construction and leaves no room for a path to escape the references directory.
func GuideContent(fsys fs.FS, topic string) (string, error) {
	if fsys == nil {
		return "", fmt.Errorf("no skill content embedded in this build")
	}
	if topic == "" {
		b, err := fs.ReadFile(fsys, guideOverviewFile)
		if err != nil {
			return "", err
		}
		return stripFrontmatter(string(b)), nil
	}
	topics, err := GuideTopics(fsys)
	if err != nil {
		return "", err
	}
	if !slices.Contains(topics, topic) {
		return "", fmt.Errorf("unknown guide topic %q; available topics: %s (omit the topic for the overview)",
			topic, strings.Join(topics, ", "))
	}
	// topic is a verified GuideTopics member — a base filename with no separator —
	// so this join cannot escape guideRefDir.
	b, err := fs.ReadFile(fsys, path.Join(guideRefDir, topic+".md"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stripFrontmatter drops a leading YAML frontmatter block ("---" … "---") from
// markdown and returns the body. Markdown with no frontmatter is returned
// unchanged; an unterminated block is left intact (better to show too much than
// to silently truncate the guide).
func stripFrontmatter(s string) string {
	_, body, ok := splitFrontmatter(s)
	if !ok {
		return s
	}
	// Trim the blank line(s) between the frontmatter and the body; the cutset
	// covers \r so a CRLF blank line leaves no stray carriage return.
	return strings.TrimLeft(body, "\r\n")
}
