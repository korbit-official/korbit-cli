// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/dop251/goja"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, shared with the journal
)

// botDB is the script-local SQLite database behind db.* — a USER-owned store,
// strictly separate from the action journal (a bot must never touch the
// journal file). Opened lazily on first use, so a script that never calls
// db.* creates no file. The bot owns the schema (CREATE TABLE IF NOT EXISTS
// in --init); we never migrate or version it.
type botDB struct {
	path    string
	noFsync bool // synchronous=OFF (the --no-fsync/KORBIT_CLI_NO_FSYNC opt-in)
	mu      sync.Mutex
	db      *sql.DB
}

// handle opens the database on first use. Same concurrency posture as the
// journal: WAL + busy_timeout for cross-process safety, one pooled connection
// to serialize in-process access (worker goroutines share it).
func (d *botDB) handle() (*sql.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db != nil {
		return d.db, nil
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	db, err := sql.Open("sqlite", d.path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	pragmas := []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
	}
	if d.noFsync {
		pragmas = append(pragmas, "PRAGMA synchronous=OFF")
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	d.db = db
	return db, nil
}

func (d *botDB) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db != nil {
		_ = d.db.Close()
		d.db = nil
	}
}

// queryResult is a fetched result set, shaped for JS conversion on the loop:
// column order preserved, values already normalized to JS-safe Go types.
type queryResult struct {
	cols []string
	rows [][]any
}

// installDB builds the db global: async exec/query/get over the worker pool.
func (r *Runtime) installDB(vm *goja.Runtime) error {
	dbObj := vm.NewObject()

	method := func(name string, run func(sqlText string, args []any) (any, error), convert func(vm *goja.Runtime, res any) (goja.Value, error)) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			if r.inWhere {
				panic(vm.NewTypeError("db.%s is not available inside --where — the filter must stay synchronous; query in --init or --on", name))
			}
			if r.db == nil {
				panic(vm.NewTypeError("db.%s is not available here (no database path configured)", name))
			}
			sqlArg := call.Argument(0)
			if goja.IsUndefined(sqlArg) || goja.IsNull(sqlArg) {
				panic(vm.NewTypeError("db.%s: the first argument must be a SQL string", name))
			}
			sqlText, ok := sqlArg.Export().(string)
			if !ok {
				panic(vm.NewTypeError("db.%s: the first argument must be a SQL string", name))
			}
			args := make([]any, 0, len(call.Arguments)-1)
			for i := 1; i < len(call.Arguments); i++ {
				a, err := sqlBindArg(call.Arguments[i])
				if err != nil {
					panic(vm.NewTypeError("db.%s: parameter %d: %s", name, i, err.Error()))
				}
				args = append(args, a)
			}
			return r.dispatch(vm, func() (any, error) {
				return run(sqlText, args)
			}, convert)
		}
	}

	// db.exec(sql, ...params) -> {rowsAffected, lastInsertId}
	exec := method("exec", func(sqlText string, args []any) (any, error) {
		db, err := r.db.handle()
		if err != nil {
			return nil, err
		}
		res, err := db.Exec(sqlText, args...)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		lastID, _ := res.LastInsertId()
		return [2]int64{affected, lastID}, nil
	}, func(vm *goja.Runtime, res any) (goja.Value, error) {
		nums := res.([2]int64)
		obj := vm.NewObject()
		_ = obj.Set("rowsAffected", nums[0])
		_ = obj.Set("lastInsertId", nums[1])
		return obj, nil
	})

	runQuery := func(sqlText string, args []any) (any, error) {
		db, err := r.db.handle()
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(sqlText, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		out := queryResult{cols: cols}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b) // TEXT affinity round-trips; BLOBs become strings
				}
			}
			out.rows = append(out.rows, vals)
		}
		return out, rows.Err()
	}

	rowsToJS := func(vm *goja.Runtime, res queryResult) []goja.Value {
		out := make([]goja.Value, 0, len(res.rows))
		for _, row := range res.rows {
			obj := vm.NewObject()
			for i, col := range res.cols {
				_ = obj.Set(col, vm.ToValue(row[i]))
			}
			out = append(out, obj)
		}
		return out
	}

	// db.query(sql, ...params) -> array of row objects (column -> value)
	query := method("query", runQuery, func(vm *goja.Runtime, res any) (goja.Value, error) {
		return vm.ToValue(rowsToJS(vm, res.(queryResult))), nil
	})

	// db.get(sql, ...params) -> first row object or null
	get := method("get", runQuery, func(vm *goja.Runtime, res any) (goja.Value, error) {
		rows := rowsToJS(vm, res.(queryResult))
		if len(rows) == 0 {
			return goja.Null(), nil
		}
		return rows[0], nil
	})

	for name, fn := range map[string]func(goja.FunctionCall) goja.Value{
		"exec": exec, "query": query, "get": get,
	} {
		if err := dbObj.Set(name, fn); err != nil {
			return err
		}
	}
	return vm.Set("db", dbObj)
}

// sqlBindArg converts one JS parameter to a SQLite bind value. Positional `?`
// parameters only — the natural SQL-injection-safe shape. Objects/arrays are
// rejected (bind JSON.stringify(...) yourself); money should be stored as
// TEXT and stay a string end to end, same rule as the API.
func sqlBindArg(v goja.Value) (any, error) {
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	switch x := v.Export().(type) {
	case string:
		return x, nil
	case int64:
		return x, nil
	case float64:
		return x, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	default:
		return nil, fmt.Errorf("only strings, numbers, booleans, and null bind — pass objects through JSON.stringify")
	}
}
