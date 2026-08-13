// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"bytes"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// joinRows reassembles verbatim row documents into a JSON array. It cannot
// fail, so a result whose rows were already gathered can never be dropped at
// the marshal step.
func joinRows(rows []json.RawMessage) json.RawMessage {
	out := make([]byte, 0, 2)
	out = append(out, '[')
	for i, row := range rows {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, row...)
	}
	return append(out, ']')
}

// orderedJSON renders ordered params as a JSON object preserving order — the
// pre-signing parameter record the order journal stores for an order's intent.
func orderedJSON(kvs []korbit.KV) string {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(kv.Key)
		val, _ := json.Marshal(kv.Value)
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.String()
}

// jsonNumberField extracts a top-level field from a JSON object as its string
// rendering ("" when absent). Numbers come back as digits, strings unquoted.
func jsonNumberField(doc json.RawMessage, field string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(doc, &m) != nil {
		return ""
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// numField is an ordered numeric field for withFields.
type numField struct {
	key string
	val int64
}

// withFields returns obj with the given numeric fields appended in order,
// preserving obj's existing fields and their order (additive — the server bytes
// are untouched). obj must be a brace-delimited JSON object; it is returned
// unchanged otherwise, so a caller can splice unconditionally.
func withFields(obj json.RawMessage, fields ...numField) json.RawMessage {
	t := bytes.TrimSpace(obj)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return obj
	}
	var b bytes.Buffer
	b.Write(t[:len(t)-1]) // everything up to the closing brace
	nonEmpty := len(bytes.TrimSpace(t[1:len(t)-1])) > 0
	for i, f := range fields {
		if nonEmpty || i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		v, _ := json.Marshal(f.val)
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonInt64Field extracts a top-level integer field, 0 when absent/invalid.
func jsonInt64Field(doc json.RawMessage, field string) int64 {
	var m map[string]json.RawMessage
	if json.Unmarshal(doc, &m) != nil {
		return 0
	}
	raw, ok := m[field]
	if !ok {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}
