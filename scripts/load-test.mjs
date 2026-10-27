#!/usr/bin/env node
// Load test: drives a realistic mixed workload through the public API, then verifies the books.
//
//   node scripts/load-test.mjs [--base http://localhost:5080] [--tx 12000] [--customers 400]
//                              [--merchants 20] [--concurrency 32]
//
// Start the API (sandbox on, for the M-Pesa callbacks) with a high rate limit first, e.g.
//   ZIRNIS_ENV=Development ZIRNIS_RATE_LIMIT_PER_MINUTE=10000000 go run ./cmd/zirnis
//
// Mix (by weight): card collections, M-Pesa STK collections (+ callback), book transfers,
// payouts (bank + M-Pesa B2C), card authorisations with signed captures.

import crypto from 'node:crypto'

const args = Object.fromEntries(process.argv.slice(2).reduce((acc, a, i, all) => (a.startsWith('--') ? [...acc, [a.slice(2), all[i + 1]]] : acc), []))
const BASE = args.base ?? 'http://localhost:5080'
const TX = Number(args.tx ?? 12_000)
const CUSTOMERS = Number(args.customers ?? 400)
const MERCHANTS = Number(args.merchants ?? 20)
const CONCURRENCY = Number(args.concurrency ?? 32)
const KEY = 'zr_platform_demo_key'
const OPS = 'zr_ops_amina_demo_key'
const NETWORK_SECRET = 'whsec_sandbox_cardnetwork'

const pick = (xs) => xs[Math.floor(Math.random() * xs.length)]
const between = (lo, hi) => lo + Math.floor(Math.random() * (hi - lo + 1))

async function call(method, path, body, { idempotent = false, key = KEY, signed = false } = {}) {
  const headers = { 'Content-Type': 'application/json' }
  const json = body === undefined ? undefined : JSON.stringify(body)
  if (signed) {
    const t = Math.floor(Date.now() / 1000)
    const mac = crypto.createHmac('sha256', NETWORK_SECRET).update(`${t}.${json}`).digest('hex')
    headers['Zirnis-Signature'] = `t=${t},v1=${mac}`
  } else {
    headers.Authorization = `Bearer ${key}`
  }
  if (idempotent) headers['Idempotency-Key'] = crypto.randomUUID()
  const started = performance.now()
  const response = await fetch(BASE + path, { method, headers, body: json })
  const text = await response.text()
  return { status: response.status, ms: performance.now() - started, body: text ? JSON.parse(text) : undefined }
}

async function pool(items, concurrency, worker) {
  let next = 0
  await Promise.all(Array.from({ length: concurrency }, async () => {
    while (next < items.length) await worker(items[next++])
  }))
}

// ---------- setup ----------
async function onboard(type, i) {
  const c = (await call('POST', '/customers', { type, legalName: `${type === 'Business' ? 'Merchant' : 'Customer'} ${i} Ltd`, email: `${type}.${i}.${crypto.randomUUID().slice(0, 8)}@load.test`, phone: '+254712345678', country: 'KE' })).body
  await call('POST', `/customers/${c.id}/kyc`, {
    documentType: 'national_id', documentNumber: String(30_000_000 + i), dateOfBirth: '1990-01-01',
    registrationNumber: type === 'Business' ? `PVT-${i}` : null, directors: type === 'Business' ? ['Director'] : null,
  })
  const account = (await call('POST', '/accounts', { customerId: c.id, currency: 'KES' })).body
  return { customerId: c.id, accountId: account.id, accountNumber: account.accountNumber }
}

console.log(`Load test against ${BASE}: ${TX} transactions, ${CUSTOMERS} customers, ${MERCHANTS} merchants, concurrency ${CONCURRENCY}`)
const setupStarted = performance.now()
const merchants = []
const customers = []
await pool([...Array(MERCHANTS).keys()], 16, async (i) => merchants.push(await onboard('Business', i)))
await pool([...Array(CUSTOMERS).keys()], 16, async (i) => customers.push(await onboard('Individual', i)))
// Fund every customer wallet (KES 20,000) and give a quarter of them a card.
await pool(customers, 16, async (c) => {
  await call('POST', '/payments', { accountId: c.accountId, amount: 2_000_000, currency: 'KES', method: 'Card', source: 'tok_visa' }, { idempotent: true })
})
const cardholders = customers.slice(0, Math.ceil(CUSTOMERS / 4))
await pool(cardholders, 16, async (c) => {
  c.cardToken = (await call('POST', '/cards', { accountId: c.accountId }, { idempotent: true })).body.token
})
console.log(`Setup: ${((performance.now() - setupStarted) / 1000).toFixed(1)}s`)

// ---------- workload ----------
const mix = [
  ['card_collection', 40],
  ['mpesa_collection', 10],
  ['transfer', 20],
  ['payout', 15],
  ['card_purchase', 15],
]
const bag = mix.flatMap(([op, weight]) => Array(weight).fill(op))
const ops = Array.from({ length: TX }, () => pick(bag))
const stats = new Map()
const record = (op, r) => {
  const s = stats.get(op) ?? { latencies: [], codes: {} }
  s.latencies.push(r.ms)
  s.codes[r.status] = (s.codes[r.status] ?? 0) + 1
  stats.set(op, s)
}

const run = {
  async card_collection() {
    const m = pick(merchants)
    return call('POST', '/payments', { accountId: m.accountId, amount: between(500, 5_000) * 100, currency: 'KES', method: 'Card', source: Math.random() < 0.03 ? 'tok_decline' : 'tok_visa' }, { idempotent: true })
  },
  async mpesa_collection() {
    const m = pick(merchants)
    const amount = between(100, 3_000) * 100
    const r = await call('POST', '/payments', { accountId: m.accountId, amount, currency: 'KES', method: 'MobileMoney', source: '+254722334455' }, { idempotent: true })
    if (r.status === 201 && r.body.providerReference) {
      const cb = await call('POST', '/sandbox/providers/mpesa/events', { type: 'charge.succeeded', reference: r.body.providerReference, amount, currency: 'KES' })
      return { ...r, ms: r.ms + cb.ms }
    }
    return r
  },
  async transfer() {
    const from = pick(customers)
    let to = pick(customers)
    while (to === from) to = pick(customers)
    return call('POST', '/transfers', { sourceAccountId: from.accountId, destinationAccountId: to.accountId, amount: between(50, 500) * 100 }, { idempotent: true })
  },
  async payout() {
    const m = pick(merchants)
    const bank = Math.random() < 0.5
    return call('POST', '/payouts', {
      accountId: m.accountId, amount: between(100, 1_000) * 100, rail: bank ? 'Bank' : 'MobileMoney',
      destinationAccount: bank ? '1234567890' : '+254722000111', destinationName: 'Supplier', bankCode: bank ? '11' : null,
    }, { idempotent: true })
  },
  async card_purchase() {
    const c = pick(cardholders)
    const amount = between(100, 2_000) * 100
    const auth = await call('POST', '/card-network/authorizations', { networkReference: `NET${crypto.randomUUID()}`, cardToken: c.cardToken, amount, currency: 'KES', merchantName: 'Java House', merchantCategoryCode: '5812' }, { signed: true })
    if (auth.status === 200 && auth.body.approved) {
      const cap = await call('POST', `/card-network/authorizations/${auth.body.authorizationId}/capture`, { amount }, { signed: true })
      return { ...auth, status: cap.status, ms: auth.ms + cap.ms }
    }
    return auth
  },
}

const started = performance.now()
let done = 0
await pool(ops, CONCURRENCY, async (op) => {
  try {
    record(op, await run[op]())
  } catch (e) {
    record(op, { status: 'network', ms: 0 })
  }
  if (++done % 1000 === 0) process.stdout.write(`  ${done}/${TX}\n`)
})
const seconds = (performance.now() - started) / 1000

// ---------- report ----------
const pct = (xs, p) => xs.sort((a, b) => a - b)[Math.min(xs.length - 1, Math.floor((p / 100) * xs.length))] ?? 0
const all = [...stats.values()].flatMap((s) => s.latencies)
let errors = 0
console.log(`\n${TX} transactions in ${seconds.toFixed(1)}s → ${(TX / seconds).toFixed(1)} tx/s`)
console.log('op'.padEnd(18), 'count'.padStart(6), 'p50 ms'.padStart(8), 'p95 ms'.padStart(8), 'p99 ms'.padStart(8), '  status codes')
for (const [op, s] of stats) {
  errors += Object.entries(s.codes).filter(([code]) => code === 'network' || Number(code) >= 500 || code === '409').reduce((n, [, v]) => n + v, 0)
  console.log(op.padEnd(18), String(s.latencies.length).padStart(6), pct(s.latencies, 50).toFixed(0).padStart(8), pct(s.latencies, 95).toFixed(0).padStart(8), pct(s.latencies, 99).toFixed(0).padStart(8), ' ', JSON.stringify(s.codes))
}
console.log('all'.padEnd(18), String(all.length).padStart(6), pct(all, 50).toFixed(0).padStart(8), pct(all, 95).toFixed(0).padStart(8), pct(all, 99).toFixed(0).padStart(8))

const trial = (await call('GET', '/ops/gl/trial-balance', undefined, { key: OPS })).body
const integrity = (await call('GET', '/ops/gl/integrity', undefined, { key: OPS })).body
const balanced = trial.every((t) => t.balanced)
console.log(`\nTrial balance: ${balanced ? 'BALANCED' : 'BROKEN'} · ledger integrity: ${integrity.healthy ? 'no drift' : `${integrity.drift.length} drifted accounts`}`)
console.log(`Errors (5xx / 409 conflicts / network): ${errors}`)
console.log(`At this rate, 10,000 transactions take ${(10_000 / (TX / seconds)).toFixed(0)}s; one day of capacity ≈ ${Math.round((TX / seconds) * 86_400).toLocaleString()} transactions.`)
process.exit(balanced && integrity.healthy && errors === 0 ? 0 : 1)
