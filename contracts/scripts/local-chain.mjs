// Local development only: Ganache has immediate simulated finality and lacks EIP-1898.
// This adapter checks canonical hashes before translating hash-pinned calls to block numbers.
// Production modules require real finalized blocks and EIP-1898 from their RPC provider.
import http from 'node:http'
import ganache from 'ganache'
import { BrowserProvider, ContractFactory } from 'ethers'
import { compile } from './compile.mjs'

export async function startLocalChain(port = 8545) {
  const node = ganache.provider({ logging: { quiet: true }, chain: { chainId: 31337, hardfork: 'shanghai' } })
  const provider = new BrowserProvider(node)
  provider.pollingInterval = 20
  const payer = await provider.getSigner(0)
  const treasury = await (await provider.getSigner(1)).getAddress()
  const artifacts = compile(true)
  const deploy = async (name, args = []) => {
    const a = artifacts[name]
    const c = await new ContractFactory(a.abi, a.bytecode, payer).deploy(...args)
    await c.waitForDeployment()
    return c
  }
  const token = await deploy('MockUSDC')
  await (await token.mint(await payer.getAddress(), 1_000_000_000_000n)).wait()
  const payments = await deploy('ZirnisPayments', [await token.getAddress(), treasury])
  const escrow = await deploy('ZirnisEscrow', [await token.getAddress()])

  const server = http.createServer(async (req, res) => {
    let id = null
    try {
      if (req.method !== 'POST') { res.writeHead(405); res.end(); return }
      const chunks = []
      let size = 0
      for await (const chunk of req) {
        size += chunk.length
        if (size > 1_048_576) throw new Error('Request too large')
        chunks.push(chunk)
      }
      const rpc = JSON.parse(Buffer.concat(chunks).toString())
      id = rpc.id
      if (rpc.method === 'eth_call' && typeof rpc.params?.[1] === 'object') {
        const at = rpc.params[1]
        if (!at.requireCanonical || !/^0x[0-9a-f]{64}$/i.test(at.blockHash)) throw new Error('A canonical block hash is required')
        const block = await node.request({ method: 'eth_getBlockByHash', params: [at.blockHash, false] })
        if (!block) throw new Error('Block not found')
        const canonical = await node.request({ method: 'eth_getBlockByNumber', params: [block.number, false] })
        if (canonical.hash !== block.hash) throw new Error('Noncanonical block')
        rpc.params[1] = block.number
      }
      const result = await node.request({ method: rpc.method, params: rpc.params ?? [] })
      res.setHeader('Content-Type', 'application/json')
      res.end(JSON.stringify({ jsonrpc: '2.0', id, result }))
    } catch (error) {
      res.setHeader('Content-Type', 'application/json')
      res.end(JSON.stringify({ jsonrpc: '2.0', id, error: { code: -32000, message: error.message } }))
    }
  })
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve) })
  return {
    rpcURL: `http://127.0.0.1:${server.address().port}`,
    chainId: 31337,
    token: await token.getAddress(),
    paymentsContract: await payments.getAddress(),
    escrowContract: await escrow.getAddress(),
    treasury,
    payer: await payer.getAddress(),
    close: async () => { await new Promise((resolve) => server.close(resolve)); await node.disconnect() },
  }
}
