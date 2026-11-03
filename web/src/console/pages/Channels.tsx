import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api, getSession } from '../api'
import { dateTime, money, shortDay, toMinor } from '../format'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { Account, Channel, ChannelSpec, ChannelTransfer, ChannelView, Extension, LocalField, ProviderCapabilities, SuspenseEvent, TransactionView } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel, Stat } from '../ui'

const currencies = ['KES', 'UGX', 'TZS', 'NGN', 'USD', 'EUR', 'GBP', 'USDC']

const blank: ChannelSpec = {
  name: '', description: '', currencies: ['KES'], inward: false, outward: true, extension: '', settlementAccounts: {},
  localFields: [], minAmount: 0, maxAmount: 0, stp: true, autoClearAt: 0, escalateAfterDays: 2, settlementHoldDays: 0,
}

// A provider channel's item fields are recorded by the product flows, not typed by operators.
const providerFields =['provider', 'account_reference', 'payer', 'payer_name', 'destination']

export default function Channels() {
  const channels = useLoad(() => api<Channel[]>('/ops/channels'))
  const [selected, setSelected] = useState<string>()
  const [editing, setEditing] = useState<Editing>()
  const [version, setVersion] = useState(0)

  return (
    <>
      <PageHeader module="CH · PROVIDER CHANNELS" title="Provider channels">
        <Button onClick={() => setEditing({ code: '', spec: { ...blank, localFields: [] }, amending: false, kind: 'provider' })}>Onboard a payment provider</Button>
        <Button variant="ghost" onClick={() => setEditing({ code: '', spec: { ...blank, localFields: [] }, amending: false })}>Onboard a transfer channel</Button>
      </PageHeader>
      <p className="muted intro">
        Every provider starts with its books: an <strong>outward</strong> suspense account (customer money waiting for the provider, GL 2530) and an
        <strong> inward</strong> one (provider money waiting for its beneficiary, GL 2520), never the same account, plus a settlement account. No provider's
        code is in Zirnis: each is a <strong>module</strong>, its own service, registered under <Link to="/console/extensions">Extensions</Link>.
        A <strong>payment provider</strong> (M-Pesa, KCB, Equity, a card acquirer) is served over the provider protocol: payments, payouts and deposits
        route to it with the capabilities its module described and a checker authorised, and its float is the settlement account. A <strong>transfer
        channel</strong> (Tieto prepaid cards, Verto) fires its module whenever money lands in its books; the module answers complete, return or hold.
      </p>
      <div className="grid-main-side">
        <Panel title="Channels">
          <ErrorNote error={channels.error} />
          {channels.data?.length === 0 && <Empty>No providers onboarded yet.</Empty>}
          {!!channels.data?.length && (
            <table>
              <thead><tr><th>Code</th><th>Provider</th><th>Directions</th><th>Suspense</th><th>Module</th><th>STP</th><th /></tr></thead>
              <tbody>
                {channels.data.map((c) => (
                  <tr key={c.id} className={`clickable ${selected === c.code ? 'selected' : ''}`} onClick={() => setSelected(c.code)}>
                    <td><Mono>{c.code}</Mono> {c.provider && <Badge value="Provider" />} {!c.active && <Badge value="Retired" />}</td>
                    <td>{c.name}<br /><small className="muted">{c.currencies.join(', ')}</small></td>
                    <td>{[c.inward && 'Inward', c.outward && 'Outward'].filter(Boolean).join(' · ')}</td>
                    <td>{c.inwardType && <Mono>{c.inwardType} </Mono>}{c.outwardType && <Mono>{c.outwardType}</Mono>}</td>
                    <td>
                      {c.provider ? <><Mono>{c.module || 'simulator'}</Mono> <small className="muted">serves {c.provider}</small></> : !c.extension && <span className="error-text">none bound</span>}
                      {c.extension && <>{c.provider && <br />}<Mono>{c.extension}</Mono>{c.provider && <small className="muted"> matches</small>}</>}
                    </td>
                    <td>{c.provider ? <span className="muted">—</span> : <Badge value={c.stp} />}</td>
                    <td><button className="link-btn" onClick={(e) => { e.stopPropagation(); setEditing({ code: c.code, spec: specOf(c), amending: true, provider: c.provider, kind: c.provider ? 'provider' : undefined }) }}>Amend</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <div className="stack">
          {editing?.kind === 'provider' && <ProviderForm key={editing.code || 'new-provider'} initial={editing} onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />}
          {editing && editing.kind !== 'provider' && <ChannelForm key={editing.code || 'new'} initial={editing} onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />}
          <PendingChanges types={['ProviderChannel']} version={version} onDecided={channels.reload} />
        </div>
      </div>
      {selected && <ChannelDetail key={`${selected}:${version}`} code={selected} />}
    </>
  )
}

type Editing = { code: string; spec: ChannelSpec; amending: boolean; provider?: string; kind?: 'provider' }

function specOf(c: Channel): ChannelSpec {
  return {
    name: c.name, description: c.description, currencies: [...c.currencies], inward: c.inward, outward: c.outward, extension: c.extension,
    settlementAccounts: { ...c.settlementAccounts }, localFields: c.localFields.filter((f) => !c.provider || !providerFields.includes(f.name)).map((f) => ({ ...f })),
    minAmount: c.minAmount, maxAmount: c.maxAmount, stp: c.stp, autoClearAt: c.autoClearAt, escalateAfterDays: c.escalateAfterDays,
    settlementHoldDays: c.settlementHoldDays, active: c.active, provider: c.provider || undefined, module: c.module || undefined, priority: c.priority || undefined,
  }
}

// ProviderForm onboards (or amends) a payment provider: the module serving it and how it is
// routed. What the provider can do comes from the module and is shown to the checker.
function ProviderForm({ initial, onDone }: { initial: Editing; onDone: () => void }) {
  const extensions = useLoad(() => api<Extension[]>('/core/extensions'))
  const [code, setCode] = useState(initial.code)
  const [spec, setSpec] = useState<ChannelSpec>({ ...initial.spec, provider: initial.spec.provider ?? '', module: initial.spec.module ?? '', priority: initial.spec.priority ?? 100 })
  const set = <K extends keyof ChannelSpec>(key: K, value: ChannelSpec[K]) => setSpec({ ...spec, [key]: value })
  const body = {
    provider: spec.provider, module: spec.module, priority: spec.priority, name: initial.amending ? spec.name : '', extension: spec.extension,
    autoClearAt: spec.autoClearAt, minAmount: spec.minAmount, maxAmount: spec.maxAmount, escalateAfterDays: spec.escalateAfterDays,
    settlementHoldDays: spec.settlementHoldDays, active: spec.active,
  }
  const submit = useAction(() => api(`/ops/channels/${code.trim().toUpperCase()}`, { method: 'POST', body }))
  const modules = extensions.data?.filter((x) => x.hooks.includes('provider')) ?? []
  const matchers = extensions.data?.filter((x) => x.hooks.includes('suspense.match') || x.hooks.includes('suspense.classify')) ?? []

  return (
    <Panel title={initial.amending ? `Amend ${initial.code}` : 'Onboard a payment provider'} aside={<span className="muted">INAU until authorised</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <p className="muted">Zirnis asks the module what the provider can do; the checker sees it, and nothing routes to the provider until it is authorised.
          Its books, currencies and float follow those capabilities.</p>
        <div className="row-2">
          <Field label="Code" hint="e.g. MPESA, KCB — names the books (MPESAIN, MPESAOUT)">
            <input required readOnly={initial.amending} value={code} onChange={(e) => setCode(e.target.value.toUpperCase())} />
          </Field>
          <Field label="Provider" hint="as the module names it, e.g. mpesa, kcb-mpesa">
            <input required readOnly={initial.amending} value={spec.provider} onChange={(e) => set('provider', e.target.value.trim().toLowerCase())} />
          </Field>
        </div>
        <div className="row-2">
          <Field label="Module" hint="A registered endpoint serving the provider protocol">
            <select required={!initial.amending} value={spec.module} onChange={(e) => set('module', e.target.value)}>
              <option value="">{initial.amending ? '— keep —' : '— choose —'}</option>
              {modules.map((x) => <option key={x.name} value={x.name}>{x.name}</option>)}
            </select>
          </Field>
          <Field label="Priority" hint="Lower is tried first among providers that can serve the same payment">
            <input type="number" min={1} max={1000} value={spec.priority} onChange={(e) => set('priority', Number(e.target.value))} />
          </Field>
        </div>
        {initial.amending && <Field label="Name"><input value={spec.name} onChange={(e) => set('name', e.target.value)} /></Field>}
        <div className="row-2">
          <Field label="Matching extension" hint="Classifies and matches unidentified money in the inward book">
            <select value={spec.extension} onChange={(e) => setSpec({ ...spec, extension: e.target.value, autoClearAt: e.target.value ? spec.autoClearAt : 0 })}>
              <option value="">— none —</option>
              {matchers.map((x) => <option key={x.name} value={x.name}>{x.name}</option>)}
            </select>
          </Field>
          <Field label="Auto-clear matches at (%)" hint="0 = every proposed match waits for a checker">
            <input type="number" min={0} max={100} disabled={!spec.extension} value={spec.autoClearAt} onChange={(e) => set('autoClearAt', Number(e.target.value))} />
          </Field>
        </div>
        <div className="row-3">
          <Field label="Minimum (minor units)"><input type="number" min={0} value={spec.minAmount} onChange={(e) => set('minAmount', Number(e.target.value))} /></Field>
          <Field label="Maximum (0 = none)"><input type="number" min={0} value={spec.maxAmount} onChange={(e) => set('maxAmount', Number(e.target.value))} /></Field>
          <Field label="Settlement hold (business days)" hint="cards: 2 by default">
            <input type="number" min={0} max={30} value={spec.settlementHoldDays} onChange={(e) => set('settlementHoldDays', Number(e.target.value))} />
          </Field>
        </div>
        {initial.amending && <label className="check"><input type="checkbox" checked={spec.active ?? true} onChange={(e) => set('active', e.target.checked)} /> Active</label>}
        <ErrorNote error={submit.error} />
        <div className="actions">
          <Button type="button" variant="ghost" onClick={onDone}>Cancel</Button>
          <Button busy={submit.busy}>Input {initial.amending ? 'amendment' : 'provider'} (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}

// capabilityLines describes what a provider may do, for operators.
function capabilityLines(c: ProviderCapabilities): string[] {
  const lines: string[] = []
  for (const col of c.collections ?? []) lines.push(`Collects ${col.method} in ${col.currencies.join(', ')}${col.hosted ? ' (hosted checkout)' : ''}`)
  for (const p of c.payouts ?? []) lines.push(`Pays out ${p.rail} in ${p.currencies.join(', ')}`)
  if (c.deposits) lines.push('Notifies deposits into our account')
  if (c.bankCode) lines.push(`Bank code ${c.bankCode} (preferred for on-us payouts)`)
  const checks = [c.checkCharge && 'charge status', c.checkTransaction && 'transaction check', c.requestStatus && 'status query'].filter(Boolean)
  if (checks.length) lines.push(`Callbacks confirmed by ${checks.join(', ')}`)
  else if (c.settlementConfirmed) lines.push('Collections credited only on a settlement notification')
  else lines.push(`Callbacks trusted on ${c.notificationValidation || "the module's authentication"}`)
  if (c.idempotentCharges) lines.push('A charge whose answer was lost is asked about again')
  if (c.balance) lines.push('Live balance enquiry')
  return lines
}

function ProviderPanel({ channel, onDone }: { channel: ChannelView; onDone: () => void }) {
  const run = useAction((action: string) => api<Record<string, unknown>>(`/ops/channels/${channel.code}/actions/${action}`, { method: 'POST' }))
  return (
    <Panel title={channel.capabilities.name || channel.provider} aside={<Badge value={channel.routable ? 'Routable' : 'Not routable'} />}>
      {!channel.routable && channel.notRoutable && <p className="error-text">{channel.notRoutable}</p>}
      <p className="muted">Provider <Mono>{channel.provider}</Mono>, served by {channel.module ? <>module <Mono>{channel.module}</Mono></> : 'the sandbox simulator'}; priority {channel.priority}.</p>
      <ul>{capabilityLines(channel.capabilities).map((l) => <li key={l}>{l}</li>)}</ul>
      {!!channel.capabilities.actions?.length && (
        <div className="actions">
          {channel.capabilities.actions.map((a) => (
            <Button key={a.name} variant="ghost" busy={run.busy} onClick={async () => { if (await run.run(a.name)) onDone() }}>{a.label}</Button>
          ))}
        </div>
      )}
      <ErrorNote error={run.error} />
      {run.result && <p className="muted">Done: <Mono>{JSON.stringify(run.result)}</Mono></p>}
    </Panel>
  )
}

function ChannelForm({ initial, onDone }: { initial: Editing; onDone: () => void }) {
  const extensions = useLoad(() => api<Extension[]>('/core/extensions'))
  const [code, setCode] = useState(initial.code)
  const [spec, setSpec] = useState(initial.spec)
  const set = <K extends keyof ChannelSpec>(key: K, value: ChannelSpec[K]) => setSpec({ ...spec, [key]: value })
  const setField = (i: number, patch: Partial<LocalField>) => set('localFields', spec.localFields.map((f, j) => (i === j ? { ...f, ...patch } : f)))
  const submit = useAction(() => api(`/ops/channels/${code.trim().toUpperCase()}`, { method: 'POST', body: spec }))
  const builtIn = !!initial.provider
  // A built-in channel's connector moves its money; an extension bound to it classifies and
  // matches unidentified money.
  const modules = extensions.data?.filter((x) => x.hooks.includes(builtIn ? 'suspense.match' : 'suspense.opened') || (builtIn && x.hooks.includes('suspense.classify'))) ?? []
  const toggleCcy = (c: string) => set('currencies', spec.currencies.includes(c) ? spec.currencies.filter((x) => x !== c) : [...spec.currencies, c])

  return (
    <Panel title={initial.amending ? `Amend ${initial.code}` : 'Onboard a provider'} aside={<span className="muted">INAU until authorised</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <div className="row-2">
          <Field label="Code" hint="e.g. TIETO, AIRTEL — names the suspense types (TIETOOUT, AIRTELIN)">
            <input required readOnly={initial.amending} value={code} onChange={(e) => setCode(e.target.value.toUpperCase())} />
          </Field>
          <Field label="Provider"><input required value={spec.name} onChange={(e) => set('name', e.target.value)} placeholder="Tieto prepaid cards" /></Field>
        </div>
        <Field label="Directions" hint="Each direction gets its own suspense account and GL line; directions can be added, never removed">
          <div className="chips">
            <label className="check"><input type="checkbox" checked={spec.outward} disabled={builtIn || (initial.amending && initial.spec.outward)} onChange={(e) => set('outward', e.target.checked)} /> Outward — customer → provider (card loads, wallet pushes)</label>
            <label className="check"><input type="checkbox" checked={spec.inward} disabled={builtIn || (initial.amending && initial.spec.inward)} onChange={(e) => set('inward', e.target.checked)} /> Inward — provider → customer</label>
          </div>
        </Field>
        <Field label="Currencies">
          <div className="chips">{currencies.map((c) => <label key={c} className="check"><input type="checkbox" disabled={builtIn} checked={spec.currencies.includes(c)} onChange={() => toggleCcy(c)} /> {c}</label>)}</div>
        </Field>
        <Field label="Settlement accounts" hint="Blank opens a new settlement account with the provider (nostro category 14003)">
          <div className="row-3">
            {spec.currencies.map((c) => (
              <input key={c} readOnly={builtIn} placeholder={`${c} — open new`} value={spec.settlementAccounts[c] ?? ''}
                onChange={(e) => set('settlementAccounts', { ...spec.settlementAccounts, [c]: e.target.value.trim() })} />
            ))}
          </div>
        </Field>
        <div className="row-2">
          <Field label={builtIn ? 'Matching extension' : 'Provider module'}
            hint={builtIn ? 'Classifies and matches unidentified money in the inward book' : 'An extension serving suspense.opened; it can be bound later, but nothing settles without it'}>
            <select value={spec.extension} onChange={(e) => setSpec({ ...spec, extension: e.target.value, autoClearAt: e.target.value ? spec.autoClearAt : 0 })}>
              <option value="">— none yet —</option>
              {modules.map((x) => <option key={x.name} value={x.name}>{x.name} ({x.kind === 'InProcess' ? 'Go, in-process' : 'remote'})</option>)}
            </select>
          </Field>
          <Field label="Auto-clear matches at (%)" hint="0 = every proposed match waits for a checker">
            <input type="number" min={0} max={100} disabled={!spec.extension} value={spec.autoClearAt} onChange={(e) => set('autoClearAt', Number(e.target.value))} />
          </Field>
        </div>
        <div className="row-3">
          <Field label="Minimum (minor units)"><input type="number" min={0} value={spec.minAmount} onChange={(e) => set('minAmount', Number(e.target.value))} /></Field>
          <Field label="Maximum (0 = none)"><input type="number" min={0} value={spec.maxAmount} onChange={(e) => set('maxAmount', Number(e.target.value))} /></Field>
          <Field label="Escalate after (days)"><input type="number" min={1} value={spec.escalateAfterDays} onChange={(e) => set('escalateAfterDays', Number(e.target.value))} /></Field>
        </div>
        <Field label="Settlement hold (business days)" hint="Collections through this provider stay in the merchant's held balance until the provider has paid them to us (cards: 2)">
          <input type="number" min={0} max={30} value={spec.settlementHoldDays} onChange={(e) => set('settlementHoldDays', Number(e.target.value))} />
        </Field>
        {!builtIn && <label className="check"><input type="checkbox" checked={spec.stp} onChange={(e) => set('stp', e.target.checked)} /> Straight-through: apply the module's outcomes without a human checker</label>}

        <h3 className="sub">Request fields</h3>
        <p className="muted">What a transfer must carry for this provider (card token, MSISDN, …). <Mono>provider_reference</Mono> is always recorded.</p>
        {spec.localFields.map((f, i) => (
          <div key={i} className="field-row">
            <input placeholder="name" value={f.name} onChange={(e) => setField(i, { name: e.target.value })} />
            <input placeholder="Label" value={f.label} onChange={(e) => setField(i, { label: e.target.value })} />
            <select value={f.type} onChange={(e) => setField(i, { type: e.target.value as LocalField['type'] })}>{['text', 'number', 'date', 'choice'].map((t) => <option key={t}>{t}</option>)}</select>
            <input placeholder="Pattern (regex)" value={f.pattern ?? ''} onChange={(e) => setField(i, { pattern: e.target.value })} />
            <label className="check"><input type="checkbox" checked={f.required} onChange={(e) => setField(i, { required: e.target.checked })} /> Required</label>
            <button type="button" className="link-btn" onClick={() => set('localFields', spec.localFields.filter((_, j) => j !== i))}>Remove</button>
          </div>
        ))}
        <div><Button type="button" variant="ghost" onClick={() => set('localFields', [...spec.localFields, { name: '', label: '', type: 'text', required: true }])}>Add field</Button></div>
        {initial.amending && <label className="check"><input type="checkbox" checked={spec.active ?? true} onChange={(e) => set('active', e.target.checked)} /> Active</label>}
        <ErrorNote error={submit.error} />
        <div className="actions">
          <Button type="button" variant="ghost" onClick={onDone}>Cancel</Button>
          <Button busy={submit.busy}>Input {initial.amending ? 'amendment' : 'channel'} (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}

function ChannelDetail({ code }: { code: string }) {
  const channel = useLoad(() => api<ChannelView>(`/ops/channels/${code}`), [code])
  const transfers = useLoad(() => api<ChannelTransfer[]>(`/ops/channels/${code}/transfers`), [code])
  const events = useLoad(() => api<SuspenseEvent[]>('/ops/suspense/events?limit=200'), [code])
  const retry = useAction((id: string) => api(`/ops/suspense/events/${id}/retry`, { method: 'POST' }))
  const c = channel.data
  const reload = () => { channel.reload(); transfers.reload(); events.reload() }
  if (channel.error) return <Panel><ErrorNote error={channel.error} /></Panel>
  if (!c) return null
  const mine = (events.data ?? []).filter((e) => e.typeCode === c.inwardType || e.typeCode === c.outwardType)
  const role = (glCode: string) => ({ '2520': 'Inward suspense', '2530': 'Outward suspense', '1150': 'Settlement', '1200': 'Settlement (float)' }[glCode] ?? glCode)

  return (
    <>
      <div className="stat-grid">
        <Stat label="In flight" value={c.openItems} hint="items waiting in this channel's suspense" tone={c.openItems ? 'warn' : 'good'} />
        <Stat label="Events pending / held" value={c.pendingEvents} hint="waiting for the module or the provider" />
        <Stat label="Events failed" value={c.failedEvents} tone={c.failedEvents ? 'bad' : 'good'} hint="need an operator" />
      </div>
      <div className="grid-side-main">
        <div className="stack">
          <Panel title={`${c.code} books`}>
            <table>
              <thead><tr><th>Role</th><th>Account</th><th>GL</th><th className="num">Balance</th></tr></thead>
              <tbody>
                {c.accounts.map((a) => (
                  <tr key={a.id}><td>{role(a.glCode)}</td><td><Mono>{a.accountNumber}</Mono></td><td><Mono>{a.glCode}</Mono></td><td className="num">{money(a.balance, a.currency)}</td></tr>
                ))}
              </tbody>
            </table>
            <p className="muted">Work stuck items under <Link to="/console/suspense">Suspense</Link> (types {[c.inwardType, c.outwardType].filter(Boolean).join(', ')}).</p>
          </Panel>
          {c.provider && <ProviderPanel channel={c} onDone={reload} />}
          {c.outward && !c.provider && getSession()?.sandbox && <TestTransfer channel={c} onDone={reload} />}
        </div>
        <div className="stack">
          <Panel title="Transfers">
            {transfers.data?.length === 0 && <Empty>No transfers yet.</Empty>}
            {!!transfers.data?.length && (
              <table>
                <thead><tr><th>Opened</th><th>Direction</th><th>Customer</th><th className="num">Amount</th><th>Provider ref</th><th>Outcome</th></tr></thead>
                <tbody>
                  {transfers.data.map((t) => (
                    <tr key={t.id}>
                      <td className="nowrap">{shortDay(t.openedOn)}</td>
                      <td>{t.direction}{c.provider && <><br /><small className="muted">{t.sourceType}</small></>}</td>
                      <td><Mono>{t.contraAccountNumber}</Mono><br /><small className="muted">{t.narrative}</small></td>
                      <td className="num">{money(t.amount, t.currency)}</td>
                      <td><Mono>{t.providerReference || '—'}</Mono></td>
                      <td><Badge value={t.outcome} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>
          <Panel title="Module events" aside={<span className="muted">fired when funds land; retried with backoff</span>}>
            <ErrorNote error={retry.error} />
            {c.provider && <p className="muted">The <Mono>{c.provider}</Mono> module is called by the payout, refund or payment that landed the money, with its own retries and failover; see provider calls under <Link to="/console/operations">Operations</Link>.</p>}
            {mine.length === 0 && !c.provider && <Empty>No events yet.</Empty>}
            <ul className="audit-feed">
              {mine.map((e) => (
                <li key={e.id}>
                  <Badge value={e.status === 'Delivered' ? 'Succeeded' : e.status} />
                  <span><Mono>{e.typeCode}</Mono> · attempt {e.attempts}{e.outcome && <> · {e.outcome}</>}{e.extension && <> by <Mono>{e.extension}</Mono></>}
                    {e.lastError && <span className="error-text"> · {e.lastError}</span>}</span>
                  <span className="nowrap">
                    {(e.status === 'Failed' || e.status === 'Held' || e.status === 'Pending') && (
                      <Button variant="ghost" busy={retry.busy} onClick={async () => { if (await retry.run(e.id)) reload() }}>{e.status === 'Held' ? 'Ask again' : 'Retry'}</Button>
                    )} <span className="muted">{dateTime(e.createdAt)}</span>
                  </span>
                </li>
              ))}
            </ul>
          </Panel>
        </div>
      </div>
    </>
  )
}

function TestTransfer({ channel, onDone }: { channel: ChannelView; onDone: () => void }) {
  const [form, setForm] = useState({ accountNumber: '', amount: '100', narrative: '' })
  const [attrs, setAttrs] = useState<Record<string, string>>({})
  const ccy = channel.currencies[0]
  const send = useAction(async () => {
    const account = await api<Account>(`/accounts/by-number/${form.accountNumber}`)
    return api<TransactionView>('/transactions', {
      method: 'POST', body: { version: 'OT.CONSOLE', channelCode: channel.code, debitAccount: account.accountNumber,
        amount: toMinor(form.amount, account.currency), narrative: form.narrative, attributes: attrs },
    })
  })
  return (
    <Panel title={`Send through ${channel.code}`} aside={<span className="muted">version OT.CONSOLE · a checker authorises it</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await send.run()) onDone() }}>
        <div className="row-2">
          <Field label="Customer account"><input required value={form.accountNumber} onChange={(e) => setForm({ ...form, accountNumber: e.target.value.trim() })} /></Field>
          <Field label={`Amount (${ccy})`}><input required value={form.amount} onChange={(e) => setForm({ ...form, amount: e.target.value })} /></Field>
        </div>
        {channel.localFields.map((f) => (
          <Field key={f.name} label={`${f.label}${f.required ? ' *' : ''}`}>
            <input value={attrs[f.name] ?? ''} onChange={(e) => setAttrs({ ...attrs, [f.name]: e.target.value })} />
          </Field>
        ))}
        <Field label="Narrative"><input value={form.narrative} onChange={(e) => setForm({ ...form, narrative: e.target.value })} /></Field>
        <ErrorNote error={send.error} />
        <Button busy={send.busy}>Send</Button>
        {send.result && <p>Input as <Mono>{send.result.reference}</Mono> <Badge value={send.result.recordStatus} /> — it moves once a checker authorises it under Input a transaction.</p>}
      </form>
    </Panel>
  )
}
