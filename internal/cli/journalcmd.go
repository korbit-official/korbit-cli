// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/version"
	"github.com/spf13/cobra"
)

// This file is the cli-side glue for the action journal (internal/journal): it
// implements the `logs` and `debug bundle` builtins that read the journal back.
// The journal rows themselves are written through internal/callrec, behind the
// L2 ops layer.

// openJournalForRead opens the journal for the retrieval commands, mapping the
// disabled case to a clear config error. Open is read-write and creates the db
// file/schema if absent, so `logs` on a fresh install creates an empty journal
// (and, like any command, needs a writable home).
func (rt *runtime) openJournalForRead() (*journal.Logger, error) {
	if journal.Disabled(rt.deps.Getenv) {
		return nil, output.Configf("journaling is disabled (KORBIT_CLI_NO_JOURNAL is set) — unset it to record and view activity")
	}
	path := journal.DefaultPath(config.Home(rt.deps.Getenv))
	jl, err := journal.Open(path, false, rt.logger()) // read path: synchronous mode is irrelevant
	if err != nil {
		return nil, output.Configf("cannot open the action journal at %s: %v", path, err)
	}
	return jl, nil
}

// runLogs implements the `logs` command: print recent API calls (or, with
// --orders, recent orders; with --operations, recent operations) from the local
// journal.
func (rt *runtime) runLogs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q — `logs` takes flags only (--limit, --orders, --operations)", args[0])
	}
	jl, err := rt.openJournalForRead()
	if err != nil {
		return err
	}
	defer jl.Close()

	limit := 20
	if cmd.Flags().Changed("limit") {
		raw, _ := cmd.Flags().GetString("limit")
		if limit, err = parseRange(raw, "--limit", 1, 1000); err != nil {
			return err
		}
	}

	if cmd.Flags().Changed("operations") {
		operations, err := jl.RecentOperations(limit)
		if err != nil {
			return err
		}
		return rt.emitDoc(fmtLogsOperations(operations), logsOperationsDoc{Operations: operations})
	}
	if cmd.Flags().Changed("orders") {
		orders, err := jl.RecentOrders(limit)
		if err != nil {
			return err
		}
		return rt.emitDoc(fmtLogsOrders(orders), logsOrdersDoc{Orders: orders})
	}
	calls, err := jl.RecentCalls(limit)
	if err != nil {
		return err
	}
	return rt.emitDoc(fmtLogsCalls(calls), logsCallsDoc{Calls: calls})
}

type logsCallsDoc struct {
	Calls []journal.CallRow `json:"calls"`
}

type logsOrdersDoc struct {
	Orders []journal.OrderRow `json:"orders"`
}

type logsOperationsDoc struct {
	Operations []journal.OperationRow `json:"operations"`
}

// debugBundle is the redacted diagnostic package for support. It deliberately
// carries no secrets — keys are listed by name/binding only (the keystore is
// never read), and recorded params/api-key-ids are the same non-secret values
// the journal already holds.
type debugBundle struct {
	GeneratedAtMs int64  `json:"generatedAtMs"`
	CLIVersion    string `json:"cliVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Home          string `json:"home"`
	// DefaultKeystore is the backend for NEW keys; each key lists its own.
	DefaultKeystore string                 `json:"defaultKeystore"`
	ConfigBaseURL   string                 `json:"configBaseUrl,omitempty"`
	JournalDisabled bool                   `json:"journalDisabled"`
	Keys            []bundleKey            `json:"keys"`
	Operations      []journal.OperationRow `json:"operations"`
	APICalls        []journal.CallRow      `json:"apiCalls"`
	Orders          []journal.OrderRow     `json:"orders"`
}

// bundleKey is a key's non-secret metadata for the support bundle.
type bundleKey struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Keystore  string `json:"keystore"`
	APIKeyID  string `json:"apiKeyId,omitempty"`
	Bound     bool   `json:"bound"`
	IsDefault bool   `json:"isDefault"`
	BaseURL   string `json:"baseUrl,omitempty"`
}

// runDebugBundle implements the `debug bundle` command: gather a redacted
// diagnostic file (CLI/OS info, config, key metadata, recent journal rows) for
// sending to Korbit support. Writes the file and prints where it went.
func (rt *runtime) runDebugBundle(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q — `debug bundle` takes flags only (--limit, --out)", args[0])
	}
	home, cfg, err := rt.LoadConfig()
	if err != nil {
		return err
	}

	limit := 200
	if cmd.Flags().Changed("limit") {
		raw, _ := cmd.Flags().GetString("limit")
		if limit, err = parseRange(raw, "--limit", 1, 100000); err != nil {
			return err
		}
	}

	bundle := debugBundle{
		GeneratedAtMs:   rt.deps.Now(),
		CLIVersion:      version.Version,
		OS:              goruntime.GOOS,
		Arch:            goruntime.GOARCH,
		Home:            home,
		DefaultKeystore: cfg.Keystore,
		ConfigBaseURL:   cfg.BaseURL,
		JournalDisabled: journal.Disabled(rt.deps.Getenv),
		Keys:            []bundleKey{},
		Operations:      []journal.OperationRow{},
		APICalls:        []journal.CallRow{},
		Orders:          []journal.OrderRow{},
	}

	// Key metadata only — never the keystore (no private material).
	km := rt.KeyManager(home, cfg)
	if summaries, err := km.List(); err == nil {
		for _, s := range summaries {
			bk := bundleKey{Name: s.Name, Type: s.Type, Keystore: s.Keystore, Bound: s.Bound, IsDefault: s.IsDefault, BaseURL: s.BaseURL}
			if s.APIKeyID != nil {
				bk.APIKeyID = *s.APIKeyID
			}
			bundle.Keys = append(bundle.Keys, bk)
		}
	}

	if !bundle.JournalDisabled {
		jl, err := journal.Open(journal.DefaultPath(home), false, rt.logger()) // read path: synchronous mode is irrelevant
		if err != nil {
			return output.Configf("cannot open the action journal at %s: %v", journal.DefaultPath(home), err)
		}
		defer jl.Close()
		if bundle.Operations, err = jl.RecentOperations(limit); err != nil {
			return err
		}
		if bundle.APICalls, err = jl.RecentCalls(limit); err != nil {
			return err
		}
		if bundle.Orders, err = jl.RecentOrders(limit); err != nil {
			return err
		}
	}

	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}

	outPath := defaultBundlePath(home, rt.deps.Now())
	if cmd.Flags().Changed("out") {
		if outPath, _ = cmd.Flags().GetString("out"); outPath == "" {
			return output.Usagef("--out must be a file path")
		}
	}
	if err := os.WriteFile(outPath, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write debug bundle: %w", err)
	}

	summary := bundleSummary{
		Path:       outPath,
		Operations: len(bundle.Operations),
		APICalls:   len(bundle.APICalls),
		Orders:     len(bundle.Orders),
		Keys:       len(bundle.Keys),
	}
	return rt.emitDoc(fmtBundleSummary(summary), summary)
}

type bundleSummary struct {
	Path       string `json:"path"`
	Operations int    `json:"operations"`
	APICalls   int    `json:"apiCalls"`
	Orders     int    `json:"orders"`
	Keys       int    `json:"keys"`
}

func defaultBundlePath(home string, nowMs int64) string {
	return filepath.Join(home, fmt.Sprintf("korbit-cli-debug-%d.json", nowMs))
}

// whenUTC formats a unix-ms timestamp for the human log tables.
func whenUTC(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05")
}

func fmtLogsCalls(calls []journal.CallRow) string {
	if len(calls) == 0 {
		return "(no API calls recorded yet)"
	}
	headers := []string{"id", "when (UTC)", "endpoint", "retries", "result"}
	align := []bool{true, false, false, true, false}
	rows := make([][]string, 0, len(calls))
	for _, c := range calls {
		result := "ok"
		if !c.Success {
			result = "FAILED"
			if c.ErrorCode != "" {
				result = c.ErrorCode
			}
			if c.HTTPStatus != nil {
				result += fmt.Sprintf(" (HTTP %d)", *c.HTTPStatus)
			}
		}
		rows = append(rows, []string{
			strconv.FormatInt(c.ID, 10),
			whenUTC(c.StartedAtMs),
			c.Method + " " + c.Path,
			strconv.Itoa(c.RetryCount),
			result,
		})
	}
	return textout.Table(headers, rows, align)
}

func fmtLogsOperations(ops []journal.OperationRow) string {
	if len(ops) == 0 {
		return "(no operations recorded yet)"
	}
	headers := []string{"id", "when (UTC)", "operation", "surface", "outcome", "attempts"}
	align := []bool{true, false, false, false, false, true}
	rows := make([][]string, 0, len(ops))
	for _, o := range ops {
		outcome := o.Outcome
		if o.ErrorCode != "" {
			outcome = o.Outcome + " (" + o.ErrorCode + ")"
		}
		rows = append(rows, []string{
			strconv.FormatInt(o.ID, 10),
			whenUTC(o.StartedAtMs),
			o.OpID,
			o.Surface,
			outcome,
			strconv.Itoa(o.Attempts),
		})
	}
	return textout.Table(headers, rows, align)
}

func fmtLogsOrders(orders []journal.OrderRow) string {
	if len(orders) == 0 {
		return "(no orders recorded yet)"
	}
	headers := []string{"when (UTC)", "symbol", "side", "type", "status", "orderId", "clientOrderId", "retries"}
	align := []bool{false, false, false, false, false, true, false, true}
	rows := make([][]string, 0, len(orders))
	for _, o := range orders {
		status := o.Status
		if o.ErrorCode != "" {
			status = o.Status + " (" + o.ErrorCode + ")"
		}
		rows = append(rows, []string{
			whenUTC(o.CreatedAtMs),
			o.Symbol,
			o.Side,
			o.OrderType,
			status,
			dashIfEmpty(o.OrderID),
			o.ClientOrderID,
			strconv.Itoa(o.RetryCount),
		})
	}
	return textout.Table(headers, rows, align)
}

func fmtBundleSummary(s bundleSummary) string {
	return textout.KVBlock([][2]string{
		{"path", s.Path},
		{"operations", strconv.Itoa(s.Operations)},
		{"apiCalls", strconv.Itoa(s.APICalls)},
		{"orders", strconv.Itoa(s.Orders)},
		{"keys", strconv.Itoa(s.Keys)},
	}) + "\n\nSend this file to Korbit support (it contains no secrets — no private keys)."
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// emitDoc routes a retrieval command's result: the machine doc in JSON mode, or
// the precomputed human text otherwise. (The shared per-command formatter map
// keys off a json.RawMessage payload; these commands produce typed docs, so
// they render their own human text here.)
func (rt *runtime) emitDoc(humanText string, doc any) error {
	if rt.jsonMode() {
		return rt.io.EmitJSON(doc, rt.compact)
	}
	return rt.io.EmitText(humanText)
}
