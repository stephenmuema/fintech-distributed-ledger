import { useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { api } from '../api'
import { amount, dateTime, money, shortDay } from '../format'
import { useLoad } from '../hooks'
import type { Transaction, TransactionView } from '../types'
import { Badge, Empty, ErrorNote, Field, Json, Loading, Mono, PageHeader, Panel } from '../ui'

export default function Transactions() {
  const { reference } = useParams()
  return reference ? <Detail reference={reference} /> : <Search />
}

const emptyFilter = { type: '', version: '', account: '', status: '', recordStatus: '', from: '', to: '' }

function Search() {
  const navigate = useNavigate()
  const [filter, setFilter] = useState(emptyFilter)
  const [applied, setApplied] = useState(emptyFilter)
  const [jump, setJump] = useState('')
  const query = new URLSearchParams(Object.entries(applied).filter(([, v]) => v)).toString()
  const list = useLoad(() => api<Transaction[]>(`/transactions?limit=200${query ? `&${query}` : ''}`), [query])
  const set = (k: keyof typeof filter) => (e: { target: { value: string } }) => setFilter({ ...filter, [k]: e.target.value })

  return (
    <>
      <PageHeader module="FT · TRANSACTIONS" title="Transactions">
        <form className="inline-form" onSubmit={(e) => { e.preventDefault(); if (jump.trim()) navigate(`/console/transactions/${jump.trim().toUpperCase()}`) }}>
          <input className="mono" placeholder="FT26301K3QZ8" value={jump} onChange={(e) => setJump(e.target.value)} />
        </form>
      </PageHeader>
      <Panel>
        <form className="form filters" onSubmit={(e) => { e.preventDefault(); setApplied(filter) }}>
          <div className="row-4">
            <Field label="Type"><input className="mono" value={filter.type} onChange={set('type')} placeholder="AC" /></Field>
            <Field label="Version"><input className="mono" value={filter.version} onChange={set('version')} placeholder="PAYOUT.API" /></Field>
            <Field label="Account"><input className="mono" value={filter.account} onChange={set('account')} /></Field>
            <Field label="Status">
              <select value={filter.status} onChange={set('status')}>
                <option value="">Any</option>{['Pending', 'Processing', 'Completed', 'Failed', 'Reversed'].map((s) => <option key={s}>{s}</option>)}
              </select>
            </Field>
          </div>
          <div className="row-4">
            <Field label="Record">
              <select value={filter.recordStatus} onChange={set('recordStatus')}>
                <option value="">Any</option>{['INAU', 'LIVE', 'RJCT'].map((s) => <option key={s}>{s}</option>)}
              </select>
            </Field>
            <Field label="Booked from"><input type="date" value={filter.from} onChange={set('from')} /></Field>
            <Field label="Booked to"><input type="date" value={filter.to} onChange={set('to')} /></Field>
            <div className="actions"><button className="btn btn-primary">Search</button></div>
          </div>
        </form>
      </Panel>
      <Panel title="Results" aside={<span className="muted">newest first · up to 200</span>}>
        {list.loading && <Loading />}
        <ErrorNote error={list.error} />
        {list.data?.length === 0 && <Empty>No transactions match.</Empty>}
        {!!list.data?.length && (
          <table className="table">
            <thead><tr><th>Reference</th><th>Booked</th><th>Type · version</th><th>Debit</th><th>Credit</th><th className="num">Amount</th><th>Status</th><th>Channel</th></tr></thead>
            <tbody>
              {list.data.map((t) => (
                <tr key={t.id}>
                  <td><Link to={`/console/transactions/${t.reference}`}><Mono>{t.reference}</Mono></Link></td>
                  <td>{shortDay(t.bookingDate)}</td>
                  <td><Mono>{t.transactionType}</Mono> <span className="muted">{t.version}</span></td>
                  <td><Mono>{t.debitAccount ?? '—'}</Mono></td>
                  <td><Mono>{t.creditAccount ?? '—'}</Mono></td>
                  <td className="num">{amount(t.amount, t.currency)} <span className="muted">{t.currency}</span></td>
                  <td><Badge value={t.recordStatus} /> <Badge value={t.status} /></td>
                  <td className="muted">{t.channel}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  if (children === undefined || children === null || children === '') return null
  return <><dt>{label}</dt><dd>{children}</dd></>
}

function Detail({ reference }: { reference: string }) {
  const txn = useLoad(() => api<TransactionView>(`/transactions/${reference}`), [reference])
  const t = txn.data
  return (
    <>
      <PageHeader module="FT · TRANSACTION" title={reference}>
        <Link to="/console/transactions">← All transactions</Link>
      </PageHeader>
      {txn.loading && <Loading />}
      <ErrorNote error={txn.error} />
      {t && (
        <>
          <div className="grid-2">
            <Panel title="Transaction" aside={<span><Badge value={t.recordStatus} /> <Badge value={t.status} /></span>}>
              <dl className="kv">
                <Row label="Amount">{money(t.amount, t.currency)}</Row>
                <Row label="Type">{t.transactionType} · {t.flow} · {t.direction}</Row>
                <Row label="Version">{t.version}</Row>
                <Row label="Channel">{t.channel}</Row>
                <Row label="Reporting class">{t.reportingClass}</Row>
                <Row label="Booking date">{shortDay(t.bookingDate)}</Row>
                <Row label="Narrative">{t.narrative}</Row>
                <Row label="Payment details">{t.paymentDetails?.join(' / ')}</Row>
                <Row label="Charge">{t.chargeAmount ? money(t.chargeAmount, t.currency) : undefined}</Row>
                <Row label="Provider">{t.provider && <>{t.provider} <Mono>{t.providerReference}</Mono></>}</Row>
                <Row label="Flow">{t.flowRecordType && <>{t.flowRecordType} · {t.flowStatus}</>}</Row>
                <Row label="Decline">{t.declineCode}</Row>
                <Row label="Overrides">{t.overridesAccepted?.join(', ')}</Row>
                <Row label="Inputter">{t.inputter} · {dateTime(t.inputAt)}</Row>
                <Row label="Authoriser">{t.authoriser && <>{t.authoriser} · {dateTime(t.authorisedAt)}</>}</Row>
              </dl>
            </Panel>
            <Panel title="Parties">
              <dl className="kv">
                <Row label="Debit account"><Mono>{t.debitAccount}</Mono></Row>
                <Row label="Ordering party">{t.orderingName}</Row>
                <Row label="Ordering account"><Mono>{t.orderingAccount}</Mono></Row>
                <Row label="Ordering phone">{t.orderingPhone}</Row>
                <Row label="Ordering institution">{t.orderingInstitution}</Row>
                <Row label="Credit account"><Mono>{t.creditAccount}</Mono></Row>
                <Row label="Beneficiary">{t.beneficiaryName}</Row>
                <Row label="Beneficiary account"><Mono>{t.beneficiaryAccount}</Mono></Row>
                <Row label="Beneficiary phone">{t.beneficiaryPhone}</Row>
                <Row label="Beneficiary institution">{t.beneficiaryInstitution}</Row>
                <Row label="Rail">{t.rail}</Row>
              </dl>
            </Panel>
          </div>
          <div className="grid-2">
            <Panel title="Local fields" aside={<span className="muted">lt_transaction</span>}>
              {Object.keys(t.local).length === 0 ? <Empty>None.</Empty> : (
                <dl className="kv">{Object.entries(t.local).map(([k, v]) => <Row key={k} label={k}>{v}</Row>)}</dl>
              )}
            </Panel>
            <Panel title="Payment details" aside={<span className="muted">from the provider's module</span>}>
              {!t.details || Object.keys(t.details).length === 0 ? <Empty>None.</Empty> : Object.entries(t.details).map(([provider, values]) => (
                <dl key={provider} className="kv">
                  <Row label="Provider">{provider}</Row>
                  {Object.entries(values).map(([k, v]) => <Row key={k} label={k}>{v}</Row>)}
                </dl>
              ))}
            </Panel>
          </div>
          <Panel title="Journal entries" aside={<span className="muted">STMT.ENTRY ↔ transaction</span>}>
            {!t.entries?.length && <Empty>Nothing posted.</Empty>}
            {!!t.entries?.length && (
              <table className="table">
                <thead><tr><th>Entry</th><th>Booked</th><th>Category</th><th>Description</th><th className="num">Legs</th></tr></thead>
                <tbody>
                  {t.entries.map((e) => (
                    <tr key={e.id}>
                      <td><Mono>{e.transactionReference}</Mono></td>
                      <td>{shortDay(e.bookingDate)}</td>
                      <td>{e.category}</td>
                      <td>{e.description}</td>
                      <td className="num">{e.lines.length}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>
          {t.flow && <Panel title="Flow record"><Json value={t.flow} /></Panel>}
          <History id={t.id} />
        </>
      )}
    </>
  )
}

function History({ id }: { id: string }) {
  const history = useLoad(() => api<{ currNo: number; changedBy: string; changedAt: string; snapshot: string }[]>(`/ops/history/Transaction/${id}`), [id])
  if (!history.data?.length) return null
  return (
    <Panel title="Record history" aside={<span className="muted">$HIS · previous images</span>}>
      <table className="table">
        <thead><tr><th>Curr no</th><th>Changed by</th><th>At</th><th>Record status</th><th>Status</th></tr></thead>
        <tbody>
          {history.data.map((h) => {
            const image = JSON.parse(h.snapshot) as Transaction
            return (
              <tr key={h.currNo}>
                <td className="num">{h.currNo}</td><td>{h.changedBy}</td><td>{dateTime(h.changedAt)}</td>
                <td><Badge value={image.recordStatus} /></td><td><Badge value={image.status} /></td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </Panel>
  )
}
