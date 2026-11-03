import { useEffect, useState, type FormEvent } from 'react'
import { Search } from 'lucide-react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { dateTime, money } from '../format'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { Account, Customer, KycReview, RecordChange } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel } from '../ui'
import { LocalValues } from './LocalTables'

type Position = { customer: Customer; accounts: Account[] }

const PAGE = 50

export default function Customers() {
  const [query, setQuery] = useState('')
  const [search, setSearch] = useState('')
  const [pages, setPages] = useState<Customer[][]>([])
  const [more, setMore] = useState(false)
  const first = useLoad(() => api<Customer[]>(`/customers?limit=${PAGE}&q=${encodeURIComponent(search)}`), [search])
  const next = useAction((before: number) => api<Customer[]>(`/customers?limit=${PAGE}&q=${encodeURIComponent(search)}&before=${before}`))
  const [selected, setSelected] = useState<number>()
  const rows = [...(first.data ?? []), ...pages.flat()]
  const hasMore = more || (first.data?.length === PAGE && pages.length === 0)

  // A new search (typed, then settled for 300 ms) starts again from the newest customer.
  useEffect(() => {
    const t = setTimeout(() => {
      setSearch(query.trim())
      setPages([])
      setMore(false)
    }, 300)
    return () => clearTimeout(t)
  }, [query])

  const loadMore = async () => {
    const last = rows[rows.length - 1]
    const page = last && (await next.run(last.customerNumber))
    if (page) {
      setPages((p) => [...p, page])
      setMore(page.length === PAGE)
    }
  }
  const reload = () => { setPages([]); first.reload() }

  return (
    <>
      <PageHeader module="ST · CUSTOMERS" title="Customer information file" />
      <div className="grid-main-side">
        <Panel title="Customers" aside={<span className="muted">{rows.length}{hasMore ? '+' : ''} shown</span>}>
          <div className="relative mb-3">
            <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
            <input className="pl-9" placeholder="Search by CIF number, name, email or mnemonic" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Search customers" />
          </div>
          {first.loading && !first.data && <Loading />}
          <ErrorNote error={first.error ?? next.error} />
          {first.data?.length === 0 && <Empty>{search ? `No customer matches “${search}”.` : 'No customers yet — onboard one on the right.'}</Empty>}
          {rows.length > 0 && (
            <table>
              <thead>
                <tr><th>CIF</th><th>Mnemonic</th><th>Name</th><th>Type</th><th>KYC</th><th className="num">Tier</th></tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <tr key={c.id} className={`clickable ${selected === c.customerNumber ? 'selected' : ''}`} onClick={() => setSelected(c.customerNumber)}>
                    <td><Mono>{c.customerNumber}</Mono></td>
                    <td><Mono>{c.mnemonic}</Mono></td>
                    <td>{c.legalName}</td>
                    <td>{c.type} <span className="muted">({c.sector})</span></td>
                    <td><Badge value={c.kycStatus} /></td>
                    <td className="num">{c.tier}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {hasMore && rows.length > 0 && (
            <div className="mt-3 flex justify-center">
              <Button variant="ghost" busy={next.busy} onClick={() => void loadMore()}>Load {PAGE} more</Button>
            </div>
          )}
        </Panel>

        <div className="stack">
          <KycQueue onDecided={reload} />
          {selected !== undefined && <CustomerPosition number={selected} onChange={reload} />}
          <Onboard onCreated={(c) => (reload(), setSelected(c.customerNumber))} />
        </div>
      </div>
    </>
  )
}

function Onboard({ onCreated }: { onCreated: (c: Customer) => void }) {
  const [form, setForm] = useState({ type: 'Individual', legalName: '', email: '', phone: '+2547', country: 'KE' })
  const create = useAction(() => api<Customer>('/customers', { method: 'POST', body: { ...form, phone: form.phone.length > 5 ? form.phone : null } }))

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const customer = await create.run()
    if (customer) {
      onCreated(customer)
      setForm({ ...form, legalName: '', email: '' })
    }
  }

  return (
    <Panel title="Onboard a customer">
      <form className="form" onSubmit={submit}>
        <div className="segmented">
          {['Individual', 'Business', 'FinancialInstitution'].map((t) => (
            <button type="button" key={t} className={form.type === t ? 'on' : ''} onClick={() => setForm({ ...form, type: t })}>{t}</button>
          ))}
        </div>
        <Field label={form.type !== 'Individual' ? 'Registered name' : 'Full legal name'}>
          <input required value={form.legalName} onChange={(e) => setForm({ ...form, legalName: e.target.value })} placeholder={form.type === 'Business' ? 'Safari Tours Ltd' : form.type === 'FinancialInstitution' ? 'Stanbic Bank Uganda' : 'Amani Wanjiku'} />
        </Field>
        <Field label="Email">
          <input required type="email" value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="name@example.co.ke" />
        </Field>
        <div className="row-2">
          <Field label="Phone"><input value={form.phone} onChange={(e) => setForm({ ...form, phone: e.target.value })} /></Field>
          <Field label="Country"><input maxLength={2} value={form.country} onChange={(e) => setForm({ ...form, country: e.target.value.toUpperCase() })} /></Field>
        </div>
        <ErrorNote error={create.error} />
        <Button busy={create.busy}>Create CIF record</Button>
      </form>
    </Panel>
  )
}

function CustomerPosition({ number, onChange }: { number: number; onChange: () => void }) {
  const position = useLoad(() => api<Position>(`/customers/by-number/${number}`), [number])
  const [currency, setCurrency] = useState('KES')
  const kyc = useAction((c: Customer) =>
    api(`/customers/${c.id}/kyc`, {
      method: 'POST',
      body: {
        documentType: c.type === 'Business' ? 'certificate_of_incorporation' : c.type === 'FinancialInstitution' ? 'banking_licence' : 'national_id',
        documentNumber: String(20_000_000 + c.customerNumber),
        dateOfBirth: c.type !== 'Individual' ? null : '1991-04-12',
        registrationNumber: c.type !== 'Individual' ? `PVT-${c.customerNumber}` : null,
        directors: c.type !== 'Individual' ? [c.legalName] : null,
      },
    }),
  )
  const open = useAction((c: Customer) => api('/accounts', { method: 'POST', body: { customerId: c.id, currency } }))

  if (position.loading && !position.data) return <Panel title="Customer position"><Loading /></Panel>
  if (!position.data) return <ErrorNote error={position.error} />
  const { customer, accounts } = position.data

  const after = async (p: Promise<unknown>) => {
    if (await p) {
      position.reload()
      onChange()
    }
  }

  return (
    <Panel title={`${customer.legalName}`} aside={<Badge value={customer.kycStatus} />}>
      <dl className="facts">
        <div><dt>CIF</dt><dd><Mono>{customer.customerNumber}</Mono></dd></div>
        <div><dt>Mnemonic</dt><dd><Mono>{customer.mnemonic}</Mono></dd></div>
        <div><dt>Sector</dt><dd>{customer.sector}</dd></div>
        <div><dt>Tier</dt><dd>{customer.tier}</dd></div>
        <div><dt>Officer</dt><dd>DAO {customer.accountOfficer}</dd></div>
        <div><dt>Record</dt><dd>CURR.NO {customer.version}</dd></div>
      </dl>

      {customer.kycStatus !== 'Verified' && (
        <div className="callout">
          <p>{customer.type !== 'Individual' ? 'KYB' : 'KYC'} not complete — the customer can receive money but cannot send it.</p>
          <ErrorNote error={kyc.error} />
          <Button variant="accent" busy={kyc.busy} onClick={() => after(kyc.run(customer))}>Run {customer.type !== 'Individual' ? 'KYB' : 'KYC'} check</Button>
        </div>
      )}

      <h3 className="sub">Accounts</h3>
      {accounts.length === 0 ? <Empty>No accounts yet.</Empty> : (
        <table>
          <thead><tr><th>Account</th><th>Category</th><th className="num">Working balance</th><th /></tr></thead>
          <tbody>
            {accounts.map((a) => (
              <tr key={a.id}>
                <td><Link to={`/console/accounts/${a.accountNumber}`}><Mono>{a.accountNumber}</Mono></Link></td>
                <td>{a.category}</td>
                <td className="num">{money(a.workingBalance, a.currency)}</td>
                <td>{a.closedAt ? <Badge value="Closed" /> : a.restriction !== 'None' && <Badge value={a.restriction} />}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="inline-form">
        <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
          {['KES', 'USD', 'EUR', 'GBP', 'UGX', 'TZS', 'NGN', 'USDC'].map((c) => <option key={c}>{c}</option>)}
        </select>
        <Button variant="ghost" busy={open.busy} onClick={() => after(open.run(customer))}>Open {currency} account</Button>
      </div>
      <ErrorNote error={open.error} />
      <LocalValues table="CUSTOMER" recordKey={customer.id} />
      <ChangeLog customerId={customer.id} version={customer.version} />
    </Panel>
  )
}

/** Every change to the customer and what is theirs: who, when, from what to what. */
function ChangeLog({ customerId, version }: { customerId: string; version: number }) {
  const changes = useLoad(() => api<RecordChange[]>(`/customers/${customerId}/changes?limit=50`), [customerId, version])
  const shown = (v?: string | null) => (v === null || v === undefined || v === '' ? <span className="muted">—</span> : <Mono>{v}</Mono>)
  return (
    <>
      <h3 className="sub">Change log</h3>
      <ErrorNote error={changes.error} />
      {changes.data?.length === 0 && <Empty>No changes since the customer was created.</Empty>}
      {!!changes.data?.length && (
        <table>
          <thead><tr><th>When</th><th>Who</th><th>Record</th><th>Field</th><th>From</th><th>To</th></tr></thead>
          <tbody>
            {changes.data.map((c) => (
              <tr key={c.id}>
                <td className="nowrap">{dateTime(c.changedAt)}</td>
                <td><Mono>{c.changedBy}</Mono></td>
                <td>{c.entityType}</td>
                <td>{c.field}</td>
                <td>{shown(c.oldValue)}</td>
                <td>{shown(c.newValue)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

/**
 * KYC submissions waiting for review (production, until an identity vendor is integrated): a
 * maker checks the documents and inputs a decision; a checker authorises it.
 */
function KycQueue({ onDecided }: { onDecided: () => void }) {
  const pending = useLoad(() => api<KycReview[]>('/ops/kyc/pending'))
  const [version, setVersion] = useState(0)
  const [notes, setNotes] = useState<Record<string, string>>({})
  const decide = useAction((r: KycReview, approve: boolean) => api(`/ops/customers/${r.customerId}/kyc-decision`, {
    method: 'POST', body: approve ? { approve, reference: notes[r.id] ?? '' } : { approve, reason: notes[r.id] ?? '' },
  }))
  if (!pending.data?.length && !pending.error) return <PendingChanges types={['KycDecision', 'CustomerDetails']} version={version} title="KYC decisions and identity changes awaiting authorisation" onDecided={() => { pending.reload(); onDecided() }} />
  return (
    <>
      <Panel title="KYC review" aside={<span className="muted">{pending.data?.length ?? 0} waiting</span>}>
        <ErrorNote error={pending.error ?? decide.error} />
        {pending.data?.map((r) => (
          <div key={r.id} className="approval">
            <div>
              <strong>{r.legalName}</strong> <span className="muted">({r.type} · {r.email})</span>
              <p className="muted">
                {r.documentType} <Mono>{r.documentNumber}</Mono>
                {r.dateOfBirth && <> · born {r.dateOfBirth}</>}
                {r.registrationNumber && <> · reg. <Mono>{r.registrationNumber}</Mono></>}
                {r.directors.length > 0 && <> · directors {r.directors.join(', ')}</>}
              </p>
              <p className="muted">Submitted {dateTime(r.submittedAt)}</p>
              <input placeholder="What you checked (approve) or why not (reject)" value={notes[r.id] ?? ''} onChange={(e) => setNotes({ ...notes, [r.id]: e.target.value })} />
            </div>
            <div className="actions">
              <Button variant="accent" busy={decide.busy} onClick={async () => { if (await decide.run(r, true)) setVersion((v) => v + 1) }}>Input approval</Button>
              <Button variant="ghost" busy={decide.busy} onClick={async () => { if (await decide.run(r, false)) setVersion((v) => v + 1) }}>Input rejection</Button>
            </div>
          </div>
        ))}
      </Panel>
      <PendingChanges types={['KycDecision', 'CustomerDetails']} version={version} title="KYC decisions and identity changes awaiting authorisation" onDecided={() => { pending.reload(); onDecided() }} />
    </>
  )
}
