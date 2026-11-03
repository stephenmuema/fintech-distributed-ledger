import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import solc from 'solc'

export const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

export function compile(includeTests = false) {
  const files = ['src/ZirnisPayments.sol', 'src/ZirnisEscrow.sol', ...(includeTests ? ['test/MockUSDC.sol'] : [])]
  const input = {
    language: 'Solidity',
    sources: Object.fromEntries(files.map((file) => [file, { content: fs.readFileSync(path.join(root, file), 'utf8') }])),
    settings: { optimizer: { enabled: true, runs: 200 }, evmVersion: 'shanghai', outputSelection: { '*': { '*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object'] } } },
  }
  const output = JSON.parse(solc.compile(JSON.stringify(input), { import: (file) => {
    if (!file.startsWith('@openzeppelin/contracts/') || file.includes('..')) return { error: 'Import is not allowed' }
    try { return { contents: fs.readFileSync(path.join(root, 'node_modules', file), 'utf8') } }
    catch { return { error: `Missing dependency: ${file}` } }
  } }))
  const errors = (output.errors ?? []).filter((error) => error.severity === 'error')
  if (errors.length) throw new Error(errors.map((error) => error.formattedMessage).join('\n'))
  return Object.fromEntries(Object.entries(output.contracts).filter(([file]) => files.includes(file)).flatMap(([, contracts]) =>
    Object.entries(contracts).map(([name, c]) => [name, { abi: c.abi, bytecode: `0x${c.evm.bytecode.object}`, deployedBytecode: `0x${c.evm.deployedBytecode.object}` }]),
  ))
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const artifacts = compile()
  fs.mkdirSync(path.join(root, 'artifacts'), { recursive: true })
  for (const [name, artifact] of Object.entries(artifacts)) fs.writeFileSync(path.join(root, 'artifacts', `${name}.json`), `${JSON.stringify(artifact, null, 2)}\n`)
  console.log(`Compiled ${Object.keys(artifacts).join(', ')} with solc ${solc.version()}`)
}
