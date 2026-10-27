import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, getSession } from '../api'
import { amount, dateTime, day, money, shortDay, toMinor } from '../format'
import { useAction, useLoad, type Loadable } from '../hooks'
import { CheckerNote, Decision } from '../pending'
import type {
  AgeingRow, HookRun, LedgerAccount, SuspenseClearing, SuspenseDrift, SuspenseItem, SuspenseItemDetail, SuspenseType,
} from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel, Stat } from '../ui'

const buckets = ['0-7', '8-30', '31-90', '90+']

export default function Suspense() {
  const types = useLoad(() => api<SuspenseType[]>('/ops/suspense/types'))
  const ageing = useLoad(() => api<AgeingRow[]>('/ops/suspense/ageing'))
  const drift = useLoad(() => api<SuspenseDrift[]>('/ops/suspense/integrity'))
  const [filter, setFilter] = useState({ type: '', status: 'open', bucket: '', mine: false })
  const items = useLoad(() => {
    const q = new URLSearchParams()
    if (filter.type) q.set('type', filter.type)
    if (filter.status === 'all') q.set('open', 'false')
    else if (filter.status !== 'open') q.set('status', filter.status)
    if (filter.bucket) q.set('bucket', filter.bucket)
    if (filter.mine) q.set('assignedTo', operatorLogin())
    return api<SuspenseItem[]>(`/ops/suspense/items?${q}`)
  }, [filter.type, filter.status, filter.bucket, filter.mine])
  const clearings = useLoad(() => api<SuspenseClearing[]>('/ops/suspense/clearings?status=Unauthorised'))
  const [selected, setSelected] = useState<string>()
  const [raising, setRaising] = useState(false)
  const runHooks = useAction(() => api<HookRun>('/ops/suspense/run-hooks', { method: 'POST' }))

  const refresh = () => {
    items.reload()
    ageing.reload()
    drift.reload()
    clearings.reload()
  }

  const totals = useMemo(() => {
    const rows = ageing.data ?? []
    return {
      items: rows.reduce((n, r) => n + r.openItems, 0),
      escalated: rows.reduce((n, r) => n + r.escalated, 0),
      warning: rows.reduce((n, r) => n + r.overWarning, 0),
      accounts: rows.filter((r) => r.openItems > 0).length,
    }
  }, [ageing.data])

  return (
    <>
      <PageHeader module="AC · SUSPENSE" title="Suspense accounts">
        <Link className="btn btn-ghost" to="/console/suspense/types">Suspense types</Link>
        <Button variant="ghost" busy={runHooks.busy} onClick={async () => { if (await runHooks.run()) refresh() }}>Run extensions now</Button>
        <Button onClick={() => { setRaising(true); setSelected(undefined) }}>Raise item</Button>
      </PageHeader>
      <p className="muted intro">
        Every amount in suspense is an item: who parked it, when, why, and where it goes. A suspense account's balance is always the sum of
        its open items — Close of Business halts if they ever disagree. Clearings are input by an operator, an extension or an ageing rule and
        post only when a <em>different</em> party authorises them.
      </p>
      {runHooks.result && (
        <div className="callout">
          <p>Extensions classified <strong>{runHooks.result.classified}</strong> item(s) and proposed <strong>{runHooks.result.proposed}</strong> clearing(s);
            {' '}<strong>{runHooks.result.autoCleared}</strong> cleared straight through, {runHooks.result.rejected} rejected by the core.</p>
        </div>
      )}
      <ErrorNote error={runHooks.error} />

      <div className="stat-grid">
        <Stat label="Open items" value={totals.items} hint={`across ${totals.accounts} suspense account(s)`} />
        <Stat label="Past warning age" value={totals.warning} tone={totals.warning ? 'warn' : undefined} hint="per type's warning threshold" />
        <Stat label="Escalated" value={totals.escalated} tone={totals.escalated ? 'bad' : 'good'} hint="past escalation; HaltCob types stop COB" />
        <Stat label="Sub-ledger integrity" value={drift.data ? (drift.data.length ? `${drift.data.length} drift` : 'Agrees') : '—'}
          tone={drift.data?.length ? 'bad' : 'good'} hint="balances = sum of open items" />
      </div>

      <AgeingPanel ageing={ageing} onPick={(type) => setFilter({ ...filter, type })} />

      <div className="grid-main-side">
        <Panel title="Items" aside={
          <span className="inline-form">
            <select value={filter.type} onChange={(e) => setFilter({ ...filter, type: e.target.value })}>
              <option value="">All types</option>
              {types.data?.map((t) => <option key={t.code} value={t.code}>{t.code}</option>)}
            </select>
            <select value={filter.status} onChange={(e) => setFilter({ ...filter, status: e.target.value })}>
              <option value="open">In suspense</option>
              <option value="Unauthorised">Awaiting authorisation</option>
              <option value="Cleared">Cleared</option>
              <option value="WrittenOff">Written off</option>
              <option value="all">All</option>
            </select>
            <select value={filter.bucket} onChange={(e) => setFilter({ ...filter, bucket: e.target.value })}>
              <option value="">Any age</option>
              {buckets.map((b) => <option key={b} value={b}>{b} days</option>)}
            </select>
            <label className="check"><input type="checkbox" checked={filter.mine} onChange={(e) => setFilter({ ...filter, mine: e.target.checked })} /> Mine</label>
          </span>
        }>
          <ErrorNote error={items.error} />
          {items.data?.length === 0 && <Empty>No items match.</Empty>}
          {!!items.data?.length && (
            <table>
              <thead><tr><th>Type</th><th>Opened</th><th>Reference</th><th>Narrative</th><th className="num">Open</th><th>Age</th><th>Owner</th><th>Status</th></tr></thead>
              <tbody>
                {items.data.map((i) => (
                  <tr key={i.id} className={`clickable ${selected === i.id ? 'selected' : ''}`} onClick={() => { setSelected(i.id); setRaising(false) }}>
                    <td><Mono>{i.typeCode}</Mono></td>
                    <td className="nowrap">{shortDay(i.openedOn)}</td>
                    <td><Mono>{i.reference || '—'}</Mono></td>
                    <td>{i.narrative}</td>
                    <td className="num">{money(i.openAmount, i.currency)}</td>
                    <td className="nowrap">{i.ageDays}d {i.escalated && <Badge value="Escalated" />}</td>
                    <td>{i.assignedTo ?? <span className="muted">—</span>}</td>
                    <td><Badge value={i.pendingClearing ? 'PendingClearing' : i.status} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <div className="stack">
          {raising && <RaiseItem types={types.data ?? []} onDone={() => { setRaising(false); refresh() }} />}
          {selected && !raising && <ItemDrawer id={selected} onChange={refresh} />}
          {!selected && !raising && <Panel><Empty>Select an item to work it, or raise a new one.</Empty></Panel>}
          <ClearingQueue clearings={clearings} onChange={refresh} />
          <RaisedQueue onChange={refresh} />
        </div>
      </div>
    </>
  )
}

/** The signed-in operator's login, as the API records it. */
const operatorLogin = () => getSession()?.username ?? ''

function AgeingPanel({ ageing, onPick }: { ageing: Loadable<AgeingRow[]>; onPick: (type: string) => void }) {
  return (
    <Panel title="Ageing" aside={<span className="muted">open amount by days in suspense</span>}>
      <ErrorNote error={ageing.error} />
      {ageing.data?.length === 0 && <Empty>Every suspense account is empty.</Empty>}
      {!!ageing.data?.length && (
        <table>
          <thead>
            <tr><th>Type</th><th>Account</th><th>Side</th>{buckets.map((b) => <th key={b} className="num">{b}</th>)}<th className="num">Balance</th><th className="num">Items</th><th>Oldest</th><th>COB</th></tr>
          </thead>
          <tbody>
            {ageing.data.map((r) => (
              <tr key={r.accountNumber} className="clickable" onClick={() => onPick(r.type)}>
                <td><Mono>{r.type}</Mono> <span className="muted">{r.name}</span></td>
                <td><Mono>{r.accountNumber}</Mono></td>
                <td>{r.side}</td>
                {buckets.map((b) => <td key={b} className={`num ${b !== '0-7' && r.buckets[b] ? 'error-text' : ''}`}>{r.buckets[b] ? amount(r.buckets[b], r.currency) : '—'}</td>)}
                <td className={`num ${r.balance !== r.openAmount ? 'error-text' : ''}`}>{money(r.balance, r.currency)}</td>
                <td className="num">{r.openItems}</td>
                <td>{r.oldestDays}d {r.escalated > 0 && <Badge value="Escalated" />}</td>
                <td><Badge value={r.cobPolicy === 'HaltCob' ? 'Halts COB' : 'Warn'} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Panel>
  )
}

function useAccounts() {
  return useLoad(() => api<LedgerAccount[]>('/ops/gl/internal-accounts'))
}

function RaiseItem({ types, onDone }: { types: SuspenseType[]; onDone: () => void }) {
  const accounts = useAccounts()
  const active = types.filter((t) => t.active)
  const [form, setForm] = useState({ type: '', currency: 'KES', amount: '', contra: '', reference: '', narrative: '' })
  const [attrs, setAttrs] = useState<Record<string, string>>({})
  const type = active.find((t) => t.code === form.type)
  const suspenseCategories = new Set(types.map((t) => t.category))
  const contras = (accounts.data ?? []).filter((a) => a.currency === form.currency && !suspenseCategories.has(a.category))
  const raise = useAction(() =>
    api<SuspenseItem>('/ops/suspense/items', {
      method: 'POST',
      body: { type: form.type, currency: form.currency, amount: toMinor(form.amount, form.currency), contraAccountNumber: form.contra,
        reference: form.reference, narrative: form.narrative, attributes: attrs },
    }),
  )
  return (
    <Panel title="Raise a suspense item" aside={<span className="muted">INAU until authorised</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await raise.run()) onDone() }}>
        <div className="row-2">
          <Field label="Suspense type">
            <select required value={form.type} onChange={(e) => {
              const t = active.find((x) => x.code === e.target.value)
              setForm({ ...form, type: e.target.value, currency: t?.currencies.includes(form.currency) ? form.currency : t?.currencies[0] ?? 'KES' })
              setAttrs({})
            }}>
              <option value="">Choose type</option>
              {active.map((t) => <option key={t.code} value={t.code}>{t.code} — {t.name}</option>)}
            </select>
          </Field>
          <Field label="Currency">
            <select value={form.currency} onChange={(e) => setForm({ ...form, currency: e.target.value, contra: '' })}>
              {(type?.currencies ?? ['KES']).map((c) => <option key={c}>{c}</option>)}
            </select>
          </Field>
        </div>
        <Field label="Contra account" hint={type ? `${type.side === 'Debit' ? 'Credited' : 'Debited'} — internal or P&L, never a customer` : undefined}>
          <select required value={form.contra} onChange={(e) => setForm({ ...form, contra: e.target.value })}>
            <option value="">Choose account</option>
            {contras.map((a) => <option key={a.id} value={a.accountNumber}>{a.accountNumber} — {a.name}</option>)}
          </select>
        </Field>
        <div className="row-2">
          <Field label={`Amount (${form.currency})`}><input required value={form.amount} onChange={(e) => setForm({ ...form, amount: e.target.value })} /></Field>
          <Field label="Reference"><input value={form.reference} onChange={(e) => setForm({ ...form, reference: e.target.value })} /></Field>
        </div>
        <Field label="Narrative"><input required value={form.narrative} onChange={(e) => setForm({ ...form, narrative: e.target.value })} /></Field>
        {type && type.localFields.length > 0 && <LocalFieldInputs type={type} values={attrs} onChange={setAttrs} />}
        <ErrorNote error={raise.error} />
        <div className="actions">
          <Button variant="ghost" type="button" onClick={onDone}>Cancel</Button>
          <Button busy={raise.busy}>Input item (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}

function LocalFieldInputs({ type, values, onChange }: { type: SuspenseType; values: Record<string, string>; onChange: (v: Record<string, string>) => void }) {
  return (
    <div className="row-2">
      {type.localFields.map((f) => (
        <Field key={f.name} label={`${f.label}${f.required ? ' *' : ''}`} hint={f.pattern ? `format ${f.pattern}` : undefined}>
          {f.type === 'choice' ? (
            <select value={values[f.name] ?? ''} onChange={(e) => onChange({ ...values, [f.name]: e.target.value })}>
              <option value="">—</option>
              {f.options?.map((o) => <option key={o}>{o}</option>)}
            </select>
          ) : (
            <input type={f.type === 'date' ? 'date' : 'text'} inputMode={f.type === 'number' ? 'decimal' : undefined}
              value={values[f.name] ?? ''} onChange={(e) => onChange({ ...values, [f.name]: e.target.value })} />
          )}
        </Field>
      ))}
    </div>
  )
}

function ItemDrawer({ id, onChange }: { id: string; onChange: () => void }) {
  const detail = useLoad(() => api<SuspenseItemDetail>(`/ops/suspense/items/${id}`), [id])
  const d = detail.data
  const changed = () => {
    detail.reload()
    onChange()
  }
  if (detail.error) return <Panel><ErrorNote error={detail.error} /></Panel>
  if (!d) return <Panel><Empty>Loading item…</Empty></Panel>
  const { item, type } = d
  const open = item.status === 'Open' || item.status === 'PartiallyCleared'
  return (
    <Panel title={`${item.typeCode} · ${money(item.openAmount, item.currency)} open`} aside={<Badge value={item.status} />}>
      <dl className="facts">
        <div><dt>Account</dt><dd><Mono>{item.accountNumber}</Mono></dd></div>
        <div><dt>Side</dt><dd>{item.side} suspense</dd></div>
        <div><dt>Original</dt><dd>{money(item.amount, item.currency)}</dd></div>
        <div><dt>Opened</dt><dd>{day(item.openedOn)}</dd></div>
        <div><dt>Age</dt><dd>{item.ageDays} days {item.escalated && <Badge value="Escalated" />}</dd></div>
        <div><dt>Source</dt><dd>{item.sourceType}{item.sourceType === 'deposit' && <> · <Link to="/console/operations">deposit</Link></>}</dd></div>
        <div><dt>Reference</dt><dd><Mono>{item.reference || '—'}</Mono></dd></div>
        <div><dt>Input by</dt><dd>{item.inputter}{item.authoriser && <> · auth {item.authoriser}</>}</dd></div>
        <div><dt>Transaction</dt><dd><Mono>{d.entry?.transactionReference ?? '—'}</Mono></dd></div>
      </dl>
      <p>{item.narrative}</p>

      {item.status === 'Unauthorised' && <RaiseDecision item={item} onDone={changed} />}
      {item.status !== 'Unauthorised' && <Assign key={`${item.id}:${item.version}`} item={item} onDone={changed} />}
      {type.localFields.length > 0 && <AttributesEditor key={`${item.id}:${item.version}`} item={item} type={type} editable={open} onDone={changed} />}
      {open && !item.pendingClearing && <ClearingForm item={item} type={type} onDone={changed} />}
      {item.pendingClearing && <p className="callout">A clearing for this item is awaiting authorisation (see the queue).</p>}

      <h3 className="sub">Clearings</h3>
      {d.clearings.length === 0 && <Empty>None yet.</Empty>}
      {d.clearings.map((c) => (
        <div key={c.id} className="approval">
          <div>
            <Badge value={c.status} /> {c.kind === 'WriteOff' ? 'Write-off' : 'Clear'} {money(c.amount, c.currency)} → <Mono>{c.targetAccountNumber}</Mono>
            <p className="muted">{c.narrative} · input by <strong>{c.inputter}</strong>{c.confidence != null && ` (${c.confidence}% confident)`}
              {c.authoriser && <> · {c.status === 'Rejected' ? 'rejected' : 'authorised'} by <strong>{c.authoriser}</strong></>}</p>
          </div>
        </div>
      ))}
      {d.related.length > 0 && (
        <>
          <h3 className="sub">Reclassified</h3>
          {d.related.map((r) => <p key={r.id}><Mono>{r.typeCode}</Mono> {money(r.amount, r.currency)} · <Badge value={r.status} /></p>)}
        </>
      )}
      {d.extensionCalls.length > 0 && (
        <>
          <h3 className="sub">Extension calls</h3>
          <ul className="audit-feed">
            {d.extensionCalls.slice(0, 8).map((c) => (
              <li key={c.id}><Mono>{c.extension}</Mono><span>{c.hook} · {c.latencyMs} ms{c.error && <span className="error-text"> · {c.error}</span>}</span><Badge value={c.outcome === 'ok' ? 'Succeeded' : 'Failed'} /></li>
            ))}
          </ul>
        </>
      )}
      {d.events.length > 0 && (
        <>
          <h3 className="sub">Module events</h3>
          <ul className="audit-feed">
            {d.events.map((e) => (
              <li key={e.id}><Badge value={e.status === 'Delivered' ? 'Succeeded' : e.status} /><span>attempt {e.attempts}{e.outcome && <> · {e.outcome}</>}{e.extension && <> by <Mono>{e.extension}</Mono></>}{e.lastError && <span className="error-text"> · {e.lastError}</span>}</span><span className="muted nowrap">{dateTime(e.createdAt)}</span></li>
            ))}
          </ul>
        </>
      )}
      {d.history.length > 0 && <p className="muted">{d.history.length} earlier version(s) in record history ($HIS).</p>}
    </Panel>
  )
}

function RaiseDecision({ item, onDone }: { item: SuspenseItem; onDone: () => void }) {
  const decide = useAction((ok: boolean) => api(`/ops/suspense/items/${item.id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not supported by evidence' } }))
  return (
    <div className="callout warn">
      <p>Raised by <strong>{item.inputter}</strong> against <Mono>{item.contraAccountNumber}</Mono>. Nothing is posted until a different operator authorises.</p>
      <ErrorNote error={decide.error} />
      <div className="actions">
        <Button variant="accent" busy={decide.busy} onClick={async () => { if (await decide.run(true)) onDone() }}>Authorise & post</Button>
        <Button variant="ghost" busy={decide.busy} onClick={async () => { if (await decide.run(false)) onDone() }}>Reject</Button>
      </div>
    </div>
  )
}

function Assign({ item, onDone }: { item: SuspenseItem; onDone: () => void }) {
  const [owner, setOwner] = useState(item.assignedTo ?? '')
  const assign = useAction(() => api(`/ops/suspense/items/${item.id}/assign`, { method: 'POST', body: { assignedTo: owner || null } }))
  return (
    <form className="inline-form" onSubmit={async (e) => { e.preventDefault(); if (await assign.run()) onDone() }}>
      <span className="mono-label">Owner</span>
      <input placeholder="Unassigned" value={owner} onChange={(e) => setOwner(e.target.value)} />
      <Button variant="ghost" busy={assign.busy}>Assign</Button>
      <button type="button" className="link-btn" onClick={() => setOwner(operatorLogin())}>me</button>
      <ErrorNote error={assign.error} />
    </form>
  )
}

function AttributesEditor({ item, type, editable, onDone }: { item: SuspenseItem; type: SuspenseType; editable: boolean; onDone: () => void }) {
  const [values, setValues] = useState(item.attributes ?? {})
  const save = useAction(() => api(`/ops/suspense/items/${item.id}/attributes`, { method: 'PUT', body: { attributes: values } }))
  return (
    <>
      <h3 className="sub">Local fields</h3>
      {editable ? (
        <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await save.run()) onDone() }}>
          <LocalFieldInputs type={type} values={values} onChange={setValues} />
          <ErrorNote error={save.error} />
          <div className="actions"><Button variant="ghost" busy={save.busy}>Save fields</Button></div>
        </form>
      ) : (
        <dl className="facts">
          {type.localFields.map((f) => <div key={f.name}><dt>{f.label}</dt><dd>{item.attributes?.[f.name] || '—'}</dd></div>)}
        </dl>
      )}
    </>
  )
}

function ClearingForm({ item, type, onDone }: { item: SuspenseItem; type: SuspenseType; onDone: () => void }) {
  const accounts = useAccounts()
  const managed = item.sourceType === 'deposit'
  const [form, setForm] = useState({ kind: 'Clear', target: '', amount: amount(item.openAmount, item.currency).replace(/,/g, ''), narrative: '' })
  const allowsCustomer = type.allowedTargets.includes('Customer')
  const targets = (accounts.data ?? []).filter((a) => {
    if (a.currency !== item.currency || a.accountNumber === item.accountNumber) return false
    const pnl = a.category >= 50000 && a.category <= 69999
    return pnl ? type.allowedTargets.includes('PnL') : type.allowedTargets.includes('Internal')
  })
  const propose = useAction(() =>
    api<SuspenseClearing>(`/ops/suspense/items/${item.id}/clearings`, {
      method: 'POST',
      body: { kind: form.kind, targetAccountNumber: form.target, amount: toMinor(form.amount, item.currency), narrative: form.narrative },
    }),
  )
  return (
    <>
      <h3 className="sub">Clear</h3>
      {managed && <p className="muted">An unapplied deposit is applied whole to the payer's customer account; the deposit and this item move together.</p>}
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await propose.run()) onDone() }}>
        {!managed && type.writeOffCategory > 0 && (
          <div className="segmented">
            {['Clear', 'WriteOff'].map((k) => <button type="button" key={k} className={form.kind === k ? 'on' : ''} onClick={() => setForm({ ...form, kind: k })}>{k === 'WriteOff' ? 'Write off' : 'Clear'}</button>)}
          </div>
        )}
        {form.kind === 'Clear' && (
          <Field label="Target account" hint={`Allowed: ${type.allowedTargets.join(', ')}${allowsCustomer ? ' — type a customer account number' : ''}`}>
            <input list={`targets-${item.id}`} required value={form.target} onChange={(e) => setForm({ ...form, target: e.target.value.trim() })} placeholder="Account number" />
            <datalist id={`targets-${item.id}`}>
              {targets.map((a) => <option key={a.id} value={a.accountNumber}>{a.name}</option>)}
            </datalist>
          </Field>
        )}
        {form.kind === 'WriteOff' && <p className="muted">Written off to P&L category {type.writeOffCategory}.</p>}
        <div className="row-2">
          <Field label={`Amount (${item.currency})`} hint={managed ? 'Deposits clear whole' : 'Partial clearing allowed'}>
            <input required value={form.amount} readOnly={managed} onChange={(e) => setForm({ ...form, amount: e.target.value })} />
          </Field>
          <Field label="Narrative"><input required value={form.narrative} onChange={(e) => setForm({ ...form, narrative: e.target.value })} /></Field>
        </div>
        <ErrorNote error={propose.error} />
        <Button busy={propose.busy}>Input clearing (INAU)</Button>
      </form>
    </>
  )
}

function ClearingQueue({ clearings, onChange }: { clearings: Loadable<SuspenseClearing[]>; onChange: () => void }) {
  const decide = useAction((id: string, ok: boolean) => api(`/ops/suspense/clearings/${id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not supported by evidence' } }))
  const act = async (id: string, ok: boolean) => {
    if (await decide.run(id, ok)) onChange()
  }
  return (
    <Panel title="Clearings awaiting authorisation" aside={<CheckerNote />}>
      <ErrorNote error={decide.error} />
      {clearings.data?.length === 0 && <Empty>Nothing waiting.</Empty>}
      {clearings.data?.map((c) => (
        <div key={c.id} className="approval">
          <div>
            <Mono>{c.typeCode}</Mono> {c.kind === 'WriteOff' ? 'write-off' : 'clear'} <strong>{money(c.amount, c.currency)}</strong> → <Mono>{c.targetAccountNumber}</Mono>
            <p className="muted">{c.narrative}</p>
            <p className="muted">Input by <strong>{c.inputter}</strong>{c.confidence != null && ` · ${c.confidence}% confident`} · {dateTime(c.inputAt)}</p>
          </div>
          <Decision inputter={c.inputter}>
            <Button variant="accent" busy={decide.busy} onClick={() => void act(c.id, true)}>Authorise</Button>
            <Button variant="ghost" busy={decide.busy} onClick={() => void act(c.id, false)}>Reject</Button>
          </Decision>
        </div>
      ))}
    </Panel>
  )
}

function RaisedQueue({ onChange }: { onChange: () => void }) {
  const raised = useLoad(() => api<SuspenseItem[]>('/ops/suspense/items?status=Unauthorised'))
  const decide = useAction((id: string, ok: boolean) => api(`/ops/suspense/items/${id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not supported by evidence' } }))
  if (!raised.data?.length) return null
  const act = async (id: string, ok: boolean) => {
    if (await decide.run(id, ok)) {
      raised.reload()
      onChange()
    }
  }
  return (
    <Panel title="Raised items awaiting authorisation">
      <ErrorNote error={decide.error} />
      {raised.data.map((i) => (
        <div key={i.id} className="approval">
          <div>
            <Mono>{i.typeCode}</Mono> <strong>{money(i.amount, i.currency)}</strong> against <Mono>{i.contraAccountNumber}</Mono>
            <p className="muted">{i.narrative} · input by <strong>{i.inputter}</strong></p>
          </div>
          <Decision inputter={i.inputter}>
            <Button variant="accent" busy={decide.busy} onClick={() => void act(i.id, true)}>Authorise</Button>
            <Button variant="ghost" busy={decide.busy} onClick={() => void act(i.id, false)}>Reject</Button>
          </Decision>
        </div>
      ))}
    </Panel>
  )
}
