import { useState, type FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../api'
import { amount, day, money, shortDay } from '../format'
import { useAction, useLoad } from '../hooks'
import type { Account, StatementLine } from '../types'
import { PendingChanges } from '../pending'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel, Stat } from '../ui'
import { LocalValues } from './LocalTables'

export default function Accounts() {
  const { accountNumber } = useParams()
  const navigate = useNavigate()
  const [query, setQuery] = useState(accountNumber ?? '')

  const search = (e: FormEvent) => {
    e.preventDefault()
    if (query.trim()) navigate(`/console/accounts/${query.trim()}`)
  }

  return (
    <>
      <PageHeader module="AC · ACCOUNTS" title="Account enquiry">
        <form className="inline-form" onSubmit={search}>
          <input className="mono" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="10-digit account number" maxLength={10} />
          <Button>Enquire</Button>
        </form>
      </PageHeader>
      {accountNumber ? <AccountView accountNumber={accountNumber} /> : <Empty>Enter an account number, or open one from a customer's position.</Empty>}
    </>
  )
}

function AccountView({ accountNumber }: { accountNumber: string }) {
  const account = useLoad(() => api<Account>(`/accounts/by-number/${accountNumber}`), [accountNumber])
  const statement = useLoad(
    async () => (account.data ? api<StatementLine[]>(`/accounts/${account.data.id}/statement?limit=200`) : []),
    [account.data?.id],
  )

  if (account.loading && !account.data) return <Loading />
  if (!account.data) return <ErrorNote error={account.error} />
  const a = account.data

  return (
    <>
      <div className="stat-grid">
        <Stat label="Working balance" value={money(a.workingBalance, a.currency)} hint="Available to spend" />
        <Stat label="Locked amount" value={money(a.lockedAmount, a.currency)} hint="Card holds & disputes" />
        <Stat label="Online actual balance" value={money(a.onlineActualBalance, a.currency)} hint={`${a.title} · category ${a.category}`} />
        <Stat label="Posting restriction" value={<Badge value={a.restriction === 'None' ? 'Active' : a.restriction} />} hint={a.restrictionReason ?? `Opened ${day(a.openingDate)}`} />
      </div>

      <div className="grid-main-side">
        <Panel title="Statement" aside={<span className="muted">STMT.ENTRY · running balance</span>}>
          {statement.loading && !statement.data && <Loading />}
          {statement.data?.length === 0 && <Empty>No entries on this account yet.</Empty>}
          {!!statement.data?.length && (
            <table>
              <thead>
                <tr><th>Booking</th><th>Reference</th><th>Narrative</th><th className="num">Debit</th><th className="num">Credit</th><th className="num">Balance</th></tr>
              </thead>
              <tbody>
                {statement.data.map((l) => (
                  <tr key={`${l.entryId}-${l.direction}`}>
                    <td className="nowrap">{shortDay(l.bookingDate)}</td>
                    <td><Mono>{l.transactionReference}</Mono></td>
                    <td>{l.description}</td>
                    <td className="num debit">{l.direction === 'Debit' ? amount(l.amount, a.currency) : ''}</td>
                    <td className="num credit">{l.direction === 'Credit' ? amount(l.amount, a.currency) : ''}</td>
                    <td className="num">{amount(l.balanceAfter, a.currency)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <Restriction account={a} onChange={account.reload} />
        <LocalValues table="ACCOUNT" recordKey={a.accountNumber} />
        <PendingChanges types={['PostingRestriction', 'LocalRecord']} title="Awaiting authorisation" onDecided={account.reload} />
      </div>
    </>
  )
}

function Restriction({ account, onChange }: { account: Account; onChange: () => void }) {
  const [restriction, setRestriction] = useState(account.restriction)
  const [reason, setReason] = useState('')
  const save = useAction(() => api(`/ops/accounts/${account.id}/restriction`, { method: 'POST', body: { restriction, reason } }))

  return (
    <Panel title="Posting restriction" aside={<span className="muted">now: {account.restriction}</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await save.run()) onChange() }}>
        <Field label="Restriction" hint="T24 POSTING.RESTRICT — blocks debits, credits or both.">
          <select value={restriction} onChange={(e) => setRestriction(e.target.value)}>
            {['None', 'DebitBlock', 'CreditBlock', 'FullBlock'].map((r) => <option key={r}>{r}</option>)}
          </select>
        </Field>
        <Field label="Reason"><input required value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. Court order 114/2026" /></Field>
        <ErrorNote error={save.error} />
        <Button variant="ghost" busy={save.busy}>Input restriction (INAU)</Button>
        {save.result !== undefined && <p className="muted">Input. It applies when a checker authorises it below.</p>}
      </form>
    </Panel>
  )
}
