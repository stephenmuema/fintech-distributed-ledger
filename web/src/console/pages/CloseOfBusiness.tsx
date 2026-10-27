import { useState } from 'react'
import { api } from '../api'
import { dateTime, day } from '../format'
import { useAction, useLoad } from '../hooks'
import type { CobRun, SystemDates } from '../types'
import { Badge, Button, Empty, ErrorNote, Json, Loading, Mono, PageHeader, Panel, Stat } from '../ui'

const stageNames: Record<string, string> = { Application: '1 · Application', SystemWide: '2 · System-wide', Reporting: '3 · Reporting' }

export default function CloseOfBusiness() {
  const dates = useLoad(() => api<SystemDates>('/core/dates'))
  const runs = useLoad(() => api<CobRun[]>('/ops/cob/runs'))
  const [open, setOpen] = useState<string>()
  const run = useAction(() => api<CobRun>('/ops/cob/run', { method: 'POST' }))

  const execute = async () => {
    const result = await run.run()
    if (result) {
      setOpen(result.id)
      dates.reload()
      runs.reload()
    }
  }

  const d = dates.data
  const halted = runs.data?.find((r) => r.status === 'Halted')

  return (
    <>
      <PageHeader module="EB · CLOSE OF BUSINESS" title="Close of business">
        <Button variant={halted ? 'accent' : 'primary'} busy={run.busy} onClick={() => void execute()}>
          {halted ? `Resume halted COB (${day(halted.businessDate)})` : d ? `Close ${day(d.today)}` : 'Run COB'}
        </Button>
      </PageHeader>

      {d && (
        <div className="stat-grid">
          <Stat label="Today (T24 DATES)" value={day(d.today)} hint={<Badge value={d.status} />} />
          <Stat label="Last working day" value={day(d.lastWorkingDay)} />
          <Stat label="Next working day" value={day(d.nextWorkingDay)} hint={d.isMonthEnd ? 'COB will run month-end interest capitalisation' : 'Kenyan holiday calendar applied'} />
          <Stat label="Online booking date" value={day(d.onlineBookingDate)} hint="Postings made now land on this date" />
        </div>
      )}
      <ErrorNote error={run.error} />

      <Panel title="Runs" aside={<span className="muted">Stages run in order; each job is idempotent, so a halted run resumes safely</span>}>
        {runs.loading && !runs.data && <Loading />}
        {runs.data?.length === 0 && <Empty>No COB has run yet. Closing the day accrues interest, snapshots balances and checks the books.</Empty>}
        {runs.data?.map((r) => (
          <article key={r.id} className={`cob-run ${open === r.id ? 'open' : ''}`}>
            <button className="cob-run-head" onClick={() => setOpen(open === r.id ? undefined : r.id)}>
              <Badge value={r.status} />
              <strong>{day(r.businessDate)}</strong>
              {r.isMonthEnd && <span className="tag">MONTH END</span>}
              <span className="muted">{r.newBusinessDate ? `→ ${day(r.newBusinessDate)}` : ''}</span>
              <span className="muted push">{r.startedBy} · {dateTime(r.startedAt)}</span>
            </button>
            {open === r.id && (
              <div className="cob-run-body">
                <table>
                  <thead>
                    <tr><th>#</th><th>Stage</th><th>Job</th><th>Status</th><th className="num">Records</th><th className="num">Attempts</th><th>Summary</th></tr>
                  </thead>
                  <tbody>
                    {r.jobs.slice().sort((a, b) => a.order - b.order).map((j) => (
                      <tr key={j.id}>
                        <td>{j.order}</td>
                        <td className="muted">{stageNames[j.stage] ?? j.stage}</td>
                        <td><Mono>{j.name}</Mono></td>
                        <td><Badge value={j.status} /></td>
                        <td className="num">{j.recordsProcessed}</td>
                        <td className="num">{j.attempts}</td>
                        <td className={j.error ? 'error-text' : 'muted'}>{j.error ?? (j.name === 'RE.EOD.REPORT' ? 'see report below' : j.summary)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {r.report && (
                  <>
                    <h3 className="sub">End-of-day report</h3>
                    <Json value={JSON.parse(r.report)} />
                  </>
                )}
              </div>
            )}
          </article>
        ))}
      </Panel>
    </>
  )
}
