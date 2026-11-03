export type Wallet = { request(args: { method: string; params?: unknown[] }): Promise<unknown> }
export type WalletProgress = {
  approvalHash: string
  paymentHash?: string
  stage(message: string): void
  approval(hash: string): void
  payment(hash: string): void
}
const address = /^0x[0-9a-fA-F]{40}$/
const hash = /^0x[0-9a-fA-F]{64}$/
class Reverted extends Error {}

async function mined(wallet: Wallet, transactionHash: string, kind: string): Promise<void> {
  const deadline = Date.now() + 180_000
  while (Date.now() < deadline) {
    const receipt = await wallet.request({ method: 'eth_getTransactionReceipt', params: [transactionHash] }) as { status: string } | null
    if (receipt) {
      if (receipt.status !== '0x1') throw new Reverted(`The USDC ${kind} reverted. Please try again.`)
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 2000))
  }
  throw new Error(`The ${kind} is still pending. Check your wallet before continuing.`)
}

export async function submitWalletPayment(wallet: Wallet, fields: Record<string, string>, progress: WalletProgress): Promise<string> {
  const { chainId, token, contract, reference, amount, treasury } = fields
  if (!/^0x[0-9a-fA-F]+$/.test(chainId ?? '') || BigInt(chainId) <= 0n || BigInt(chainId) > 18446744073709551615n
    || !address.test(token ?? '') || !address.test(contract ?? '') || !address.test(treasury ?? '')
    || !hash.test(reference ?? '') || !/^[1-9]\d*$/.test(amount ?? '')
    || fields.payData?.toLowerCase() !== `0x8609cad1${reference?.slice(2).toLowerCase()}`
    || BigInt(amount) > 9223372036854775807n || BigInt(`0x${reference.slice(34)}`) !== BigInt(amount)
    || /^0+$/.test(reference.slice(2, 34))) throw new Error('The payment instructions are invalid. Contact the merchant.')
  const accounts = await wallet.request({ method: 'eth_requestAccounts' }) as string[]
  const from = accounts[0]
  if (!address.test(from ?? '')) throw new Error('Select a wallet account to pay.')
  const current = await wallet.request({ method: 'eth_chainId' }) as string
  if (BigInt(current) !== BigInt(chainId)) await wallet.request({ method: 'wallet_switchEthereumChain', params: [{ chainId }] })
  const checkChain = async () => {
    const selected = await wallet.request({ method: 'eth_chainId' }) as string
    if (BigInt(selected) !== BigInt(chainId)) throw new Error('Switch your wallet to the payment network.')
  }
  await checkChain()
  const confirmPayment = async (tx: string) => {
    progress.stage('Payment submitted. Waiting for the transaction to be mined…')
    try { await mined(wallet, tx, 'payment') }
    catch (error) {
      if (error instanceof Reverted) progress.payment('')
      throw error
    }
    progress.stage('Payment mined. Waiting for network finality…')
    return tx
  }
  if (progress.paymentHash) return confirmPayment(progress.paymentHash)
  const word = (value: string) => value.replace(/^0x/, '').padStart(64, '0')
  const allowance = async () => BigInt(await wallet.request({ method: 'eth_call', params: [
    { to: token, data: `0xdd62ed3e${word(from)}${word(contract)}` }, 'latest',
  ] }) as string)
  if (await allowance() < BigInt(amount)) {
    progress.stage('Approve the exact USDC amount in your wallet.')
    const tx = progress.approvalHash || await wallet.request({ method: 'eth_sendTransaction', params: [
      { from, to: token, chainId, value: '0x0', data: `0x095ea7b3${word(contract)}${word(BigInt(amount).toString(16))}` },
    ] }) as string
    if (!hash.test(tx)) throw new Error('The wallet returned an invalid approval transaction.')
    progress.approval(tx)
    progress.stage('Waiting for your USDC approval…')
    try { await mined(wallet, tx, 'approval') }
    catch (error) {
      if (error instanceof Reverted) progress.approval('')
      throw error
    }
    progress.approval('')
    if (await allowance() < BigInt(amount)) throw new Error('USDC approval is insufficient for the selected wallet. Please try again.')
  }
  await checkChain()
  progress.stage('Confirm the USDC payment in your wallet.')
  const tx = await wallet.request({ method: 'eth_sendTransaction', params: [
    { from, to: contract, chainId, value: '0x0', data: fields.payData },
  ] }) as string
  if (!hash.test(tx)) throw new Error('The wallet returned an invalid payment transaction.')
  progress.payment(tx)
  return confirmPayment(tx)
}
