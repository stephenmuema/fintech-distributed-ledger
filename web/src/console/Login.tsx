import { useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { api, setSession, type Session } from './api'
import { useAction } from './hooks'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button, ErrorNote, Field } from './ui'
import './console.css'

/** Sign-in for operators. The API answers with an HttpOnly session cookie and a CSRF token. */
export default function Login() {
  const navigate = useNavigate()
  const from = (useLocation().state as { from?: string } | null)?.from
  const [form, setForm] = useState({ username: '', password: '' })
  const login = useAction(() => api<Session>('/auth/login', { method: 'POST', body: form }))

  return (
    <main className="grid min-h-svh lg:grid-cols-2">
      <section className="hidden flex-col justify-between bg-sidebar p-10 text-sidebar-foreground lg:flex">
        <div className="flex items-center gap-2 font-semibold">
          <div className="flex size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground">Z</div>
          Zirnis
        </div>
        <div className="space-y-3">
          <p className="text-3xl font-semibold tracking-tight">Core banking and payments, run by your own team.</p>
          <p className="max-w-md text-sm text-sidebar-foreground/70">
            Every change here is input by one person and authorised by another, recorded field by field, and reconciled with the providers.
          </p>
        </div>
        <p className="text-xs text-sidebar-foreground/50">Internal network only</p>
      </section>
      <section className="flex items-center justify-center p-6">
        <Card className="w-full max-w-sm shadow-sm">
          <CardHeader>
            <CardTitle className="text-xl">Sign in</CardTitle>
            <CardDescription>Use your Zirnis operator account.</CardDescription>
          </CardHeader>
          <CardContent>
            <form className="form" onSubmit={async (e) => {
              e.preventDefault()
              const session = await login.run()
              if (session) {
                setSession(session)
                navigate(from && from.startsWith('/console') ? from : '/console', { replace: true })
              }
            }}>
              <Field label="Username"><input required autoFocus autoComplete="username" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value.trim() })} /></Field>
              <Field label="Password"><input required type="password" autoComplete="current-password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} /></Field>
              <ErrorNote error={login.error} />
              <Button busy={login.busy} className="w-full">Sign in</Button>
              <p className="text-xs text-muted-foreground">
                Five wrong passwords lock the account for 15 minutes; sessions end after 30 minutes without activity.
              </p>
            </form>
          </CardContent>
        </Card>
      </section>
    </main>
  )
}

/** Changing your own password; required after an administrator issues a one-time password. */
export function ChangePassword({ forced, onDone }: { forced: boolean; onDone: (session: Session) => void }) {
  const [form, setForm] = useState({ currentPassword: '', newPassword: '', confirm: '' })
  const change = useAction(() => {
    if (form.newPassword !== form.confirm) return Promise.reject(new Error('The new passwords do not match.'))
    return api<Session>('/auth/password', { method: 'POST', body: { currentPassword: form.currentPassword, newPassword: form.newPassword } })
  })
  return (
    <form className="form max-w-xl" onSubmit={async (e) => { e.preventDefault(); const s = await change.run(); if (s) onDone(s) }}>
      <span className="mono-label">{forced ? 'CHOOSE YOUR OWN PASSWORD TO CONTINUE' : 'CHANGE PASSWORD'}</span>
      {forced && <p className="muted">You signed in with a one-time password from an administrator. Choose your own before doing anything else.</p>}
      <Field label={forced ? 'One-time password' : 'Current password'}>
        <input required type="password" autoComplete="current-password" value={form.currentPassword} onChange={(e) => setForm({ ...form, currentPassword: e.target.value })} />
      </Field>
      <div className="row-2">
        <Field label="New password" hint="At least 12 characters, without your username">
          <input required minLength={12} type="password" autoComplete="new-password" value={form.newPassword} onChange={(e) => setForm({ ...form, newPassword: e.target.value })} />
        </Field>
        <Field label="Repeat it"><input required type="password" autoComplete="new-password" value={form.confirm} onChange={(e) => setForm({ ...form, confirm: e.target.value })} /></Field>
      </div>
      <ErrorNote error={change.error} />
      <Button busy={change.busy}>Change password</Button>
    </form>
  )
}
