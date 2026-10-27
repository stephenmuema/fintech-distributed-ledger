import { useState } from 'react'
import { api } from '../api'
import { PendingChanges } from '../pending'
import { dateTime } from '../format'
import { useAction, useLoad } from '../hooks'
import type { LocalColumn, LocalFieldType, LocalRecord, LocalTable, LocalTableSpec } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel } from '../ui'
import { LocalInput } from './Transact'

const types: LocalFieldType[] = ['text', 'number', 'amount', 'date', 'boolean', 'choice', 'lookup']

export default function LocalTables() {
  const tables = useLoad(() => api<LocalTable[]>('/ops/local-tables'))
  const [version, setVersion] = useState(0)
  const [editing, setEditing] = useState<LocalTable | 'application'>()
  const [open, setOpen] = useState<string>()
  const selected = tables.data?.find((t) => t.name === open)

  return (
    <>
      <PageHeader module="EB.LOCAL · LOCAL.TABLE" title="Local tables">
        <Button variant="ghost" onClick={() => setEditing('application')}>New application</Button>
      </PageHeader>
      <Panel>
        <p className="muted">
          Local fields are real columns. When a checker authorises a definition the core adds them to the table beside the record —
          <Mono> lt_customer</Mono>, <Mono>lt_account</Mono>, <Mono>lt_transaction</Mono> — or creates a local application's own table. Columns are only ever added:
          retire a field instead of removing it. Provider details tables (<Mono>pd_…</Mono>) follow their provider channel.
        </p>
        {tables.loading && <Loading />}
        <ErrorNote error={tables.error} />
        {!!tables.data?.length && (
          <table>
            <thead><tr><th>Name</th><th>Kind</th><th>Table</th><th>Fields</th><th>Columns</th><th /></tr></thead>
            <tbody>
              {tables.data.map((t) => (
                <tr key={t.name}>
                  <td><Mono>{t.name}</Mono><br /><span className="muted">{t.description}</span></td>
                  <td>{t.kind}{t.kind === 'application' && t.authorisers === 0 ? ' · no checker' : ''}</td>
                  <td><Mono>{t.tableName}</Mono></td>
                  <td>{t.fields.filter((f) => !f.retired).length}{t.fields.some((f) => f.retired) ? <span className="muted"> (+{t.fields.filter((f) => f.retired).length} retired)</span> : null}</td>
                  <td>{t.ready ? <Badge value="applied" /> : <Badge value={t.changes.some((c) => c.status === 'failed') ? 'failed' : 'pending'} />}</td>
                  <td className="actions">
                    <Button variant="ghost" onClick={() => setOpen(open === t.name ? undefined : t.name)}>{open === t.name ? 'Close' : 'Open'}</Button>
                    {t.kind !== 'details' && <Button variant="ghost" onClick={() => setEditing(t)}>Amend</Button>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {editing && (
          <TableForm key={editing === 'application' ? 'new' : editing.name} existing={editing === 'application' ? undefined : editing}
            onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />
        )}
      </Panel>
      {selected && <TableDetail table={selected} onChanged={tables.reload} />}
      {selected?.kind === 'application' && <Records table={selected} />}
      <PendingChanges types={['LocalTable', 'LocalRecord']} version={version} onDecided={tables.reload} />
    </>
  )
}

function TableDetail({ table, onChanged }: { table: LocalTable; onChanged: () => void }) {
  const ddl = useLoad(() => api<{ engine: string; statements: string[] }>(`/ops/local-tables/${table.name}/ddl`), [table.name, table.version, table.ready])
  const apply = useAction(() => api(`/ops/local-tables/${table.name}/apply`, { method: 'POST' }))
  return (
    <Panel title={`${table.name} · ${table.tableName}`} aside={<span className="muted">{table.kind}</span>}>
      <table>
        <thead><tr><th>Field</th><th>Type</th><th>Column</th><th>Status</th><th>Options</th></tr></thead>
        <tbody>
          {table.fields.map((f) => {
            const column = table.columns.find((c) => c.name === f.name)
            return (
              <tr key={f.name}>
                <td><Mono>{f.name}</Mono> <span className="muted">{f.label}</span> {f.retired && <Badge value="Retired" />}</td>
                <td>{f.type}{f.type === 'lookup' ? ` → ${f.lookup}` : ''}{f.type === 'number' && f.scale ? ` (${f.scale} dp)` : ''}</td>
                <td><Mono>{column?.columnName ?? '—'}</Mono></td>
                <td>{column ? <Badge value={column.status} /> : '—'} {column?.error && <span className="error-text">{column.error}</span>}</td>
                <td className="muted">{[f.required && 'required', f.searchable && 'searchable', f.pattern && `pattern ${f.pattern}`, f.options?.join(' | ')].filter(Boolean).join(' · ')}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
      {!table.ready && (
        <div className="callout warn">
          <span className="mono-label">DDL OUTSTANDING ({ddl.data?.engine})</span>
          <p className="muted">Zirnis runs these when a definition is authorised. Under <Mono>ZIRNIS_LOCAL_DDL=manual</Mono> a DBA runs them; then pick them up here.</p>
          <pre className="json ddl">{ddl.data?.statements.join(';\n\n')}</pre>
          {table.changes.filter((c) => c.error).map((c) => <p key={c.id} className="error-text">{c.target}: {c.error}</p>)}
          <ErrorNote error={apply.error} />
          <Button variant="accent" busy={apply.busy} onClick={async () => { if (await apply.run()) onChanged() }}>Apply now</Button>
        </div>
      )}
    </Panel>
  )
}

function TableForm({ existing, onDone }: { existing?: LocalTable; onDone: () => void }) {
  const [name, setName] = useState(existing?.name ?? '')
  const [spec, setSpec] = useState<LocalTableSpec>(existing ? { description: existing.description, kind: existing.kind, keyLabel: existing.keyLabel,
    keyPattern: existing.keyPattern, authorisers: existing.authorisers, fields: existing.fields.map((f) => ({ ...f })) }
    : { description: '', kind: 'application', keyLabel: 'Key', authorisers: 1, fields: [] })
  const known = new Set(existing?.fields.map((f) => f.name))
  const setField = (i: number, f: Partial<LocalColumn>) => setSpec({ ...spec, fields: spec.fields.map((x, j) => (j === i ? { ...x, ...f } : x)) })
  const submit = useAction(() => api(`/ops/local-tables/${name.trim().toUpperCase()}`, { method: 'POST', body: spec }))

  return (
    <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
      <span className="mono-label">{existing ? `AMEND ${existing.name}` : 'NEW LOCAL APPLICATION'}</span>
      <div className="row-3">
        <Field label="Name" hint={spec.kind === 'application' ? 'e.g. MERCHANT.CATEGORY' : undefined}><input className="mono" required disabled={!!existing} value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label="Description"><input required value={spec.description} onChange={(e) => setSpec({ ...spec, description: e.target.value })} /></Field>
        <Field label="Record changes"><select value={spec.authorisers} onChange={(e) => setSpec({ ...spec, authorisers: Number(e.target.value) })}><option value={1}>A checker authorises</option><option value={0}>Apply on input (audited)</option></select></Field>
      </div>
      {spec.kind === 'application' && (
        <div className="row-2">
          <Field label="Key label"><input value={spec.keyLabel ?? ''} onChange={(e) => setSpec({ ...spec, keyLabel: e.target.value })} /></Field>
          <Field label="Key pattern" hint="A regular expression"><input className="mono" value={spec.keyPattern ?? ''} onChange={(e) => setSpec({ ...spec, keyPattern: e.target.value })} /></Field>
        </div>
      )}
      <span className="mono-label">FIELDS</span>
      {spec.fields.map((f, i) => (
        <div key={i} className="design-row">
          <input className="mono" placeholder="name" disabled={known.has(f.name)} value={f.name} onChange={(e) => setField(i, { name: e.target.value })} />
          <input placeholder="Label" value={f.label} onChange={(e) => setField(i, { label: e.target.value })} />
          <select value={f.type} disabled={known.has(f.name)} onChange={(e) => setField(i, { type: e.target.value as LocalFieldType })}>{types.map((t) => <option key={t}>{t}</option>)}</select>
          {f.type === 'choice' ? <input placeholder="option, option" value={f.options?.join(', ') ?? ''} onChange={(e) => setField(i, { options: e.target.value.split(',').map((o) => o.trim()).filter(Boolean) })} />
            : f.type === 'lookup' ? <input className="mono" placeholder="APPLICATION" value={f.lookup ?? ''} onChange={(e) => setField(i, { lookup: e.target.value.toUpperCase() })} />
            : f.type === 'number' ? <input inputMode="numeric" placeholder="decimals" disabled={known.has(f.name)} value={f.scale ?? ''} onChange={(e) => setField(i, { scale: Number(e.target.value) || 0 })} />
            : <input placeholder="pattern" value={f.pattern ?? ''} onChange={(e) => setField(i, { pattern: e.target.value })} />}
          <span className="chips">
            <label className="check"><input type="checkbox" checked={!!f.required} onChange={(e) => setField(i, { required: e.target.checked })} /> required</label>
            <label className="check"><input type="checkbox" checked={!!f.searchable} disabled={known.has(f.name) && existing?.fields.find((x) => x.name === f.name)?.searchable} onChange={(e) => setField(i, { searchable: e.target.checked })} /> searchable</label>
          </span>
          {known.has(f.name)
            ? <label className="check"><input type="checkbox" checked={!!f.retired} onChange={(e) => setField(i, { retired: e.target.checked })} /> retired</label>
            : <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fields: spec.fields.filter((_, j) => j !== i) })}>Remove</Button>}
        </div>
      ))}
      <div className="actions"><Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fields: [...spec.fields, { name: '', label: '', type: 'text' }] })}>Add field</Button></div>
      <ErrorNote error={submit.error} />
      <div className="actions"><Button busy={submit.busy}>Input for authorisation</Button><Button type="button" variant="ghost" onClick={onDone}>Cancel</Button></div>
    </form>
  )
}

/** A local application's records, and the form that maintains one. */
function Records({ table }: { table: LocalTable }) {
  const records = useLoad(() => api<LocalRecord[]>(`/ops/local/${table.name}?limit=200`), [table.name])
  const fields = table.fields.filter((f) => !f.retired)
  const [key, setKey] = useState('')
  const [values, setValues] = useState<Record<string, string>>({})
  const save = useAction(() => api<LocalRecord | { status: string }>(`/ops/local/${table.name}/${encodeURIComponent(key.trim())}`, { method: 'POST', body: { values } }))
  const edit = (r: LocalRecord) => { setKey(r.key); setValues({ ...r.values }) }

  return (
    <Panel title={`${table.name} records`} aside={<span className="muted">{table.authorisers ? 'changes need a checker' : 'changes apply on input'}</span>}>
      {records.loading && <Loading />}
      <ErrorNote error={records.error} />
      {records.data?.length === 0 && <Empty>No records yet.</Empty>}
      {!!records.data?.length && (
        <table>
          <thead><tr><th>{table.keyLabel || 'Key'}</th>{fields.map((f) => <th key={f.name}>{f.label || f.name}</th>)}<th>Updated</th><th /></tr></thead>
          <tbody>
            {records.data.map((r) => (
              <tr key={r.key}>
                <td><Mono>{r.key}</Mono></td>
                {fields.map((f) => <td key={f.name}>{r.values[f.name] ?? '—'}</td>)}
                <td className="muted">{r.updatedBy} · {dateTime(r.updatedAt)}</td>
                <td className="actions"><Button variant="ghost" onClick={() => edit(r)}>Edit</Button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form className="form callout" onSubmit={async (e) => { e.preventDefault(); if (await save.run()) records.reload() }}>
        <span className="mono-label">RECORD</span>
        <Field label={table.keyLabel || 'Key'} hint={table.keyPattern}><input className="mono" required value={key} onChange={(e) => setKey(e.target.value)} /></Field>
        <div className="row-3">
          {fields.map((f) => (
            <Field key={f.name} label={`${f.label || f.name}${f.required ? ' *' : ''}`}>
              <LocalInput field={f} value={values[f.name] ?? ''} onChange={(v) => setValues({ ...values, [f.name]: v })} />
            </Field>
          ))}
        </div>
        <ErrorNote error={save.error} />
        <Button busy={save.busy}>{table.authorisers ? 'Input for authorisation' : 'Save'}</Button>
      </form>
    </Panel>
  )
}

/** The local fields of one customer or account, with an input form (maker-checker). */
export function LocalValues({ table, recordKey }: { table: 'CUSTOMER' | 'ACCOUNT'; recordKey: string }) {
  const definition = useLoad(() => api<LocalTable>(`/ops/local-tables/${table}`), [table])
  const record = useLoad(() => api<LocalRecord>(`/ops/local/${table}/${recordKey}`), [table, recordKey])
  const [values, setValues] = useState<Record<string, string>>()
  const save = useAction(() => api(`/ops/local/${table}/${recordKey}`, { method: 'POST', body: { values } }))
  const live = definition.data?.fields.filter((f) => !f.retired && definition.data?.columns.find((c) => c.name === f.name)?.status === 'applied') ?? []
  if (!live.length) return null
  const current = values ?? record.data?.values ?? {}
  return (
    <Panel title="Local fields" aside={<span className="muted">{table === 'CUSTOMER' ? 'lt_customer' : 'lt_account'} · a checker authorises changes</span>}>
      <ErrorNote error={record.error} />
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await save.run()) { setValues(undefined); record.reload() } }}>
        <div className="row-3">
          {live.map((f) => (
            <Field key={f.name} label={`${f.label || f.name}${f.required ? ' *' : ''}`}>
              <LocalInput field={f} value={current[f.name] ?? ''} onChange={(v) => setValues({ ...current, [f.name]: v })} />
            </Field>
          ))}
        </div>
        <ErrorNote error={save.error} />
        {values && <Button busy={save.busy}>Input for authorisation</Button>}
        {save.result !== undefined && !values && <p className="muted">Input — waiting for a checker under Local tables.</p>}
      </form>
    </Panel>
  )
}
