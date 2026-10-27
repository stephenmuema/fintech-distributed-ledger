import { useState } from 'react'
import { api } from '../api'
import { dateTime } from '../format'
import { useAction, useLoad } from '../hooks'
import { PendingChanges } from '../pending'
import type { Extension, ExtensionCall } from '../types'
import { Badge, Button, Empty, ErrorNote, Field, Mono, PageHeader, Panel } from '../ui'

export default function Extensions() {
  const extensions = useLoad(() => api<Extension[]>('/core/extensions'))
  const hooks = useLoad(() => api<string[]>('/core/extensions/hooks'))
  const [selected, setSelected] = useState<string>()
  const [editing, setEditing] = useState<Extension | 'new'>()
  const [version, setVersion] = useState(0)
  const calls = useLoad(() => api<ExtensionCall[]>(`/core/extensions/calls?limit=50${selected ? `&extension=${selected}` : ''}`), [selected, version])

  return (
    <>
      <PageHeader module="EB.API · EXTENSIONS" title="Extensions">
        <Button onClick={() => setEditing('new')}>Register remote extension</Button>
      </PageHeader>
      <p className="muted intro">
        Extensions are local routines: bank-specific code consulted at hook points — classify and match suspense items, veto clearings, match
        nostro statements, extra Close-of-Business work. Compile Go into the binary (<Mono>App.Extensions.Register</Mono>), or run any service that
        speaks the signed HTTP contract — the Python and Go SDKs do — and register it here under maker-checker. Extensions never post: the core
        re-validates every decision and books it under its own controls.
      </p>
      <div className="grid-main-side">
        <Panel title="Registered">
          <ErrorNote error={extensions.error} />
          {extensions.data?.length === 0 && <Empty>No extensions yet.</Empty>}
          {!!extensions.data?.length && (
            <table>
              <thead><tr><th>Name</th><th>Kind</th><th>Hooks</th><th>On failure</th><th>Circuit</th><th>Last call</th><th /></tr></thead>
              <tbody>
                {extensions.data.map((x) => (
                  <tr key={x.name} className={`clickable ${selected === x.name ? 'selected' : ''}`} onClick={() => setSelected(selected === x.name ? undefined : x.name)}>
                    <td><Mono>{x.name}</Mono>{x.url && <><br /><small className="muted">{x.url}</small></>}
                      {x.secretPresent === false && <><br /><small className="error-text">secret not set on this instance</small></>}</td>
                    <td>{x.kind === 'InProcess' ? 'Go, in-process' : 'Remote'} {!x.enabled && <Badge value="Disabled" />}</td>
                    <td>{x.hooks.map((h) => <div key={h}><Mono>{h}</Mono></div>)}</td>
                    <td>{x.failurePolicy === 'FailClosed' ? 'Block' : 'Carry on'}</td>
                    <td><Badge value={x.circuit} />{x.consecutiveFailures > 0 && <><br /><small className="error-text">{x.consecutiveFailures} failure(s): {x.lastError}</small></>}</td>
                    <td className="nowrap">{dateTime(x.lastCallAt)}</td>
                    <td>{x.kind === 'Remote' && <button className="link-btn" onClick={(e) => { e.stopPropagation(); setEditing(x) }}>Amend</button>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <h3 className="sub">Calls {selected && <>· <Mono>{selected}</Mono></>}</h3>
          {calls.data?.length === 0 && <Empty>No calls yet.</Empty>}
          <ul className="audit-feed">
            {calls.data?.map((c) => (
              <li key={c.id}>
                <Badge value={c.outcome === 'ok' ? 'Succeeded' : 'Failed'} />
                <span><Mono>{c.extension}</Mono> · {c.hook} · {c.subject} · {c.latencyMs} ms{c.error && <span className="error-text"> · {c.error}</span>}</span>
                <span className="muted nowrap">{dateTime(c.calledAt)}</span>
              </li>
            ))}
          </ul>
        </Panel>
        <div className="stack">
          {editing && <EndpointForm key={editing === 'new' ? 'new' : editing.name} hooks={hooks.data ?? []} existing={editing === 'new' ? undefined : editing}
            onDone={() => { setEditing(undefined); setVersion((v) => v + 1) }} />}
          <PendingChanges types={['HookEndpoint']} version={version} onDecided={() => { extensions.reload(); setVersion((v) => v + 1) }} />
          <Panel title="Writing an extension">
            <p className="muted">Python (stdlib only):</p>
            <pre className="json">{`from zirnis_ext import Extension, serve

ext = Extension("mpesa-matcher")

@ext.match
def match(req):
    return {"proposals": [...]}

serve(ext, port=8090, secret_env="ZIRNIS_HOOK_SECRET")`}</pre>
            <p className="muted">Then register its URL here, name the environment variable holding the shared secret, and add the extension to a suspense type or nostro.</p>
          </Panel>
        </div>
      </div>
    </>
  )
}

function EndpointForm({ hooks, existing, onDone }: { hooks: string[]; existing?: Extension; onDone: () => void }) {
  const [name, setName] = useState(existing?.name ?? '')
  const [form, setForm] = useState({
    url: existing?.url ?? 'http://localhost:8090/hooks', hooks: existing?.hooks ?? ['suspense.match'], secretEnv: existing?.secretEnv ?? 'ZIRNIS_HOOK_SECRET',
    timeoutMs: existing?.timeoutMs ?? 5000, failurePolicy: existing?.failurePolicy ?? 'FailOpen', enabled: existing?.enabled ?? true,
  })
  const submit = useAction(() => api(`/ops/parameters/hooks/${name}`, { method: 'POST', body: form }))
  return (
    <Panel title={existing ? `Amend ${existing.name}` : 'Register a remote extension'} aside={<span className="muted">INAU until authorised</span>}>
      <form className="form" onSubmit={async (e) => { e.preventDefault(); if (await submit.run()) onDone() }}>
        <div className="row-2">
          <Field label="Name" hint="lower-case, e.g. mpesa-matcher"><input required readOnly={!!existing} value={name} onChange={(e) => setName(e.target.value.toLowerCase())} /></Field>
          <Field label="URL"><input required value={form.url} onChange={(e) => setForm({ ...form, url: e.target.value })} /></Field>
        </div>
        <Field label="Hooks served">
          <div className="chips">
            {hooks.map((h) => (
              <label key={h} className="check"><input type="checkbox" checked={form.hooks.includes(h)}
                onChange={() => setForm({ ...form, hooks: form.hooks.includes(h) ? form.hooks.filter((x) => x !== h) : [...form.hooks, h] })} /> <Mono>{h}</Mono></label>
            ))}
          </div>
        </Field>
        <div className="row-3">
          <Field label="Secret env var" hint="the secret itself is never stored"><input required value={form.secretEnv} onChange={(e) => setForm({ ...form, secretEnv: e.target.value.toUpperCase() })} /></Field>
          <Field label="Timeout (ms)"><input type="number" min={100} max={60000} value={form.timeoutMs} onChange={(e) => setForm({ ...form, timeoutMs: Number(e.target.value) })} /></Field>
          <Field label="When it fails">
            <select value={form.failurePolicy} onChange={(e) => setForm({ ...form, failurePolicy: e.target.value })}>
              <option value="FailOpen">Carry on (FailOpen)</option>
              <option value="FailClosed">Block (FailClosed)</option>
            </select>
          </Field>
        </div>
        <label className="check"><input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} /> Enabled</label>
        <ErrorNote error={submit.error} />
        <div className="actions">
          <Button type="button" variant="ghost" onClick={onDone}>Cancel</Button>
          <Button busy={submit.busy}>Input (INAU)</Button>
        </div>
      </form>
    </Panel>
  )
}
