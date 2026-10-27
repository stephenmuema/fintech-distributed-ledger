import { useState } from 'react'
import { api } from '../api'
import { dateTime, short } from '../format'
import { useAction, useLoad } from '../hooks'
import type { AuditEvent, OutboxMessage, ProviderStatus } from '../types'
import { PendingChanges } from '../pending'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel, Stat } from '../ui'

export default function Operations() {
  return (
    <>
      <PageHeader module="EB · OPERATIONS" title="Reliability & audit" />
      <div className="grid-2">
        <Providers />
        <AuditChain />
        <Outbox />
        <Blocklist />
      </div>
    </>
  )
}

function Providers() {
  const providers = useLoad(() => api<ProviderStatus[]>('/ops/providers'))
  const toggle = useAction((name: string, down: boolean) => api(`/ops/providers/${name}/outage`, { method: 'POST', body: { down } }))
  return (
    <Panel title="Payment providers" aside={<span className="muted">Incident drill</span>}>
      <p className="muted intro">Simulate an outage, then collect a card payment: traffic fails over to the next acquirer and, after repeated failures, the circuit opens.</p>
      <ErrorNote error={toggle.error} />
      <table>
        <thead><tr><th>Provider</th><th>Module</th><th>Circuit</th><th>Simulated outage</th><th /></tr></thead>
        <tbody>
          {providers.data?.map((p) => (
            <tr key={p.name}>
              <td><Mono>{p.name}</Mono></td>
              <td>{p.module ? <Mono>{p.module}</Mono> : p.notRoutable ? <span className="error-text">{p.notRoutable}</span> : <span className="muted">simulator</span>}</td>
              <td>{p.notRoutable ? <Badge value="Not routable" /> : <Badge value={p.circuit} />}</td>
              <td>{p.simulatedOutage ? <Badge value="Failed" /> : <span className="muted">no</span>}</td>
              <td className="actions">
                {!p.notRoutable && (
                  <Button variant={p.simulatedOutage ? 'accent' : 'danger'} busy={toggle.busy} onClick={async () => { if (await toggle.run(p.name, !p.simulatedOutage)) providers.reload() }}>
                    {p.simulatedOutage ? 'Restore' : 'Take down'}
                  </Button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

function AuditChain() {
  const events = useLoad(() => api<AuditEvent[]>('/ops/audit?limit=40'))
  const verify = useAction(() => api<{ valid: boolean; checkedEvents: number; brokenAtSequence?: number }>('/ops/audit/verify'))
  return (
    <Panel title="Audit trail" aside={<Button variant="ghost" busy={verify.busy} onClick={() => void verify.run()}>Verify hash chain</Button>}>
      {verify.result && (
        <Stat label="Hash chain" value={verify.result.valid ? 'Intact' : `Broken at #${verify.result.brokenAtSequence}`} tone={verify.result.valid ? 'good' : 'bad'}
          hint={`${verify.result.checkedEvents} events re-hashed with SHA-256`} />
      )}
      <ul className="audit-feed">
        {events.data?.map((e) => (
          <li key={e.sequence}>
            <span className="mono muted">#{e.sequence}</span>
            <span><strong>{e.actor}</strong> <Mono>{e.action}</Mono> <span className="muted">{e.entityType} {short(e.entityId)}</span></span>
            <span className="muted mono" title={e.hash}>{e.hash.slice(0, 10)}</span>
          </li>
        ))}
      </ul>
      {events.data?.length === 0 && <Empty>No audit events yet.</Empty>}
    </Panel>
  )
}

function Outbox() {
  const [status, setStatus] = useState('')
  const messages = useLoad(() => api<OutboxMessage[]>(`/ops/outbox${status ? `?status=${status}` : ''}`), [status])
  const replay = useAction((id: string) => api(`/ops/outbox/${id}/replay`, { method: 'POST' }))
  return (
    <Panel title="Webhook outbox" aside={
      <select value={status} onChange={(e) => setStatus(e.target.value)}>
        {['', 'Pending', 'Delivered', 'Skipped', 'DeadLettered'].map((s) => <option key={s} value={s}>{s || 'All'}</option>)}
      </select>
    }>
      <p className="muted intro">Events are written in the same transaction as the money movement, then delivered with HMAC signatures and exponential backoff.</p>
      <ErrorNote error={replay.error} />
      <table>
        <thead><tr><th>Event</th><th>Status</th><th className="num">Attempts</th><th>Occurred</th><th /></tr></thead>
        <tbody>
          {messages.data?.slice(0, 30).map((m) => (
            <tr key={m.id}>
              <td><Mono>{m.type}</Mono></td>
              <td><Badge value={m.status} /></td>
              <td className="num">{m.attempts}</td>
              <td className="muted">{dateTime(m.occurredAt)}</td>
              <td>{m.status === 'DeadLettered' && <button className="link-btn" onClick={async () => { if (await replay.run(m.id)) messages.reload() }}>Replay</button>}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {messages.data?.length === 0 && <Empty>No events.</Empty>}
    </Panel>
  )
}

function Blocklist() {
  const list = useLoad(() => api<{ value: string; reason: string; createdBy: string; createdAt: string }[]>('/ops/risk/blocklist'))
  const [form, setForm] = useState({ value: '', reason: '' })
  const add = useAction(() => api('/ops/risk/blocklist', { method: 'POST', body: form }))
  return (
    <Panel title="Risk blocklist" aside={<span className="muted">RK · hard decline</span>}>
      <form className="inline-form" onSubmit={async (e) => { e.preventDefault(); if (await add.run()) { setForm({ value: '', reason: '' }); list.reload() } }}>
        <Field label="MSISDN or account"><input className="mono" required value={form.value} onChange={(e) => setForm({ ...form, value: e.target.value })} /></Field>
        <Field label="Reason"><input required value={form.reason} onChange={(e) => setForm({ ...form, reason: e.target.value })} /></Field>
        <Button variant="danger" busy={add.busy}>Input block (INAU)</Button>
      </form>
      <ErrorNote error={add.error} />
      {add.result !== undefined && <p className="muted">Input. The entry takes effect when a checker authorises it.</p>}
      <PendingChanges types={['Blocklist']} title="Blocklist entries awaiting authorisation" onDecided={list.reload} />
      <ul className="audit-feed">
        {list.data?.map((b) => (
          <li key={b.value}><Mono>{b.value}</Mono><span>{b.reason}</span><span className="muted">{b.createdBy}</span></li>
        ))}
      </ul>
    </Panel>
  )
}
