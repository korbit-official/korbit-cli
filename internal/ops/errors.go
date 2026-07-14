// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"errors"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// apiErrOf extracts the *output.ApiError from an error, or nil.
func apiErrOf(err error) *output.ApiError {
	var ae *output.ApiError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// codeOf is the symbolic code of an ApiError, "" when absent. It guards a nil ae
// (a transient/network failure).
func codeOf(ae *output.ApiError) string {
	if ae == nil {
		return ""
	}
	return ae.Code
}
