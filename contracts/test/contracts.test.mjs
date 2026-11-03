import test from 'node:test'
import assert from 'node:assert/strict'
import ganache from 'ganache'
import { AbiCoder, BrowserProvider, ContractFactory, keccak256, toBeHex, zeroPadValue } from 'ethers'
import { compile } from '../scripts/compile.mjs'

const artifacts = compile(true)
const invoice = (amount = 1_234_567n) => `0x00112233445566778899aabbccddeeff${amount.toString(16).padStart(32, '0')}`

async function setup(t) {
  const node = ganache.provider({ logging: { quiet: true }, chain: { chainId: 31337, hardfork: 'shanghai' } })
  t.after(async () => { await node.disconnect() })
  const provider = new BrowserProvider(node)
  provider.pollingInterval = 20
  const buyer = await provider.getSigner(0)
  const seller = await provider.getSigner(1)
  const arbiter = await provider.getSigner(2)
  const outsider = await provider.getSigner(3)
  const deploy = async (name, args = []) => {
    const a = artifacts[name]
    const c = await new ContractFactory(a.abi, a.bytecode, buyer).deploy(...args)
    await c.waitForDeployment()
    return c
  }
  const token = await deploy('MockUSDC')
  await (await token.mint(await buyer.getAddress(), 100_000_000n)).wait()
  const payments = await deploy('ZirnisPayments', [await token.getAddress(), await seller.getAddress()])
  const escrow = await deploy('ZirnisEscrow', [await token.getAddress()])
  return { node, provider, buyer, seller, arbiter, outsider, token, payments, escrow, deploy }
}

async function fund(f, reference = invoice(), amount = 10_000_001n) {
  const buyer = await f.buyer.getAddress()
  const id = keccak256(AbiCoder.defaultAbiCoder().encode(['address', 'bytes32'], [buyer, reference]))
  const block = await f.node.request({ method: 'eth_getBlockByNumber', params: ['latest', false] })
  const deadline = BigInt(block.timestamp) + 3600n
  await (await f.token.approve(await f.escrow.getAddress(), amount)).wait()
  await (await f.escrow.fund(reference, await f.seller.getAddress(), await f.arbiter.getAddress(), amount, deadline)).wait()
  return { id, deadline, amount, reference }
}

test('payment transfers exact atomic units, records the invoice and rejects replay', async (t) => {
  const f = await setup(t)
  const ref = invoice()
  await assert.rejects(f.payments.pay(ref)) // no allowance
  await (await f.token.approve(await f.payments.getAddress(), 1_234_567n)).wait()
  await (await f.payments.pay(ref, { gasLimit: 250_000 })).wait()
  assert.equal(await f.token.balanceOf(await f.seller.getAddress()), 1_234_567n)
  assert.equal(await f.token.balanceOf(await f.payments.getAddress()), 0n)
  const paid = await f.payments.payments(ref)
  assert.equal(paid.payer, await f.buyer.getAddress())
  assert.equal(paid.amount, 1_234_567n)
  assert.equal((await f.payments.queryFilter(f.payments.filters.Paid(ref))).length, 1)
  await assert.rejects(f.payments.pay(ref))
  assert.equal(await f.token.balanceOf(await f.seller.getAddress()), 1_234_567n)
})

test('invalid invoice ids, zero amounts and int64 overflow cannot be paid', async (t) => {
  const f = await setup(t)
  for (const ref of [invoice(0n), zeroPadValue(toBeHex(1), 32), invoice(9223372036854775808n)]) {
    await assert.rejects(f.payments.pay(ref))
  }
})

test('a fee-on-transfer token cannot create an underfunded collection or escrow', async (t) => {
  const f = await setup(t)
  const token = await f.deploy('FeeToken')
  const payments = await f.deploy('ZirnisPayments', [await token.getAddress(), await f.seller.getAddress()])
  const escrow = await f.deploy('ZirnisEscrow', [await token.getAddress()])
  await (await token.mint(await f.buyer.getAddress(), 100_000_000n)).wait()
  await (await token.approve(await payments.getAddress(), 1_234_567n)).wait()
  await assert.rejects(payments.pay(invoice()))
  assert.equal((await payments.payments(invoice())).amount, 0n)
  await (await token.approve(await escrow.getAddress(), 1_234_567n)).wait()
  const block = await f.node.request({ method: 'eth_getBlockByNumber', params: ['latest', false] })
  await assert.rejects(escrow.fund(invoice(), await f.seller.getAddress(), await f.arbiter.getAddress(), 1_234_567n, BigInt(block.timestamp) + 3600n))
  assert.equal(await token.balanceOf(await escrow.getAddress()), 0n)
})

test('only the buyer releases escrow, once, and only the seller can refund early', async (t) => {
  const f = await setup(t)
  const e = await fund(f)
  await assert.rejects(f.escrow.connect(f.seller).release(e.id))
  await assert.rejects(f.escrow.connect(f.outsider).refund(e.id))
  await (await f.escrow.release(e.id)).wait()
  assert.equal(await f.token.balanceOf(await f.seller.getAddress()), e.amount)
  assert.equal((await f.escrow.escrows(e.id)).state, 3n)
  await assert.rejects(f.escrow.release(e.id))
  const other = await fund(f, invoice(2n))
  const before = await f.token.balanceOf(await f.buyer.getAddress())
  await (await f.escrow.connect(f.seller).refund(other.id)).wait()
  assert.equal(await f.token.balanceOf(await f.buyer.getAddress()), before + other.amount)
})

test('disputes block unilateral withdrawals and only the arbiter can split funds', async (t) => {
  const f = await setup(t)
  const e = await fund(f)
  await assert.rejects(f.escrow.connect(f.outsider).dispute(e.id))
  await (await f.escrow.connect(f.seller).dispute(e.id)).wait()
  await assert.rejects(f.escrow.release(e.id))
  await assert.rejects(f.escrow.connect(f.seller).refund(e.id))
  await assert.rejects(f.escrow.resolve(e.id, 6_000_000n))
  await assert.rejects(f.escrow.connect(f.arbiter).resolve(e.id, e.amount + 1n))
  const buyerBefore = await f.token.balanceOf(await f.buyer.getAddress())
  await (await f.escrow.connect(f.arbiter).resolve(e.id, 6_000_000n)).wait()
  assert.equal(await f.token.balanceOf(await f.seller.getAddress()), 6_000_000n)
  assert.equal(await f.token.balanceOf(await f.buyer.getAddress()), buyerBefore + e.amount - 6_000_000n)
  assert.equal(await f.token.balanceOf(await f.escrow.getAddress()), 0n)
  await assert.rejects(f.escrow.connect(f.arbiter).resolve(e.id, 0n))
})

test('expired undisputed escrow refunds the buyer and cannot acquire a late dispute', async (t) => {
  const f = await setup(t)
  const e = await fund(f)
  await assert.rejects(f.escrow.refundExpired(e.id))
  await f.node.request({ method: 'evm_increaseTime', params: [3601] })
  await f.node.request({ method: 'evm_mine', params: [] })
  await assert.rejects(f.escrow.connect(f.seller).dispute(e.id))
  await (await f.escrow.refundExpired(e.id, { gasLimit: 250_000 })).wait()
  assert.equal(await f.token.balanceOf(await f.buyer.getAddress()), 100_000_000n)
})

test('a dispute stays locked after the deadline until the chosen arbiter resolves it', async (t) => {
  const f = await setup(t)
  const e = await fund(f)
  await (await f.escrow.dispute(e.id)).wait()
  await f.node.request({ method: 'evm_increaseTime', params: [3601] })
  await f.node.request({ method: 'evm_mine', params: [] })
  await assert.rejects(f.escrow.refundExpired(e.id))
  await (await f.escrow.connect(f.arbiter).resolve(e.id, 0n)).wait()
  assert.equal(await f.token.balanceOf(await f.buyer.getAddress()), 100_000_000n)
})

test('escrow references are buyer-scoped and cannot be funded twice', async (t) => {
  const f = await setup(t)
  const e = await fund(f)
  await assert.rejects(f.escrow.fund(e.reference, await f.seller.getAddress(), await f.arbiter.getAddress(), e.amount, e.deadline))
  await assert.rejects(f.escrow.fund(invoice(3n), await f.buyer.getAddress(), await f.arbiter.getAddress(), e.amount, e.deadline))
  assert.equal(await f.token.balanceOf(await f.escrow.getAddress()), e.amount)
})
