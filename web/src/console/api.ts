// Thin client for the Zirnis API. Every state-changing request carries an Idempotency-Key
// (required on money-moving endpoints, honoured on all others) and the session's CSRF token, and
// errors are parsed from RFC 9457 problem details into ApiError.

// The console signs in with a username and password; the API keeps the session in an HttpOnly
// cookie the page never sees. Every state-changing request also carries the session's CSRF token,
// which the API hands out on sign-in and on /auth/session.

export type Session = {
  username: string
  name: string
  permissions: ('maker' | 'checker' | 'admin' | 'analyst')[]
  mustChangePassword: boolean
  csrfToken?: string
  /** The console may play the platform (demo money movement) only in the sandbox. */
  sandbox: boolean
}

const BASE = (import.meta.env.VITE_API_URL as string | undefined) ?? '/api'

let current: Session | undefined

export const getSession = () => current

export function setSession(session: Session | undefined) {
  current = session
}

export const can = (permission: Session['permissions'][number]) => !!current?.permissions.includes(permission)

/** Fired when the API says the session is gone (expired, signed out elsewhere, revoked). */
export const SIGNED_OUT = 'zirnis:signed-out'

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly body: Record<string, unknown>

  constructor(status: number, code: string, message: string, body: Record<string, unknown>) {
    super(message)
    this.status = status
    this.code = code
    this.body = body
  }
}

type Options = {
  method?: 'GET' | 'POST' | 'PUT'
  body?: unknown
  /** Forces an Idempotency-Key on a GET (state-changing methods always send one). */
  idempotent?: boolean
  /** Reuse a key across retries of the same logical operation. */
  idempotencyKey?: string
  /** Raw text body, e.g. a CSV settlement report. */
  text?: string
  contentType?: string
  /** Public pages (checkout): no session cookie, no CSRF token. */
  anonymous?: boolean
}

export async function api<T = unknown>(path: string, options: Options = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  const changesState = (options.method ?? 'GET') !== 'GET'
  if (changesState && current?.csrfToken && !options.anonymous) headers['X-CSRF-Token'] = current.csrfToken
  // Sign-in and passwords are never replayed by the API's idempotency store.
  const replayable = !path.startsWith('/auth/') && !path.endsWith('/password')
  if (replayable && (changesState || options.idempotent || options.idempotencyKey)) headers['Idempotency-Key'] = options.idempotencyKey ?? crypto.randomUUID()

  let body: BodyInit | undefined
  if (options.text !== undefined) {
    body = options.text
    headers['Content-Type'] = options.contentType ?? 'text/plain'
  } else if (options.body !== undefined) {
    body = JSON.stringify(options.body)
    headers['Content-Type'] = 'application/json'
  } else if (options.method === 'POST') {
    body = '{}'
    headers['Content-Type'] = 'application/json'
  }

  const response = await fetch(`${BASE}${path}`, { method: options.method ?? 'GET', headers, body, credentials: options.anonymous ? 'omit' : 'same-origin' })
  const text = await response.text()
  const json = text ? safeParse(text) : undefined

  if (!response.ok) {
    const problem = (json ?? {}) as Record<string, unknown>
    const code = (problem.code as string) ?? (problem.title as string) ?? `http_${response.status}`
    const message = (problem.detail as string) ?? validationMessage(problem) ?? response.statusText
    if (response.status === 401 && path !== '/auth/login' && !options.anonymous) {
      current = undefined
      window.dispatchEvent(new Event(SIGNED_OUT))
    }
    throw new ApiError(response.status, code, message, problem)
  }
  return json as T
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

function validationMessage(problem: Record<string, unknown>): string | undefined {
  const errors = problem.errors as Record<string, string[]> | undefined
  if (!errors) return undefined
  return Object.entries(errors).map(([field, messages]) => `${field}: ${messages.join(' ')}`).join(' · ')
}

/** Downloads a file response (e.g. an MT940 statement) and hands it to the browser. */
export async function download(path: string): Promise<void> {
  const response = await fetch(`${BASE}${path}`, { credentials: 'same-origin' })
  if (!response.ok) {
    const problem = (safeParse(await response.text()) ?? {}) as Record<string, unknown>
    throw new ApiError(response.status, (problem.code as string) ?? `http_${response.status}`, (problem.detail as string) ?? response.statusText, problem)
  }
  const name = /filename="([^"]+)"/.exec(response.headers.get('Content-Disposition') ?? '')?.[1] ?? 'statement'
  const url = URL.createObjectURL(await response.blob())
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}
