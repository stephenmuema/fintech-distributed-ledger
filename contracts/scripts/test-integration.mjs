import { spawn } from 'node:child_process'
import path from 'node:path'
import { root } from './compile.mjs'
import { startLocalChain } from './local-chain.mjs'

const chain = await startLocalChain(0)
try {
  const child = spawn('go', ['test', '-count=1', '-run', '^TestLocalEVMEndToEnd$', './...'], {
    cwd: path.join(root, '..', 'modules', 'stablecoin'), stdio: 'inherit',
    env: { ...process.env, STABLECOIN_LOCAL_RPC: chain.rpcURL, STABLECOIN_LOCAL_TOKEN: chain.token,
      STABLECOIN_LOCAL_CONTRACT: chain.paymentsContract, STABLECOIN_LOCAL_TREASURY: chain.treasury, STABLECOIN_LOCAL_PAYER: chain.payer },
  })
  const code = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve) })
  process.exitCode = code ?? 1
} finally { await chain.close() }
