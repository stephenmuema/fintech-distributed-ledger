import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { Extension, LocalField, SuspenseType, SuspenseTypeSpec } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel } from '../ui'

const currencies = ['KES', 'UGX', 'TZS', 'NGN', 'USD', 'EUR', 'GBP', 'USDC']
const targets = ['Customer', 'Internal', 'PnL']

const blank: SuspenseTypeSpec = {
  name: '', description: '', side: 'Credit', currencies: ['KES'], warnAfterDays: 3, escalateAfterDays: 14, cobPolicy: 'Warn',
  writeOffAfterDays: 0, writeOffCategory: 60300, allowedTargets: ['Customer'], hooks: [], autoClearAt: 0, localFields: [],
}

type Chart = { categories: { code: number; name: string; glCode: string }[] }

export default function SuspenseTypes() {
  const types = useLoad(() => api<SuspenseType[]>('/ops/suspense/types'))
  const [editing, setEditing] = useState<{ code: string; spec: SuspenseTypeSpec; existing?: SuspenseType }>()
  const [version, setVersion] = useState(0)

  return (
    <>
      <PageHeader module="AC · SUSPENSE · PARAMETERS" title="Suspense types">
        <Link className="btn btn-ghost" to="/console/suspense">Suspense items</Link>
        <Button onClick={() => setEditing({ code: '', spec: { ...blank, localFields: [] } })}>New suspense type</Button>
      </PageHeader>
      <p className="muted intro">
        A suspense type tailors a suspense account to one job: which side it carries, how long items may age, what they may be cleared to,
        which extensions classify, match and vet them, and the local fields operators must capture. Types are parameters — a change is input
        here, authorised by a second operator, and versioned in record history. A new type gets its own category and one account per currency.
      </p>
      <div className="grid-main-side">
        <Panel title="Types">
          <ErrorNote error={types.error} />
          {types.data?.length === 0 && <Empty>No suspense types.</Empty>}
          {!!types.data?.length && (
            <table>
              <thead><tr><th>Code</th><th>Name</th><th>Side</th><th>Category · GL</th><th>Ageing</th><th>Targets</th><th>Extensions</th><th>CURR.NO</th><th /></tr></thead>
              <tbody>
                {types.data.map((t) => (
                  <tr key={t.id}>
                    <td><Mono>{t.code}</Mono> {t.builtIn && <span className="tag">BUILT-IN</span>}</td>
                    <td>{t.name} {!t.active && <Badge value="Retired" />}<br /><small className="muted">{t.description}</small></td>
                    <td>{t.side}</td>
                    <td><Mono>{t.category} · {t.glCode}</Mono></td>
                    <td className="nowrap">warn {t.warnAfterDays}d · escalate {t.escalateAfterDays || '—'}d{t.cobPolicy === 'HaltCob' && <> · <Badge value="Halts COB" /></>}
                      {t.writeOffAfterDays > 0 && <><br /><small className="muted">write off after {t.writeOffAfterDays}d</small></>}</td>
                    <td>{t.allowedTargets.join(', ')}</td>
                    <td>{t.hooks.length ? t.hooks.map((h) => <Mono key={h}>{h} </Mono>) : <span className="muted">—</span>}
                      {t.autoClearAt > 0 && <><br /><small className="muted">STP ≥ {t.autoClearAt}%</small></>}</td>
                    <td className="muted">{t.version}</td>
                    <td><button className="link-btn" onClick={() => setEditing({ code: t.code, spec: specOf(t), existing: t })}>Amend</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <div className="stack">
          {editing && <TypeForm key={editing.code || 'new'} initial={editing} onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />}
          <PendingChanges types={['SuspenseType']} version={version} onDecided={types.reload} />
        </div>
      </div>
    </>
  )
}

function specOf(t: SuspenseType): SuspenseTypeSpec {
  return {
    name: t.name, description: t.description, side: t.side, currencies: [...t.currencies], warnAfterDays: t.warnAfterDays, escalateAfterDays: t.escalateAfterDays,
    cobPolicy: t.cobPolicy, writeOffAfterDays: t.writeOffAfterDays, writeOffCategory: t.writeOffCategory || 60300, allowedTargets: [...t.allowedTargets],
    hooks: [...t.hooks], autoClearAt: t.autoClearAt, localFields: t.localFields.map((f) => ({ ...f })), active: t.active,
  }
}

function toggle(list: string[], value: string) {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value]
}

function TypeForm({ initial, onDone }: { initial: { code: string; spec: SuspenseTypeSpec; existing?: SuspenseType }; onDone: () => void }) {
  const extensions = useLoad(() => api<Extension[]>('/core/extensions'))
  const chart = useLoad(() => api<Chart>('/ops/gl/chart'))
  const [code, setCode] = useState(initial.code)
  const [spec, setSpec] = useState<SuspenseTypeSpec>(initial.spec)
  const amending = !!initial.existing
  const pnl = chart.data?.categories.filter((c) => c.code >= 50000 && c.code <= 69999) ?? []
  const submit = useAction(() =>
    api(`/ops/suspense/types/${code.trim().toUpperCase()}`, {
      method: 'POST',
      body: { ...spec, writeOffCategory: spec.writeOffAfterDays > 0 ? spec.writeOffCategory : 0 },
    }),
  )
  const set = <K extends keyof SuspenseTypeSpec>(key: K, value: SuspenseTypeSpec[K]) => setSpec({ ...spec, [key]: value })
  const setField = (i: number, patch: Partial<LocalField>) => set('localFields', spec.localFields.map((f, j) => (i === j ? { ...f, ...patch } : f)))

  return (
    <Panel title={amending ? `Amend ${initial.code}` : 'New suspense type'} aside={<span className="muted">INAU until authorised</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <div className="row-2">
          <Field label="Code" hint="3–16 upper-case letters or digits, e.g. UNIDMPESA">
            <input required readOnly={amending} value={code} onChange={(e) => setCode(e.target.value.toUpperCase())} />
          </Field>
          <Field label="Name"><input required value={spec.name} onChange={(e) => set('name', e.target.value)} /></Field>
        </div>
        <Field label="Purpose"><input value={spec.description} onChange={(e) => set('description', e.target.value)} placeholder="What lands here, and who clears it" /></Field>
        <div className="row-2">
          <Field label="Side" hint={spec.side === 'Credit' ? 'Money received without a known owner (liability, GL 2510)' : 'Money paid out without an explanation (asset, GL 1910)'}>
            <div className="segmented">
              {(['Credit', 'Debit'] as const).map((s) => (
                <button type="button" key={s} disabled={amending} className={spec.side === s ? 'on' : ''} onClick={() => set('side', s)}>{s}</button>
              ))}
            </div>
          </Field>
          <Field label="Currencies">
            <div className="chips">
              {currencies.map((c) => (
                <label key={c} className="check"><input type="checkbox" checked={spec.currencies.includes(c)} onChange={() => set('currencies', toggle(spec.currencies, c))} /> {c}</label>
              ))}
            </div>
          </Field>
        </div>
        <div className="row-3">
          <Field label="Warn after (days)"><input type="number" min={0} value={spec.warnAfterDays} onChange={(e) => set('warnAfterDays', Number(e.target.value))} /></Field>
          <Field label="Escalate after (days)"><input type="number" min={0} value={spec.escalateAfterDays} onChange={(e) => set('escalateAfterDays', Number(e.target.value))} /></Field>
          <Field label="At escalation">
            <select value={spec.cobPolicy} onChange={(e) => set('cobPolicy', e.target.value as SuspenseTypeSpec['cobPolicy'])}>
              <option value="Warn">Warn in EOD report</option>
              <option value="HaltCob">Halt Close of Business</option>
            </select>
          </Field>
        </div>
        <div className="row-2">
          <Field label="Write off after (days)" hint="0 = never; COB inputs the write-off, a human authorises it">
            <input type="number" min={0} value={spec.writeOffAfterDays} onChange={(e) => set('writeOffAfterDays', Number(e.target.value))} />
          </Field>
          <Field label="Write-off P&L category">
            <select disabled={spec.writeOffAfterDays === 0} value={spec.writeOffCategory} onChange={(e) => set('writeOffCategory', Number(e.target.value))}>
              {pnl.map((c) => <option key={c.code} value={c.code}>{c.code} — {c.name}</option>)}
            </select>
          </Field>
        </div>
        <Field label="May be cleared to">
          <div className="chips">
            {targets.map((t) => (
              <label key={t} className="check"><input type="checkbox" checked={spec.allowedTargets.includes(t)} onChange={() => set('allowedTargets', toggle(spec.allowedTargets, t))} /> {t === 'PnL' ? 'P&L' : t}</label>
            ))}
          </div>
        </Field>
        <Field label="Extensions" hint="Consulted to classify, match and vet this type's items">
          <div className="chips">
            {extensions.data?.length === 0 && <span className="muted">No extensions registered — see <Link to="/console/extensions">Extensions</Link>.</span>}
            {extensions.data?.map((x) => (
              <label key={x.name} className="check" title={x.hooks.join(', ')}>
                <input type="checkbox" checked={spec.hooks.includes(x.name)} onChange={() => set('hooks', toggle(spec.hooks, x.name))} /> <Mono>{x.name}</Mono>
              </label>
            ))}
          </div>
        </Field>
        <Field label="Straight-through clearing" hint="0 = every clearing needs a human checker; otherwise extension proposals at or above this confidence post automatically">
          <input type="number" min={0} max={100} value={spec.autoClearAt} onChange={(e) => set('autoClearAt', Number(e.target.value))} />
        </Field>

        <h3 className="sub">Local fields</h3>
        {spec.localFields.length === 0 && <p className="muted">Extra attributes operators and extensions capture on each item (T24 LOCAL.REF).</p>}
        {spec.localFields.map((f, i) => (
          <div key={i} className="field-row">
            <input placeholder="name" value={f.name} onChange={(e) => setField(i, { name: e.target.value })} />
            <input placeholder="Label" value={f.label} onChange={(e) => setField(i, { label: e.target.value })} />
            <select value={f.type} onChange={(e) => setField(i, { type: e.target.value as LocalField['type'] })}>
              {['text', 'number', 'date', 'choice'].map((t) => <option key={t}>{t}</option>)}
            </select>
            {f.type === 'choice' ? (
              <input placeholder="Options, comma separated" value={(f.options ?? []).join(', ')}
                onChange={(e) => setField(i, { options: e.target.value.split(',').map((o) => o.trim()).filter(Boolean) })} />
            ) : (
              <input placeholder="Pattern (regex)" value={f.pattern ?? ''} onChange={(e) => setField(i, { pattern: e.target.value })} />
            )}
            <label className="check"><input type="checkbox" checked={f.required} onChange={(e) => setField(i, { required: e.target.checked })} /> Required</label>
            <button type="button" className="link-btn" onClick={() => set('localFields', spec.localFields.filter((_, j) => j !== i))}>Remove</button>
          </div>
        ))}
        <div>
          <Button type="button" variant="ghost" onClick={() => set('localFields', [...spec.localFields, { name: '', label: '', type: 'text', required: false }])}>Add field</Button>
        </div>
        {amending && !initial.existing?.builtIn && (
          <label className="check"><input type="checkbox" checked={spec.active ?? true} onChange={(e) => set('active', e.target.checked)} /> Active (untick to retire: no new items)</label>
        )}
        <ErrorNote error={submit.error} />
        <div className="actions">
          <Button type="button" variant="ghost" onClick={onDone}>Cancel</Button>
          <Button busy={submit.busy}>Input {amending ? 'amendment' : 'type'} (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}
