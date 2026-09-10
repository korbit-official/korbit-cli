// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tuicmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/tui"
)

// tuiFunding is the tui.Funding implementation: every call — the catalog and
// address/history reads as much as the money-moving requests — runs through
// the same ops operations as the endpoint commands, so param validation, the
// per-op retry/idempotency policy (money movers are single-shot), and
// journaling are identical to `deposit …`/`withdraw …`/`krw …` on the command
// line. Reads may run concurrently (the UI fans out fetches); the struct is
// immutable after construction and the ops/client layers are safe to share.
type tuiFunding struct {
	api      *ops.API
	keyName  string
	apiKeyID string
}

// run executes one operation with the given wire-named values: flags are
// validated per the op's params, positionals per their declared kinds — the
// same normalization the CLI's flag/positional paths apply.
func (t *tuiFunding) run(values map[string]string, ids ...string) (json.RawMessage, error) {
	op := ops.Find(ids...)
	params, err := validateParams(op, values)
	if err != nil {
		return nil, err
	}
	for _, pos := range op.Meta().Positionals {
		raw, ok := values[pos.API]
		if !ok || raw == "" {
			if pos.Required {
				return nil, output.Usagef("<%s> is required", pos.Name)
			}
			continue
		}
		v, err := cmdmeta.NormalizeValue(cmdmeta.Param{Kind: pos.Kind}, raw, "<"+pos.Name+">")
		if err != nil {
			return nil, err
		}
		params[pos.API] = v
	}
	res, err := op.Run(context.Background(), t.api, ops.RunInput{
		Values:   params,
		Controls: ops.Controls{Surface: apiclient.SurfaceTUI},
		KeyName:  t.keyName,
		APIKeyID: t.apiKeyID,
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// runSeq is run with the account-scoped seam's accountSeq stamped in — the
// funding screen always passes the CURRENTLY ACTIVE account (never defaulting to
// main). Funding is main-account-only server-side, so a non-main seq is rejected
// with ACCOUNT_SEQ_NOT_ALLOWED; the seam does not retarget.
func (t *tuiFunding) runSeq(accountSeq int, values map[string]string, ids ...string) (json.RawMessage, error) {
	if values == nil {
		values = map[string]string{}
	}
	values[accountseq.APIName] = strconv.Itoa(accountSeq)
	return t.run(values, ids...)
}

// fundingNetworkDetail is the per-network object of GET /v2/currencies, with
// the withdrawal-constraint fields the TUI's form validates against. The
// typed rawapi surface keeps these verbatim in Raw; this decode names them.
type fundingNetworkDetail struct {
	Name                string `json:"name"`
	WithdrawalStatus    string `json:"withdrawalStatus"`
	DepositStatus       string `json:"depositStatus"`
	WithdrawalTxFee     string `json:"withdrawalTxFee"`
	WithdrawalMinAmount string `json:"withdrawalMinAmount"`
	WithdrawalPrecision *int   `json:"withdrawalPrecision"`
	HasSecondaryAddr    bool   `json:"hasSecondaryAddr"`
}

func (t *tuiFunding) Currencies() ([]tui.FundingCurrency, error) {
	data, err := t.run(nil, "currencies")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name           string                 `json:"name"`
		FullName       string                 `json:"fullName"`
		DefaultNetwork string                 `json:"defaultNetwork"`
		NetworkList    []fundingNetworkDetail `json:"networkList"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("currencies response: %w", err)
	}
	out := make([]tui.FundingCurrency, 0, len(rows))
	for _, r := range rows {
		c := tui.FundingCurrency{Currency: r.Name, FullName: r.FullName, DefaultNetwork: r.DefaultNetwork}
		for _, n := range r.NetworkList {
			precision := -1
			if n.WithdrawalPrecision != nil {
				precision = *n.WithdrawalPrecision
			}
			c.Networks = append(c.Networks, tui.FundingNetwork{
				Name: n.Name,
				// Permissive on an absent status: only an explicit "stopped"
				// suspends the direction in the UI (the server still enforces).
				DepositLaunched:     n.DepositStatus != "stopped",
				WithdrawalLaunched:  n.WithdrawalStatus != "stopped",
				WithdrawalFee:       n.WithdrawalTxFee,
				WithdrawalMin:       n.WithdrawalMinAmount,
				WithdrawalPrecision: precision,
				HasSecondaryAddress: n.HasSecondaryAddr,
			})
		}
		out = append(out, c)
	}
	return out, nil
}

// fundingAddr is the shared shape of deposit-address and registered-
// withdrawal-address rows.
type fundingAddr struct {
	Currency         string `json:"currency"`
	Network          string `json:"network"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress"`
}

func (t *tuiFunding) DepositAddresses(accountSeq int) ([]tui.FundingDepositAddress, error) {
	data, err := t.runSeq(accountSeq, nil, "deposit", "addresses")
	if err != nil {
		return nil, err
	}
	var rows []fundingAddr
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("deposit addresses response: %w", err)
	}
	out := make([]tui.FundingDepositAddress, len(rows))
	for i, r := range rows {
		out[i] = tui.FundingDepositAddress{Currency: r.Currency, Network: r.Network, Address: r.Address, SecondaryAddress: r.SecondaryAddress}
	}
	return out, nil
}

func (t *tuiFunding) GenerateDepositAddress(accountSeq int, currency, network string) (tui.FundingDepositAddress, error) {
	data, err := t.runSeq(accountSeq, map[string]string{"currency": currency, "network": network}, "deposit", "generate")
	if err != nil {
		return tui.FundingDepositAddress{}, err
	}
	var r fundingAddr
	if err := json.Unmarshal(data, &r); err != nil {
		return tui.FundingDepositAddress{}, fmt.Errorf("deposit generate response: %w", err)
	}
	return tui.FundingDepositAddress{Currency: r.Currency, Network: r.Network, Address: r.Address, SecondaryAddress: r.SecondaryAddress}, nil
}

func (t *tuiFunding) WithdrawAddresses(accountSeq int) ([]tui.FundingWithdrawAddress, error) {
	data, err := t.runSeq(accountSeq, nil, "withdraw", "addresses")
	if err != nil {
		return nil, err
	}
	var rows []fundingAddr
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("withdraw addresses response: %w", err)
	}
	out := make([]tui.FundingWithdrawAddress, len(rows))
	for i, r := range rows {
		out[i] = tui.FundingWithdrawAddress{Currency: r.Currency, Network: r.Network, Address: r.Address, SecondaryAddress: r.SecondaryAddress}
	}
	return out, nil
}

func (t *tuiFunding) Withdrawable(accountSeq int, currency string) (tui.FundingWithdrawable, error) {
	data, err := t.runSeq(accountSeq, map[string]string{"currency": currency}, "withdraw", "amount")
	if err != nil {
		return tui.FundingWithdrawable{}, err
	}
	var rows []struct {
		Currency              string `json:"currency"`
		WithdrawableAmount    string `json:"withdrawableAmount"`
		WithdrawalInUseAmount string `json:"withdrawalInUseAmount"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return tui.FundingWithdrawable{}, fmt.Errorf("withdrawable amount response: %w", err)
	}
	for _, r := range rows {
		if r.Currency == currency {
			return tui.FundingWithdrawable{Currency: r.Currency, Amount: r.WithdrawableAmount, InUse: r.WithdrawalInUseAmount}, nil
		}
	}
	if len(rows) == 1 {
		r := rows[0]
		return tui.FundingWithdrawable{Currency: r.Currency, Amount: r.WithdrawableAmount, InUse: r.WithdrawalInUseAmount}, nil
	}
	return tui.FundingWithdrawable{Currency: currency}, nil
}

// fundingTransferRow is the union of the crypto and KRW history row shapes;
// absent fields decode to their zero values.
type fundingTransferRow struct {
	ID               int64  `json:"id"`
	Currency         string `json:"currency"`
	Quantity         string `json:"quantity"`
	Fee              string `json:"fee"`
	Status           string `json:"status"`
	Network          string `json:"network"`
	Address          string `json:"address"`
	SecondaryAddress string `json:"secondaryAddress"`
	TransactionHash  string `json:"transactionHash"`
	CreatedAt        int64  `json:"createdAt"`
}

func (t *tuiFunding) history(accountSeq int, currency string, limit int, deposit bool) ([]tui.FundingTransfer, error) {
	values := map[string]string{"limit": strconv.Itoa(limit)}
	var ids []string
	switch {
	case currency == "krw" && deposit:
		ids = []string{"krw", "deposit", "history"}
	case currency == "krw":
		ids = []string{"krw", "withdraw", "history"}
	case deposit:
		values["currency"] = currency
		ids = []string{"deposit", "history"}
	default:
		values["currency"] = currency
		ids = []string{"withdraw", "history"}
	}
	data, err := t.runSeq(accountSeq, values, ids...)
	if err != nil {
		return nil, err
	}
	var rows []fundingTransferRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("transfer history response: %w", err)
	}
	out := make([]tui.FundingTransfer, len(rows))
	for i, r := range rows {
		cur := r.Currency
		if cur == "" {
			cur = currency
		}
		out[i] = tui.FundingTransfer{
			ID: r.ID, Currency: cur, Amount: r.Quantity, Fee: r.Fee, Status: r.Status,
			Network: r.Network, Address: r.Address, TxHash: r.TransactionHash, CreatedAt: r.CreatedAt,
		}
	}
	return out, nil
}

func (t *tuiFunding) DepositHistory(accountSeq int, currency string, limit int) ([]tui.FundingTransfer, error) {
	return t.history(accountSeq, currency, limit, true)
}

func (t *tuiFunding) WithdrawHistory(accountSeq int, currency string, limit int) ([]tui.FundingTransfer, error) {
	return t.history(accountSeq, currency, limit, false)
}

func (t *tuiFunding) RequestWithdrawal(accountSeq int, o tui.FundingWithdrawOrder) (tui.FundingReceipt, error) {
	data, err := t.runSeq(accountSeq, map[string]string{
		"currency": o.Currency, "amount": o.Amount, "address": o.Address,
		"network": o.Network, "secondaryAddress": o.SecondaryAddress,
	}, "withdraw", "request")
	if err != nil {
		return tui.FundingReceipt{}, err
	}
	var r struct {
		Status           string `json:"status"`
		CoinWithdrawalID int64  `json:"coinWithdrawalId"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return tui.FundingReceipt{}, fmt.Errorf("withdraw request response: %w", err)
	}
	return tui.FundingReceipt{ID: r.CoinWithdrawalID, Status: r.Status}, nil
}

func (t *tuiFunding) CancelWithdrawal(accountSeq int, id int64) error {
	_, err := t.runSeq(accountSeq, map[string]string{"coinWithdrawalId": strconv.FormatInt(id, 10)}, "withdraw", "cancel")
	return err
}

func (t *tuiFunding) RequestKRWDeposit(accountSeq int, amount string) error {
	_, err := t.runSeq(accountSeq, map[string]string{"amount": amount}, "krw", "deposit", "request")
	return err
}

func (t *tuiFunding) RequestKRWWithdraw(accountSeq int, amount string) error {
	_, err := t.runSeq(accountSeq, map[string]string{"amount": amount}, "krw", "withdraw", "request")
	return err
}
