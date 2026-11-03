import fs from 'node:fs'
import path from 'node:path'
import { Contract, JsonRpcProvider } from 'ethers'
import { root, compile } from './compile.mjs'

const ref = process.argv[2]
if (!/^0x[0-9a-fA-F]{64}$/.test(ref ?? '')) throw new Error('Pass the bytes32 providerReference from the collection response.')
const chain = JSON.parse(fs.readFileSync(path.join(root, '.local-chain.json'), 'utf8'))
const url = new URL(chain.rpcURL)
if (url.hostname !== '127.0.0.1' || chain.chainId !== 31337) throw new Error('This script can only pay on the local test chain.')
const provider = new JsonRpcProvider(chain.rpcURL, undefined, { cacheTimeout: -1 })
if ((await provider.getNetwork()).chainId !== 31337n) throw new Error('Wrong chain')
const signer = await provider.getSigner(chain.payer)
const artifacts = compile(true)
const amount = BigInt(`0x${ref.slice(34)}`)
const token = new Contract(chain.token, artifacts.MockUSDC.abi, signer)
const payments = new Contract(chain.paymentsContract, artifacts.ZirnisPayments.abi, signer)
await (await token.approve(chain.paymentsContract, amount)).wait()
const tx = await payments.pay(ref)
await tx.wait()
console.log(`Paid ${amount} atomic test USDC. Transaction: ${tx.hash}. Zirnis will confirm it on its next stale-payment sweep.`)
provider.destroy()
