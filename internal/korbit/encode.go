// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"net/url"
	"strings"
)

// orderedParams is an insertion-ordered list of key/value pairs. Order is
// load-bearing for signing: the ED25519 signature is computed over the exact
// encoded string that is sent, with `signature` appended LAST. Go's
// url.Values.Encode sorts keys, which would both reorder `signature` and change
// the signed bytes — so this type encodes in append order instead.
type orderedParams struct {
	keys []string
	vals []string
}

func (p *orderedParams) add(key, value string) {
	p.keys = append(p.keys, key)
	p.vals = append(p.vals, value)
}

// encode joins the pairs as application/x-www-form-urlencoded in append order.
// Encoding matches url.QueryEscape (spaces as "+"), the same as the URLSearchParams
// the server verifies against.
func (p *orderedParams) encode() string {
	var b strings.Builder
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.vals[i]))
	}
	return b.String()
}
