// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import "time"

// SetClaimTimingForTest shrinks the auto-claim poll cadences so the poll loop
// runs fast under test, and returns a restore func. Test-only seam.
func SetClaimTimingForTest(pollMs, backoffMs, keyActivePollMs int, keyActiveBudget time.Duration) (restore func()) {
	op, ob, okp, okb := setupClaimPollMs, setupClaimBackoffMs, setupKeyActivePollMs, setupKeyActiveBudget
	setupClaimPollMs, setupClaimBackoffMs, setupKeyActivePollMs, setupKeyActiveBudget = pollMs, backoffMs, keyActivePollMs, keyActiveBudget
	return func() {
		setupClaimPollMs, setupClaimBackoffMs, setupKeyActivePollMs, setupKeyActiveBudget = op, ob, okp, okb
	}
}
