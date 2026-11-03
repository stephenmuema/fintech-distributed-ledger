import test from 'node:test'
import assert from 'node:assert/strict'
import { toMinor, exponent, amount } from '../src/console/format.ts'
import { submitWalletPayment } from '../src/checkout/evm.ts'

const token = `0x${'1'.repeat(40)}`
const contract = `0x${'2'.repeat(40)}`
const treasury = `0x${'3'.repeat(40)}`
const payer = `0x${'4'.repeat(40)}`
const reference = `0x00112233445566778899aabbccddeeff${(1234567n).toString(16).padStart(32, '0')}`
const fields = { token, contract, treasury, reference, chainId: '0x7a69', amount: '1234567', payData: `0x8609cad1${reference.slice(2)}` }

function wallet({ allowance = 0n, receiptStatus = '0x1', switchWorks = true, changeAfterApproval = false } = {}) {
  const sent = []
  let chainId = '0x1'
  const fake = { async request({ method, params }) {
    if (method === 'eth_requestAccounts') return [payer]
    if (method === 'eth_chainId') return chainId
    if (method === 'wallet_switchEthereumChain') { if (switchWorks) chainId = params[0].chainId; return null }
    if (method === 'eth_call') return `0x${allowance.toString(16)}`
    if (method === 'eth_sendTransaction') { sent.push(params[0]); return `0x${sent.length.toString().repeat(64)}` }
    if (method === 'eth_getTransactionReceipt') {
      if (receiptStatus === '0x1') allowance = 1234567n
      if (changeAfterApproval) chainId = '0x1'
      return { status: receiptStatus }
    }
    throw new Error(method)
  } }
  let approved = ''
  let paid = ''
  const progress = { approvalHash: '', stage() {}, approval(value) { approved = value }, payment(value) { paid = value } }
  return { fake, sent, progress, approved: () => approved, paid: () => paid }
}

test('USDC input preserves six decimals and rejects rounding and unsafe JSON amounts', () => {
  assert.equal(exponent('USDC'), 6)
  assert.equal(toMinor('0.000001', 'USDC'), 1)
  assert.equal(toMinor('1,234.567891', 'USDC'), 1234567891)
  assert.equal(toMinor('0.29', 'USD'), 29)
  assert.equal(amount(9007199254740991, 'USDC'), '9,007,199,254.740991')
  assert.equal(amount(-1234567, 'USDC'), '-1.234567')
  for (const amount of ['0.0000001', '-1', 'NaN', '1e4', '9007199254.740992']) assert.throws(() => toMinor(amount, 'USDC'))
})

test('wallet switches chains, approves only the exact amount and submits the bound invoice', async () => {
  const w = wallet()
  await submitWalletPayment(w.fake, fields, w.progress)
  assert.equal(w.sent.length, 2)
  assert.equal(w.sent[0].to, token)
  assert.equal(BigInt(`0x${w.sent[0].data.slice(-64)}`), 1234567n)
  assert.equal(w.sent[1].to, contract)
  assert.equal(w.sent[1].data, fields.payData)
  assert.ok(w.sent.every((tx) => tx.chainId === fields.chainId && tx.value === '0x0'))
  assert.ok(w.paid())
})

test('existing allowance and pending approval are reused without another approval', async () => {
  const approved = wallet({ allowance: 1234567n })
  await submitWalletPayment(approved.fake, fields, approved.progress)
  assert.equal(approved.sent.length, 1)
  const pending = wallet()
  pending.progress.approvalHash = `0x${'a'.repeat(64)}`
  await submitWalletPayment(pending.fake, fields, pending.progress)
  assert.equal(pending.sent.length, 1)
  assert.equal(pending.sent[0].to, contract)
})

test('failed approval or chain changes never submit the payment', async () => {
  for (const options of [{ receiptStatus: '0x0' }, { changeAfterApproval: true }, { switchWorks: false }]) {
    const w = wallet(options)
    await assert.rejects(submitWalletPayment(w.fake, fields, w.progress))
    assert.equal(w.paid(), '')
    assert.ok(w.sent.every((tx) => tx.to === token))
  }
})

test('checking a submitted transaction never submits it again, and a revert permits retry', async () => {
  const w = wallet()
  w.progress.paymentHash = `0x${'a'.repeat(64)}`
  await submitWalletPayment(w.fake, fields, w.progress)
  assert.equal(w.sent.length, 0)
  const reverted = wallet({ receiptStatus: '0x0' })
  reverted.progress.paymentHash = `0x${'a'.repeat(64)}`
  reverted.progress.payment(reverted.progress.paymentHash)
  await assert.rejects(submitWalletPayment(reverted.fake, fields, reverted.progress), /payment reverted/)
  assert.equal(reverted.paid(), '')
  assert.equal(reverted.sent.length, 0)
})

test('mismatched invoice amount, calldata or network is rejected before connecting a wallet', async () => {
  for (const patch of [{ amount: '1' }, { payData: `0x095ea7b3${reference.slice(2)}` }, { reference: `0x${'0'.repeat(64)}` }, { chainId: '0x0' }]) {
    const w = wallet()
    await assert.rejects(submitWalletPayment(w.fake, { ...fields, ...patch }, w.progress))
    assert.equal(w.sent.length, 0)
  }
})
