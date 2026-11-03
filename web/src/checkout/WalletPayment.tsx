import { useState } from 'react'
import { Button, ErrorNote } from '../console/ui'
import { useAction } from '../console/hooks'
import { submitWalletPayment, type Wallet } from './evm'

export type WalletAction = { type: string; url: string; fields?: Record<string, string> }
export function WalletPayment({ next }: { next: WalletAction }) {
  const [paymentHash, setPaymentHash] = useState('')
  const [approvalHash, setApprovalHash] = useState('')
  const [stage, setStage] = useState('')
  const fields = next.fields ?? {}

  const send = useAction(async () => {
    const wallet = (window as unknown as { ethereum?: Wallet }).ethereum
    if (!wallet) throw new Error('Open this page in a browser with an Ethereum-compatible wallet.')
    return submitWalletPayment(wallet, fields, {
      approvalHash, paymentHash, stage: setStage, approval: setApprovalHash, payment: setPaymentHash,
    })
  })

  return <div className="flex flex-col gap-3">
    <strong>Pay with USDC</strong>
    <p>Your wallet will request approval for this amount, then a payment. You also need the network’s gas token.</p>
    <p className="text-xs break-all">Network ID: {/^0x[0-9a-fA-F]+$/.test(fields.chainId ?? '') ? BigInt(fields.chainId).toString() : '—'}<br />Recipient wallet: {fields.treasury}</p>
    <Button busy={send.busy} onClick={() => void send.run()}>{paymentHash ? 'Check transaction' : approvalHash ? 'Check approval and continue' : 'Connect wallet and pay'}</Button>
    {stage && <p>{stage}</p>}
    {paymentHash && <p className="text-xs break-all">Transaction: {paymentHash}</p>}
    <ErrorNote error={send.error} />
    <p className="text-xs">The merchant receives confirmation after Zirnis verifies network finality. Sending a transaction alone does not confirm payment.</p>
  </div>
}
