import type { ReactNode } from 'react'
import { api, can, getSession } from './api'
import { dateTime } from './format'
import { useAction, useLoad } from './hooks'
import type { ParameterChange } from './types'
import { Badge, Button, Empty, ErrorNote, Mono, Panel } from './ui'

/**
 * Unauthorised parameter changes of the given types, with authorise / reject for the checker.
 * `version` lets the page force a reload after it inputs a change.
 */
export function PendingChanges({ types, version, onDecided, title = 'Awaiting authorisation' }: {
  types: string[]
  version?: number
  onDecided?: () => void
  title?: string
}) {
  const pending = useLoad(() => api<ParameterChange[]>('/ops/parameters/pending'), [version])
  const decide = useAction((id: string, ok: boolean) =>
    api(`/ops/parameters/pending/${id}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not approved' } }),
  )
  const rows = pending.data?.filter((p) => types.includes(p.type)) ?? []
  const act = async (id: string, ok: boolean) => {
    if (await decide.run(id, ok)) {
      pending.reload()
      onDecided?.()
    }
  }
  return (
    <Panel title={title} aside={<CheckerNote />}>
      <ErrorNote error={decide.error} />
      {rows.length === 0 && <Empty>Nothing waiting. Inputs appear here until a different operator authorises them.</Empty>}
      {rows.map((p) => (
        <div key={p.id} className="approval">
          <div>
            <Badge value={p.status} /> <Mono>{p.type} · {p.key}</Mono>
            <p className="muted">Input by <strong>{p.inputter}</strong> · {dateTime(p.inputAt)}</p>
            <code className="payload">{p.payload}</code>
          </div>
          <Decision inputter={p.inputter}>
            <Button variant="accent" busy={decide.busy} onClick={() => void act(p.id, true)}>Authorise</Button>
            <Button variant="ghost" busy={decide.busy} onClick={() => void act(p.id, false)}>Reject</Button>
          </Decision>
        </div>
      ))}
    </Panel>
  )
}

/** Who may decide here: checkers, never on their own input. */
export function CheckerNote() {
  const s = getSession()
  return <span className="muted">{can('checker') ? `checker: ${s?.name}` : 'authorised by a checker'}</span>
}

/** The authorise / reject buttons, for a checker who did not input the record. */
export function Decision({ inputter, children }: { inputter: string; children: ReactNode }) {
  if (inputter === getSession()?.username) return <p className="muted actions">Your input — another checker authorises it.</p>
  if (!can('checker')) return <p className="muted actions">Waiting for a checker.</p>
  return <div className="actions">{children}</div>
}
