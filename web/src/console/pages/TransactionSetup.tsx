import { useState } from 'react'
import { api } from '../api'
import { fieldLabel } from '../format'
import { PendingChanges } from '../pending'
import { useAction, useLoad } from '../hooks'
import type { LocalTable, OverrideClass, TransactionFlow, TransactionType, TransactionTypeSpec, Version, VersionField, VersionSpec } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel } from '../ui'

const flows: TransactionFlow[] = ['transfer', 'collection', 'payout', 'refund', 'channel_out', 'deposit', 'channel_in']

// The core fields each flow takes (mirrors domain.FlowFields).
const flowFields: Record<TransactionFlow, string[]> = {
  transfer: ['debitAccount', 'creditAccount', 'amount', 'narrative', 'paymentDetails'],
  collection: ['creditAccount', 'amount', 'currency', 'method', 'source', 'payer.firstName', 'payer.lastName', 'payer.email', 'payer.phone', 'payer.address', 'payer.postalCode', 'returnUrl', 'narrative', 'paymentDetails'],
  payout: ['debitAccount', 'amount', 'rail', 'beneficiary.account', 'beneficiary.name', 'beneficiary.bankCode', 'narrative', 'paymentDetails'],
  refund: ['originalReference', 'amount', 'reason', 'narrative'],
  channel_out: ['channelCode', 'debitAccount', 'amount', 'reference', 'narrative', 'paymentDetails', 'attributes'],
  deposit: [],
  channel_in: [],
}

const channels = ['api', 'console', 'link', 'system']
const coreOverrides = ['HIGH.VALUE', 'NEW.BENEFICIARY', 'BENEFICIARY.RESTRICTED']
const list = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean)

export default function TransactionSetup() {
  const types = useLoad(() => api<TransactionType[]>('/ops/transaction-types'))
  const versions = useLoad(() => api<Version[]>('/versions'))
  const local = useLoad(() => api<LocalTable>('/ops/local-tables/TRANSACTION'))
  const [version, setVersion] = useState(0)
  const [editType, setEditType] = useState<TransactionType | 'new'>()
  const [editVersion, setEditVersion] = useState<Version | 'new'>()
  const reload = () => { types.reload(); versions.reload() }

  return (
    <>
      <PageHeader module="FT.TXN.TYPE.CONDITION · VERSION" title="Transaction setup" />
      <Panel title="Transaction types" aside={<Button variant="ghost" onClick={() => setEditType('new')}>New type</Button>}>
        <p className="muted">A type decides which core flow executes its transactions and how they are reported. Its flow is fixed once authorised.</p>
        {types.loading && <Loading />}
        <ErrorNote error={types.error} />
        {!!types.data?.length && (
          <table>
            <thead><tr><th>Code</th><th>Description</th><th>Flow</th><th>Reporting class</th><th>Currencies</th><th /></tr></thead>
            <tbody>
              {types.data.map((t) => (
                <tr key={t.code}>
                  <td><Mono>{t.code}</Mono> {t.disabled && <Badge value="Disabled" />}</td>
                  <td>{t.description}</td>
                  <td>{t.flow} <span className="muted">· {t.direction}</span></td>
                  <td><Mono>{t.reportingClass || '—'}</Mono></td>
                  <td>{t.currencies?.length ? t.currencies.join(', ') : 'any'}</td>
                  <td className="actions"><Button variant="ghost" onClick={() => setEditType(t)}>Amend</Button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {editType && <TypeForm key={editType === 'new' ? 'new' : editType.code} existing={editType === 'new' ? undefined : editType}
          onDone={() => { setEditType(undefined); setVersion((v) => v + 1) }} />}
      </Panel>

      <Panel title="Versions" aside={<Button variant="ghost" onClick={() => setEditVersion('new')}>New version</Button>}>
        <p className="muted">A version is a screen for one use case: which fields are input, their defaults, which overrides need a checker, the routines consulted, and whether a checker authorises every input.</p>
        {versions.loading && <Loading />}
        <ErrorNote error={versions.error} />
        {!!versions.data?.length && (
          <table>
            <thead><tr><th>Version</th><th>Type</th><th>Channels</th><th>Authorisers</th><th>Screen</th><th /></tr></thead>
            <tbody>
              {versions.data.map((v) => (
                <tr key={v.name}>
                  <td><Mono>{v.name}</Mono> {v.disabled && <Badge value="Disabled" />}<br /><span className="muted">{v.description}</span></td>
                  <td><Mono>{v.transactionType}</Mono> <span className="muted">{v.flow}</span></td>
                  <td>{v.channels.join(', ')}</td>
                  <td>{v.authorisers > 0 ? 'checker' : 'straight through'}</td>
                  <td className="muted">{v.fields?.length ? `${v.fields.length} fields` : 'open'}{v.inputRoutines?.length ? ` · routines ${v.inputRoutines.join(', ')}` : ''}</td>
                  <td className="actions"><Button variant="ghost" onClick={() => setEditVersion(v)}>Amend</Button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {editVersion && types.data && (
          <VersionForm key={editVersion === 'new' ? 'new' : editVersion.name} existing={editVersion === 'new' ? undefined : editVersion}
            types={types.data} localFields={local.data?.fields.filter((f) => !f.retired).map((f) => f.name) ?? []}
            onDone={() => { setEditVersion(undefined); setVersion((v) => v + 1) }} />
        )}
      </Panel>
      <PendingChanges types={['TransactionType', 'Version']} version={version} onDecided={reload} />
      {versions.data?.length === 0 && <Empty>No versions.</Empty>}
    </>
  )
}

function TypeForm({ existing, onDone }: { existing?: TransactionType; onDone: () => void }) {
  const [code, setCode] = useState(existing?.code ?? '')
  const [spec, setSpec] = useState<TransactionTypeSpec>(existing ? { description: existing.description, flow: existing.flow, narrative: existing.narrative,
    reportingClass: existing.reportingClass, currencies: existing.currencies, minAmount: existing.minAmount, maxAmount: existing.maxAmount, disabled: existing.disabled }
    : { description: '', flow: 'transfer' })
  const submit = useAction(() => api(`/ops/transaction-types/${code.trim().toUpperCase()}`, { method: 'POST', body: spec }))
  return (
    <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
      <span className="mono-label">{existing ? `AMEND ${existing.code}` : 'NEW TRANSACTION TYPE'}</span>
      <div className="row-3">
        <Field label="Code" hint="2-6 letters or digits, e.g. ACMM"><input className="mono" required disabled={!!existing} value={code} onChange={(e) => setCode(e.target.value)} /></Field>
        <Field label="Flow" hint="Fixed once authorised">
          <select value={spec.flow} disabled={!!existing} onChange={(e) => setSpec({ ...spec, flow: e.target.value as TransactionFlow })}>{flows.map((f) => <option key={f}>{f}</option>)}</select>
        </Field>
        <Field label="Reporting class" hint="e.g. goAML conduction MM01"><input className="mono" value={spec.reportingClass ?? ''} onChange={(e) => setSpec({ ...spec, reportingClass: e.target.value })} /></Field>
      </div>
      <Field label="Description"><input required value={spec.description} onChange={(e) => setSpec({ ...spec, description: e.target.value })} /></Field>
      <div className="row-3">
        <Field label="Default narrative"><input value={spec.narrative ?? ''} onChange={(e) => setSpec({ ...spec, narrative: e.target.value })} /></Field>
        <Field label="Currencies" hint="Comma-separated; empty = any"><input value={spec.currencies?.join(', ') ?? ''} onChange={(e) => setSpec({ ...spec, currencies: list(e.target.value) })} /></Field>
        <Field label="Disabled"><select value={spec.disabled ? 'yes' : 'no'} onChange={(e) => setSpec({ ...spec, disabled: e.target.value === 'yes' })}><option value="no">No</option><option value="yes">Yes</option></select></Field>
      </div>
      <div className="row-2">
        <Field label="Minimum (minor units)"><input inputMode="numeric" value={spec.minAmount ?? ''} onChange={(e) => setSpec({ ...spec, minAmount: Number(e.target.value) || 0 })} /></Field>
        <Field label="Maximum (minor units)"><input inputMode="numeric" value={spec.maxAmount ?? ''} onChange={(e) => setSpec({ ...spec, maxAmount: Number(e.target.value) || 0 })} /></Field>
      </div>
      <ErrorNote error={submit.error} />
      <div className="actions"><Button busy={submit.busy}>Input for authorisation</Button><Button type="button" variant="ghost" onClick={onDone}>Cancel</Button></div>
    </form>
  )
}

function VersionForm({ existing, types, localFields, onDone }: { existing?: Version; types: TransactionType[]; localFields: string[]; onDone: () => void }) {
  const [name, setName] = useState(existing?.name ?? '')
  const [spec, setSpec] = useState<VersionSpec>(existing ? {
    description: existing.description, transactionType: existing.transactionType, channels: existing.channels, authorisers: existing.authorisers,
    fields: existing.fields ?? [], minAmount: existing.minAmount, maxAmount: existing.maxAmount, currencies: existing.currencies,
    inputRoutines: existing.inputRoutines, authRoutines: existing.authRoutines, overrides: existing.overrides ?? {}, reportingClass: existing.reportingClass,
    disabled: existing.disabled,
  } : { description: '', transactionType: types[0]?.code ?? '', channels: ['console'], authorisers: 1, fields: [], overrides: {} })
  const flow = types.find((t) => t.code === spec.transactionType)?.flow ?? 'transfer'
  const available = [...flowFields[flow], ...localFields.map((f) => `local.${f}`)]
  const fields = spec.fields ?? []
  const setField = (i: number, f: Partial<VersionField>) => setSpec({ ...spec, fields: fields.map((x, j) => (j === i ? { ...x, ...f } : x)) })
  const overrides = spec.overrides ?? {}
  const [newOverride, setNewOverride] = useState('')
  const submit = useAction(() => api(`/ops/versions/${name.trim().toUpperCase()}`, { method: 'POST', body: spec }))

  return (
    <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
      <span className="mono-label">{existing ? `AMEND ${existing.name}` : 'NEW VERSION'}</span>
      <div className="row-3">
        <Field label="Name" hint="Upper case with dots, e.g. MPESA.PAYBILL.CORP"><input className="mono" required disabled={!!existing} value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label="Transaction type" hint="Fixed once authorised">
          <select value={spec.transactionType} disabled={!!existing} onChange={(e) => setSpec({ ...spec, transactionType: e.target.value, fields: [] })}>
            {types.map((t) => <option key={t.code} value={t.code}>{t.code} — {t.description}</option>)}
          </select>
        </Field>
        <Field label="Authorisers"><select value={spec.authorisers} onChange={(e) => setSpec({ ...spec, authorisers: Number(e.target.value) })}><option value={0}>0 — straight through</option><option value={1}>1 — a checker authorises</option></select></Field>
      </div>
      <Field label="Description"><input required value={spec.description} onChange={(e) => setSpec({ ...spec, description: e.target.value })} /></Field>
      <div className="row-3">
        <Field label="Channels">
          <div className="chips">
            {channels.map((c) => (
              <label key={c} className="check">
                <input type="checkbox" checked={spec.channels.includes(c)} onChange={(e) => setSpec({ ...spec, channels: e.target.checked ? [...spec.channels, c] : spec.channels.filter((x) => x !== c) })} /> {c}
              </label>
            ))}
          </div>
        </Field>
        <Field label="Input routines" hint="Extension names"><input value={spec.inputRoutines?.join(', ') ?? ''} onChange={(e) => setSpec({ ...spec, inputRoutines: list(e.target.value) })} /></Field>
        <Field label="Authorisation routines"><input value={spec.authRoutines?.join(', ') ?? ''} onChange={(e) => setSpec({ ...spec, authRoutines: list(e.target.value) })} /></Field>
      </div>
      <div className="row-4">
        <Field label="Reporting class" hint="Overrides the type's"><input className="mono" value={spec.reportingClass ?? ''} onChange={(e) => setSpec({ ...spec, reportingClass: e.target.value })} /></Field>
        <Field label="Currencies"><input value={spec.currencies?.join(', ') ?? ''} onChange={(e) => setSpec({ ...spec, currencies: list(e.target.value) })} /></Field>
        <Field label="Min (minor)"><input inputMode="numeric" value={spec.minAmount ?? ''} onChange={(e) => setSpec({ ...spec, minAmount: Number(e.target.value) || 0 })} /></Field>
        <Field label="Max (minor)"><input inputMode="numeric" value={spec.maxAmount ?? ''} onChange={(e) => setSpec({ ...spec, maxAmount: Number(e.target.value) || 0 })} /></Field>
      </div>

      <span className="mono-label">SCREEN — {fields.length ? 'only these fields are input' : 'open: every field of the flow'}</span>
      {fields.map((f, i) => (
        <div key={i} className="design-row">
          <select value={f.field} onChange={(e) => setField(i, { field: e.target.value })}>
            {available.map((a) => <option key={a} value={a}>{a.startsWith('local.') ? a : fieldLabel(a)}</option>)}
          </select>
          <input placeholder="Label" value={f.label ?? ''} onChange={(e) => setField(i, { label: e.target.value })} />
          <input placeholder="Default" value={f.default ?? ''} onChange={(e) => setField(i, { default: e.target.value })} />
          <label className="check"><input type="checkbox" checked={!!f.mandatory} onChange={(e) => setField(i, { mandatory: e.target.checked })} /> mandatory</label>
          <label className="check"><input type="checkbox" checked={!!f.noInput} onChange={(e) => setField(i, { noInput: e.target.checked })} /> no input</label>
          <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fields: fields.filter((_, j) => j !== i) })}>Remove</Button>
        </div>
      ))}
      <div className="actions">
        <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fields: [...fields, { field: available.find((a) => !fields.some((f) => f.field === a)) ?? available[0] }] })}>Add field</Button>
      </div>

      <span className="mono-label">OVERRIDE CLASSES — not listed: the inputter accepts them</span>
      {Object.entries(overrides).map(([code, cls]) => (
        <div key={code} className="design-row">
          <Mono>{code}</Mono>
          <select value={cls} onChange={(e) => setSpec({ ...spec, overrides: { ...overrides, [code]: e.target.value as OverrideClass } })}>
            {['accept', 'checker', 'refuse', 'ignore'].map((c) => <option key={c}>{c}</option>)}
          </select>
          <span /><span /><span />
          <Button type="button" variant="ghost" onClick={() => { const rest = { ...overrides }; delete rest[code]; setSpec({ ...spec, overrides: rest }) }}>Remove</Button>
        </div>
      ))}
      <div className="inline-form">
        <input className="mono" list="override-codes" placeholder="HIGH.VALUE or a routine's code" value={newOverride} onChange={(e) => setNewOverride(e.target.value.toUpperCase())} />
        <datalist id="override-codes">{coreOverrides.map((c) => <option key={c} value={c} />)}</datalist>
        <Button type="button" variant="ghost" onClick={() => { if (newOverride) { setSpec({ ...spec, overrides: { ...overrides, [newOverride]: 'checker' } }); setNewOverride('') } }}>Add override class</Button>
      </div>

      <ErrorNote error={submit.error} />
      <div className="actions"><Button busy={submit.busy}>Input for authorisation</Button><Button type="button" variant="ghost" onClick={onDone}>Cancel</Button></div>
    </form>
  )
}
