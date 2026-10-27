import { useState } from 'react'
import { api, getSession } from '../api'
import { dateTime, money, toMinor } from '../format'
import { useAction, useLoad, type Loadable } from '../hooks'
import type { Adjustment, ChargeType, CurrencyRate, InterestCondition, LedgerAccount, ParameterChange, SuspenseType } from '../types'
import { Decision } from '../pending'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel } from '../ui'

export default function Controls() {
  const pending = useLoad(() => api<ParameterChange[]>('/ops/parameters/pending'))
  const adjustments = useLoad(() => api<Adjustment[]>('/ops/adjustments'))

  return (
    <>
      <PageHeader module="EB · MAKER-CHECKER" title="Parameters & manual adjustments" />
      <p className="muted intro">
        Nothing here takes effect on input. A record is created as <strong>INAU</strong> (unauthorised) and must be authorised by a
        <em> different</em> operator with the checker permission — you are signed in as <strong>{getSession()?.name}</strong>
        ({getSession()?.permissions.join(', ') || 'enquiry only'}). Account restrictions, tier overrides, blocklist entries, chargebacks and
        settlements wait here too.
      </p>
      <div className="grid-2">
        <Charges onProposed={pending.reload} />
        <PendingChanges pending={pending} />
        <Rates onProposed={pending.reload} />
        <InputAdjustment onInput={adjustments.reload} />
        <AdjustmentQueue adjustments={adjustments} />
      </div>
    </>
  )
}

function Charges({ onProposed }: { onProposed: () => void }) {
  const charges = useLoad(() => api<ChargeType[]>('/ops/parameters/charges'))
  const interest = useLoad(() => api<InterestCondition[]>('/ops/parameters/interest'))
  const [edit, setEdit] = useState<{ code: string; description: string; bps: string }>()
  const propose = useAction(() => api(`/ops/parameters/charges/${edit!.code}`, { method: 'POST', body: { description: edit!.description, basisPoints: Number(edit!.bps) } }))

  return (
    <Panel title="Parameters" aside={<span className="muted">FT.COMMISSION.TYPE · GROUP.CREDIT.INT</span>}>
      <table>
        <thead><tr><th>Charge</th><th>Description</th><th className="num">Rate</th><th>CURR.NO</th><th /></tr></thead>
        <tbody>
          {charges.data?.map((c) => (
            <tr key={c.id}>
              <td><Mono>{c.code}</Mono></td>
              <td>{c.description}</td>
              <td className="num">{(c.basisPoints / 100).toFixed(2)}%</td>
              <td className="muted">{c.version}</td>
              <td><button className="link-btn" onClick={() => setEdit({ code: c.code, description: c.description, bps: String(c.basisPoints) })}>Amend</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      {edit && (
        <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await propose.run()) { setEdit(undefined); onProposed() } }}>
          <span className="mono-label">AMEND {edit.code}</span>
          <div className="row-2">
            <Field label="Description"><input value={edit.description} onChange={(e) => setEdit({ ...edit, description: e.target.value })} /></Field>
            <Field label="Basis points"><input value={edit.bps} onChange={(e) => setEdit({ ...edit, bps: e.target.value })} /></Field>
          </div>
          <ErrorNote error={propose.error} />
          <Button busy={propose.busy}>Input change (INAU)</Button>
        </form>
      )}
      <h3 className="sub">Credit interest conditions</h3>
      <table>
        <thead><tr><th>Category</th><th>Currency</th><th className="num">Annual rate</th></tr></thead>
        <tbody>
          {interest.data?.map((i) => (
            <tr key={i.id}><td>{i.category === 6001 ? '6001 Mobile wallet' : i.category === 1001 ? '1001 Merchant current' : i.category}</td><td><Mono>{i.currency}</Mono></td><td className="num">{(i.annualRate * 100).toFixed(2)}%</td></tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

function PendingChanges({ pending }: { pending: Loadable<ParameterChange[]> }) {
  const decide = useAction((id: string, ok: boolean) => api(`/ops/parameters/pending/${id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not approved' } }))
  const act = async (id: string, ok: boolean) => {
    if (await decide.run(id, ok)) pending.reload()
  }
  return (
    <Panel title="Unauthorised parameter changes">
      <ErrorNote error={decide.error} />
      {pending.data?.length === 0 && <Empty>Nothing waiting for authorisation.</Empty>}
      {pending.data?.map((p) => (
        <div key={p.id} className="approval">
          <div>
            <Badge value={p.status} /> <Mono>{p.type} · {p.key}</Mono>
            <p className="muted">Input by <strong>{p.inputter}</strong> · {dateTime(p.inputAt)}</p>
            <code className="payload">{p.payload}</code>
          </div>
          <Decision inputter={p.inputter}>
            <Button variant="accent" busy={decide.busy} onClick={() => void act(p.id, true)}>Authorise</Button>
            <Button variant="ghost" busy={decide.busy} onClick={() => void act(p.id, false)}>Reject</Button>
          </Decision>
        </div>
      ))}
    </Panel>
  )
}

function InputAdjustment({ onInput }: { onInput: () => void }) {
  const accounts = useLoad(() => api<LedgerAccount[]>('/ops/gl/internal-accounts'))
  const suspense = useLoad(() => api<SuspenseType[]>('/ops/suspense/types'))
  // Suspense accounts are worked item by item (raise / clear), never adjusted directly.
  const suspenseCategories = new Set(suspense.data?.map((t) => t.category))
  const kes = accounts.data?.filter((a) => a.currency === 'KES' && !suspenseCategories.has(a.category)) ?? []
  const [form, setForm] = useState({ debit: '', credit: '', amount: '50', narrative: 'Correct a mis-posted bank transfer' })
  const input = useAction(() =>
    api('/ops/adjustments', {
      method: 'POST',
      body: {
        currency: 'KES',
        narrative: form.narrative,
        lines: [
          { accountNumber: form.debit, direction: 'Debit', amount: toMinor(form.amount, 'KES') },
          { accountNumber: form.credit, direction: 'Credit', amount: toMinor(form.amount, 'KES') },
        ],
      },
    }),
  )

  const options = kes.map((a) => <option key={a.id} value={a.accountNumber}>{a.accountNumber} — {a.name}</option>)
  return (
    <Panel title="Manual GL adjustment" aside={<span className="muted">Internal & P&L accounts — suspense is worked under Suspense</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await input.run()) onInput() }}>
        <Field label="Debit"><select required value={form.debit} onChange={(e) => setForm({ ...form, debit: e.target.value })}><option value="">Choose account</option>{options}</select></Field>
        <Field label="Credit"><select required value={form.credit} onChange={(e) => setForm({ ...form, credit: e.target.value })}><option value="">Choose account</option>{options}</select></Field>
        <div className="row-2">
          <Field label="Amount (KES)"><input value={form.amount} onChange={(e) => setForm({ ...form, amount: e.target.value })} /></Field>
          <Field label="Narrative"><input value={form.narrative} onChange={(e) => setForm({ ...form, narrative: e.target.value })} /></Field>
        </div>
        <ErrorNote error={input.error} />
        <Button busy={input.busy}>Input adjustment (INAU)</Button>
      </form>
    </Panel>
  )
}

function AdjustmentQueue({ adjustments }: { adjustments: Loadable<Adjustment[]> }) {
  const decide = useAction((id: string, ok: boolean) => api(`/ops/adjustments/${id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not supported by evidence' } }))
  const act = async (id: string, ok: boolean) => {
    if (await decide.run(id, ok)) adjustments.reload()
  }
  return (
    <Panel title="Adjustments">
      <ErrorNote error={decide.error} />
      {adjustments.data?.length === 0 && <Empty>No adjustments.</Empty>}
      {adjustments.data?.map((a) => (
        <div key={a.id} className="approval">
          <div>
            <Badge value={a.status} /> {a.narrative}
            <p className="muted">
              {a.lines.map((l) => `${l.direction === 'Debit' ? 'Dr' : 'Cr'} ${l.accountNumber}`).join(' / ')} · {money(a.lines[0]?.amount ?? 0, a.currency)}
            </p>
            <p className="muted">Inputter <strong>{a.inputter}</strong>{a.authoriser && <> · {a.status === 'Rejected' ? 'rejected' : 'authorised'} by <strong>{a.authoriser}</strong></>}</p>
          </div>
          {a.status === 'Unauthorised' && (
            <Decision inputter={a.inputter}>
              <Button variant="accent" busy={decide.busy} onClick={() => void act(a.id, true)}>Authorise</Button>
              <Button variant="ghost" busy={decide.busy} onClick={() => void act(a.id, false)}>Reject</Button>
            </Decision>
          )}
        </div>
      ))}
    </Panel>
  )
}

/** Treasury's mid rates (T24 CURRENCY): input under maker-checker; FX is refused on stale ones. */
function Rates({ onProposed }: { onProposed: () => void }) {
  const rates = useLoad(() => api<CurrencyRate[]>('/ops/fx/rates'))
  const [edit, setEdit] = useState<Record<string, string>>()
  const propose = useAction(() => api('/ops/fx/rates', { method: 'POST', body: { rates: Object.fromEntries(Object.entries(edit ?? {}).filter(([, v]) => v.trim() !== '')) } }))
  if (rates.error) return null // the sandbox uses simulated rates
  return (
    <Panel title="FX rates" aside={<span className="muted">units per USD · CURRENCY</span>}>
      <table>
        <thead><tr><th>Currency</th><th className="num">Per USD</th><th>As of</th><th /></tr></thead>
        <tbody>
          {rates.data?.map((r) => (
            <tr key={r.currency}>
              <td><Mono>{r.currency}</Mono></td>
              <td className="num">{r.perUsd}</td>
              <td>{r.ageMinutes > 60 * 24 * 365 ? 'never set' : dateTime(r.asOf)}</td>
              <td>{r.stale ? <Badge value="Stale" /> : <Badge value="Current" />}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!edit && <Button variant="ghost" onClick={() => setEdit(Object.fromEntries((rates.data ?? []).map((r) => [r.currency, String(r.perUsd)])))}>Input today's rates</Button>}
      {edit && (
        <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await propose.run()) { setEdit(undefined); onProposed() } }}>
          <span className="mono-label">MID RATES — UNITS PER US DOLLAR</span>
          <div className="row-3">
            {Object.entries(edit).map(([ccy, v]) => (
              <Field key={ccy} label={ccy}><input disabled={ccy === 'USD'} value={v} onChange={(e) => setEdit({ ...edit, [ccy]: e.target.value })} /></Field>
            ))}
          </div>
          <ErrorNote error={propose.error} />
          <Button busy={propose.busy}>Input rates (INAU)</Button>
        </form>
      )}
    </Panel>
  )
}
