// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"sync"
)

// syncWriter serializes concurrent writes to a shared sink so independent
// writers can't interleave mid-line. It backs the one locked stderr sink
// (buildLogger): the operational logger, output.IO notes/errors, a bot's
// console.log, and ops honesty notes all write through it from different
// goroutines (monitor/mcp), and the --log-file writer when logs are diverted.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
