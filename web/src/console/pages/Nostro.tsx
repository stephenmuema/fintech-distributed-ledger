import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { amount, dateTime, money, shortDay } from '../format'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { BookLine, Correspondent, Nostro as NostroView, NostroStatement, Proof, NostroLine, StatementDetail, SuspenseType } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel, Stat } from '../ui'

const sampleMt940 = `:20:STMT261028
:25:36001234
:28C:00001/001
:60F:C261027USD0,00
:61:2610281028C500,00NTRFFT26301ABCDE//BANK0001
:86:FUNDING
:61:2610281028D12,50NCHGNONREF//BANK0002
:86:WIRE FEE
:62F:C261028USD487,50`

export default function Nostro() {
  const [live, setLive] = useState(false)
  const nostros = useLoad(() => api<NostroView[]>(`/ops/nostros${live ? '?live=true' : ''}`), [live])
  const [selected, setSelected] = useState<string>()
  const [statementId, setStatementId] = useState<string>()
  const [version, setVersion] = useState(0)
  const nostro = nostros.data?.find((n) => n.id === selected)

  const totals = useMemo(() => {
    const rows = nostros.data ?? []
    return {
      lines: rows.reduce((n, r) => n + r.unmatchedStatementLines, 0),
      book: rows.reduce((n, r) => n + r.unmatchedBookEntries, 0),
      breaking: rows.filter((r) => r.lastStatement?.proof && !r.lastStatement.proof.closes).length,
      live: rows.filter((r) => r.liveDifference != null && r.liveDifference !== 0).length,
    }
  }, [nostros.data])

  return (
    <>
      <PageHeader module="NV · NOSTRO" title="Nostro reconciliation">
        <label className="check"><input type="checkbox" checked={live} onChange={(e) => setLive(e.target.checked)} /> Ask correspondents for live balances</label>
        <Link className="btn btn-ghost" to="/console/vostro">Vostro accounts</Link>
      </PageHeader>
      <p className="muted intro">
        Our money at banks, PSPs and mobile operators. Each nostro mirrors a ledger account; correspondents' statements (MT940/MT950, camt.053, CSV)
        are matched line by line — by our reference, then amount and value date, then by the nostro's extensions, then by hand. Entries our books
        still cannot explain are parked to suspense, so the books reflect the correspondent and the break is worked as a suspense item.
      </p>
      <div className="stat-grid">
        <Stat label="Nostros" value={nostros.data?.length ?? '—'} />
        <Stat label="Statement lines open" value={totals.lines} tone={totals.lines ? 'warn' : 'good'} hint="on statements, not yet in our books" />
        <Stat label="Book entries open" value={totals.book} tone={totals.book ? 'warn' : 'good'} hint="in our books, not yet on a statement" />
        <Stat label="Proofs not closing" value={totals.breaking} tone={totals.breaking ? 'bad' : 'good'} hint={live ? `${totals.live} live balance difference(s)` : 'latest statement per nostro'} />
      </div>

      <Panel title="Positions">
        <ErrorNote error={nostros.error} />
        {nostros.data?.length === 0 && <Empty>No nostros yet.</Empty>}
        {!!nostros.data?.length && (
          <table>
            <thead><tr><th>Nostro</th><th>Correspondent</th><th>Ledger account</th><th className="num">Book balance</th>{live && <th className="num">Live · difference</th>}<th>Last statement</th><th className="num">Open</th><th>Proof</th></tr></thead>
            <tbody>
              {nostros.data.map((n) => (
                <tr key={n.id} className={`clickable ${selected === n.id ? 'selected' : ''}`} onClick={() => { setSelected(n.id); setStatementId(undefined) }}>
                  <td><Mono>{n.code}</Mono> {!n.active && <Badge value="Closed" />}</td>
                  <td>{n.correspondentName}<br /><small className="muted">{n.externalAccount || 'no external account set'}</small></td>
                  <td><Mono>{n.ledgerAccountNumber}</Mono></td>
                  <td className="num">{money(n.ledgerBalance, n.currency)}</td>
                  {live && <td className="num">{n.liveBalance != null ? <>{amount(n.liveBalance, n.currency)} · <span className={n.liveDifference ? 'error-text' : 'good-text'}>{amount(n.liveDifference ?? 0, n.currency)}</span></> : <span className="muted">{n.liveError || (n.live ? '—' : 'simulated')}</span>}</td>}
                  <td>{n.lastStatement ? <>{shortDay(n.lastStatement.toDate)} <Badge value={n.lastStatement.status} /></> : <span className="muted">none</span>}</td>
                  <td className="num">{n.unmatchedStatementLines} / {n.unmatchedBookEntries}</td>
                  <td>{n.lastStatement?.proof ? <Badge value={n.lastStatement.proof.closes ? 'Closes' : 'Break'} /> : <span className="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {nostro && (
        <div className="grid-side-main">
          <div className="stack">
            <Upload nostro={nostro} onReceived={(d) => { setStatementId(d.statement.id); nostros.reload() }} />
            <Statements nostro={nostro} selected={statementId} onSelect={setStatementId} version={version} />
          </div>
          {statementId ? <StatementPanel id={statementId} nostro={nostro} onChange={() => { nostros.reload(); setVersion((v) => v + 1) }} />
            : <Panel><Empty>Receive a statement, or pick one, to reconcile it.</Empty></Panel>}
        </div>
      )}

      <div className="grid-2">
        <Setup onInput={() => setVersion((v) => v + 1)} />
        <PendingChanges types={['Correspondent', 'NostroAccount']} version={version} onDecided={nostros.reload} />
      </div>
    </>
  )
}

function Upload({ nostro, onReceived }: { nostro: NostroView; onReceived: (d: StatementDetail) => void }) {
  const [form, setForm] = useState({ format: 'mt940', content: sampleMt940.replaceAll('USD', nostro.currency), opening: '', closing: '' })
  const csv = form.format === 'csv'
  const receive = useAction(() =>
    api<StatementDetail>(`/ops/nostros/${nostro.id}/statements`, {
      method: 'POST',
      body: { format: form.format, content: form.content, openingBalance: csv && form.opening ? Number(form.opening) : undefined, closingBalance: csv && form.closing ? Number(form.closing) : undefined },
    }),
  )
  return (
    <Panel title={`Receive a ${nostro.code} statement`}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); const d = await receive.run(); if (d) onReceived(d) }}>
        <div className="segmented">
          {[['mt940', 'MT940 / MT950'], ['camt053', 'camt.053'], ['csv', 'CSV']].map(([value, text]) => (
            <button type="button" key={value} className={form.format === value ? 'on' : ''} onClick={() => setForm({ ...form, format: value, content: value === 'csv' ? 'value_date,direction,amount,bank_reference,customer_reference,narrative\n' : form.content })}>{text}</button>
          ))}
        </div>
        <Field label="Statement" hint={csv ? 'value_date, direction (C/D), amount (minor units), bank_reference, customer_reference, narrative' : 'Paste the file as received'}>
          <textarea className="mono" rows={10} value={form.content} onChange={(e) => setForm({ ...form, content: e.target.value })} />
        </Field>
        {csv && (
          <div className="row-2">
            <Field label="Opening balance (minor units)" hint="positive = in our favour"><input value={form.opening} onChange={(e) => setForm({ ...form, opening: e.target.value })} /></Field>
            <Field label="Closing balance (minor units)"><input value={form.closing} onChange={(e) => setForm({ ...form, closing: e.target.value })} /></Field>
          </div>
        )}
        <ErrorNote error={receive.error} />
        <Button busy={receive.busy}>Receive & reconcile</Button>
        {receive.result?.reconciliation && (
          <p className="muted">Matched {receive.result.reconciliation.byReference} by reference, {receive.result.reconciliation.byAmountAndDate} by amount and date, {receive.result.reconciliation.byExtension} by extension.</p>
        )}
      </form>
    </Panel>
  )
}

function Statements({ nostro, selected, onSelect, version }: { nostro: NostroView; selected?: string; onSelect: (id: string) => void; version: number }) {
  const statements = useLoad(() => api<NostroStatement[]>(`/ops/nostros/${nostro.id}/statements`), [nostro.id, version])
  return (
    <Panel title="Statements" aside={nostro.reconciledFrom ? <span className="muted">reconciled from {shortDay(nostro.reconciledFrom)}</span> : undefined}>
      {statements.data?.length === 0 && <Empty>No statements received.</Empty>}
      <ul className="run-list">
        {statements.data?.map((s) => (
          <li key={s.id}>
            <button className={selected === s.id ? 'on' : ''} onClick={() => onSelect(s.id)}>
              <span><Mono>{s.statementNumber || s.format}</Mono> · {shortDay(s.fromDate)}–{shortDay(s.toDate)} · {s.lineCount} lines</span>
              <Badge value={s.status} />
            </button>
          </li>
        ))}
      </ul>
      {nostro.openingBreak !== 0 && <p className="callout warn">Opening break at cut-over: {money(nostro.openingBreak, nostro.currency)} — carried in every proof until explained.</p>}
    </Panel>
  )
}

function StatementPanel({ id, nostro, onChange }: { id: string; nostro: NostroView; onChange: () => void }) {
  const detail = useLoad(() => api<StatementDetail>(`/ops/nostro-statements/${id}`), [id])
  const [pickedLines, setPickedLines] = useState<string[]>([])
  const [pickedBook, setPickedBook] = useState<string[]>([])
  const reload = () => {
    setPickedLines([])
    setPickedBook([])
    detail.reload()
    onChange()
  }
  const match = useAction(() => api(`/ops/nostros/${nostro.id}/matches`, { method: 'POST', body: { statementLineIds: pickedLines, bookLineIds: pickedBook } }))
  const unmatch = useAction((matchId: string) => api(`/ops/nostros/${nostro.id}/matches/${matchId}/unmatch`, { method: 'POST' }))
  const park = useAction((lineId: string) => api(`/ops/nostros/${nostro.id}/lines/${lineId}/park`, { method: 'POST' }))
  const rematch = useAction(() => api(`/ops/nostros/${nostro.id}/reconcile`, { method: 'POST' }))

  const d = detail.data
  if (detail.error) return <Panel><ErrorNote error={detail.error} /></Panel>
  if (!d) return <Panel><Empty>Loading statement…</Empty></Panel>
  const ccy = d.statement.currency
  const lineIn = (l: NostroLine) => (l.direction === 'Credit' ? l.amount : -l.amount)
  const bookIn = (b: BookLine) => (b.direction === 'Debit' ? b.amount : -b.amount)
  const selectedLines = d.lines.filter((l) => pickedLines.includes(l.id)).reduce((n, l) => n + lineIn(l), 0)
  const selectedBook = d.openBookEntries.filter((b) => pickedBook.includes(b.lineId)).reduce((n, b) => n + bookIn(b), 0)
  const balanced = pickedLines.length + pickedBook.length > 0 && selectedLines === selectedBook
  const error = match.error ?? unmatch.error ?? park.error ?? rematch.error

  return (
    <Panel title={`${d.nostroCode} · statement ${d.statement.statementNumber || ''}`} aside={
      <span className="actions">
        <Badge value={d.statement.status} />
        <Button variant="ghost" busy={rematch.busy} onClick={async () => { if (await rematch.run()) reload() }}>Re-match</Button>
      </span>
    }>
      <p className="muted">{d.statement.format.toUpperCase()} · {shortDay(d.statement.fromDate)} to {shortDay(d.statement.toDate)} · received by {d.statement.receivedBy} {dateTime(d.statement.receivedAt)}
        {d.statement.openingBalance != null && <> · opening {money(d.statement.openingBalance, ccy)}</>}</p>
      {d.statement.proof ? <ProofTable proof={d.statement.proof} currency={ccy} /> : <p className="callout warn">No closing balance on this statement, so it cannot be proved.</p>}
      <ErrorNote error={error} />

      <div className="match-grid">
        <div>
          <h3 className="sub">Statement lines</h3>
          <table>
            <thead><tr><th /><th>Value</th><th>Reference</th><th className="num">In / out</th><th>Status</th></tr></thead>
            <tbody>
              {d.lines.map((l) => (
                <tr key={l.id} className={pickedLines.includes(l.id) ? 'selected' : ''}>
                  <td>{l.status === 'Unmatched' && <input type="checkbox" checked={pickedLines.includes(l.id)} onChange={() => setPickedLines(toggle(pickedLines, l.id))} />}</td>
                  <td className="nowrap">{shortDay(l.valueDate)}</td>
                  <td><Mono>{l.customerReference || l.bankReference || '—'}</Mono><br /><small className="muted">{l.narrative}</small></td>
                  <td className={`num ${l.direction === 'Credit' ? 'credit' : 'debit'}`}>{l.direction === 'Credit' ? '+' : '−'}{amount(l.amount, ccy)}</td>
                  <td>
                    <Badge value={l.status} />
                    {l.status === 'Unmatched' && <> <button className="link-btn" onClick={async () => { if (await park.run(l.id)) reload() }}>park</button></>}
                    {l.suspenseItemId && <> <Link to="/console/suspense">item</Link></>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div>
          <h3 className="sub">Open book entries</h3>
          {d.openBookEntries.length === 0 && <Empty>Every posting on this nostro is explained.</Empty>}
          {d.openBookEntries.length > 0 && (
            <table>
              <thead><tr><th /><th>Booked</th><th>Transaction</th><th className="num">In / out</th></tr></thead>
              <tbody>
                {d.openBookEntries.map((b) => (
                  <tr key={b.lineId} className={pickedBook.includes(b.lineId) ? 'selected' : ''}>
                    <td><input type="checkbox" checked={pickedBook.includes(b.lineId)} onChange={() => setPickedBook(toggle(pickedBook, b.lineId))} /></td>
                    <td className="nowrap">{shortDay(b.bookingDate)}</td>
                    <td><Mono>{b.transactionReference}</Mono><br /><small className="muted">{b.description}</small></td>
                    <td className={`num ${b.direction === 'Debit' ? 'credit' : 'debit'}`}>{b.direction === 'Debit' ? '+' : '−'}{amount(b.amount, ccy)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
      {(pickedLines.length > 0 || pickedBook.length > 0) && (
        <div className="callout">
          <p>Selected: statement {amount(selectedLines, ccy)} · book {amount(selectedBook, ccy)} {balanced ? <Badge value="Matched" /> : <span className="error-text">— must agree exactly</span>}</p>
          <div className="actions"><Button busy={match.busy} disabled={!balanced} onClick={async () => { if (await match.run()) reload() }}>Match selected</Button></div>
        </div>
      )}

      <h3 className="sub">Matches</h3>
      {d.matches.length === 0 && <Empty>None yet.</Empty>}
      {d.matches.map((m) => (
        <div key={m.id} className="approval">
          <div>
            <Badge value={m.method} /> {m.statementLineIds.length} statement line(s) ↔ {m.bookLines.map((b) => b.transactionReference).join(', ') || 'no book entry'}
            <p className="muted">by {m.matchedBy}{m.confidence != null && ` · ${m.confidence}% confident`} · {dateTime(m.createdAt)}</p>
          </div>
          {m.method !== 'Parked' && <div className="actions"><Button variant="ghost" busy={unmatch.busy} onClick={async () => { if (await unmatch.run(m.id)) reload() }}>Unmatch</Button></div>}
        </div>
      ))}
    </Panel>
  )
}

function toggle(list: string[], value: string) {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value]
}

function ProofTable({ proof, currency }: { proof: Proof; currency: string }) {
  const row = (label: string, value: number, sign = '') => (
    <tr><td>{label}</td><td className="num">{sign}{amount(value, currency)}</td></tr>
  )
  return (
    <table className="proof">
      <tbody>
        {row(`Book balance at ${shortDay(proof.asOf)}`, proof.ledgerBalance)}
        {proof.openingBreak !== 0 && row('Opening break at cut-over', proof.openingBreak, '+ ')}
        {row('Statement credits not in our books', proof.unmatchedStatementCredits, '+ ')}
        {row('Statement debits not in our books', proof.unmatchedStatementDebits, '− ')}
        {row('Book debits not on a statement', proof.unmatchedBookDebits, '− ')}
        {row('Book credits not on a statement', proof.unmatchedBookCredits, '+ ')}
        <tr className="total"><td>Statement closing balance</td><td className="num">{amount(proof.statementClosing, currency)}</td></tr>
        <tr className="total"><td>Difference</td><td className={`num ${proof.closes ? 'good-text' : 'error-text'}`}>{amount(proof.difference, currency)} {proof.closes ? '✓ proof closes' : ''}</td></tr>
      </tbody>
    </table>
  )
}

function Setup({ onInput }: { onInput: () => void }) {
  const correspondents = useLoad(() => api<Correspondent[]>('/ops/correspondents'))
  const types = useLoad(() => api<SuspenseType[]>('/ops/suspense/types'))
  const [c, setC] = useState({ code: '', name: '', kind: 'Bank', bic: '', country: 'KE', provider: '' })
  const [n, setN] = useState({ correspondent: '', currency: 'USD', externalAccount: '', ledgerAccountNumber: '', parkAfterDays: '1', creditSuspenseType: 'NOSTROCR', debitSuspenseType: 'NOSTRODR', proofPolicy: 'Warn' })
  const addCorrespondent = useAction(() => api(`/ops/correspondents/${c.code.toUpperCase()}`, { method: 'POST', body: { name: c.name, kind: c.kind, bic: c.bic, country: c.country, provider: c.provider } }))
  const addNostro = useAction(() => api('/ops/nostros', { method: 'POST', body: { ...n, parkAfterDays: Number(n.parkAfterDays) } }))
  const credit = types.data?.filter((t) => t.side === 'Credit') ?? []
  const debit = types.data?.filter((t) => t.side === 'Debit') ?? []
  return (
    <Panel title="Correspondents & nostros" aside={<span className="muted">parameters · INAU</span>}>
      <table>
        <thead><tr><th>Code</th><th>Name</th><th>Kind</th><th>BIC</th><th>Connector</th></tr></thead>
        <tbody>
          {correspondents.data?.map((x) => (
            <tr key={x.id}><td><Mono>{x.code}</Mono></td><td>{x.name}</td><td>{x.kind}</td><td><Mono>{x.bic || '—'}</Mono></td><td>{x.provider || <span className="muted">—</span>}</td></tr>
          ))}
        </tbody>
      </table>
      <h3 className="sub">New correspondent</h3>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await addCorrespondent.run()) { onInput(); correspondents.reload() } }}>
        <div className="row-3">
          <Field label="Code"><input required value={c.code} onChange={(e) => setC({ ...c, code: e.target.value.toUpperCase() })} placeholder="CITIUS" /></Field>
          <Field label="Name"><input required value={c.name} onChange={(e) => setC({ ...c, name: e.target.value })} /></Field>
          <Field label="Kind"><select value={c.kind} onChange={(e) => setC({ ...c, kind: e.target.value })}>{['Bank', 'PSP', 'MNO'].map((k) => <option key={k}>{k}</option>)}</select></Field>
        </div>
        <div className="row-3">
          <Field label="BIC"><input value={c.bic} onChange={(e) => setC({ ...c, bic: e.target.value.toUpperCase() })} placeholder="CITIUS33" /></Field>
          <Field label="Country"><input required maxLength={2} value={c.country} onChange={(e) => setC({ ...c, country: e.target.value.toUpperCase() })} /></Field>
          <Field label="Connector" hint="optional, for live balances"><input value={c.provider} onChange={(e) => setC({ ...c, provider: e.target.value })} placeholder="equity" /></Field>
        </div>
        <ErrorNote error={addCorrespondent.error} />
        <Button busy={addCorrespondent.busy}>Input correspondent (INAU)</Button>
      </form>
      <h3 className="sub">New nostro</h3>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await addNostro.run()) onInput() }}>
        <div className="row-3">
          <Field label="Correspondent">
            <select required value={n.correspondent} onChange={(e) => setN({ ...n, correspondent: e.target.value })}>
              <option value="">Choose</option>
              {correspondents.data?.map((x) => <option key={x.code} value={x.code}>{x.code}</option>)}
            </select>
          </Field>
          <Field label="Currency"><select value={n.currency} onChange={(e) => setN({ ...n, currency: e.target.value })}>{['KES', 'UGX', 'TZS', 'NGN', 'USD', 'EUR', 'GBP'].map((x) => <option key={x}>{x}</option>)}</select></Field>
          <Field label="Our account with them"><input value={n.externalAccount} onChange={(e) => setN({ ...n, externalAccount: e.target.value })} placeholder="MT940 :25:" /></Field>
        </div>
        <div className="row-3">
          <Field label="Adopt ledger account" hint="blank opens a new 14003 account"><input value={n.ledgerAccountNumber} onChange={(e) => setN({ ...n, ledgerAccountNumber: e.target.value })} /></Field>
          <Field label="Park breaks after (days)"><input type="number" min={0} value={n.parkAfterDays} onChange={(e) => setN({ ...n, parkAfterDays: e.target.value })} /></Field>
          <Field label="Proof not closing"><select value={n.proofPolicy} onChange={(e) => setN({ ...n, proofPolicy: e.target.value })}><option value="Warn">Warn</option><option value="HaltCob">Halt COB</option></select></Field>
        </div>
        <div className="row-2">
          <Field label="Unexplained credits to"><select value={n.creditSuspenseType} onChange={(e) => setN({ ...n, creditSuspenseType: e.target.value })}>{credit.map((t) => <option key={t.code}>{t.code}</option>)}</select></Field>
          <Field label="Unexplained debits to"><select value={n.debitSuspenseType} onChange={(e) => setN({ ...n, debitSuspenseType: e.target.value })}>{debit.map((t) => <option key={t.code}>{t.code}</option>)}</select></Field>
        </div>
        <ErrorNote error={addNostro.error} />
        <Button busy={addNostro.busy}>Input nostro (INAU)</Button>
      </form>
    </Panel>
  )
}
