// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/cli/textout"
)

// Human-readable formatters for the funding (deposit/withdrawal) commands.
// They are data-driven: a column list maps response fields to display columns,
// so the many similar deposit/withdrawal payloads don't each need a bespoke
// function. money columns are passed through textout.Num() for thousand-grouping on the
// display copy only — the wire path never sees it (see humanout.go).

// fcol describes one display column: a header, the JSON field it reads, whether
// it right-aligns (numeric), and whether to group thousands via textout.Num().
type fcol struct {
	header string
	key    string
	right  bool
	money  bool
}

func (c fcol) cell(m map[string]json.RawMessage) string {
	v := textout.Jstr(m, c.key)
	if c.money {
		v = textout.Num(v)
	}
	return v
}

// arrayTableFmt renders a JSON array of objects as a table with the given
// columns, or empty when the array is empty. Defers (ok=false) on a non-array or
// non-object element so a surprising shape falls back to pretty JSON.
func arrayTableFmt(empty string, cols []fcol) endpointFormatter {
	headers := make([]string, len(cols))
	align := make([]bool, len(cols))
	for i, c := range cols {
		headers[i] = c.header
		align[i] = c.right
	}
	return func(raw json.RawMessage) (string, bool) {
		// Funding histories can be truncated (the 100-row ceiling), in which case
		// the payload is the {"data":[...],"truncated":true,"note":...} envelope;
		// a complete result (and every non-history funding command) is a bare array.
		data, note, ok := textout.ListView(raw)
		if !ok {
			return "", false
		}
		arr, ok := textout.AsArray(data)
		if !ok {
			return "", false
		}
		if len(arr) == 0 {
			return textout.WithTruncationNote(empty, note), true
		}
		var rows [][]string
		for _, e := range arr {
			m, ok := textout.AsObject(e)
			if !ok {
				return "", false
			}
			row := make([]string, len(cols))
			for i, c := range cols {
				row[i] = c.cell(m)
			}
			rows = append(rows, row)
		}
		return textout.WithTruncationNote(textout.Table(headers, rows, align), note), true
	}
}

// objectKVFmt renders a single JSON object as an aligned label/value block.
func objectKVFmt(cols []fcol) endpointFormatter {
	return func(raw json.RawMessage) (string, bool) {
		m, ok := textout.AsObject(raw)
		if !ok {
			return "", false
		}
		rows := make([][2]string, len(cols))
		for i, c := range cols {
			rows[i] = [2]string{c.header, c.cell(m)}
		}
		return textout.KVBlock(rows), true
	}
}

// ackFmt renders a bare {"success": true} acknowledgement as a fixed message
// (and defers to pretty JSON if the payload carries more than that).
func ackFmt(msg string) endpointFormatter {
	return func(raw json.RawMessage) (string, bool) {
		m, ok := textout.AsObject(raw)
		if !ok {
			return "", false
		}
		if _, ok := m["success"]; ok && len(m) == 1 {
			return msg, true
		}
		return "", false
	}
}

// Column sets shared across the deposit/withdrawal formatters.
var (
	colsDepositAddress = []fcol{
		{header: "currency", key: "currency"},
		{header: "network", key: "network"},
		{header: "address", key: "address"},
		{header: "secondary", key: "secondaryAddress"},
	}
	colsWithdrawAddress = []fcol{
		{header: "network", key: "network"},
		{header: "currency", key: "currency"},
		{header: "address", key: "address"},
		{header: "secondary", key: "secondaryAddress"},
	}
	colsCoinDeposit = []fcol{
		{header: "id", key: "id", right: true},
		{header: "currency", key: "currency"},
		{header: "network", key: "network"},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", right: true, money: true},
		{header: "createdAt", key: "createdAt", right: true},
	}
	colsCoinWithdrawal = []fcol{
		{header: "id", key: "id", right: true},
		{header: "currency", key: "currency"},
		{header: "network", key: "network"},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", right: true, money: true},
		{header: "fee", key: "fee", right: true, money: true},
		{header: "createdAt", key: "createdAt", right: true},
	}
	colsKrwDeposit = []fcol{
		{header: "id", key: "id", right: true},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", right: true, money: true},
		{header: "createdAt", key: "createdAt", right: true},
	}
	colsKrwWithdrawal = []fcol{
		{header: "id", key: "id", right: true},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", right: true, money: true},
		{header: "fee", key: "fee", right: true, money: true},
		{header: "createdAt", key: "createdAt", right: true},
	}
	colsWithdrawAmount = []fcol{
		{header: "currency", key: "currency"},
		{header: "withdrawable", key: "withdrawableAmount", right: true, money: true},
		{header: "inUse", key: "withdrawalInUseAmount", right: true, money: true},
	}
	// Single-object detail views (deposit/withdraw status, withdraw request).
	colsCoinDepositDetail = []fcol{
		{header: "id", key: "id"},
		{header: "currency", key: "currency"},
		{header: "network", key: "network"},
		{header: "address", key: "address"},
		{header: "secondaryAddress", key: "secondaryAddress"},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", money: true},
		{header: "transactionHash", key: "transactionHash"},
		{header: "createdAt", key: "createdAt"},
	}
	colsCoinWithdrawalDetail = []fcol{
		{header: "id", key: "id"},
		{header: "currency", key: "currency"},
		{header: "network", key: "network"},
		{header: "address", key: "address"},
		{header: "secondaryAddress", key: "secondaryAddress"},
		{header: "status", key: "status"},
		{header: "quantity", key: "quantity", money: true},
		{header: "fee", key: "fee", money: true},
		{header: "transactionHash", key: "transactionHash"},
		{header: "createdAt", key: "createdAt"},
	}
	colsWithdrawRequest = []fcol{
		{header: "status", key: "status"},
		{header: "coinWithdrawalId", key: "coinWithdrawalId"},
	}
)
