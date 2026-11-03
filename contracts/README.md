# Zirnis contracts

`ZirnisPayments` collects USDC into a fixed treasury and binds each receipt to a Zirnis
payment UUID and exact amount. `ZirnisEscrow` locks USDC with buyer release, seller refund,
timeout refund and per-agreement arbitration. Both use immutable token addresses, pinned
OpenZeppelin dependencies and Solidity 0.8.30.

```sh
npm ci --no-audit --no-fund
npm run build
npm test
npm run test:integration
```

`npm run deploy:local` starts an ephemeral local chain, deploys a test token and both
contracts, and prints module configuration. `npm run pay:local -- <providerReference>`
pays an invoice there. These commands never connect to a public chain or use real funds.
The local chain uses unlocked generated test accounts and simulates finality.

See [USDC payments and escrow](../docs/guides/stablecoins.md) for module registration,
checkout, escrow semantics, accounting boundaries and deployment configuration. Escrow
is supplied as a contract; escrow screens and off-chain accounting are not implemented.
Public deployment requires separate configuration and contract security review.
