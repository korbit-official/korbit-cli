// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ipPath is the public endpoint that echoes the caller's source IP as Digital X
// sees it. Unlike every other endpoint it returns text/plain (the bare IP),
// not the {success,data} envelope — so it bypasses Execute.
const ipPath = "/v2/ip"

// IPProber fetches the caller's public IP from baseURL over a specific network
// family ("tcp4" or "tcp6") and returns the trimmed plaintext IP. Forcing the
// family is what lets the CLI report both the IPv4 and the IPv6 address Digital X
// sees you from, so an agent can allowlist whichever family it will connect over.
// It is a seam: the default dials the real network (the cli builds it over
// netbind.FamilyDoer); tests inject a stub. The userAgent is the caller-composed
// User-Agent; an empty string falls back to the bare "digitalx-cli/<version>".
type IPProber func(ctx context.Context, network, baseURL, userAgent string, timeoutMs int) (string, error)

// ProbeIP performs GET /v2/ip through doer and returns the trimmed plaintext IP
// the server echoes back. The family pin and source binding live in the doer the
// caller supplies (see netbind.FamilyDoer), so this carries no networking policy
// of its own. An empty userAgent falls back to "digitalx-cli/<version>".
func ProbeIP(ctx context.Context, doer Doer, baseURL, userAgent string) (string, error) {
	if userAgent == "" {
		userAgent = defaultUserAgent()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+ipPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("accept", "text/plain")
	req.Header.Set("user-agent", userAgent)

	res, err := doer.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	// The body is a single IP literal; cap the read so a misbehaving endpoint
	// can't stream unbounded data into the probe.
	body, _ := io.ReadAll(io.LimitReader(res.Body, 256))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s returned HTTP %d", ipPath, res.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}
