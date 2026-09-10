// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"

	"embed"
)

// embeddedSkills carries the bundled Agent Skill (skills/digitalx-cli) inside the
// binary, so `dgx-cli agent skill install` needs nothing on disk and the skill
// can never drift from the CLI it documents. The embed directive's path is
// relative to this file and cannot use "..", which is why the embed lives here
// at the repo root rather than in internal/agentskill. `all:` includes any file
// the skill might add whose name starts with "." or "_".
//
//go:embed all:skills/digitalx-cli
var embeddedSkills embed.FS

// skillSource returns the embedded skill rooted at its own directory (SKILL.md
// at the top), the shape internal/agentskill expects. A failure here is a build
// fault (the path is embedded above), so it panics rather than degrading.
func skillSource() fs.FS {
	sub, err := fs.Sub(embeddedSkills, "skills/digitalx-cli")
	if err != nil {
		panic("embedded skill missing: " + err.Error())
	}
	return sub
}
