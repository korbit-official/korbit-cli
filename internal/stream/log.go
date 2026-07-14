// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package stream

// Log-tag vocabulary for the stream layer. Every stream-layer record carries
// LogComponentKey = LogComponentStream (connection mechanics) or
// LogComponentState (the materialized state store), so `grep component=stream`
// isolates the whole layer; a mirrored reliability notice adds
// LogKindKey = LogKindNotice (+ LogCodeKey = <CODE>), so `grep kind=stream_notice`
// isolates just those. Under --log-format json these become real JSON fields.
//
// The vocabulary lives here, with the layer it describes, so the cli frontends
// that build the stream loggers (monitor/tui) and stream.LogNotice (which stamps
// the code) all reference one source and cannot drift. See doc.go "Logging".
const (
	LogComponentKey    = "component"
	LogComponentStream = "stream"       // connection mechanics (internal/stream)
	LogComponentState  = "stream/state" // state reconcile (internal/stream/state)
	LogKindKey         = "kind"
	LogKindNotice      = "stream_notice" // a mirrored Notice
	LogCodeKey         = "code"          // the notice's symbolic code
)
