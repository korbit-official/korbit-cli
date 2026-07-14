// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package journal is the CLI's local action journal: a SQLite database under
// the CLI home that records what the tool did, in three linked tables. An
// operations row groups the one-or-more API calls a single logical operation
// makes (a place reconcile sends and then looks up; a history walk pages);
// an api_calls row records each underlying HTTP call (linked to its operation,
// with a 1-based sequence within it); and an orders row holds a dedicated
// record per placed order (minted at clientOrderId time and updated with the
// server order id once the placement resolves, linked to its operation).
//
// It exists for two audiences. First, the AI agents this CLI is built for: an
// agent can make mistakes, so the CLI keeps an automatic, durable source of
// truth of what it actually did — when a human asks "what did you do?", the
// agent can read the journal back instead of guessing. Second, support: when a
// user hits a problem, the journal (or a redacted `debug bundle` of it) is the
// diagnostic record to send to Korbit.
//
// It never stores secrets — only the public API-key id and the pre-signing
// request parameters (the ED25519 signature and private key never reach here).
package journal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/korbit-official/korbit-cli/internal/logging"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo — keeps cross-compiles working)
)

// FileName is the journal database file under the CLI home.
const FileName = "korbit-cli.db"

// schemaVersion is the journal schema version, stamped into the SQLite file
// header via PRAGMA user_version (see Open). Bump it whenever the schema below
// changes in a way an existing database can't satisfy by itself.
//
// The journal is a recreatable local log, not a system of record, so Open
// resets it rather than migrating it: a database stamped with a different
// non-zero version is dropped and recreated from the current schema (see Open).
// A fresh database (user_version 0) is created and stamped. This keeps the
// schema honest with no migration code to maintain.
const schemaVersion = 2

// DefaultPath returns the journal database path under home.
func DefaultPath(home string) string { return filepath.Join(home, FileName) }

// Disabled reports whether journaling has been turned off via the environment
// (KORBIT_CLI_NO_JOURNAL=1/true/yes). The escape hatch lets a caller run
// against a read-only home, or opt out of local logging entirely.
func Disabled(getenv func(string) string) bool {
	switch getenv("KORBIT_CLI_NO_JOURNAL") {
	case "1", "true", "yes":
		return true
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS operations (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at_ms   INTEGER NOT NULL,
  finished_at_ms  INTEGER,
  op_id           TEXT NOT NULL,
  surface         TEXT NOT NULL,
  safety          TEXT,
  outcome         TEXT NOT NULL,
  error_code      TEXT,
  attempts        INTEGER NOT NULL DEFAULT 0,
  key_name        TEXT,
  api_key_id      TEXT,
  cli_version     TEXT
) STRICT;
CREATE INDEX IF NOT EXISTS idx_operations_started ON operations(started_at_ms);

CREATE TABLE IF NOT EXISTS api_calls (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_id    INTEGER REFERENCES operations(id),
  seq             INTEGER NOT NULL DEFAULT 0,
  started_at_ms   INTEGER NOT NULL,
  finished_at_ms  INTEGER,
  method          TEXT NOT NULL,
  path            TEXT NOT NULL,
  base_url        TEXT NOT NULL,
  auth            INTEGER NOT NULL,
  key_name        TEXT,
  api_key_id      TEXT,
  params_json     TEXT,
  http_status     INTEGER,
  success         INTEGER NOT NULL,
  error_code      TEXT,
  error_message   TEXT,
  retry_count     INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER,
  cli_version     TEXT
) STRICT;
CREATE INDEX IF NOT EXISTS idx_api_calls_started   ON api_calls(started_at_ms);
CREATE INDEX IF NOT EXISTS idx_api_calls_operation ON api_calls(operation_id);

CREATE TABLE IF NOT EXISTS orders (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_id    INTEGER REFERENCES operations(id),
  created_at_ms   INTEGER NOT NULL,
  finished_at_ms  INTEGER,
  client_order_id TEXT NOT NULL,
  symbol          TEXT,
  side            TEXT,
  order_type      TEXT,
  price           TEXT,
  qty             TEXT,
  amt             TEXT,
  tif             TEXT,
  params_json     TEXT,
  key_name        TEXT,
  api_key_id      TEXT,
  status          TEXT NOT NULL,
  order_id        TEXT,
  error_code      TEXT,
  attempt_count   INTEGER NOT NULL DEFAULT 1,
  retry_count     INTEGER NOT NULL DEFAULT 0
) STRICT;
-- One row per logical order: reusing a clientOrderId to retry the SAME order
-- (the documented idempotent-retry flow) upserts onto this row rather than
-- creating a duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_client_order_id ON orders(client_order_id);
CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at_ms);
`

// Logger is a handle to the journal database.
type Logger struct {
	db     *sql.DB
	path   string
	logger *slog.Logger // operational diagnostics (DB open/pragma/write health); never nil after Open
}

// Open opens (creating if needed) the journal database at path and ensures the
// schema exists. It enables WAL so concurrent korbit-cli invocations can read
// while one writes, and a busy_timeout so a writer waits for the lock instead
// of failing with SQLITE_BUSY. A single pooled connection (SetMaxOpenConns(1))
// serializes all in-process access, so no additional application-level locking
// is needed; WAL + busy_timeout cover the cross-process case.
//
// noFsync sets PRAGMA synchronous=OFF: writes are not flushed to disk, which is
// faster but means a power loss or OS crash may lose or corrupt recent writes.
// The pre-send ordering guarantee (a journaled row is written before the request
// proceeds) is unaffected — only crash-durability is traded for speed.
//
// log receives operational diagnostics (the path, pragmas, schema-version
// reconciliation, and an Info milestone once opened); nil is silent
// (logging.Or).
func Open(path string, noFsync bool, log *slog.Logger) (*Logger, error) {
	log = logging.Or(log)
	log.Debug("opening action journal", "path", path, "noFsync", noFsync)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One long-lived connection: makes the connection-scoped busy_timeout stick
	// and serializes writes within this process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	pragmas := []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	}
	if noFsync {
		pragmas = append(pragmas, "PRAGMA synchronous=OFF")
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	log.Debug("journal pragmas applied", "walMode", true, "busyTimeoutMs", 5000, "synchronous", synchronousMode(noFsync))
	// Reconcile the on-disk schema with the current one. The journal is a
	// recreatable local log, so a database stamped with a DIFFERENT non-zero
	// version is reset rather than migrated: its tables are dropped and recreated
	// from the current schema. A fresh database (user_version 0) just gets the
	// schema. The stamp is written only AFTER the schema is in place, so an
	// interrupted reset re-runs cleanly on the next open.
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if v != 0 && v != schemaVersion {
		log.Debug("journal schema version differs — resetting", "onDisk", v, "want", schemaVersion)
		if _, err := db.Exec(`DROP TABLE IF EXISTS orders; DROP TABLE IF EXISTS api_calls; DROP TABLE IF EXISTS operations;`); err != nil {
			db.Close()
			return nil, fmt.Errorf("reset journal schema: %w", err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Stamp the current schema version, only after the schema is in place, so a
	// half-created or interrupted reset never looks current.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("stamp schema version: %w", err)
	}
	log.Info("action journal opened", "path", path, "schemaVersion", schemaVersion)
	return &Logger{db: db, path: path, logger: log}, nil
}

// synchronousMode names the PRAGMA synchronous mode for a log attribute.
func synchronousMode(noFsync bool) string {
	if noFsync {
		return "OFF"
	}
	return "NORMAL"
}

// log returns the journal's operational logger, never nil (logging.Or).
func (l *Logger) log() *slog.Logger { return logging.Or(l.logger) }

// Path returns the database file path.
func (l *Logger) Path() string { return l.path }

// Close closes the database handle.
func (l *Logger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

// CallRecord is one API-call outcome to record in api_calls. OperationID links
// the call to the operation that issued it (0 ⇒ a NULL link, for an ad-hoc
// call), and Seq is its 1-based position within that operation (0 for ad-hoc).
type CallRecord struct {
	OperationID  int64 // the operations row this call belongs to (0 => NULL)
	Seq          int   // 1-based within the operation; 0 for ad-hoc calls
	StartedAtMs  int64
	FinishedAtMs int64
	Method       string
	Path         string
	BaseURL      string
	Auth         bool
	KeyName      string
	APIKeyID     string
	ParamsJSON   string
	HTTPStatus   *int // nil when no HTTP response was received (network failure / success)
	Success      bool
	ErrorCode    string
	ErrorMessage string
	RetryCount   int
	DurationMs   int64
	CLIVersion   string
}

// LogCall inserts an api_calls row and returns its id.
func (l *Logger) LogCall(r CallRecord) (int64, error) {
	res, err := l.db.Exec(`
INSERT INTO api_calls
  (operation_id, seq, started_at_ms, finished_at_ms, method, path,
   base_url, auth, key_name, api_key_id, params_json, http_status, success,
   error_code, error_message, retry_count, duration_ms, cli_version)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullInt64(r.OperationID), r.Seq, r.StartedAtMs, r.FinishedAtMs,
		r.Method, r.Path, r.BaseURL, boolToInt(r.Auth), nullStr(r.KeyName),
		nullStr(r.APIKeyID), nullStr(r.ParamsJSON), nullIntPtr(r.HTTPStatus),
		boolToInt(r.Success), nullStr(r.ErrorCode), nullStr(r.ErrorMessage),
		r.RetryCount, r.DurationMs, nullStr(r.CLIVersion))
	if err != nil {
		// The hard-guarantee write failed; surface the context (path/method/path)
		// at Warn — the caller still maps the returned error per its FailMode and
		// owns the user-facing message, so this is diagnostic only (no secrets:
		// params_json is pre-signing, never logged here).
		l.log().Warn("journal api_calls write failed", "path", l.path, "method", r.Method, "apiPath", r.Path, "err", err.Error())
		return 0, err
	}
	id, err := res.LastInsertId()
	l.log().Debug("journal api_calls row written", "method", r.Method, "apiPath", r.Path, "auth", r.Auth, "success", r.Success)
	return id, err
}

// OperationStart is the operations row written when an operation begins, before
// any of its calls are sent. outcome is set to 'running'; FinishOperation folds
// in the result.
type OperationStart struct {
	StartedAtMs int64
	OpID        string // dotted operation id, e.g. "order.place"
	Surface     string // cli|monitor|mcp|...
	Safety      string // read-only|idempotent|non-idempotent
	KeyName     string
	APIKeyID    string
	CLIVersion  string
}

// StartOperation inserts an operations row with outcome 'running' and returns
// its id, to be folded with FinishOperation once the operation completes.
func (l *Logger) StartOperation(o OperationStart) (int64, error) {
	res, err := l.db.Exec(`
INSERT INTO operations
  (started_at_ms, op_id, surface, safety, outcome, key_name, api_key_id, cli_version)
VALUES (?,?,?,?, 'running', ?,?,?)`,
		o.StartedAtMs, o.OpID, o.Surface, nullStr(o.Safety),
		nullStr(o.KeyName), nullStr(o.APIKeyID), nullStr(o.CLIVersion))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OperationFinish is the result folded into an operations row once the
// operation completes.
type OperationFinish struct {
	FinishedAtMs int64
	Outcome      string // ok|failed|unknown
	ErrorCode    string
	Attempts     int // total api_calls issued under this operation
}

// FinishOperation updates the operations row identified by id with the result.
func (l *Logger) FinishOperation(id int64, f OperationFinish) error {
	_, err := l.db.Exec(`
UPDATE operations SET
  finished_at_ms = ?,
  outcome        = ?,
  error_code     = ?,
  attempts       = ?
WHERE id = ?`,
		f.FinishedAtMs, f.Outcome, nullStr(f.ErrorCode), f.Attempts, id)
	return err
}

// OrderStart is the mint-time order intent, recorded before the placement call
// is sent (so a crash mid-call still leaves a record of what was attempted).
// OperationID links the order to the operation that placed it.
type OrderStart struct {
	CreatedAtMs   int64
	OperationID   int64 // the operations row this order belongs to (0 => NULL)
	ClientOrderID string
	Symbol        string
	Side          string
	OrderType     string
	Price         string
	Qty           string
	Amt           string
	Tif           string
	ParamsJSON    string
	KeyName       string
	APIKeyID      string
}

// StartOrder records (or, when the clientOrderId is being reused to retry the
// same order, refreshes) the order's mint-time row and returns its id. Reusing
// a clientOrderId bumps attempt_count and re-arms the row rather than inserting
// a duplicate — honoring both the UNIQUE(client_order_id) index and the
// documented idempotent-retry flow.
func (l *Logger) StartOrder(o OrderStart) (int64, error) {
	_, err := l.db.Exec(`
INSERT INTO orders
  (created_at_ms, operation_id, client_order_id, symbol, side, order_type,
   price, qty, amt, tif, params_json, key_name, api_key_id, status,
   attempt_count, retry_count)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, 'attempting', 1, 0)
ON CONFLICT(client_order_id) DO UPDATE SET
  attempt_count  = attempt_count + 1,
  status         = CASE WHEN status = 'accepted' THEN 'accepted' ELSE 'attempting' END,
  finished_at_ms = NULL`,
		o.CreatedAtMs, nullInt64(o.OperationID), o.ClientOrderID, nullStr(o.Symbol), nullStr(o.Side),
		nullStr(o.OrderType), nullStr(o.Price), nullStr(o.Qty), nullStr(o.Amt), nullStr(o.Tif),
		nullStr(o.ParamsJSON), nullStr(o.KeyName), nullStr(o.APIKeyID))
	if err != nil {
		// Pre-send hard-guarantee write: fail context at Warn (clientOrderId is a
		// minted public id, safe to log; no money/secret fields).
		l.log().Warn("journal StartOrder write failed", "path", l.path, "clientOrderId", o.ClientOrderID, "err", err.Error())
		return 0, err
	}
	var id int64
	if err := l.db.QueryRow(`SELECT id FROM orders WHERE client_order_id = ?`, o.ClientOrderID).Scan(&id); err != nil {
		return 0, err
	}
	// The upsert collapses a clientOrderId reuse (the documented idempotent-retry
	// flow) onto the existing row; the row id reveals whether this was new or a
	// re-arm of an existing one only at the caller, so log the intent here.
	l.log().Debug("journal order intent recorded", "clientOrderId", o.ClientOrderID, "symbol", o.Symbol, "rowId", id)
	return id, nil
}

// OrderFinish is the outcome to fold into the order's row once the placement
// call has returned.
type OrderFinish struct {
	FinishedAtMs int64
	Status       string
	OrderID      string
	ErrorCode    string
	RetryCount   int
}

// FinishOrder updates the order row identified by id with the placement result.
// 'accepted' is positive proof the order landed, so it is sticky: a later result
// of 'failed' or 'unknown' never downgrades it, and the previously-recorded
// order_id is kept when the new result carries none. So reusing a clientOrderId
// to retry an order that already went through — which the server answers with
// DUPLICATE_CLIENT_ORDER_ID, or which a later ambiguous attempt can only call
// UNKNOWN — doesn't erase the truth that the order is live. The CASE clauses read
// the row's pre-update status, so the guard compares against the existing value.
// Every per-attempt outcome is still preserved in full in api_calls.
func (l *Logger) FinishOrder(id int64, f OrderFinish) error {
	_, err := l.db.Exec(`
UPDATE orders SET
  finished_at_ms = ?,
  status         = CASE WHEN status = 'accepted' AND ? IN ('failed', 'unknown') THEN status ELSE ? END,
  order_id       = COALESCE(NULLIF(?, ''), order_id),
  error_code     = CASE WHEN status = 'accepted' AND ? IN ('failed', 'unknown') THEN error_code ELSE ? END,
  retry_count    = ?
WHERE id = ?`,
		f.FinishedAtMs,
		f.Status, f.Status,
		f.OrderID,
		f.Status, nullStr(f.ErrorCode),
		f.RetryCount, id)
	if err != nil {
		l.log().Warn("journal FinishOrder upsert failed", "path", l.path, "rowId", id, "status", f.Status, "err", err.Error())
		return err
	}
	l.log().Debug("journal order finished", "rowId", id, "status", f.Status, "orderId", f.OrderID)
	return nil
}

// CallRow is a recorded api_calls row, shaped for JSON output and the support
// bundle. Absent/empty fields are omitted.
type CallRow struct {
	ID           int64           `json:"id"`
	OperationID  *int64          `json:"operationId,omitempty"`
	Seq          int             `json:"seq"`
	StartedAtMs  int64           `json:"startedAtMs"`
	FinishedAtMs *int64          `json:"finishedAtMs,omitempty"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	BaseURL      string          `json:"baseUrl"`
	Auth         bool            `json:"auth"`
	KeyName      string          `json:"keyName,omitempty"`
	APIKeyID     string          `json:"apiKeyId,omitempty"`
	Params       json.RawMessage `json:"params,omitempty"`
	HTTPStatus   *int            `json:"httpStatus,omitempty"`
	Success      bool            `json:"success"`
	ErrorCode    string          `json:"errorCode,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	RetryCount   int             `json:"retryCount"`
	DurationMs   *int64          `json:"durationMs,omitempty"`
	CLIVersion   string          `json:"cliVersion,omitempty"`
}

// RecentCalls returns the most recent api_calls rows, newest first.
func (l *Logger) RecentCalls(limit int) ([]CallRow, error) {
	rows, err := l.db.Query(`
SELECT id, operation_id, seq, started_at_ms, finished_at_ms, method,
       path, base_url, auth, key_name, api_key_id, params_json, http_status,
       success, error_code, error_message, retry_count, duration_ms, cli_version
FROM api_calls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallRow
	for rows.Next() {
		var (
			r        CallRow
			opID     sql.NullInt64
			finished sql.NullInt64
			keyName  sql.NullString
			apiKeyID sql.NullString
			params   sql.NullString
			status   sql.NullInt64
			auth     int
			success  int
			errCode  sql.NullString
			errMsg   sql.NullString
			duration sql.NullInt64
			version  sql.NullString
		)
		if err := rows.Scan(&r.ID, &opID, &r.Seq, &r.StartedAtMs, &finished, &r.Method,
			&r.Path, &r.BaseURL, &auth, &keyName, &apiKeyID, &params, &status,
			&success, &errCode, &errMsg, &r.RetryCount, &duration, &version); err != nil {
			return nil, err
		}
		r.OperationID = int64PtrOrNil(opID)
		r.Auth = auth != 0
		r.Success = success != 0
		r.FinishedAtMs = int64PtrOrNil(finished)
		r.KeyName = keyName.String
		r.APIKeyID = apiKeyID.String
		r.Params = rawOrNil(params)
		r.HTTPStatus = intPtrOrNil(status)
		r.ErrorCode = errCode.String
		r.ErrorMessage = errMsg.String
		r.DurationMs = int64PtrOrNil(duration)
		r.CLIVersion = version.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// OrderRow is a recorded orders row, shaped for JSON output and the bundle.
type OrderRow struct {
	ID            int64           `json:"id"`
	OperationID   *int64          `json:"operationId,omitempty"`
	CreatedAtMs   int64           `json:"createdAtMs"`
	FinishedAtMs  *int64          `json:"finishedAtMs,omitempty"`
	ClientOrderID string          `json:"clientOrderId"`
	Symbol        string          `json:"symbol,omitempty"`
	Side          string          `json:"side,omitempty"`
	OrderType     string          `json:"type,omitempty"`
	Price         string          `json:"price,omitempty"`
	Qty           string          `json:"qty,omitempty"`
	Amt           string          `json:"amt,omitempty"`
	Tif           string          `json:"tif,omitempty"`
	Params        json.RawMessage `json:"params,omitempty"`
	KeyName       string          `json:"keyName,omitempty"`
	APIKeyID      string          `json:"apiKeyId,omitempty"`
	Status        string          `json:"status"`
	OrderID       string          `json:"orderId,omitempty"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	AttemptCount  int             `json:"attemptCount"`
	RetryCount    int             `json:"retryCount"`
}

// RecentOrders returns the most recent orders rows, newest first.
func (l *Logger) RecentOrders(limit int) ([]OrderRow, error) {
	rows, err := l.db.Query(`
SELECT id, operation_id, created_at_ms, finished_at_ms,
       client_order_id, symbol, side, order_type, price, qty, amt, tif,
       params_json, key_name, api_key_id, status, order_id, error_code,
       attempt_count, retry_count
FROM orders ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderRow
	for rows.Next() {
		var (
			r        OrderRow
			opID     sql.NullInt64
			finished sql.NullInt64
			symbol   sql.NullString
			side     sql.NullString
			otype    sql.NullString
			price    sql.NullString
			qty      sql.NullString
			amt      sql.NullString
			tif      sql.NullString
			params   sql.NullString
			keyName  sql.NullString
			apiKeyID sql.NullString
			orderID  sql.NullString
			errCode  sql.NullString
		)
		if err := rows.Scan(&r.ID, &opID, &r.CreatedAtMs, &finished,
			&r.ClientOrderID, &symbol, &side, &otype, &price, &qty, &amt, &tif,
			&params, &keyName, &apiKeyID, &r.Status, &orderID, &errCode,
			&r.AttemptCount, &r.RetryCount); err != nil {
			return nil, err
		}
		r.OperationID = int64PtrOrNil(opID)
		r.FinishedAtMs = int64PtrOrNil(finished)
		r.Symbol = symbol.String
		r.Side = side.String
		r.OrderType = otype.String
		r.Price = price.String
		r.Qty = qty.String
		r.Amt = amt.String
		r.Tif = tif.String
		r.Params = rawOrNil(params)
		r.KeyName = keyName.String
		r.APIKeyID = apiKeyID.String
		r.OrderID = orderID.String
		r.ErrorCode = errCode.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// OperationRow is a recorded operations row, shaped for JSON output and the
// support bundle. Absent/empty fields are omitted.
type OperationRow struct {
	ID           int64  `json:"id"`
	StartedAtMs  int64  `json:"startedAtMs"`
	FinishedAtMs *int64 `json:"finishedAtMs,omitempty"`
	OpID         string `json:"opId"`
	Surface      string `json:"surface"`
	Safety       string `json:"safety,omitempty"`
	Outcome      string `json:"outcome"`
	ErrorCode    string `json:"errorCode,omitempty"`
	Attempts     int    `json:"attempts"`
	KeyName      string `json:"keyName,omitempty"`
	APIKeyID     string `json:"apiKeyId,omitempty"`
	CLIVersion   string `json:"cliVersion,omitempty"`
}

// RecentOperations returns the most recent operations rows, newest first.
func (l *Logger) RecentOperations(limit int) ([]OperationRow, error) {
	rows, err := l.db.Query(`
SELECT id, started_at_ms, finished_at_ms, op_id, surface, safety, outcome,
       error_code, attempts, key_name, api_key_id, cli_version
FROM operations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationRow
	for rows.Next() {
		var (
			r        OperationRow
			finished sql.NullInt64
			safety   sql.NullString
			errCode  sql.NullString
			keyName  sql.NullString
			apiKeyID sql.NullString
			version  sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.StartedAtMs, &finished, &r.OpID, &r.Surface,
			&safety, &r.Outcome, &errCode, &r.Attempts, &keyName, &apiKeyID, &version); err != nil {
			return nil, err
		}
		r.FinishedAtMs = int64PtrOrNil(finished)
		r.Safety = safety.String
		r.ErrorCode = errCode.String
		r.KeyName = keyName.String
		r.APIKeyID = apiKeyID.String
		r.CLIVersion = version.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullStr maps "" to a SQL NULL so absent fields aren't stored as empty strings.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func int64PtrOrNil(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func intPtrOrNil(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func rawOrNil(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}
