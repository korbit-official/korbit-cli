// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"github.com/shopspring/decimal"

	"github.com/korbit-official/korbit-cli/internal/i18n"
)

// The request pane's form state helpers and validation. The form is not a
// modal: its fields live directly on fundingModel (netIdx, wdAddrIdx, amount,
// formErr, formCursor) and render inside the persistent request pane. The
// selectors are operated with ←/→, the amount is the one text input, and the
// button submits to the confirm step. Validation here is for usability; the
// Funding seam re-validates with the same rules as the endpoint commands.

// wdAddrChoices is the registered withdrawal addresses offered for the
// selected currency on the selected network — the address selector's value
// set. When no network is known (catalog missing/loading) every address usable
// for the currency is offered, and the picked address's own network rides the
// order instead.
func (f fundingModel) wdAddrChoices() []FundingWithdrawAddress {
	all := f.withdrawAddrsFor(f.selected)
	net, ok := f.selectedNetwork()
	if !ok {
		return all
	}
	var out []FundingWithdrawAddress
	for _, a := range all {
		if a.Network == net.Name {
			out = append(out, a)
		}
	}
	return out
}

// chosenWdAddr is the explicitly picked destination; ok=false until the user
// has picked one. A money destination is never auto-selected.
func (f fundingModel) chosenWdAddr() (FundingWithdrawAddress, bool) {
	addrs := f.wdAddrChoices()
	if f.wdAddrIdx < 0 || f.wdAddrIdx >= len(addrs) {
		return FundingWithdrawAddress{}, false
	}
	return addrs[f.wdAddrIdx], true
}

// networkConstraints finds the catalog constraints for a network of the
// selected currency, by name.
func (f fundingModel) networkConstraints(name string) (FundingNetwork, bool) {
	for _, n := range f.selectedNetworks() {
		if n.Name == name {
			return n, true
		}
	}
	return FundingNetwork{}, false
}

// moveWdAddr steps the destination selector. It starts UNCHOSEN (-1): the
// first → enters at the first address, and thereafter the pick clamps to the
// list — ← from unchosen is a no-op and there is no way back to "none",
// because a deliberate first pick is the point.
func (f *fundingModel) moveWdAddr(forward bool) {
	n := len(f.wdAddrChoices())
	if n == 0 {
		return
	}
	switch {
	case f.wdAddrIdx < 0 && forward:
		f.wdAddrIdx = 0
	case f.wdAddrIdx < 0:
		return
	case forward:
		f.wdAddrIdx = clamp(f.wdAddrIdx+1, 0, n-1)
	default:
		f.wdAddrIdx = clamp(f.wdAddrIdx-1, 0, n-1)
	}
	f.formErr = ""
}

// withdrawableAmount is the loaded withdrawable bound for the selected
// currency ("" while unknown — then unchecked; the server enforces it anyway).
func (f fundingModel) withdrawableAmount() string {
	if a, ok := f.wdAmts[f.selected]; ok && a.loaded {
		return a.v.Amount
	}
	return ""
}

// validateWithdraw checks the crypto-withdrawal inputs: a destination is
// explicitly chosen, and the amount is a positive decimal within the network's
// precision and minimum and the withdrawable amount. Bounds whose metadata is
// missing are skipped. Returns "" when the request may proceed to confirm.
func (f fundingModel) validateWithdraw() string {
	if f.wdErr != "" {
		// The load finished with an error (e.g. a non-main account, where funding
		// is server-side refused). Surface that, not a false "still loading".
		return i18n.T("registered withdrawal addresses unavailable: %s — press r to retry", f.wdErr)
	}
	if !f.wdAddrsReady {
		return i18n.T("registered withdrawal addresses are still loading — try again in a moment")
	}
	if len(f.wdAddrChoices()) == 0 {
		return i18n.T("no addresses are registered for API withdrawals of %s here — register one in the Korbit developers portal, then press r to refresh", strings.ToUpper(f.selected))
	}
	a, ok := f.chosenWdAddr()
	if !ok {
		return i18n.T("pick a destination address first (←/→ on the address row)")
	}
	raw := strings.TrimSpace(f.amount.Value())
	if raw == "" {
		return i18n.T("enter the amount to withdraw")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return i18n.T("amount must be a decimal number, e.g. 0.01")
	}
	if !d.IsPositive() {
		return i18n.T("amount must be greater than zero")
	}
	if n, ok := f.networkConstraints(a.Network); ok {
		// Truncate-equality, not the string's exponent: "5.0" and "0.0100" carry
		// trailing zeros in their textual scale but fit a smaller precision.
		if n.WithdrawalPrecision >= 0 && !d.Truncate(int32(n.WithdrawalPrecision)).Equal(d) {
			return i18n.T("amount has too many decimal places for %s (max %d)", n.Name, n.WithdrawalPrecision)
		}
		if min, err := decimal.NewFromString(n.WithdrawalMin); err == nil && d.LessThan(min) {
			return i18n.T("amount is below the %s minimum of %s", n.Name, n.WithdrawalMin)
		}
	}
	if max, err := decimal.NewFromString(f.withdrawableAmount()); err == nil && d.GreaterThan(max) {
		return i18n.T("amount exceeds the withdrawable %s (%s)", strings.ToUpper(f.selected), f.withdrawableAmount())
	}
	return ""
}

// buildWithdrawOrder assembles the order from the picked address; the amount
// string is passed through untouched. Only called after validateWithdraw, so a
// destination is known to be chosen.
func (f fundingModel) buildWithdrawOrder() FundingWithdrawOrder {
	a, _ := f.chosenWdAddr()
	return FundingWithdrawOrder{
		Currency:         f.selected,
		Network:          a.Network,
		Amount:           strings.TrimSpace(f.amount.Value()),
		Address:          a.Address,
		SecondaryAddress: a.SecondaryAddress,
	}
}

// validateKRW checks the KRW push amount: a positive whole number, within the
// available balance for a withdrawal (skipped when unknown).
func (f fundingModel) validateKRW() string {
	raw := strings.TrimSpace(f.amount.Value())
	if raw == "" {
		return i18n.T("enter the KRW amount")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return i18n.T("amount must be a number, e.g. 50000")
	}
	if !d.IsPositive() {
		return i18n.T("amount must be greater than zero")
	}
	if d.Exponent() < 0 && !d.Equal(d.Truncate(0)) {
		return i18n.T("KRW amounts are whole numbers")
	}
	if f.tab == fundingWithdraw {
		if b, ok := f.balanceFor(fundingKRW); ok {
			if avail, err := decimal.NewFromString(b.Available); err == nil && d.GreaterThan(avail) {
				return i18n.T("amount exceeds the available KRW balance (%s)", b.Available)
			}
		}
	}
	return ""
}

// resetForm clears the transient request inputs (the destination pick, the
// amount, the inline error) and re-labels the amount input for the selection.
// Called when the currency or direction changes and after an accepted request.
func (f *fundingModel) resetForm() {
	f.wdAddrIdx = -1
	f.amount.SetValue("")
	f.formErr = ""
	if f.selected == fundingKRW {
		f.amount.Placeholder = "e.g. 50000"
	} else {
		f.amount.Placeholder = "e.g. 0.01"
	}
}
