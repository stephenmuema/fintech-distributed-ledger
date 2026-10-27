import { useState } from 'react'
import { api, getSession } from '../api'
import { dateTime } from '../format'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { OnboardingPolicy, Operator } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel } from '../ui'

const permissions = [
  { value: 'maker', label: 'Maker — inputs records and changes' },
  { value: 'checker', label: 'Checker — authorises or rejects other operators’ input' },
  { value: 'admin', label: 'Admin — maintains operators, their passwords and the onboarding policy' },
  { value: 'analyst', label: 'Analyst — business intelligence; on its own, sees only analytics' },
] as const

type Spec = { name: string; email: string; permissions: string[]; active: boolean }

/** A random one-time password, generated in the browser and shown to the administrator once. */
function oneTimePassword() {
  const alphabet = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join('')
}

export default function Operators() {
  const operators = useLoad(() => api<Operator[]>('/ops/operators'))
  const [editing, setEditing] = useState<{ username: string; spec: Spec; amending: boolean }>()
  const [version, setVersion] = useState(0)

  return (
    <>
      <PageHeader module="EB · OPERATORS" title="Operators">
        <Button onClick={() => setEditing({ username: '', spec: { name: '', email: '', permissions: ['maker'], active: true }, amending: false })}>Add operator</Button>
      </PageHeader>
      <p className="muted intro">
        Everyone who signs in to this console. An operator record is input by one administrator and authorised by another (nobody maintains
        their own); passwords are never part of it. A new operator gets a one-time password from an administrator and must choose their own
        at the first sign-in.
      </p>
      <div className="grid-main-side">
        <Panel title="Operators">
          <ErrorNote error={operators.error} />
          {operators.data?.length === 0 && <Empty>No operators.</Empty>}
          {!!operators.data?.length && (
            <table>
              <thead><tr><th>Operator</th><th>Permissions</th><th>Status</th><th>Last sign-in</th><th /></tr></thead>
              <tbody>
                {operators.data.map((o) => (
                  <tr key={o.id}>
                    <td><Mono>{o.username}</Mono><br /><small className="muted">{o.name} · {o.email}</small></td>
                    <td>{o.permissions.join(', ')}</td>
                    <td>
                      <Badge value={o.active ? 'Active' : 'Retired'} />{' '}
                      {o.lockedUntil && <small className="muted">locked until {dateTime(o.lockedUntil)}</small>}
                      {o.mustChangePassword && <small className="muted"> one-time password</small>}
                    </td>
                    <td className="nowrap">{o.lastLoginAt ? dateTime(o.lastLoginAt) : '—'}</td>
                    <td className="nowrap">
                      {o.username !== getSession()?.username && (
                        <button className="link-btn" onClick={() => setEditing({ username: o.username, amending: true,
                          spec: { name: o.name, email: o.email, permissions: [...o.permissions], active: o.active } })}>Amend</button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
        <div className="stack">
          {editing && <OperatorForm key={editing.username || 'new'} initial={editing} onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />}
          <PendingChanges types={['Operator']} version={version} title="Operator changes awaiting authorisation" onDecided={operators.reload} />
          <ResetPassword operators={(operators.data ?? []).filter((o) => o.username !== getSession()?.username)} onDone={operators.reload} />
          <OnboardingPolicyPanel />
        </div>
      </div>
    </>
  )
}

function OperatorForm({ initial, onDone }: { initial: { username: string; spec: Spec; amending: boolean }; onDone: () => void }) {
  const [username, setUsername] = useState(initial.username)
  const [spec, setSpec] = useState(initial.spec)
  const submit = useAction(() => api(`/ops/operators/${username}`, { method: 'POST', body: spec }))
  const toggle = (p: string) => setSpec({ ...spec, permissions: spec.permissions.includes(p) ? spec.permissions.filter((x) => x !== p) : [...spec.permissions, p] })

  return (
    <Panel title={initial.amending ? `Amend ${initial.username}` : 'Add an operator'} aside={<span className="muted">INAU until another admin authorises</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <div className="row-2">
          <Field label="Username" hint="Lower-case; it is the name on every record they input or authorise">
            <input required readOnly={initial.amending} value={username} onChange={(e) => setUsername(e.target.value.toLowerCase().trim())} />
          </Field>
          <Field label="Name"><input required value={spec.name} onChange={(e) => setSpec({ ...spec, name: e.target.value })} /></Field>
        </div>
        <Field label="Email"><input required type="email" value={spec.email} onChange={(e) => setSpec({ ...spec, email: e.target.value })} /></Field>
        <Field label="Permissions" hint="Separate makers from checkers wherever the team allows it">
          <div className="chips">
            {permissions.map((p) => (
              <label key={p.value} className="check"><input type="checkbox" checked={spec.permissions.includes(p.value)} onChange={() => toggle(p.value)} /> {p.label}</label>
            ))}
          </div>
        </Field>
        {initial.amending && <label className="check"><input type="checkbox" checked={spec.active} onChange={(e) => setSpec({ ...spec, active: e.target.checked })} /> Active (deactivating signs them out at once)</label>}
        <ErrorNote error={submit.error} />
        <div className="actions">
          <Button type="button" variant="ghost" onClick={onDone}>Cancel</Button>
          <Button busy={submit.busy}>Input {initial.amending ? 'amendment' : 'operator'} (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}

function ResetPassword({ operators, onDone }: { operators: Operator[]; onDone: () => void }) {
  const [username, setUsername] = useState('')
  const [issued, setIssued] = useState<{ username: string; password: string }>()
  const reset = useAction((password: string) => api(`/ops/operators/${username}/password`, { method: 'POST', body: { temporaryPassword: password } }))

  return (
    <Panel title="One-time password" aside={<span className="muted">unlocks and signs the operator out</span>}>
      <form className="inline-form" onSubmit={async (e) => {
        e.preventDefault()
        const password = oneTimePassword()
        if (await reset.run(password)) {
          setIssued({ username, password })
          onDone()
        }
      }}>
        <Field label="Operator">
          <select required value={username} onChange={(e) => { setUsername(e.target.value); setIssued(undefined) }}>
            <option value="">—</option>
            {operators.filter((o) => o.active).map((o) => <option key={o.username} value={o.username}>{o.username}</option>)}
          </select>
        </Field>
        <Button variant="ghost" busy={reset.busy} disabled={!username}>Issue</Button>
      </form>
      <ErrorNote error={reset.error} />
      {issued && (
        <p className="callout">
          Give <Mono>{issued.username}</Mono> this one-time password in person or over a separate channel. It is shown only now:{' '}
          <Mono>{issued.password}</Mono>
        </p>
      )}
    </Panel>
  )
}

/** Whether customers must give their KYC to sign up and open accounts. */
function OnboardingPolicyPanel() {
  const policy = useLoad(() => api<OnboardingPolicy>('/onboarding/policy'))
  const [reason, setReason] = useState('')
  const [version, setVersion] = useState(0)
  const required = policy.data?.kycRequired
  const propose = useAction(() => api('/ops/onboarding-policy', { method: 'POST', body: { kycRequired: !required, reason } }))

  return (
    <>
      <Panel title="Customer onboarding" aside={<span className="muted">INAU until another operator authorises</span>}>
        <ErrorNote error={policy.error} />
        {policy.data && (
          <>
            <p>
              KYC at sign-up: <Badge value={required ? 'Required' : 'Optional'} />
            </p>
            <p className="muted">
              {required
                ? 'Customers sign up with their KYC details, and no account is opened for a customer who has not submitted KYC.'
                : 'Customers can sign up and get accounts without KYC, and submit it later. Unverified customers still cannot send money.'}
              {policy.data.updatedAt && policy.data.updatedBy !== 'system' && <> Set by {policy.data.updatedBy} · {dateTime(policy.data.updatedAt)} · {policy.data.reason}</>}
            </p>
            <form className="inline-form" onSubmit={async (e) => {
              e.preventDefault()
              if (await propose.run()) {
                setReason('')
                setVersion((v) => v + 1)
              }
            }}>
              <Field label="Reason"><input required value={reason} onChange={(e) => setReason(e.target.value)} /></Field>
              <Button variant="ghost" busy={propose.busy}>{required ? 'Make KYC optional' : 'Make KYC required'}</Button>
            </form>
            <ErrorNote error={propose.error} />
          </>
        )}
      </Panel>
      <PendingChanges types={['OnboardingPolicy']} version={version} title="Onboarding changes awaiting authorisation" onDecided={policy.reload} />
    </>
  )
}
