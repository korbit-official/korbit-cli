// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"

	"embed"
)

// embeddedSkills carries the bundled Agent Skill (skills/korbit) inside the
// binary, so `korbit agent skill install` needs nothing on disk and the skill
// can never drift from the CLI it documents. The embed directive's path is
// relative to this file and cannot use "..", which is why the embed lives here
// at the repo root rather than in internal/agentskill. `all:` includes any file
// the skill might add whose name starts with "." or "_".
//
//go:embed all:skills/korbit
var embeddedSkills embed.FS

// skillSource returns the embedded skill rooted at its own directory (SKILL.md
// at the top), the shape internal/agentskill expects. A failure here is a build
// fault (the path is embedded above), so it panics rather than degrading.
func skillSource() fs.FS {
	sub, err := fs.Sub(embeddedSkills, "skills/korbit")
	if err != nil {
		panic("embedded skill missing: " + err.Error())
	}
	return sub
}
