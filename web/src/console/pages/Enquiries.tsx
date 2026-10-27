import { useState } from 'react'
import { api, download } from '../api'
import { PendingChanges } from '../pending'
import { useAction, useLoad } from '../hooks'
import type { Enquiry, EnquiryResult, EnquirySpec, Operand } from '../types'
import { Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel } from '../ui'

const operands: Operand[] = ['EQ', 'NE', 'GT', 'GE', 'LT', 'LE', 'LK', 'RG', 'IN']
const operandHelp: Record<Operand, string> = { EQ: 'equals', NE: 'not', GT: 'after / above', GE: 'from', LT: 'before / below', LE: 'up to', LK: 'contains', RG: 'from,to', IN: 'a,b,c' }

export default function Enquiries() {
  const enquiries = useLoad(() => api<Enquiry[]>('/ops/enquiries'))
  const [name, setName] = useState<string>()
  const [editing, setEditing] = useState<Enquiry | 'new'>()
  const [version, setVersion] = useState(0)
  const selected = enquiries.data?.find((e) => e.name === name)

  return (
    <>
      <PageHeader module="ENQUIRY" title="Enquiries">
        <Button variant="ghost" onClick={() => setEditing('new')}>New enquiry</Button>
      </PageHeader>
      <div className="grid-side-main">
        <Panel title="Saved enquiries">
          {enquiries.loading && <Loading />}
          <ErrorNote error={enquiries.error} />
          <div className="version-list">
            {enquiries.data?.map((e) => (
              <button key={e.name} type="button" className={`version-pick ${e.name === name ? 'on' : ''}`} onClick={() => setName(e.name)}>
                <Mono>{e.name}</Mono>
                <span>{e.description}</span>
                <small className="muted">{e.application}</small>
              </button>
            ))}
          </div>
        </Panel>
        {selected ? <Runner key={selected.name + selected.version} enquiry={selected} onEdit={() => setEditing(selected)} /> : <Panel title="Run"><Empty>Pick an enquiry.</Empty></Panel>}
      </div>
      {editing && (
        <Designer key={editing === 'new' ? 'new' : editing.name} existing={editing === 'new' ? undefined : editing}
          onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />
      )}
      <PendingChanges types={['Enquiry']} version={version} onDecided={enquiries.reload} />
    </>
  )
}

function Runner({ enquiry, onEdit }: { enquiry: Enquiry; onEdit: () => void }) {
  const prompts = enquiry.criteria.prompts ?? []
  const [values, setValues] = useState<Record<string, string>>({})
  const query = () => new URLSearchParams(Object.entries(values).filter(([, v]) => v.trim())).toString()
  const run = useAction(() => api<EnquiryResult>(`/ops/enquiries/${enquiry.name}/run?${query()}`))
  const csv = useAction(() => download(`/ops/enquiries/${enquiry.name}/run.csv?${query()}`))
  const result = run.result

  return (
    <Panel title={enquiry.name} aside={<Button variant="ghost" onClick={onEdit}>Amend</Button>}>
      <p className="muted">{enquiry.description} · {enquiry.application}
        {enquiry.criteria.fixed?.length ? <> · always {enquiry.criteria.fixed.map((f) => `${f.field} ${f.op} ${f.value}`).join(', ')}</> : null}</p>
      <form className="form" onSubmit={(e) => { e.preventDefault(); void run.run() }}>
        {prompts.length > 0 && (
          <div className="row-3">
            {prompts.map((p) => (
              <Field key={p.field} label={`${p.label || p.field}${p.mandatory ? ' *' : ''}`} hint={`${p.op} · ${operandHelp[p.op]}`}>
                <input required={p.mandatory} value={values[p.field] ?? ''} onChange={(e) => setValues({ ...values, [p.field]: e.target.value })} />
              </Field>
            ))}
          </div>
        )}
        <ErrorNote error={run.error ?? csv.error} />
        <div className="actions">
          <Button busy={run.busy}>Run</Button>
          <Button type="button" variant="ghost" busy={csv.busy} onClick={() => void csv.run()}>Download CSV</Button>
        </div>
      </form>
      {result && (
        <>
          {result.rows.length === 0 && <Empty>No rows.</Empty>}
          {result.rows.length > 0 && (
            <table>
              <thead><tr>{result.columns.map((c) => <th key={c.field}>{c.label || c.field}</th>)}</tr></thead>
              <tbody>
                {result.rows.map((row, i) => (
                  <tr key={i}>{result.columns.map((c) => <td key={c.field}>{row[c.field] === null || row[c.field] === undefined ? '—' : String(row[c.field])}</td>)}</tr>
                ))}
              </tbody>
            </table>
          )}
          {result.truncated && <p className="muted">More rows matched than the enquiry returns ({enquiry.maxRows}); narrow the selection.</p>}
        </>
      )}
    </Panel>
  )
}

function Designer({ existing, onDone }: { existing?: Enquiry; onDone: () => void }) {
  const [name, setName] = useState(existing?.name ?? '')
  const [spec, setSpec] = useState<EnquirySpec>(existing ? { description: existing.description, application: existing.application,
    fixed: existing.criteria.fixed ?? [], prompts: existing.criteria.prompts ?? [], columns: existing.output.columns, sort: existing.output.sort ?? [], maxRows: existing.maxRows }
    : { description: '', application: 'TRANSACTION', fixed: [], prompts: [], columns: [{ field: 'reference' }], sort: [], maxRows: 200 })
  const submit = useAction(() => api(`/ops/enquiries/${name.trim().toUpperCase()}`, { method: 'POST', body: spec }))
  const fixed = spec.fixed ?? []
  const prompts = spec.prompts ?? []
  const sort = spec.sort ?? []

  return (
    <Panel title={existing ? `Amend ${existing.name}` : 'New enquiry'}>
      <p className="muted">
        Fields are a transaction's, customer's or account's own (e.g. <Mono>reference</Mono>, <Mono>debitAccount</Mono>, <Mono>legalName</Mono>), its local fields as
        <Mono> local.name</Mono>, and on transactions a provider's payment details as <Mono>details.mpesa.receipt</Mono>. A local application's fields are named directly, with <Mono>key</Mono>.
      </p>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <div className="row-4">
          <Field label="Name"><input className="mono" required disabled={!!existing} value={name} onChange={(e) => setName(e.target.value)} placeholder="TXN.BY.MPESA.RECEIPT" /></Field>
          <Field label="Application" hint="TRANSACTION, CUSTOMER, ACCOUNT or a local application">
            <input className="mono" required value={spec.application} onChange={(e) => setSpec({ ...spec, application: e.target.value.toUpperCase() })} />
          </Field>
          <Field label="Description"><input required value={spec.description} onChange={(e) => setSpec({ ...spec, description: e.target.value })} /></Field>
          <Field label="Max rows"><input inputMode="numeric" value={spec.maxRows ?? 200} onChange={(e) => setSpec({ ...spec, maxRows: Number(e.target.value) || 200 })} /></Field>
        </div>

        <span className="mono-label">FIXED SELECTION — always applied</span>
        {fixed.map((f, i) => (
          <div key={i} className="design-row">
            <input className="mono" placeholder="field" value={f.field} onChange={(e) => setSpec({ ...spec, fixed: fixed.map((x, j) => (j === i ? { ...x, field: e.target.value } : x)) })} />
            <select value={f.op} onChange={(e) => setSpec({ ...spec, fixed: fixed.map((x, j) => (j === i ? { ...x, op: e.target.value as Operand } : x)) })}>{operands.map((o) => <option key={o}>{o}</option>)}</select>
            <input placeholder="value" value={f.value} onChange={(e) => setSpec({ ...spec, fixed: fixed.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)) })} />
            <span /><span />
            <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fixed: fixed.filter((_, j) => j !== i) })}>Remove</Button>
          </div>
        ))}
        <div className="actions"><Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, fixed: [...fixed, { field: '', op: 'EQ', value: '' }] })}>Add criterion</Button></div>

        <span className="mono-label">PROMPTS — filled in when run</span>
        {prompts.map((p, i) => (
          <div key={i} className="design-row">
            <input className="mono" placeholder="field" value={p.field} onChange={(e) => setSpec({ ...spec, prompts: prompts.map((x, j) => (j === i ? { ...x, field: e.target.value } : x)) })} />
            <select value={p.op} onChange={(e) => setSpec({ ...spec, prompts: prompts.map((x, j) => (j === i ? { ...x, op: e.target.value as Operand } : x)) })}>{operands.map((o) => <option key={o}>{o}</option>)}</select>
            <input placeholder="label" value={p.label ?? ''} onChange={(e) => setSpec({ ...spec, prompts: prompts.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)) })} />
            <label className="check"><input type="checkbox" checked={!!p.mandatory} onChange={(e) => setSpec({ ...spec, prompts: prompts.map((x, j) => (j === i ? { ...x, mandatory: e.target.checked } : x)) })} /> mandatory</label>
            <span />
            <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, prompts: prompts.filter((_, j) => j !== i) })}>Remove</Button>
          </div>
        ))}
        <div className="actions"><Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, prompts: [...prompts, { field: '', op: 'EQ' }] })}>Add prompt</Button></div>

        <span className="mono-label">COLUMNS</span>
        {spec.columns.map((c, i) => (
          <div key={i} className="design-row">
            <input className="mono" placeholder="field" value={c.field} onChange={(e) => setSpec({ ...spec, columns: spec.columns.map((x, j) => (j === i ? { ...x, field: e.target.value } : x)) })} />
            <input placeholder="heading" value={c.label ?? ''} onChange={(e) => setSpec({ ...spec, columns: spec.columns.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)) })} />
            <span /><span /><span />
            <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, columns: spec.columns.filter((_, j) => j !== i) })}>Remove</Button>
          </div>
        ))}
        <div className="actions"><Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, columns: [...spec.columns, { field: '' }] })}>Add column</Button></div>

        <span className="mono-label">SORT</span>
        {sort.map((s, i) => (
          <div key={i} className="design-row">
            <input className="mono" placeholder="field" value={s.field} onChange={(e) => setSpec({ ...spec, sort: sort.map((x, j) => (j === i ? { ...x, field: e.target.value } : x)) })} />
            <label className="check"><input type="checkbox" checked={!!s.desc} onChange={(e) => setSpec({ ...spec, sort: sort.map((x, j) => (j === i ? { ...x, desc: e.target.checked } : x)) })} /> descending</label>
            <span /><span /><span />
            <Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, sort: sort.filter((_, j) => j !== i) })}>Remove</Button>
          </div>
        ))}
        <div className="actions"><Button type="button" variant="ghost" onClick={() => setSpec({ ...spec, sort: [...sort, { field: '' }] })}>Add sort key</Button></div>

        <ErrorNote error={submit.error} />
        <div className="actions"><Button busy={submit.busy}>Input for authorisation</Button><Button type="button" variant="ghost" onClick={onDone}>Cancel</Button></div>
      </form>
    </Panel>
  )
}
