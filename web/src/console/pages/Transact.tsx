import { useMemo, useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { api, ApiError, can } from '../api'
import { Decision } from '../pending'
import { dateTime, fieldLabel, money, toMinor } from '../format'
import { useAction, useLoad } from '../hooks'
import type { Account, LocalColumn, Override, Transaction, TransactionView, Version, VersionField } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Loading, Mono, PageHeader, Panel } from '../ui'

/** Turns the screen's flat field values into the /transactions request body. */
function requestBody(version: Version, values: Record<string, string>, local: Record<string, string>, currency: string) {
  const body: Record<string, unknown> = { version: version.name, local }
  const nested = (group: string, key: string, value: string) => {
    body[group] = { ...((body[group] as Record<string, string>) ?? {}), [key]: value }
  }
  for (const [path, value] of Object.entries(values)) {
    if (!value.trim()) continue
    if (path === 'amount') body.amount = toMinor(value, currency)
    else if (path === 'paymentDetails') body.paymentDetails = value.split('\n').map((l) => l.trim()).filter(Boolean)
    else if (path === 'attributes') body.attributes = Object.fromEntries(value.split('\n').map((l) => l.split('=').map((p) => p.trim())).filter((p) => p.length === 2 && p[0]))
    else if (path.includes('.')) nested(...(path.split('.') as [string, string]), value)
    else body[path] = value
  }
  return body
}

export default function Transact() {
  const versions = useLoad(() => api<Version[]>('/versions?channel=console'))
  const [name, setName] = useState('')
  const version = versions.data?.find((v) => v.name === name)
  const [tick, setTick] = useState(0)

  return (
    <>
      <PageHeader module="FT · TRANSACTIONS" title="Input a transaction" />
      <div className="grid-2">
        <Panel title="Version" aside={<span className="muted">the screen decides the fields, defaults and checks</span>}>
          {versions.loading && <Loading />}
          <ErrorNote error={versions.error} />
          {versions.data?.length === 0 && <Empty>No version is input from the console. Define one under Transaction setup.</Empty>}
          <div className="version-list">
            {versions.data?.map((v) => (
              <button key={v.name} type="button" className={`version-pick ${v.name === name ? 'on' : ''}`} onClick={() => setName(v.name)}>
                <Mono>{v.name}</Mono>
                <span>{v.description}</span>
                <small className="muted">{v.transactionType} · {v.flow}{v.authorisers > 0 ? ' · needs a checker' : ' · straight through'}</small>
              </button>
            ))}
          </div>
        </Panel>
        {version ? <Screen key={version.name} version={version} onInput={() => setTick((t) => t + 1)} /> : <Panel title="Screen"><Empty>Pick a version.</Empty></Panel>}
      </div>
      <Unauthorised version={tick} />
    </>
  )
}

function Screen({ version, onInput }: { version: Version; onInput: () => void }) {
  const screen: VersionField[] = useMemo(() => {
    if (version.fields?.length) return version.fields.filter((f) => !f.hidden && !f.field.startsWith('local.'))
    return version.inputs.map((field) => ({ field }))
  }, [version])
  const localFields: (LocalColumn & { screen?: VersionField })[] = version.localFields.map((f) => ({ ...f, screen: version.fields?.find((v) => v.field === `local.${f.name}`) }))
  const [values, setValues] = useState<Record<string, string>>(() => Object.fromEntries(screen.map((f) => [f.field, f.default ?? ''])))
  const [local, setLocal] = useState<Record<string, string>>(() => Object.fromEntries(localFields.map((f) => [f.name, f.screen?.default ?? ''])))
  const [overrides, setOverrides] = useState<Override[]>([])

  const input = useAction(async (accepted: string[]) => {
    const accountNumber = values.debitAccount || values.creditAccount
    const currency = values.currency || (accountNumber ? (await api<Account>(`/accounts/by-number/${accountNumber.trim()}`)).currency : 'KES')
    const editable = Object.fromEntries(Object.entries(values).filter(([path]) => !screen.find((f) => f.field === path)?.noInput))
    const editableLocal = Object.fromEntries(Object.entries(local).filter(([n]) => !localFields.find((f) => f.name === n)?.screen?.noInput))
    try {
      const result = await api<TransactionView>('/transactions', { method: 'POST', body: { ...requestBody(version, editable, editableLocal, currency), acceptedOverrides: accepted } })
      setOverrides([])
      onInput()
      return result
    } catch (e) {
      if (e instanceof ApiError && e.code === 'overrides_required') setOverrides(e.body.overrides as Override[])
      throw e
    }
  })

  const submit = (e: FormEvent) => { e.preventDefault(); setOverrides([]); void input.run([]) }
  const result = input.result

  return (
    <Panel title={version.name} aside={<span className="muted">{version.description}</span>}>
      <form className="form" onSubmit={submit}>
        {screen.map((f) => (
          <Field key={f.field} label={`${f.label || fieldLabel(f.field)}${f.mandatory ? ' *' : ''}`} hint={f.noInput ? 'Fixed by the version' : undefined}>
            {f.field === 'paymentDetails' || f.field === 'attributes' ? (
              <textarea rows={3} value={values[f.field] ?? ''} disabled={f.noInput} placeholder={f.field === 'attributes' ? 'name=value per line' : 'Up to 4 lines'}
                onChange={(e) => setValues({ ...values, [f.field]: e.target.value })} />
            ) : (
              <input className={/Account|reference|Reference|channelCode/.test(f.field) ? 'mono' : ''} required={f.mandatory && !f.noInput} disabled={f.noInput}
                value={values[f.field] ?? ''} onChange={(e) => setValues({ ...values, [f.field]: e.target.value })} />
            )}
          </Field>
        ))}
        {localFields.length > 0 && <span className="mono-label">LOCAL FIELDS</span>}
        {localFields.map((f) => (
          <Field key={f.name} label={`${f.screen?.label || f.label}${f.screen?.mandatory || f.required ? ' *' : ''}`}>
            <LocalInput field={f} value={local[f.name] ?? ''} disabled={f.screen?.noInput} onChange={(v) => setLocal({ ...local, [f.name]: v })} />
          </Field>
        ))}
        {overrides.length > 0 ? (
          <div className="callout warn">
            <span className="mono-label">OVERRIDES RAISED</span>
            <ul>{overrides.map((o) => <li key={o.code}><Mono>{o.code}</Mono> — {o.message}</li>)}</ul>
            <Button type="button" variant="accent" busy={input.busy} onClick={() => void input.run(overrides.map((o) => o.code))}>Accept overrides and input</Button>
          </div>
        ) : (
          <>
            <ErrorNote error={input.error} />
            <Button busy={input.busy} disabled={!can('maker')}>{version.authorisers > 0 ? 'Input for authorisation' : 'Input'}</Button>
            {!can('maker') && <p className="muted">Inputting needs the maker permission.</p>}
          </>
        )}
      </form>
      {result && overrides.length === 0 && (
        <div className="result">
          <div className="result-head">
            <Badge value={result.recordStatus} /> <Badge value={result.status} />
            <strong>{money(result.amount, result.currency)}</strong>
            <Link to={`/console/transactions/${result.reference}`}><Mono>{result.reference}</Mono></Link>
          </div>
          {result.pendingOverrides?.length ? <p className="muted">Waiting for a checker: {result.pendingOverrides.map((o) => o.code).join(', ')}</p> : null}
          {result.recordStatus === 'INAU' && !result.pendingOverrides?.length && <p className="muted">Waiting for a checker: version {result.version} has authorisers.</p>}
          {result.declineCode && <p className="muted">{result.declineCode}</p>}
        </div>
      )}
    </Panel>
  )
}

/** One input for a local field, by its type. */
export function LocalInput({ field, value, onChange, disabled }: { field: LocalColumn; value: string; onChange: (v: string) => void; disabled?: boolean }) {
  if (field.type === 'choice') {
    return (
      <select value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        <option value="">—</option>
        {field.options?.map((o) => <option key={o}>{o}</option>)}
      </select>
    )
  }
  if (field.type === 'boolean') {
    return (
      <select value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        <option value="">—</option><option value="true">Yes</option><option value="false">No</option>
      </select>
    )
  }
  return (
    <input value={value} disabled={disabled} type={field.type === 'date' ? 'date' : 'text'} inputMode={field.type === 'number' || field.type === 'amount' ? 'decimal' : undefined}
      placeholder={field.type === 'lookup' ? `${field.lookup} key` : field.type === 'amount' ? 'minor units' : undefined}
      maxLength={field.maxLength} onChange={(e) => onChange(e.target.value)} />
  )
}

/** Transactions waiting for a checker, with authorise and reject. */
export function Unauthorised({ version }: { version?: number }) {
  const pending = useLoad(() => api<Transaction[]>('/ops/transactions/pending'), [version])
  const decide = useAction((reference: string, ok: boolean) =>
    api(`/transactions/${reference}/${ok ? 'authorise' : 'reject'}`, { method: 'POST', body: ok ? undefined : { reason: 'Not approved' } }))
  const act = async (reference: string, ok: boolean) => { if (await decide.run(reference, ok)) pending.reload() }
  return (
    <Panel title="Transactions awaiting authorisation" aside={<span className="muted">INAU · four eyes</span>}>
      <ErrorNote error={decide.error ?? pending.error} />
      {pending.data?.length === 0 && <Empty>Nothing waiting.</Empty>}
      {pending.data?.map((t) => (
        <div key={t.id} className="approval">
          <div>
            <Badge value={t.recordStatus} /> <Link to={`/console/transactions/${t.reference}`}><Mono>{t.reference}</Mono></Link>{' '}
            <Mono>{t.version}</Mono> <strong>{money(t.amount, t.currency)}</strong>
            <p className="muted">
              {t.debitAccount && <>from <Mono>{t.debitAccount}</Mono> </>}
              {t.creditAccount && <>to <Mono>{t.creditAccount}</Mono> </>}
              {t.beneficiaryName && <>· {t.beneficiaryName} </>}
              · input by <strong>{t.inputter}</strong> · {dateTime(t.inputAt)}
              {t.overridesAccepted.length > 0 && <> · overrides {t.overridesAccepted.join(', ')}</>}
            </p>
          </div>
          <Decision inputter={t.inputter}>
            <Button variant="accent" busy={decide.busy} onClick={() => void act(t.reference, true)}>Authorise</Button>
            <Button variant="ghost" busy={decide.busy} onClick={() => void act(t.reference, false)}>Reject</Button>
          </Decision>
        </div>
      ))}
    </Panel>
  )
}
