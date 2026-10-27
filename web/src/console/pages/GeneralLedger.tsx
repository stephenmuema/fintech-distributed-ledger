import { useState, type FormEvent } from 'react'
import { api } from '../api'
import { amount, dateTime, day, lcy, money } from '../format'
import { useAction, useLoad } from '../hooks'
import type { BalanceSheet, GlLine, LedgerAccount, ProfitAndLoss, SystemDates, JournalEntryView } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel, Stat } from '../ui'

const tabs = ['Statements', 'GL balances', 'Internal accounts', 'Journal'] as const
type Tab = (typeof tabs)[number]

export default function GeneralLedger() {
  const [tab, setTab] = useState<Tab>('Statements')
  const [asOf, setAsOf] = useState('')

  return (
    <>
      <PageHeader module="RE · GENERAL LEDGER" title="General ledger & reporting">
        <div className="segmented">
          {tabs.map((t) => <button key={t} className={tab === t ? 'on' : ''} onClick={() => setTab(t)}>{t}</button>)}
        </div>
      </PageHeader>
      {(tab === 'Statements' || tab === 'GL balances') && (
        <div className="inline-form as-of">
          <Field label="As of" hint="Blank = live balances; a closed business date reads its COB snapshot">
            <input type="date" value={asOf} onChange={(e) => setAsOf(e.target.value)} />
          </Field>
        </div>
      )}
      {tab === 'Statements' && <Statements asOf={asOf} />}
      {tab === 'GL balances' && <GlBalances asOf={asOf} />}
      {tab === 'Internal accounts' && <InternalAccounts />}
      {tab === 'Journal' && <Journal />}
    </>
  )
}

const query = (asOf: string) => (asOf ? `?asOf=${asOf}` : '')

function Statements({ asOf }: { asOf: string }) {
  const data = useLoad(
    () => Promise.all([api<BalanceSheet>(`/ops/gl/balance-sheet${query(asOf)}`), api<ProfitAndLoss>(`/ops/gl/profit-and-loss${query(asOf)}`)]),
    [asOf],
  )
  if (data.loading && !data.data) return <Loading />
  if (!data.data) return <ErrorNote error={data.error} />
  const [sheet, pnl] = data.data

  return (
    <>
      <div className="stat-grid">
        <Stat label="Total assets" value={lcy(sheet.assets, sheet.localCurrency)} />
        <Stat label="Liabilities" value={lcy(sheet.liabilities, sheet.localCurrency)} hint="Mostly customer deposits" />
        <Stat label="Equity + current earnings" value={lcy(sheet.equity + sheet.currentYearEarnings, sheet.localCurrency)} />
        <Stat label="Balance sheet" value={sheet.balances ? 'Balances' : 'Does not balance'} tone={sheet.balances ? 'good' : 'bad'} hint="Assets = Liabilities + Equity + Earnings" />
      </div>
      <div className="grid-2">
        <Panel title="Balance sheet" aside={<span className="muted">{sheet.localCurrency} equivalent</span>}>
          <GlTable lines={sheet.lines} />
        </Panel>
        <Panel title="Profit & loss" aside={<span className={pnl.netProfit >= 0 ? 'good-text' : 'error-text'}>Net {lcy(pnl.netProfit, pnl.localCurrency)}</span>}>
          {pnl.lines.length ? <GlTable lines={pnl.lines} /> : <Empty>No income or expense yet.</Empty>}
        </Panel>
      </div>
    </>
  )
}

function GlBalances({ asOf }: { asOf: string }) {
  const gl = useLoad(() => api<{ lines: GlLine[] }>(`/ops/gl/balances${query(asOf)}`), [asOf])
  const integrity = useLoad(() => api<{ healthy: boolean; drift: { accountNumber: string; storedBalance: number; derivedBalance: number }[] }>('/ops/gl/integrity'))
  return (
    <div className="grid-main-side">
      <Panel title="GL lines by currency">
        {gl.loading && !gl.data && <Loading />}
        <ErrorNote error={gl.error} />
        {gl.data && (gl.data.lines.length ? <GlTable lines={gl.data.lines} showOriginal /> : <Empty>No balances.</Empty>)}
      </Panel>
      <Panel title="Ledger integrity">
        {integrity.data && (
          <>
            <Stat label="Stored vs derived balances" value={integrity.data.healthy ? 'No drift' : `${integrity.data.drift.length} accounts drifted`} tone={integrity.data.healthy ? 'good' : 'bad'} />
            <p className="muted">Every account's running balance is recomputed from its journal lines. COB halts if anything drifts.</p>
          </>
        )}
      </Panel>
    </div>
  )
}

function GlTable({ lines, showOriginal }: { lines: GlLine[]; showOriginal?: boolean }) {
  const groups = [...new Set(lines.map((l) => l.group))]
  return (
    <table>
      <thead>
        <tr><th>GL</th><th>Line</th><th>CCY</th>{showOriginal && <th className="num">Balance</th>}<th className="num">LCY</th></tr>
      </thead>
      {groups.map((g) => (
        <tbody key={g}>
          <tr className="group-row"><td colSpan={showOriginal ? 5 : 4}>{g}</td></tr>
          {lines.filter((l) => l.group === g).map((l) => (
            <tr key={`${l.glCode}-${l.currency}`}>
              <td><Mono>{l.glCode}</Mono></td>
              <td>{l.name}</td>
              <td><Mono>{l.currency}</Mono></td>
              {showOriginal && <td className="num">{amount(l.balance, l.currency)}</td>}
              <td className="num">{l.balanceLcy.toLocaleString('en-KE', { minimumFractionDigits: 2 })}</td>
            </tr>
          ))}
        </tbody>
      ))}
    </table>
  )
}

function InternalAccounts() {
  const accounts = useLoad(() => api<LedgerAccount[]>('/ops/gl/internal-accounts'))
  const [currency, setCurrency] = useState('KES')
  if (!accounts.data) return accounts.error ? <ErrorNote error={accounts.error} /> : <Loading />
  const currencies = [...new Set(accounts.data.map((a) => a.currency))]
  return (
    <Panel title="Internal & P&L accounts" aside={
      <select value={currency} onChange={(e) => setCurrency(e.target.value)}>{currencies.map((c) => <option key={c}>{c}</option>)}</select>
    }>
      <table>
        <thead><tr><th>Account number</th><th>Name</th><th>Category</th><th>GL</th><th>Type</th><th className="num">Balance</th></tr></thead>
        <tbody>
          {accounts.data.filter((a) => a.currency === currency).map((a) => (
            <tr key={a.id}>
              <td><Mono>{a.accountNumber}</Mono></td>
              <td>{a.name}</td>
              <td>{a.category}</td>
              <td><Mono>{a.glCode}</Mono></td>
              <td className="muted">{a.type}</td>
              <td className="num">{money(a.balance, a.currency)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

function Journal() {
  const dates = useLoad(() => api<SystemDates>('/core/dates'))
  const [date, setDate] = useState('')
  const effective = date || dates.data?.onlineBookingDate
  const journal = useLoad(async () => (effective ? api<JournalEntryView[]>(`/ops/gl/journal?date=${effective}&limit=300`) : []), [effective])
  const [ref, setRef] = useState('')
  const lookup = useAction((r: string) => api<JournalEntryView>(`/entries/${r.trim().toUpperCase()}`))
  const selected = lookup.result

  return (
    <div className="grid-main-side">
      <Panel title={`Transaction journal · ${effective ? day(effective) : ''}`} aside={<input type="date" value={date} onChange={(e) => setDate(e.target.value)} />}>
        {journal.loading && !journal.data && <Loading />}
        {journal.data?.length === 0 && <Empty>No entries booked on this date.</Empty>}
        {!!journal.data?.length && (
          <table>
            <thead><tr><th>Reference</th><th>Category</th><th>Narrative</th><th>Posted</th></tr></thead>
            <tbody>
              {journal.data.map((t) => (
                <tr key={t.id} className="clickable" onClick={() => { setRef(t.transactionReference); void lookup.run(t.transactionReference) }}>
                  <td><Mono>{t.transactionReference}</Mono></td>
                  <td><Badge value={t.category} /></td>
                  <td>{t.description}</td>
                  <td className="muted">{dateTime(t.postedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
      <Panel title="Journal entry enquiry">
        <form className="inline-form" onSubmit={(e: FormEvent) => { e.preventDefault(); void lookup.run(ref) }}>
          <input className="mono" value={ref} onChange={(e) => setRef(e.target.value)} placeholder="FT26286K3QZ8" />
          <Button variant="ghost" busy={lookup.busy}>Look up</Button>
        </form>
        <ErrorNote error={lookup.error} />
        {selected && (
          <div className="txn">
            <p><Mono>{selected.transactionReference}</Mono> · {selected.description}</p>
            <p className="muted">Booked {day(selected.bookingDate)} · value {day(selected.valueDate)} · key <Mono>{selected.reference}</Mono></p>
            <table>
              <thead><tr><th>Account</th><th>GL</th><th className="num">Dr</th><th className="num">Cr</th></tr></thead>
              <tbody>
                {selected.legs.map((l, i) => (
                  <tr key={i}>
                    <td><Mono>{l.accountNumber}</Mono><br /><small className="muted">{l.accountName}</small></td>
                    <td><Mono>{l.glCode}</Mono></td>
                    <td className="num debit">{l.direction === 'Debit' ? money(l.amount, l.currency) : ''}</td>
                    <td className="num credit">{l.direction === 'Credit' ? money(l.amount, l.currency) : ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </div>
  )
}
