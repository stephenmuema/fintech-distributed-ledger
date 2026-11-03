import { useEffect, useState, type FormEvent } from 'react'
import { useParams } from 'react-router-dom'
import { api } from '../console/api'
import { money, toMinor } from '../console/format'
import { useAction, useLoad } from '../console/hooks'
import { Lock } from 'lucide-react'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { Button, ErrorNote, Loading } from '../console/ui'
import '../console/console.css'
import { WalletPayment } from './WalletPayment'

// How the payer can pay this link: a push to their phone, a card on the provider's own page, or
// (sandbox only) a test card token.
type Methods = { mobileMoney: boolean; hostedCard: boolean; cardToken: boolean; stablecoin?: boolean }
type Link = { code: string; description: string; amount?: number; currency: string; isActive: boolean; expiresAt?: string; methods: Methods; sandbox: boolean }
type NextAction = { type: string; url: string; fields?: Record<string, string> }
// The payer sees the payment's outcome: Completed, Processing (waiting for the PIN) or Failed.
type Result = { reference: string; paymentId?: string; status: string; amount: number; currency: string; failureCode?: string; providerReference?: string; nextAction?: NextAction }
type Payer = { firstName: string; lastName: string; email: string; phone: string; address: string; postalCode: string }

const noPayer: Payer = { firstName: '', lastName: '', email: '', phone: '', address: '', postalCode: '' }

/** Sends the payer's browser on to the provider's hosted page. */
function follow(next: NextAction) {
  if (next.type !== 'post_form') {
    window.location.assign(next.url)
    return
  }
  const form = document.createElement('form')
  form.method = 'POST'
  form.action = next.url
  for (const [name, value] of Object.entries(next.fields ?? {})) {
    const input = document.createElement('input')
    input.type = 'hidden'
    input.name = name
    input.value = value
    form.appendChild(input)
  }
  document.body.appendChild(form)
  form.submit()
}

/** Public hosted checkout for a payment link: what a payer opens from WhatsApp, SMS or email. */
export default function Checkout() {
  const { code = '' } = useParams()
  const link = useLoad(() => api<Link>(`/pay/${code}`, { anonymous: true }), [code])
  useEffect(() => { document.title = 'Secure payment · Zirnis' }, [])

  return (
    <div className="checkout">
      <Card className="w-full max-w-md shadow-sm">
        <CardHeader>
          <p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Lock className="size-3.5" />Zirnis · secure payment</p>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {link.error && <ErrorNote error={link.error} />}
          {link.loading && !link.data && <Loading />}
          {link.data && <Pay link={link.data} />}
        </CardContent>
      </Card>
    </div>
  )
}

function Pay({ link }: { link: Link }) {
  const card = link.methods.hostedCard || link.methods.cardToken
  const [method, setMethod] = useState<'MobileMoney' | 'Card' | 'Stablecoin'>(link.methods.mobileMoney ? 'MobileMoney' : link.methods.stablecoin ? 'Stablecoin' : 'Card')
  const [phone, setPhone] = useState(link.sandbox ? '+254712345678' : '')
  const [token, setToken] = useState('tok_visa')
  const [payer, setPayer] = useState<Payer>(noPayer)
  const [custom, setCustom] = useState('')
  // One key per checkout visit: a double tap or network retry replays the first attempt.
  const [idempotencyKey] = useState(() => crypto.randomUUID())
  const hosted = method === 'Card' && link.methods.hostedCard

  const pay = useAction(async () => {
    if (link.amount != null && !Number.isSafeInteger(link.amount)) throw new Error('This amount exceeds the checkout range. Contact the merchant.')
    const source = method === 'Stablecoin' ? 'wallet' : method === 'MobileMoney' ? phone : hosted ? 'hosted_checkout' : token
    const result = await api<Result>(`/pay/${link.code}`, {
      method: 'POST',
      anonymous: true,
      idempotencyKey,
      body: { method, source, amount: link.amount ? null : toMinor(custom, link.currency), customer: hosted ? payer : undefined },
    })
    if (result.nextAction && result.nextAction.type !== 'evm_payment') follow(result.nextAction)
    return result
  })
  // Sandbox only: stands in for the payer approving the STK push on their phone.
  const approve = useAction(async (r: Result) => {
    await api('/sandbox/providers/mpesa/events', { method: 'POST', body: { type: 'charge.succeeded', reference: r.providerReference, amount: r.amount, currency: r.currency } })
    return { ...r, status: 'Completed' }
  })

  const [observed, setObserved] = useState<Result>()
  const result = observed ?? approve.result ?? pay.result
  useEffect(() => {
    if (!result?.paymentId || result.status !== 'Processing') return
    let active = true
    const timer = setInterval(() => {
      void api<Result>(`/pay/${link.code}/payments/${result.paymentId}`, { anonymous: true }).then((latest) => {
        if (active) setObserved({ ...result, ...latest })
      }).catch(() => { /* Keep the pending state while connectivity recovers. */ })
    }, 5000)
    return () => { active = false; clearInterval(timer) }
  }, [link.code, result])
  const submit = (e: FormEvent) => {
    e.preventDefault()
    void pay.run()
  }
  const field = (name: keyof Payer, label: string, type = 'text', autoComplete?: string) => (
    <label className="field">
      <span className="mono-label">{label}</span>
      <input required type={type} autoComplete={autoComplete} value={payer[name]} onChange={(e) => setPayer({ ...payer, [name]: e.target.value })} />
    </label>
  )

  return (
    <>
      <h1 className="text-lg font-semibold">{link.description}</h1>
      <p className="checkout-amount">{link.amount ? Number.isSafeInteger(link.amount) ? money(link.amount, link.currency) : 'This amount exceeds the checkout range. Contact the merchant.' : `Enter an amount in ${link.currency}`}</p>

      {result ? (
        <div className={`checkout-result ${result.status === 'Completed' ? 'good' : result.status === 'Failed' ? 'bad' : ''}`}>
          {result.status === 'Completed' && <><strong>Payment received.</strong><p>Thank you — {money(result.amount, result.currency)} has been paid.</p></>}
          {result.status === 'Failed' && <><strong>Payment failed.</strong><p>{result.failureCode?.replace(/_/g, ' ')}. Please try another method.</p></>}
          {result.status === 'Processing' && result.nextAction?.type === 'evm_payment' && <WalletPayment next={result.nextAction} />}
          {result.status === 'Processing' && result.nextAction && result.nextAction.type !== 'evm_payment' && <><strong>Taking you to the card page…</strong><p>Finish the payment there.</p></>}
          {result.status === 'Processing' && !result.nextAction && (
            <>
              <strong>Check your phone.</strong>
              <p>Enter your M-Pesa PIN on the prompt sent to {phone} to complete the payment.</p>
              {link.sandbox && (
                <Button variant="ghost" className="mt-3" busy={approve.busy} onClick={() => void approve.run(result)}>Sandbox: approve on phone</Button>
              )}
              <ErrorNote error={approve.error} />
            </>
          )}
        </div>
      ) : !link.methods.mobileMoney && !card && !link.methods.stablecoin ? (
        <p className="muted">This link can't be paid right now. Please contact whoever sent it.</p>
      ) : (
        <form className="form" onSubmit={submit}>
          {!link.amount && (
            <label className="field"><span className="mono-label">AMOUNT</span><input required inputMode="decimal" value={custom} onChange={(e) => setCustom(e.target.value)} placeholder="1,500" /></label>
          )}
          {([link.methods.mobileMoney, card, link.methods.stablecoin].filter(Boolean).length > 1) && (
            <div className="segmented">
              {link.methods.mobileMoney && <button type="button" className={method === 'MobileMoney' ? 'on' : ''} onClick={() => setMethod('MobileMoney')}>M-Pesa</button>}
              {card && <button type="button" className={method === 'Card' ? 'on' : ''} onClick={() => setMethod('Card')}>Card</button>}
              {link.methods.stablecoin && <button type="button" className={method === 'Stablecoin' ? 'on' : ''} onClick={() => setMethod('Stablecoin')}>USDC</button>}
            </div>
          )}
          {method === 'MobileMoney' && (
            <label className="field">
              <span className="mono-label">M-PESA NUMBER</span>
              <input className="mono" required type="tel" autoComplete="tel" placeholder="+2547…" value={phone} onChange={(e) => setPhone(e.target.value)} />
            </label>
          )}
          {hosted && (
            <>
              {field('firstName', 'FIRST NAME', 'text', 'given-name')}
              {field('lastName', 'LAST NAME', 'text', 'family-name')}
              {field('email', 'EMAIL', 'email', 'email')}
              {field('phone', 'PHONE', 'tel', 'tel')}
              {field('address', 'ADDRESS', 'text', 'street-address')}
              {field('postalCode', 'POSTAL CODE', 'text', 'postal-code')}
            </>
          )}
          {method === 'Card' && !hosted && (
            <label className="field">
              <span className="mono-label">SANDBOX TEST CARD TOKEN</span>
              <input className="mono" required value={token} onChange={(e) => setToken(e.target.value)} />
            </label>
          )}
          <ErrorNote error={pay.error} />
          <Button className="w-full" busy={pay.busy} disabled={!link.isActive || (link.amount != null && !Number.isSafeInteger(link.amount))}>{hosted ? 'Continue to card payment' : 'Pay now'}</Button>
        </form>
      )}
      <p className="checkout-foot">
        {hosted ? 'You enter your card on the card provider’s own secure page; your card details never reach us.' : 'Your payment is protected: a double tap or a retry can’t charge you twice.'}
      </p>
    </>
  )
}
