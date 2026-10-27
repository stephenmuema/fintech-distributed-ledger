import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api, download } from '../api'
import { money } from '../format'
import { useAction, useLoad } from '../hooks'
import type { SystemDates, Vostro as VostroAccount } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel, Stat } from '../ui'

export default function Vostro() {
  const vostros = useLoad(() => api<VostroAccount[]>('/ops/vostros'))
  const dates = useLoad(() => api<SystemDates>('/core/dates'))
  const [selected, setSelected] = useState<string>()
  const [period, setPeriod] = useState({ from: '', to: '', format: 'mt940' })
  const account = vostros.data?.find((v) => v.id === selected)
  const today = dates.data?.today ?? ''
  const exportStatement = useAction(() =>
    download(`/accounts/${selected}/statement/export?from=${period.from || today}&to=${period.to || today}&format=${period.format}`),
  )
  const totals = (vostros.data ?? []).reduce<Record<string, number>>((acc, v) => ({ ...acc, [v.currency]: (acc[v.currency] ?? 0) + v.onlineActualBalance }), {})

  return (
    <>
      <PageHeader module="NV · VOSTRO" title="Vostro accounts">
        <Link className="btn btn-ghost" to="/console/nostro">Nostro reconciliation</Link>
      </PageHeader>
      <p className="muted intro">
        Accounts other banks and PSPs hold with us. A vostro is a customer account of a <strong>financial institution</strong> customer — category 5001,
        GL 2120 "due to banks" — so posting restrictions, the no-overdraft rule and statements apply as for any customer. Open one by creating a
        FinancialInstitution customer and opening an account for it; link it to a correspondent to see it here by code. Holders reconcile against
        the MT940 or CSV statement exported below.
      </p>
      <div className="stat-grid">
        <Stat label="Vostro accounts" value={vostros.data?.length ?? '—'} />
        {Object.entries(totals).map(([ccy, total]) => <Stat key={ccy} label={`Due to banks · ${ccy}`} value={money(total, ccy)} />)}
      </div>
      <div className="grid-main-side">
        <Panel title="Accounts">
          <ErrorNote error={vostros.error} />
          {vostros.data?.length === 0 && <Empty>No vostro accounts. Create a FinancialInstitution customer under <Link to="/console/customers">Customers</Link> and open an account.</Empty>}
          {!!vostros.data?.length && (
            <table>
              <thead><tr><th>Account</th><th>Holder</th><th>Correspondent</th><th className="num">Working</th><th className="num">Locked</th><th>Restriction</th></tr></thead>
              <tbody>
                {vostros.data.map((v) => (
                  <tr key={v.id} className={`clickable ${selected === v.id ? 'selected' : ''}`} onClick={() => setSelected(v.id)}>
                    <td><Mono>{v.accountNumber}</Mono></td>
                    <td>{v.holderName} <span className="muted">#{v.customerNumber}</span></td>
                    <td>{v.correspondent ? <Mono>{v.correspondent}</Mono> : <span className="muted">not linked</span>}</td>
                    <td className="num">{money(v.workingBalance, v.currency)}</td>
                    <td className="num">{money(v.lockedAmount, v.currency)}</td>
                    <td><Badge value={v.restriction} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <Panel title="Statement for the holder">
          {!account && <Empty>Select an account.</Empty>}
          {account && (
            <form className="form" onSubmit={async (e) => { e.preventDefault(); await exportStatement.run() }}>
              <p><Mono>{account.accountNumber}</Mono> · {account.holderName} · <Link to={`/console/accounts/${account.accountNumber}`}>account enquiry</Link></p>
              <div className="row-2">
                <Field label="From"><input type="date" value={period.from || today} onChange={(e) => setPeriod({ ...period, from: e.target.value })} /></Field>
                <Field label="To"><input type="date" value={period.to || today} onChange={(e) => setPeriod({ ...period, to: e.target.value })} /></Field>
              </div>
              <div className="segmented">
                {[['mt940', 'MT940'], ['csv', 'CSV']].map(([value, text]) => (
                  <button type="button" key={value} className={period.format === value ? 'on' : ''} onClick={() => setPeriod({ ...period, format: value })}>{text}</button>
                ))}
              </div>
              <ErrorNote error={exportStatement.error} />
              <Button busy={exportStatement.busy}>Download statement</Button>
            </form>
          )}
        </Panel>
      </div>
    </>
  )
}
