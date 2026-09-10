# Funding: deposits & withdrawals

The tool covers the full deposit/withdrawal surface, split into **safe reads** (work with the default
trading key) and **fund-moving writes** (extra permission + out-of-band human gates). Moving money is
always **initiate-and-report**: you submit a request, then surface its pending state to the user — you
never complete or bypass the confirmation.

> **Funding is main-account-only.** Deposits and withdrawals only act on the main account. `accountSeq`
> is resolved the same way as everywhere else (explicit `--account-seq` → the key's configured default →
> `1`/main), with no funding-specific exception — but the endpoints accept only `1`, so a non-main value
> is rejected with `ACCOUNT_SEQ_NOT_ALLOWED`. Normally just omit `--account-seq`; only if your key's
> default account is a sub-account do you need to pass `--account-seq 1` explicitly for funding.

## Reads — use freely (no special permission)

```sh
dgx-cli deposit addresses --json                       # your crypto deposit addresses
dgx-cli deposit address btc --network bitcoin --json   # address for one asset/network
dgx-cli deposit history btc --json                     # recent crypto deposits
dgx-cli deposit status btc --id <n> --json             # one deposit by id
dgx-cli withdraw addresses --json                      # addresses registered for API withdrawal
dgx-cli withdraw amount [btc] --json                   # withdrawable + in-use per asset
dgx-cli withdraw history btc --json                    # recent crypto withdrawals
dgx-cli withdraw status btc --id <n> --json            # one withdrawal by id
dgx-cli krw deposit history --json                     # recent KRW deposits
dgx-cli krw withdraw history --json                    # recent KRW withdrawals
```

Use these to reconcile funding state, confirm an address is registered, or check whether a transfer
landed. Watch for `truncated:true` on the history endpoints (they're capped — see the output contract
in SKILL.md).

## Writes — gated, only when the user explicitly asks

Fund-moving writes need a key provisioned with the transfer permissions (request them at setup with
`dgx-cli setup --with-transfers …`; a plain trading key gets a permission error → exit 3/4). **Always
`--dry-run` first** (or, over MCP, confirm the exact parameters with the user — there's no per-call
dry-run there).

```sh
dgx-cli deposit generate btc --network bitcoin --json              # create/return a deposit address (no funds move)
dgx-cli withdraw request btc --amount 0.01 --address <ADDR> --network bitcoin --json
dgx-cli withdraw cancel --id <coinWithdrawalId> --json             # only while actionRequired/reviewing
dgx-cli krw deposit request 100000 --json                          # sends an app push; user confirms in the Digital X app
dgx-cli krw withdraw request 100000 --json                         # sends an app push; user confirms in the Digital X app
```

## The gates you must respect

- **Crypto withdrawals only reach pre-registered addresses.** Confirm the target appears in `withdraw
  addresses` first; an unlisted address is rejected with `UNREGISTERED_WITHDRAWAL_ADDRESS`. Address
  registration happens in the Digital X app/portal, not here.
- **A withdrawal request is *submitted*, not done.** `withdraw request` returns a `coinWithdrawalId`
  and a non-final `status` (often `pending`/`actionRequired`/`reviewing`). Confirm with `withdraw
  status btc --id <id>`, and tell the user it may need email/app confirmation.
- **KRW deposit/withdraw only send an app push.** Nothing settles until the user completes
  verification in the Digital X mobile app (push notifications must be enabled). Report it as
  "push sent, awaiting your confirmation in the app."
- **These writes are single-shot** — they have no idempotency key, so the tool never auto-retries
  them (not even on `EXCEED_TIME_WINDOW`; pass `--time-sync on` if clock drift is a risk). If one is
  ambiguous, **don't blindly resend** — check status/history first and let the user decide.
- **Amounts are decimal strings**, same as orders; withdrawal `--amount` excludes fees.

Never initiate a deposit or withdrawal the user didn't explicitly request.
