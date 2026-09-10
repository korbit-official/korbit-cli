// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/selfupdate"
)

// TestSuggestSkillRefresh pins that the follow-up carries everything a stdout /
// --json consumer needs to act: the suggested flag, a runnable command, and a
// message that names the refresh commands.
func TestSuggestSkillRefresh(t *testing.T) {
	s := suggestSkillRefresh()
	if !s.Suggested {
		t.Error("Suggested should be set")
	}
	if !strings.Contains(s.Command, "agent skill doctor") {
		t.Errorf("Command = %q, want the agent skill doctor command", s.Command)
	}
	for _, want := range []string{"agent skill doctor", "agent skill install", "--claude", "--codex"} {
		if !strings.Contains(s.Message, want) {
			t.Errorf("Message %q missing %q", s.Message, want)
		}
	}
}

// TestUpdateViewJSONShape pins the --json contract an agent reads: the skill
// follow-up marshals as a "skill" object alongside the inlined UpdateResult
// fields, and is omitted entirely when there is no follow-up (dry-run /
// already-current, where runUpdate leaves Skill nil).
func TestUpdateViewJSONShape(t *testing.T) {
	base := &selfupdate.UpdateResult{PreviousVersion: "v1.0.0", LatestVersion: "v1.1.0", Updated: true}

	withSkill, err := json.Marshal(updateView{UpdateResult: base, Skill: suggestSkillRefresh()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"latestVersion":"v1.1.0"`, `"updated":true`, `"skill":{`, `"suggested":true`, `"command":`, `"message":`} {
		if !strings.Contains(string(withSkill), want) {
			t.Errorf("json %s missing %q", withSkill, want)
		}
	}

	noSkill, _ := json.Marshal(updateView{UpdateResult: base})
	if strings.Contains(string(noSkill), "skill") {
		t.Errorf("skill should be omitted when nil: %s", noSkill)
	}
}

// TestUpdateViewTextRendersSuggestion pins that human output appends the skill
// suggestion under the update line when set, and omits it when nil.
func TestUpdateViewTextRendersSuggestion(t *testing.T) {
	base := &selfupdate.UpdateResult{PreviousVersion: "v1.0.0", LatestVersion: "v1.1.0", Updated: true}

	var withSkill strings.Builder
	updateView{UpdateResult: base, Skill: suggestSkillRefresh()}.FormatText(&withSkill)
	out := withSkill.String()
	if !strings.Contains(out, "updated dgx-cli v1.0.0 → v1.1.0") {
		t.Errorf("missing update line: %q", out)
	}
	if !strings.Contains(out, "agent skill doctor") {
		t.Errorf("missing skill suggestion: %q", out)
	}

	var noSkill strings.Builder
	updateView{UpdateResult: base}.FormatText(&noSkill)
	if strings.Contains(noSkill.String(), "agent skill") {
		t.Errorf("no skill line expected when Skill is nil: %q", noSkill.String())
	}
}
