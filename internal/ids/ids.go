// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package ids mints client order ids and enforces the Korbit clientOrderId
// charset.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"time"
)

// ClientOrderIDPattern is the Korbit clientOrderId charset/length rule.
var ClientOrderIDPattern = regexp.MustCompile(`^[0-9a-zA-Z.:_-]{1,36}$`)

// UUIDv7 returns an RFC 9562 UUIDv7: a 48-bit unix-ms timestamp followed by
// random bits. It is exactly 36 characters, fits the clientOrderId charset,
// is collision-resistant, and sorts by creation time — the recommended default
// for clientOrderId.
func UUIDv7() string {
	return uuidv7At(time.Now().UnixMilli())
}

func uuidv7At(unixMilli int64) string {
	var b [16]byte
	// 48-bit big-endian timestamp in the first 6 bytes.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(unixMilli))
	copy(b[0:6], ts[2:8])
	// A failing system CSPRNG is unrecoverable and would otherwise yield a
	// non-unique id (silently breaking idempotency); fail loudly. UUIDv7 returns
	// only a string, so panic is the consistent handling here.
	if _, err := rand.Read(b[6:]); err != nil {
		panic("ids: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
