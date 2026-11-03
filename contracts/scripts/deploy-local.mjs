import fs from 'node:fs'
import path from 'node:path'
import { root } from './compile.mjs'
import { startLocalChain } from './local-chain.mjs'

const chain = await startLocalChain()
const { close, ...config } = chain
fs.writeFileSync(path.join(root, '.local-chain.json'), `${JSON.stringify(config, null, 2)}\n`)
console.log('Local test chain only. Accounts are unlocked, USDC is a mintable test token, and finality is simulated.')
console.log(`STABLECOIN_RPC_URL=${chain.rpcURL}\nSTABLECOIN_CHAIN_ID=${chain.chainId}\nSTABLECOIN_TOKEN=${chain.token}\nSTABLECOIN_PAYMENTS_CONTRACT=${chain.paymentsContract}\nSTABLECOIN_TREASURY=${chain.treasury}`)
console.log(`Escrow: ${chain.escrowContract}\nTest payer: ${chain.payer}\nRun npm run pay:local -- <payment reference> in another terminal.`)
for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, async () => { await close(); process.exit(0) })
