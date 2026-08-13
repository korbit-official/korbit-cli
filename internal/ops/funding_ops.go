// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

func init() {
	// ---- crypto deposits ----
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"deposit", "addresses"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/depositAddresses",
			Summary: "list your crypto deposit addresses (all assets)",
			Params:  []cmdmeta.Param{accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "currency", Type: "string", Desc: "asset symbol"},
				{Name: "network", Type: "string", Desc: "blockchain network symbol"},
				{Name: "address", Type: "string", Desc: "deposit address"},
				{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
			},
			Examples: []string{"{prog} deposit addresses"},
			Auth:     signedAuth("readDeposits"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.DepositAddresses(ctx, rawapi.DepositAddressesRequest{AccountSeq: &seq}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"deposit", "address"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/depositAddress",
			Summary:     "show the crypto deposit address for one asset",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params:      []cmdmeta.Param{networkFlag, accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "currency", Type: "string", Desc: "asset symbol"},
				{Name: "network", Type: "string", Desc: "blockchain network symbol"},
				{Name: "address", Type: "string", Desc: "deposit address"},
				{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
			},
			Notes:    []string{"Returns nothing until an address has been generated — run `{prog} deposit generate <currency>` first if this is empty."},
			Examples: []string{"{prog} deposit address btc", "{prog} deposit address usdt --network ETH"},
			Auth:     signedAuth("readDeposits"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.DepositAddress(ctx, rawapi.DepositAddressRequest{
				Currency:   rawapi.Currency(reqStr(in.Values, "currency")),
				Network:    optStr(in.Values, "network"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"deposit", "generate"}, Section: cmdmeta.SectionFunding,
			Method: "POST", Path: "/v2/coin/depositAddress",
			Summary:     "generate (or return the existing) crypto deposit address for an asset",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params:      []cmdmeta.Param{networkFlag, accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "currency", Type: "string", Desc: "asset symbol"},
				{Name: "network", Type: "string", Desc: "blockchain network symbol"},
				{Name: "address", Type: "string", Desc: "deposit address"},
				{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
			},
			Notes: []string{
				"Idempotent: if an address already exists it is returned rather than re-created.",
				"Requires the writeDeposits permission — provision a key with `{prog} setup --with-transfers` if your key lacks it.",
			},
			Examples: []string{"{prog} deposit generate btc", "{prog} deposit generate usdt --network ETH"},
			Auth:     signedAuth("writeDeposits"),
			Safety:   cmdmeta.SafetyIdempotent,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.DepositGenerate(ctx, rawapi.DepositGenerateRequest{
				Currency:   rawapi.Currency(reqStr(in.Values, "currency")),
				Network:    optStr(in.Values, "network"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"deposit", "status"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/deposit",
			Summary:     "status of one crypto deposit by id",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params: []cmdmeta.Param{
				{Flag: "id", API: "coinDepositId", Kind: cmdmeta.KindInt, Min: metaPtr(1), Required: true, Desc: "deposit id (from `deposit history`)"},
				accountSeq,
			},
			Response: coinDepositFields,
			Examples: []string{"{prog} deposit status btc --id 1234"},
			Auth:     signedAuth("readDeposits"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.DepositStatus(ctx, rawapi.DepositStatusRequest{
				Currency:      rawapi.Currency(reqStr(in.Values, "currency")),
				CoinDepositID: reqInt(in.Values, "coinDepositId"),
				AccountSeq:    &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(fundingHistoryOp{
		meta: OpMeta{
			ID: []string{"deposit", "history"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/recentDeposits",
			Summary:     "recent crypto deposits for an asset",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params:      []cmdmeta.Param{fundingLimit, accountSeq},
			Response:    coinDepositFields,
			Examples:    []string{"{prog} deposit history btc --limit 50"},
			Auth:        signedAuth("readDeposits"),
			Safety:      cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, args fundingArgs, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.DepositHistory(ctx, rawapi.DepositHistoryRequest{
				Currency:   rawapi.Currency(args.currency),
				Limit:      &args.limit,
				AccountSeq: &args.accountSeq,
			}, pol)
			return b, err
		},
	})

	// ---- crypto withdrawals ----
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"withdraw", "addresses"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/withdrawableAddresses",
			Summary: "list addresses registered for API withdrawals",
			Params:  []cmdmeta.Param{accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "network", Type: "string", Desc: "blockchain network symbol"},
				{Name: "currency", Type: "string", Desc: "asset symbol (absent if the address is valid for any asset on the network)"},
				{Name: "address", Type: "string", Desc: "withdrawal address"},
				{Name: "secondaryAddress", Type: "string", Desc: "destination tag / memo (absent if none)"},
			},
			Notes:    []string{"`withdraw request` can only send to an address listed here — addresses are registered out-of-band in the Korbit app/portal, not via this CLI."},
			Examples: []string{"{prog} withdraw addresses"},
			Auth:     signedAuth("readWithdrawals"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.WithdrawAddresses(ctx, rawapi.WithdrawAddressesRequest{AccountSeq: &seq}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"withdraw", "amount"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/withdrawableAmount",
			Summary:     "withdrawable amount per asset (default: all)",
			Positionals: []cmdmeta.Positional{{Name: "currency", API: "currency", Kind: cmdmeta.KindCurrency, Desc: `asset symbol, e.g. "btc" (omit for all)`}},
			Params:      []cmdmeta.Param{accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "currency", Type: "string", Desc: "asset symbol"},
				{Name: "withdrawableAmount", Type: "string", Desc: "amount available to withdraw"},
				{Name: "withdrawalInUseAmount", Type: "string", Desc: "amount locked in pending withdrawals"},
			},
			Examples: []string{"{prog} withdraw amount", "{prog} withdraw amount btc"},
			Auth:     signedAuth("readWithdrawals"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.WithdrawAmount(ctx, rawapi.WithdrawAmountRequest{
				Currency:   optStr(in.Values, "currency"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"withdraw", "request"}, Section: cmdmeta.SectionFunding,
			Method: "POST", Path: "/v2/coin/withdrawal",
			Summary:     "request a crypto withdrawal to a registered address",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params: []cmdmeta.Param{
				{Flag: "amount", API: "amount", Kind: cmdmeta.KindDecimal, Required: true, Desc: "quantity to withdraw, excluding fees (decimal string)"},
				{Flag: "address", API: "address", Kind: cmdmeta.KindString, Required: true, Desc: "recipient address — must be pre-registered for API withdrawals (`{prog} withdraw addresses`)"},
				networkFlag,
				{Flag: "secondary-address", API: "secondaryAddress", Kind: cmdmeta.KindString, Desc: "destination tag / memo, when the asset/network needs one"},
				accountSeq,
			},
			Response: []cmdmeta.ResponseField{
				{Name: "status", Type: "string", Desc: "pending | actionRequired | reviewing | processing | done | canceled | failed"},
				{Name: "coinWithdrawalId", Type: "number", Desc: "withdrawal id — pass to `withdraw status`/`withdraw cancel`"},
			},
			Notes: []string{
				"Withdrawals only go to addresses pre-registered for OpenAPI use; an unregistered address returns UNREGISTERED_WITHDRAWAL_ADDRESS.",
				"Requires the writeWithdrawals permission — provision it with `{prog} setup --with-transfers`.",
				"A `pending`/`actionRequired`/`reviewing` status is not final: confirm with `{prog} withdraw status <currency> --id <coinWithdrawalId>`. Korbit may also require email/app confirmation before it proceeds.",
				"Preview the exact request first with --dry-run.",
				"Common errors: WITHDRAWAL_SUSPENDED, FORBIDDEN_WITHDRAWAL_ADDRESS, WITHDRAWAL_ALREADY_IN_PROGRESS, NO_BALANCE, DAILY_LIMIT_EXCEEDED, INVALID_USER_STATUS.",
			},
			Examples:    []string{"{prog} withdraw request btc --amount 0.025 --address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --network BTC"},
			Auth:        signedAuth("writeWithdrawals"),
			Safety:      cmdmeta.SafetyNonIdempotent,
			Destructive: true,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.WithdrawRequest(ctx, rawapi.WithdrawRequestRequest{
				Currency:         rawapi.Currency(reqStr(in.Values, "currency")),
				Amount:           reqStr(in.Values, "amount"),
				Address:          reqStr(in.Values, "address"),
				Network:          optStr(in.Values, "network"),
				SecondaryAddress: optStr(in.Values, "secondaryAddress"),
				AccountSeq:       &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"withdraw", "cancel"}, Section: cmdmeta.SectionFunding,
			Method: "DELETE", Path: "/v2/coin/withdrawal",
			Summary: "cancel a crypto withdrawal by id (only while actionRequired/reviewing)",
			Params: []cmdmeta.Param{
				{Flag: "id", API: "coinWithdrawalId", Kind: cmdmeta.KindInt, Min: metaPtr(1), Required: true, Desc: "withdrawal id (from `withdraw request`/`withdraw history`)"},
				accountSeq,
			},
			Notes: []string{
				"Only withdrawals in actionRequired or reviewing status can be canceled; otherwise CANNOT_CANCEL_WITHDRAWAL or WITHDRAWAL_ALREADY_FINISHED.",
				"Confirm the result with `{prog} withdraw status <currency> --id <coinWithdrawalId>`.",
			},
			Examples:    []string{"{prog} withdraw cancel --id 1234"},
			Auth:        signedAuth("writeWithdrawals"),
			Safety:      cmdmeta.SafetyIdempotent,
			Destructive: true,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.WithdrawCancel(ctx, rawapi.WithdrawCancelRequest{
				CoinWithdrawalID: reqInt(in.Values, "coinWithdrawalId"),
				AccountSeq:       &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"withdraw", "status"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/withdrawal",
			Summary:     "status of one crypto withdrawal by id",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params: []cmdmeta.Param{
				{Flag: "id", API: "coinWithdrawalId", Kind: cmdmeta.KindInt, Min: metaPtr(1), Required: true, Desc: "withdrawal id (from `withdraw request`)"},
				accountSeq,
			},
			Response: coinWithdrawalFields,
			Examples: []string{"{prog} withdraw status btc --id 1234"},
			Auth:     signedAuth("readWithdrawals"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.WithdrawStatus(ctx, rawapi.WithdrawStatusRequest{
				Currency:         rawapi.Currency(reqStr(in.Values, "currency")),
				CoinWithdrawalID: reqInt(in.Values, "coinWithdrawalId"),
				AccountSeq:       &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(fundingHistoryOp{
		meta: OpMeta{
			ID: []string{"withdraw", "history"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/coin/recentWithdrawals",
			Summary:     "recent crypto withdrawals for an asset",
			Positionals: []cmdmeta.Positional{currencyPositional},
			Params:      []cmdmeta.Param{fundingLimit, accountSeq},
			Response:    coinWithdrawalFields,
			Examples:    []string{"{prog} withdraw history btc --limit 50"},
			Auth:        signedAuth("readWithdrawals"),
			Safety:      cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, args fundingArgs, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.WithdrawHistory(ctx, rawapi.WithdrawHistoryRequest{
				Currency:   rawapi.Currency(args.currency),
				Limit:      &args.limit,
				AccountSeq: &args.accountSeq,
			}, pol)
			return b, err
		},
	})

	// ---- KRW deposits & withdrawals ----
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"krw", "deposit", "request"}, Section: cmdmeta.SectionFunding,
			Method: "POST", Path: "/v2/krw/sendKrwDepositPush",
			Summary:     "send a KRW-deposit push to your Korbit app (you confirm there)",
			Positionals: []cmdmeta.Positional{{Name: "amount", API: "amount", Kind: cmdmeta.KindDecimal, Required: true, Desc: "KRW amount to deposit"}},
			Params:      []cmdmeta.Param{accountSeq},
			Notes: []string{
				"This only sends a push notification — you must complete verification in the Korbit mobile app for the deposit to proceed (enable app push notifications first).",
				"Requires the writeDeposits permission (`{prog} setup --with-transfers`).",
				"Track it with `{prog} krw deposit history`.",
			},
			Examples:    []string{"{prog} krw deposit request 50000"},
			Auth:        signedAuth("writeDeposits"),
			Safety:      cmdmeta.SafetyNonIdempotent,
			Destructive: true,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.KRWDeposit(ctx, rawapi.KRWDepositRequest{
				Amount:     reqStr(in.Values, "amount"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(passthroughOp{
		meta: OpMeta{
			ID: []string{"krw", "withdraw", "request"}, Section: cmdmeta.SectionFunding,
			Method: "POST", Path: "/v2/krw/sendKrwWithdrawalPush",
			Summary:     "send a KRW-withdrawal push to your Korbit app (you confirm there)",
			Positionals: []cmdmeta.Positional{{Name: "amount", API: "amount", Kind: cmdmeta.KindDecimal, Required: true, Desc: "KRW amount to withdraw"}},
			Params:      []cmdmeta.Param{accountSeq},
			Notes: []string{
				"This only sends a push notification — you must complete verification in the Korbit mobile app for the withdrawal to proceed (enable app push notifications first).",
				"Requires the writeWithdrawals permission (`{prog} setup --with-transfers`).",
				"Track it with `{prog} krw withdraw history`.",
			},
			Examples:    []string{"{prog} krw withdraw request 50000"},
			Auth:        signedAuth("writeWithdrawals"),
			Safety:      cmdmeta.SafetyNonIdempotent,
			Destructive: true,
		},
		call: func(ctx context.Context, raw *rawapi.Client, in RunInput, pol korbit.Policy) (json.RawMessage, korbit.Meta, error) {
			seq := reqAccountSeq(in.Values)
			_, b, meta, err := raw.KRWWithdraw(ctx, rawapi.KRWWithdrawRequest{
				Amount:     reqStr(in.Values, "amount"),
				AccountSeq: &seq,
			}, pol)
			return b, meta, err
		},
	})
	register(fundingHistoryOp{
		meta: OpMeta{
			ID: []string{"krw", "deposit", "history"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/krw/recentDeposits",
			Summary: "recent KRW deposits",
			Params:  []cmdmeta.Param{fundingLimit, accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "id", Type: "number", Desc: "KRW deposit id"},
				{Name: "status", Type: "string", Desc: "pending | processing | reviewing | done | canceling | canceled | failed"},
				{Name: "quantity", Type: "string", Desc: "deposit amount (KRW)"},
				{Name: "createdAt", Type: "number", Desc: "deposit timestamp (ms)"},
			},
			Examples: []string{"{prog} krw deposit history --limit 50"},
			Auth:     signedAuth("readDeposits"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, args fundingArgs, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.KRWDeposits(ctx, rawapi.KRWDepositsRequest{Limit: &args.limit, AccountSeq: &args.accountSeq}, pol)
			return b, err
		},
	})
	register(fundingHistoryOp{
		meta: OpMeta{
			ID: []string{"krw", "withdraw", "history"}, Section: cmdmeta.SectionFunding,
			Method: "GET", Path: "/v2/krw/recentWithdrawals",
			Summary: "recent KRW withdrawals",
			Params:  []cmdmeta.Param{fundingLimit, accountSeq},
			Response: []cmdmeta.ResponseField{
				{Name: "id", Type: "number", Desc: "KRW withdrawal id"},
				{Name: "quantity", Type: "string", Desc: "withdrawn amount excluding fee (KRW)"},
				{Name: "fee", Type: "string", Desc: "withdrawal fee (KRW)"},
				{Name: "status", Type: "string", Desc: "pending | reviewing | processing | done | failed | canceled"},
				{Name: "createdAt", Type: "number", Desc: "withdrawal request timestamp (ms)"},
			},
			Examples: []string{"{prog} krw withdraw history --limit 50"},
			Auth:     signedAuth("readWithdrawals"),
			Safety:   cmdmeta.SafetyReadOnly,
		},
		call: func(ctx context.Context, raw *rawapi.Client, args fundingArgs, pol korbit.Policy) (json.RawMessage, error) {
			_, b, _, err := raw.KRWWithdrawals(ctx, rawapi.KRWWithdrawalsRequest{Limit: &args.limit, AccountSeq: &args.accountSeq}, pol)
			return b, err
		},
	})
}
