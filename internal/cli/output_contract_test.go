// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// emittedErrorKeys returns the field names the error envelope actually emits for
// err, by running it through the real output.EmitError.
func emittedErrorKeys(t *testing.T, err error) []string {
	t.Helper()
	var out, errBuf bytes.Buffer
	io := output.IO{Out: &out, Err: &errBuf}
	io.EmitError(err, true, true) // jsonMode: extract the envelope's fields
	var doc struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if e := json.Unmarshal(errBuf.Bytes(), &doc); e != nil {
		t.Fatalf("error envelope not valid JSON: %v (%s)", e, errBuf.String())
	}
	keys := make([]string, 0, len(doc.Error))
	for k := range doc.Error {
		keys = append(keys, k)
	}
	return keys
}

// The catalog's errorShapes strings are the published description of the error
// envelope. This pins them to the REAL emitted fields: every field the envelope
// can carry must be named in the matching errorShapes string. Combined with
// output's TestErrorEnvelopeExactShape (which pins the bytes), this catches a
// struct-field rename that updates one side but not the other.
func TestErrorShapesDocumentEnvelopeFields(t *testing.T) {
	ra := 5
	apiKeys := emittedErrorKeys(t, &output.ApiError{
		Message: "m", HTTPStatus: 429, Code: "C", RetryAfterSec: &ra, Body: json.RawMessage(`{}`),
	})
	usageKeys := emittedErrorKeys(t, &output.UsageError{Message: "m"})

	out, _, code := runCLI([]string{"commands", "--json"}, nil, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var cat struct {
		ErrorShapes struct {
			Usage string `json:"usage"`
			API   string `json:"api"`
		} `json:"errorShapes"`
	}
	if err := json.Unmarshal([]byte(out), &cat); err != nil {
		t.Fatalf("catalog json: %v", err)
	}
	// Match the QUOTED field name (`"code"`), not the bare token, so a rename to a
	// string that happens to be a substring of the doc text can't false-pass.
	for _, k := range apiKeys {
		if !strings.Contains(cat.ErrorShapes.API, `"`+k+`"`) {
			t.Fatalf("errorShapes.api %q omits emitted field %q", cat.ErrorShapes.API, k)
		}
	}
	for _, k := range usageKeys {
		if !strings.Contains(cat.ErrorShapes.Usage, `"`+k+`"`) {
			t.Fatalf("errorShapes.usage %q omits emitted field %q", cat.ErrorShapes.Usage, k)
		}
	}
}

// A COMPLETE paged result keeps its bare-array shape — the truncation envelope
// (wrapTruncated) must appear ONLY when the result is incomplete, never wrapping
// a result that fully covered the window. This guards the in-band truncation
// signal from misfiring and silently reshaping a normal response.
func TestCompletePagedResultIsBareArray(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// 3 rows, well under the 100-row funding-history ceiling: not truncated.
	body := `{"success":true,"data":[{"id":1,"currency":"btc"},{"id":2,"currency":"btc"},{"id":3,"currency":"btc"}]}`
	out, stderr, code := runCLI(
		[]string{"deposit", "history", "btc", "--key", "bot", "--json"},
		map[string]string{"DIGITALX_CLI_HOME": home}, &stubDoer{resp: resp(200, body, nil)})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("a complete result must be a bare JSON array, got %s (%v)", out, err)
	}
	if len(arr) != 3 {
		t.Fatalf("want 3 rows, got %d", len(arr))
	}
	if strings.Contains(out, "truncated") {
		t.Fatalf("a complete result must not carry a truncation envelope: %s", out)
	}
}

// On the eventual-consistency fallback (the placement landed but the full order
// can't be read back within the verify window), `order place` returns the accept
// ack marked acknowledgmentOnly:true + a note, so a --json consumer can tell
// deterministically it is NOT the full order. On the normal path (the order reads
// back) the flag is absent. Mirrors the bot API's acknowledgmentOnly signal.
func TestOrderPlaceAcknowledgmentOnlyShape(t *testing.T) {
	place := func(t *testing.T, getBody string) map[string]any {
		t.Helper()
		home := t.TempDir()
		seedBoundKey(t, home)
		doer := &placeDoer{
			postStatus: 200, postBody: `{"success":true,"data":{"orderId":888}}`,
			getStatus: 200, getBody: getBody,
		}
		out, stderr, code := runCLI(
			[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
				"--price", "100000000", "--qty", "0.001", "--key", "bot", "--compact"},
			map[string]string{"DIGITALX_CLI_HOME": home}, doer)
		if code != 0 {
			t.Fatalf("place must succeed, exit=%d out=%s", code, out)
		}
		if strings.Contains(stderr, "acknowledgement") || strings.Contains(stderr, "acknowledgment") || strings.Contains(stderr, "full order") {
			t.Fatalf("ack-only result note must be carried in stdout, not duplicated on stderr: %s", stderr)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("bad json: %v (%s)", err, out)
		}
		return m
	}

	// Ack-only: the lookup never returns the order (empty every attempt).
	ackOnly := place(t, `{}`)
	if ackOnly["acknowledgmentOnly"] != true {
		t.Fatalf("ack-only result must carry acknowledgmentOnly:true, got %v", ackOnly)
	}
	if note, _ := ackOnly["note"].(string); note == "" {
		t.Fatalf("ack-only result must carry a note, got %v", ackOnly)
	}
	if ackOnly["clientOrderId"] == nil {
		t.Fatalf("ack-only result must still echo clientOrderId, got %v", ackOnly)
	}

	// Full order: the lookup returns the order, so no acknowledgmentOnly flag.
	full := place(t, `{"success":true,"data":{"orderId":888,"status":"open"}}`)
	if _, present := full["acknowledgmentOnly"]; present {
		t.Fatalf("the full-order path must NOT carry acknowledgmentOnly, got %v", full)
	}
	if full["status"] != "open" {
		t.Fatalf("full-order path must return the fetched order, got %v", full)
	}
}

// When a place hits DUPLICATE_CLIENT_ORDER_ID and the existing order can't be
// read back, the order IS placed — the recovery guidance ("do NOT re-place") must
// be machine-readable in the error envelope (error.guidance), NOT a free-floating
// prose note an stdout/JSON-only consumer would miss. This is the error-path
// sibling of the success-path acknowledgmentOnly contract above.
func TestOrderPlaceDuplicateGuidanceInEnvelope(t *testing.T) {
	run := func(t *testing.T, args ...string) (string, string, int) {
		t.Helper()
		home := t.TempDir()
		seedBoundKey(t, home)
		doer := &placeDoer{
			// POST is rejected as a duplicate; the read-back lookup never finds it.
			postStatus: 409, postBody: `{"success":false,"error":{"code":409,"message":"DUPLICATE_CLIENT_ORDER_ID"}}`,
			getStatus: 200, getBody: `{}`,
		}
		base := []string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot"}
		return runCLI(append(base, args...), map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	}

	// JSON mode: the guidance rides the structured error envelope on stderr; there
	// is no separate `korbit-cli: clientOrderId …` prose note line.
	out, stderr, code := run(t, "--compact")
	if code != 3 {
		t.Fatalf("a duplicate placement must exit ExitAPI(3), exit=%d out=%s stderr=%s", code, out, stderr)
	}
	if strings.Contains(stderr, "korbit-cli: clientOrderId") {
		t.Fatalf("the recovery guidance must not be a free-floating stderr note: %s", stderr)
	}
	var env struct {
		Error struct {
			Code     *string `json:"code"`
			Guidance string  `json:"guidance"`
		} `json:"error"`
	}
	// The signing disclosure shares stderr with the envelope; pull the JSON line.
	var envLine string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			envLine = line
		}
	}
	if err := json.Unmarshal([]byte(envLine), &env); err != nil {
		t.Fatalf("error envelope not parseable: %v\n%s", err, stderr)
	}
	if env.Error.Code == nil || *env.Error.Code != "DUPLICATE_CLIENT_ORDER_ID" {
		t.Fatalf("envelope must carry the symbolic code, got %s", envLine)
	}
	if !strings.Contains(env.Error.Guidance, "do NOT re-place") {
		t.Fatalf("envelope must carry the recovery guidance, got %s", envLine)
	}

	// Human mode: the guidance is appended to the single error line — no second note.
	_, hStderr, hCode := run(t)
	if hCode != 3 {
		t.Fatalf("human-mode duplicate must also exit ExitAPI(3), exit=%d stderr=%s", hCode, hStderr)
	}
	if strings.Contains(hStderr, "korbit-cli: clientOrderId") {
		t.Fatalf("human mode must not emit a separate prose note: %s", hStderr)
	}
	if !strings.Contains(hStderr, "do NOT re-place") {
		t.Fatalf("human error line must include the recovery guidance: %s", hStderr)
	}
}
