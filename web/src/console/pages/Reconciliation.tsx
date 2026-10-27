import { useState } from 'react'
import { api } from '../api'
import { amount, day } from '../format'
import { useAction, useLoad } from '../hooks'
import type { ReconItem, ReconRun, SystemDates } from '../types'
import { PendingChanges } from '../pending'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel } from '../ui'

const sample = `provider_reference,type,amount,fee,currency
ch_a_REPLACEME,charge,100000,1800,KES`

export default function Reconciliation() {
  const dates = useLoad(() => api<SystemDates>('/core/dates'))
  const runs = useLoad(() => api<ReconRun[]>('/ops/reconciliation/runs'))
  const [provider, setProvider] = useState('acquirer-a')
  const [date, setDate] = useState('')
  const [csv, setCsv] = useState(sample)
  const [selected, setSelected] = useState<string>()

  const upload = useAction(() =>
    api<ReconRun>(`/ops/reconciliation/${provider}?date=${date || new Date().toISOString().slice(0, 10)}`, { method: 'POST', text: csv, contentType: 'text/csv' }),
  )
  const run = runs.data?.find((r) => r.id === selected) ?? runs.data?.[0]

  return (
    <>
      <PageHeader module="RC · RECONCILIATION" title="Provider reconciliation & settlement" />
      <div className="grid-side-main">
        <Panel title="Upload settlement report">
          <form className="form" onSubmit={async (e) => { e.preventDefault(); const r = await upload.run(); if (r) { setSelected(r.id); runs.reload() } }}>
            <div className="row-2">
              <Field label="Provider">
                <select value={provider} onChange={(e) => setProvider(e.target.value)}>
                  {['acquirer-a', 'acquirer-b', 'mpesa', 'pesalink'].map((p) => <option key={p}>{p}</option>)}
                </select>
              </Field>
              <Field label="Report date" hint="UTC date of the transactions">
                <input type="date" value={date} onChange={(e) => setDate(e.target.value)} placeholder={dates.data?.today} />
              </Field>
            </div>
            <Field label="CSV" hint="provider_reference, type (charge|refund|payout), amount, fee, currency — minor units">
              <textarea className="mono" rows={8} value={csv} onChange={(e) => setCsv(e.target.value)} />
            </Field>
            <ErrorNote error={upload.error} />
            <Button busy={upload.busy}>Reconcile</Button>
          </form>
          <h3 className="sub">Runs</h3>
          {runs.data?.length === 0 && <Empty>No runs yet.</Empty>}
          <ul className="run-list">
            {runs.data?.map((r) => (
              <li key={r.id}>
                <button className={run?.id === r.id ? 'on' : ''} onClick={() => setSelected(r.id)}>
                  <Mono>{r.provider}</Mono> · {day(r.reportDate)}
                  <Badge value={r.settlementEntryId ? 'Completed' : r.items.some((i) => i.outcome !== 'Matched' && !i.resolved) ? 'Unauthorised' : 'Pending'} />
                </button>
              </li>
            ))}
          </ul>
        </Panel>
        {run ? <RunDetail run={run} onChange={runs.reload} /> : <Panel><Empty>Upload a report to reconcile it against the ledger.</Empty></Panel>}
      </div>
    </>
  )
}

function RunDetail({ run, onChange }: { run: ReconRun; onChange: () => void }) {
  const [notes, setNotes] = useState<Record<string, string>>({})
  const resolve = useAction((item: ReconItem) => api(`/ops/reconciliation/runs/${run.id}/items/${item.id}/resolve`, { method: 'POST', body: { note: notes[item.id] || 'Investigated with provider' } }))
  const settle = useAction(() => api(`/ops/reconciliation/runs/${run.id}/settle`, { method: 'POST' }))
  const open = run.items.filter((i) => i.outcome !== 'Matched' && !i.resolved).length

  return (
    <Panel title={`${run.provider} · ${day(run.reportDate)}`} aside={
      run.settlementEntryId ? <Badge value="Completed" /> : <Button busy={settle.busy} disabled={open > 0} onClick={async () => { if (await settle.run()) onChange() }}>Input settlement (INAU)</Button>
    }>
      <p className="muted intro">
        {run.items.filter((i) => i.outcome === 'Matched').length} matched · {open} open breaks · run by {run.createdBy}.
        {open > 0 ? ' Every break must be resolved before settlement posts.' : run.settlementEntryId ? ' Settled: provider float moved to the bank, net of provider fees.' : ' Ready to settle.'}
      </p>
      <ErrorNote error={resolve.error ?? settle.error} />
      {!run.settlementEntryId && <PendingChanges types={['Settlement']} title="Settlement awaiting authorisation" onDecided={onChange} />}
      <table>
        <thead><tr><th>Reference</th><th>Kind</th><th className="num">Report</th><th className="num">Ledger</th><th className="num">Fee</th><th>Outcome</th><th>Resolution</th></tr></thead>
        <tbody>
          {run.items.map((i) => (
            <tr key={i.id}>
              <td><Mono>{i.providerReference}</Mono></td>
              <td>{i.kind}</td>
              <td className="num">{i.reportAmount != null ? amount(i.reportAmount, i.currency) : '—'}</td>
              <td className="num">{i.ledgerAmount != null ? amount(i.ledgerAmount, i.currency) : '—'}</td>
              <td className="num">{i.reportFee != null ? amount(i.reportFee, i.currency) : '—'}</td>
              <td><Badge value={i.outcome} /></td>
              <td>
                {i.outcome === 'Matched' ? null : i.resolved ? (
                  <span className="muted">{i.resolutionNote} — {i.resolvedBy}</span>
                ) : (
                  <span className="inline-form">
                    <input placeholder="Resolution note" value={notes[i.id] ?? ''} onChange={(e) => setNotes({ ...notes, [i.id]: e.target.value })} />
                    <Button variant="ghost" busy={resolve.busy} onClick={async () => { if (await resolve.run(i)) onChange() }}>Resolve</Button>
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}
