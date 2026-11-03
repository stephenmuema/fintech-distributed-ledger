# USDC payments and escrow

Zirnis can collect USDC from a payer's EVM wallet and credit a merchant's USDC account.
The payer approves the exact amount and calls `ZirnisPayments.pay`. The `stablecoin`
provider module verifies finalized contract state; Zirnis then posts the collection through
its normal double-entry ledger, provider books and transactional outbox.

This implementation supports one USDC token, chain, treasury and payment contract per
provider instance. It does not issue a token, bridge assets, execute treasury withdrawals,
convert stablecoins into fiat, or credit merchant accounts for escrow deposits.

## Run the automated demonstration

Requirements: the repository's Go version and Node 22.14 or newer.

```sh
cd contracts
npm ci --no-audit --no-fund
npm run build
npm test
npm run test:integration
```

The integration command deploys the actual Solidity contracts and a mintable test token
on an ephemeral local EVM. It starts a real Zirnis sandbox, registers the module through
maker-checker, opens a USDC account, submits a payment, approves and pays on-chain, runs
the payment sweeper, and checks that the merchant is credited once and the books balance.
No external chain, credentials or real funds are used. Ganache may use its JavaScript
fallback if its optional native websocket binary does not support your Node version.

The local chain simulates immediate finality. Its development-only RPC adapter supplies
canonical hash-pinned reads because Ganache lacks EIP-1898. This adapter is not shipped in
the provider image; production RPC must implement `finalized` and EIP-1898 itself.

## Try it with a running sandbox

1. Start a local EVM in a terminal:

   ```sh
   cd contracts
   npm run deploy:local
   ```

   It prints `STABLECOIN_*` configuration, deploys both contracts, and funds an unlocked
   local payer with test USDC. Its configuration is stored in ignored
   `contracts/.local-chain.json`. The chain and accounts disappear when stopped.

2. Copy the printed settings into your module environment. Generate a module secret with
   `openssl rand -hex 32`; export it as `ZIRNIS_MODULE_SECRET` in the module terminal and
   `ZIRNIS_MODULE_STABLECOIN_SECRET` in the core terminal. Start the module:

   ```sh
   cd modules/stablecoin
   go run ./cmd/stablecoin-module
   ```

   Start the core from the repository root using the existing sandbox instructions, or:

   ```sh
   ZIRNIS_ENV=Development go run ./cmd/zirnis
   ```

   Keep the module on the internal network. The local fixture binds RPC to loopback;
   these instructions run the core and module on the host. Container deployments use
   their internal service URLs and a separately reachable RPC endpoint.

3. Register the module as one operator, then authorise the pending change as another:

   ```http
   POST /ops/parameters/hooks/stablecoin
   {"url":"http://127.0.0.1:8090/","hooks":["provider"],"secretEnv":"ZIRNIS_MODULE_STABLECOIN_SECRET","timeoutMs":10000}
   ```

   Onboard and authorise the provider channel the same way:

   ```http
   POST /ops/channels/USDC
   {"provider":"usdc","module":"stablecoin","priority":10}
   ```

   Configure the separate stablecoin collection price, then authorise the change:

   ```http
   POST /ops/parameters/charges/USDCOLL
   {"description":"USDC collection","basisPoints":0}
   ```

   Zero is an explicit choice for this demo. Pick your price before going live.
   Collections are refused before issuing wallet instructions until this charge exists;
   they never inherit mobile-money pricing.

   See [adding a provider](adding-a-provider.md) for the normal approval flow. When the
   core runs in Compose, use `http://stablecoin-module:8090/` for a container module.

4. Open an account for an existing verified customer:

   ```http
   POST /accounts
   {"customerId":"<customer UUID>","currency":"USDC"}
   ```

5. Create a collection using the returned account number. All amounts are atomic USDC
   units: `1234567` means **1.234567 USDC**.

   ```http
   POST /transactions
   Idempotency-Key: <unique key>

   {"version":"COLLECTION.API","creditAccount":"<USDC account number>","currency":"USDC","amount":1234567,"method":"Stablecoin","source":"wallet"}
   ```

   The response has `status: Processing`, a `providerReference`, and an `evm_payment`
   next action. `nextAction.fields` identifies the chain, token, contract, treasury,
   exact amount, reference, approval calldata and payment calldata. API clients must
   preserve amount strings in the instructions rather than converting them to floats.

6. In another terminal, pay that reference with the unlocked local test payer:

   ```sh
   cd contracts
   npm run pay:local -- <providerReference>
   ```

   Zirnis's stale-payment worker confirms it after the configured stale interval
   (`ZIRNIS_STALE_AFTER`, two minutes by default). Inspect `/transactions/<reference>`
   and its provider details to see the finalized block hash and payer. The payment is
   credited once, net of the configured `USDCOLL` fee. Change that price under normal
   parameter controls; the module does not invent a stablecoin fee.

## Hosted payment links

Create a normal payment link for the USDC account. `/pay/<code>` advertises
`methods.stablecoin` when an authorised route is available. The checkout connects an
injected EVM wallet, requests the right network, approves only the exact amount when
necessary, and submits the invoice payment. The payer also needs the chain's native gas
asset. The local CLI above replaces wallet signing for the unlocked development chain.

The payer sees success only after Zirnis verifies finality and credits the ledger.
The browser polls `GET /pay/<code>/payments/<paymentId>`; the UUID is returned at
checkout creation. This endpoint requires the matching random link code and payment ID
and exposes only payer status fields, with public rate limiting. It cannot trigger
provider calls, read merchant records or post money. Merchant apps still receive the
existing signed payment webhooks.

Wallet invoices remain pending until paid. Each pending status check moves its invoice
behind older unchecked invoices so unpaid checkouts cannot monopolise the sweep batch.

The backend uses signed `int64` atomic amounts and rejects major-unit input that exceeds
its range or has excess precision. Browser input is limited to safe JSON integers;
calldata uses exact integer strings and `BigInt`. USD and USDC are distinct assets.

## Escrow contract

`ZirnisEscrow` is a separate contract using the same allowlisted six-decimal token:

| Call | Who can call | Effect |
|---|---|---|
| `fund(paymentRef, seller, arbiter, amount, refundAfter)` | Buyer, after token approval | Locks an exact amount; creates a buyer-scoped agreement ID |
| `release(id)` | Buyer | Pays the seller while undisputed |
| `refund(id)` | Seller | Returns the funds to the buyer while undisputed |
| `refundExpired(id)` | Buyer | Refunds an undisputed agreement after its deadline |
| `dispute(id)` | Buyer or seller, before deadline | Blocks unilateral settlement |
| `resolve(id, sellerAmount)` | The agreement's arbiter | Pays the chosen seller amount and refunds the remainder |

The ID is `keccak256(abi.encode(buyer, paymentRef))`. Buyer, seller and arbiter must be
distinct. Deadlines must be in the future and at most 365 days away. A disputed agreement
does **not** automatically refund at expiry: the selected arbiter must resolve it.
Choose an arbiter whose availability both parties accept. There is no global admin,
upgrade mechanism, arbitrary withdrawal or sweep function.

Both contracts use OpenZeppelin `SafeERC20` and reentrancy protection. Funding checks the
actual received amount, rejecting fee-on-transfer shortfalls. Use the contract functions;
plain token transfers to contract addresses do not create payment or escrow records.

Escrow funding is not merchant income or spendable treasury float. This version supplies
the executable escrow contract, ABI and tests, but does not add escrow screens or an
off-chain escrow accounting workflow. Before integrating escrow events into Zirnis,
define segregated escrow asset/liability books and verify finalized settlement events.
Delivery approval must come from the agreed buyer or an authenticated external workflow;
the contract cannot independently prove delivery or execute an M-Pesa payment.

## Testnet and production configuration

Build artifacts with `npm run build`. Deploy `ZirnisPayments(token, treasury)` and, if
needed, `ZirnisEscrow(token)` using your chosen deployment/signing service. This repository
does not store deployment keys or automatically deploy contracts to public networks.
Verify the bytecode and constructor addresses before pointing a provider at them.

Copy [stablecoin.env.example](../../deploy/modules/stablecoin.env.example) to
`deploy/modules/stablecoin.env`. Set the exact decimal chain ID, native USDC contract,
deployed payment contract and treasury address. Put the shared module secret in the root
`.env`, then opt in with `docker compose --profile stablecoin up --build`.
The module refuses a different chain, token, treasury, decimals, missing finality or
noncanonical state. It does not fall back to `latest` or an arbitrary confirmation count.
RPC URLs may include credentials; their path/query and provider error bodies are excluded
from the module's provider-call evidence.

Use a separate provider name and contract for each chain/environment. Do not repoint an
existing provider to another chain or contract: pending references and settlement books
belong to its original deployment. Persist module configuration with deployment records.

In production, treasury must authorise a USDC valuation rate for local-currency reporting,
risk limits and any FX workflows. The sandbox alone simulates a 1:1 USD peg; production
does not seed a USDC rate or guarantee parity. A ledger FX conversion alone does not
convert external USDC liquidity into fiat.

The module's balance enquiry reads finalized USDC at the configured treasury, supporting
the existing provider-float reconciliation. Keep the treasury dedicated to this provider;
reconcile pending receipts, fees and any manual treasury movements separately. Gas assets
and escrow balances are excluded. Use provider reconciliation or CSV for USDC; bank
statement formats that require ISO-4217 currency codes remain fiat formats. Treasury signing, withdrawal approvals and key recovery
belong in your custody process. No core or provider signing key is needed for collections.

Automated stablecoin payouts and refunds are not implemented. Refund requests receive
`stablecoin_refund_requires_treasury`; Zirnis reverses the refund reservation and leaves
the merchant's balance intact. Do not mark a manual treasury transfer paid without
separately verifying its finalized result and recording its accounting.

Contracts have local tests, including a full chain-to-ledger test, but have not undergone
an independent security audit. Complete testnet validation, contract review and your
existing onboarding controls before enabling real funds. No public-network deployment
is performed by the setup or test commands above.

## References

- [Circle's official USDC contract addresses](https://developers.circle.com/stablecoins/usdc-contract-addresses)
- [Ethereum JSON-RPC](https://ethereum.org/developers/docs/apis/json-rpc/)
- [EIP-1898: block-hash-pinned RPC calls](https://eips.ethereum.org/EIPS/eip-1898)
- [OpenZeppelin ERC20 utilities](https://docs.openzeppelin.com/contracts/5.x/api/token/erc20)
