# USDC provider module

Collects `Stablecoin` / `USDC` payments through an immutable `ZirnisPayments` contract.
Payers sign wallet transactions; the module has no keys or local money-moving state.
It confirms only canonical finalized state on its configured chain and can report
the treasury's finalized USDC balance. It offers no payouts and explicitly declines
automatic refunds so the core restores refund reservations.

Copy the configuration in [stablecoin.env.example](../../deploy/modules/stablecoin.env.example),
export `ZIRNIS_MODULE_SECRET`, then run `go run ./cmd/stablecoin-module`. Onboard the
module and its provider channel through the normal maker-checker flow.

`go test ./...` exercises RPC failures, finality checks and a real Zirnis core. Run
`npm run test:integration` from `contracts/` for the Solidity-to-ledger test.
See [the full guide](../../docs/guides/stablecoins.md).
