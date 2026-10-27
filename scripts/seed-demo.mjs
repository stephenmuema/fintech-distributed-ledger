#!/usr/bin/env node
// Seeds a realistic demo day through the public API (no direct database writes).
// Usage: node scripts/seed-demo.mjs [http://localhost:5080]

const base = process.argv[2] ?? 'http://localhost:5080'
const keys = { platform: 'zr_platform_demo_key', amina: 'zr_ops_amina_demo_key' }

async function call(method, path, body, { key = keys.amina, idempotent = false } = {}) {
  const headers = { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' }
  if (idempotent) headers['Idempotency-Key'] = crypto.randomUUID()
  const response = await fetch(base + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  const text = await response.text()
  const json = text ? JSON.parse(text) : undefined
  if (!response.ok && response.status !== 409) throw new Error(`${method} ${path} → ${response.status}: ${text}`)
  return { status: response.status, json }
}

const post = (path, body, opts) => call('POST', path, body, opts)
const step = (text) => console.log(`  · ${text}`)

async function customer(type, legalName, extra) {
  const email = `${legalName.toLowerCase().replace(/[^a-z]+/g, '.')}.${Date.now() % 100000}@example.co.ke`
  const { json: c } = await post('/customers', { type, legalName, email, phone: '+254712345678', country: 'KE' })
  await post(`/customers/${c.id}/kyc`, {
    documentType: type === 'Business' ? 'certificate_of_incorporation' : 'national_id',
    documentNumber: String(20_000_000 + c.customerNumber),
    dateOfBirth: type === 'Business' ? null : '1991-04-12',
    registrationNumber: type === 'Business' ? `PVT-${c.customerNumber}` : null,
    directors: type === 'Business' ? [extra] : null,
  })
  return c
}

const account = async (customerId, currency) => (await post('/accounts', { customerId, currency })).json

const pay = (acct, amount, method = 'Card', source = 'tok_visa') =>
  post('/payments', { accountId: acct.id, amount, currency: acct.currency, method, source, description: 'Safari booking' }, { idempotent: true }).then((r) => r.json)

console.log(`Seeding demo data into ${base}`)

const safari = await customer('Business', 'Safari Tours Ltd', 'Amani Wanjiku')
const safariKes = await account(safari.id, 'KES')
const safariUsd = await account(safari.id, 'USD')
step(`Merchant ${safari.legalName} — CIF ${safari.customerNumber}, KES ${safariKes.accountNumber}, USD ${safariUsd.accountNumber}`)

const amani = await customer('Individual', 'Amani Wanjiku')
const amaniKes = await account(amani.id, 'KES')
step(`Customer ${amani.legalName} — CIF ${amani.customerNumber}, wallet ${amaniKes.accountNumber}`)

await pay(safariKes, 45_000_000)
await pay(safariKes, 12_500_000)
await pay(safariKes, 800_000, 'Card', 'tok_decline')
const mpesa = await pay(safariKes, 350_000, 'MobileMoney', '+254722334455')
await post('/sandbox/providers/mpesa/events', { type: 'charge.succeeded', reference: mpesa.providerReference, amount: 350_000, currency: 'KES' })
await pay(amaniKes, 5_000_000)
step('Collections: two card payments, one hard decline, one M-Pesa STK push confirmed by webhook, one wallet top-up')

const ft = { debitAccount: amaniKes.accountNumber, creditAccount: safariKes.accountNumber, amount: 1_200_000, narrative: 'Maasai Mara deposit' }
const challenge = await post('/funds-transfers', ft, { idempotent: true })
if (challenge.status === 409) {
  await post('/funds-transfers', { ...ft, acceptedOverrides: challenge.json.overrides.map((o) => o.code) }, { idempotent: true })
}
step('Funds transfer with NEW.BENEFICIARY override accepted')

await post('/payouts', { accountId: safariKes.id, amount: 1_500_000, rail: 'MobileMoney', destinationAccount: '+254722000111', destinationName: 'Driver float', narration: 'Driver allowances' }, { idempotent: true })
const review = await post('/payouts', { accountId: safariKes.id, amount: 20_000_000, rail: 'Bank', destinationAccount: '5550001112', destinationName: 'Mara Camps Ltd', bankCode: '11', narration: 'Lodge booking' }, { idempotent: true })
step(`Payouts: M-Pesa B2C in flight, KES 200,000 bank payout → ${review.json.status}`)

const quote = await post('/fx/quotes', { customerId: safari.id, from: 'KES', to: 'USD', amount: 1_000_000 })
await post('/fx/conversions', { quoteId: quote.json.id, sourceAccountId: safariKes.id, targetAccountId: safariUsd.id }, { idempotent: true })
step(`FX: KES 10,000 → USD ${(quote.json.targetAmount / 100).toFixed(2)}`)

const card = (await post('/cards', { accountId: amaniKes.id, perTransactionLimit: 2_000_000 }, { idempotent: true })).json
const auth = (await post('/sandbox/card-network/purchase', { networkReference: `NET${Date.now()}`, cardToken: card.token, amount: 350_000, currency: 'KES', merchantName: 'Java House Westlands', merchantCategoryCode: '5812' })).json
step(`Card ••${card.last4}: authorisation ${auth.approved ? 'approved' : auth.declineCode} (hold placed)`)

await post('/ops/parameters/charges/MMPAYOUT', { description: 'Mobile money payout', basisPoints: 120 })
step('Parameter change MMPAYOUT → 1.20% input by ops.amina (awaiting a second operator)')

console.log('\nDone. Open the console at http://localhost:5173/console')
