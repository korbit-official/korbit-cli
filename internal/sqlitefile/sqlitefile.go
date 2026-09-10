// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package sqlitefile turns a filesystem path into a SQLite DSN the driver
// cannot misread.
//
// modernc.org/sqlite parses a bare path as `<path>?<query>`, so a database whose
// path contains a `?` — legal in a directory name on every platform this CLI
// targets, and reachable through DIGITALX_CLI_HOME — would be opened at a
// truncated path with the rest read as parameters. A `file:` URI with the path
// percent-escaped has no such ambiguity.
//
// internal/journal and internal/botapi both open through it, so every database
// under the CLI home resolves the same path to the same file.
package sqlitefile

import "net/url"

// DSN turns a filesystem path into a driver DSN the driver cannot misread (see
// the package doc for why the escaping is required).
//
// Query PARAMETERS cannot be passed through it, deliberately: `?` is escaped
// along with everything else, so `DSN(path) + "?_pragma=…"` would be read as
// part of the filename. A caller that wants a pragma issues it on the
// connection, which is what every opener here does.
func DSN(path string) string {
	return "file:" + url.PathEscape(path)
}
