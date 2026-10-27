import { useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { api, can } from '../api'
import { dateTime, money } from '../format'
import { useAction, useLoad } from '../hooks'
import type { Payout } from '../types'
import { Badge, Button, Empty, ErrorNote, Loading, Mono, PageHeader, Panel } from '../ui'

/** OutcomeUnknown is not a status: Processing payouts no provider confirmed, escalated to operations. */
const views = ['PendingReview', 'OutcomeUnknown', 'Queued', 'Processing', 'Paid', 'Failed', 'Rejected']

const intro: Record<string, ReactNode> = {
  PendingReview: (
    <>
      Payouts scored between 50 and 79 by the risk engine wait here with funds already reserved in <Mono>payouts-in-transit</Mono>.
      Approving submits them to a rail; rejecting reverses the reservation back to the customer.
    </>
  ),
  OutcomeUnknown: (
    <>
      Payouts no provider confirmed: their status checks went unanswered, or the provider can&apos;t be asked. They may have been paid,
      so their funds stay reserved and nothing is reversed automatically. Confirm with the provider (portal, statement), then settle the
      payout&apos;s item in the rail&apos;s outward book on the <Link to="/console/suspense" className="panel-link">Suspense</Link> page:
      to the rail&apos;s settlement account if it was paid, back to the customer if it wasn&apos;t. A checker authorises the clearing.
    </>
  ),
}

export default function Payouts() {
  const [view, setView] = useState('PendingReview')
  const unknown = view === 'OutcomeUnknown'
  const payouts = useLoad(() => api<Payout[]>(unknown ? '/ops/payouts?escalated=true&limit=200' : `/ops/payouts?status=${view}&limit=200`), [view])
  const decide = useAction((id: string, approve: boolean) =>
    api(`/ops/payouts/${id}/${approve ? 'approve' : 'reject'}`, { method: 'POST', body: approve ? undefined : { reason: 'rejected_by_review' } }),
  )

  const act = async (id: string, approve: boolean) => {
    if (await decide.run(id, approve)) payouts.reload()
  }

  return (
    <>
      <PageHeader module="PP · PAYOUTS" title="Payouts">
        <div className="segmented">
          {views.map((s) => <button key={s} className={view === s ? 'on' : ''} onClick={() => setView(s)}>{s.replace(/([a-z])([A-Z])/g, '$1 $2')}</button>)}
        </div>
      </PageHeader>
      <Panel>
        {intro[view] && <p className="muted intro">{intro[view]}</p>}
        <ErrorNote error={decide.error ?? payouts.error} />
        {payouts.loading && !payouts.data && <Loading />}
        {payouts.data?.length === 0 && <Empty>Nothing in this state.</Empty>}
        {!!payouts.data?.length && (
          <table>
            <thead>
              <tr><th>{unknown ? 'Escalated' : 'Created'}</th><th>Payout</th><th className="num">Amount</th><th>Rail</th><th>Beneficiary</th><th>Status</th><th>Detail</th><th /></tr>
            </thead>
            <tbody>
              {payouts.data.map((p) => (
                <tr key={p.id}>
                  <td>{dateTime(unknown ? p.escalatedAt : p.createdAt)}</td>
                  <td><Mono>{p.id.slice(0, 8)}</Mono></td>
                  <td className="num">{money(p.amount, p.currency)}</td>
                  <td>{p.rail}</td>
                  <td>{p.destinationName} <span className="muted mono">{p.destinationAccount}</span></td>
                  <td><Badge value={p.status} /></td>
                  <td className="muted">
                    {unknown
                      ? <>{p.provider} <Mono>{p.providerReference}</Mono> · sent {dateTime(p.createdAt)}</>
                      : p.failureCode ?? p.reviewedBy ?? p.providerReference ?? (p.submissionAttempts ? `${p.submissionAttempts} attempts` : '')}
                  </td>
                  <td className="actions">
                    {p.status === 'PendingReview' && can('checker') && (
                      <>
                        <Button variant="accent" busy={decide.busy} onClick={() => void act(p.id, true)}>Approve</Button>
                        <Button variant="ghost" busy={decide.busy} onClick={() => void act(p.id, false)}>Reject</Button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  )
}
