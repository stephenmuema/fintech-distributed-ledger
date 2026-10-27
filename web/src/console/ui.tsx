import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { AlertCircle, Inbox, Loader2 } from 'lucide-react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge as ShadBadge } from '@/components/ui/badge'
import { Button as ShadButton } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'
import { ApiError } from './api'
import { label } from './format'

// The console's building blocks, on shadcn/ui. Pages use these rather than the components
// directly, so the look stays consistent and can change in one place.

export function PageHeader({ module, title, children, description }: { module: string; title: string; children?: ReactNode; description?: ReactNode }) {
  return (
    <header className="page-header">
      <div className="space-y-1">
        <p className="mono-label">{module}</p>
        <h1>{title}</h1>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {children && <div className="page-actions">{children}</div>}
    </header>
  )
}

export function Panel({ title, aside, children, className }: { title?: string; aside?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <Card className={cn('panel gap-4 py-5 shadow-xs', className)}>
      {(title || aside) && (
        <CardHeader className="px-5">
          {title && <CardTitle className="text-base">{title}</CardTitle>}
          {aside && <CardAction className="text-sm">{aside}</CardAction>}
        </CardHeader>
      )}
      <CardContent className="min-w-0 overflow-x-auto px-5">{children}</CardContent>
    </Card>
  )
}

const toneText = { good: 'text-success', warn: 'text-warning', bad: 'text-destructive' } as const

export function Stat({ label: name, value, hint, tone, icon }: { label: string; value: ReactNode; hint?: ReactNode; tone?: 'good' | 'warn' | 'bad'; icon?: ReactNode }) {
  return (
    <Card className="stat gap-2 py-4 shadow-xs">
      <CardHeader className="px-4">
        <span className="text-sm font-medium text-muted-foreground">{name}</span>
        {icon && <CardAction className="text-muted-foreground [&_svg]:size-4">{icon}</CardAction>}
      </CardHeader>
      <CardContent className="px-4">
        <div className={cn('text-2xl font-semibold tracking-tight tabular-nums', tone && toneText[tone])}>{value}</div>
        {hint && <p className="mt-1 text-xs text-muted-foreground">{hint}</p>}
      </CardContent>
    </Card>
  )
}

const tones: Record<string, 'good' | 'warn' | 'bad' | 'info'> = {
  Succeeded: 'good', Completed: 'good', Paid: 'good', Verified: 'good', Authorised: 'good', Matched: 'good', Approved: 'good',
  Captured: 'good', Executed: 'good', Won: 'good', Delivered: 'good', Online: 'good', Closed: 'good', Active: 'good', True: 'good',
  Cleared: 'good', Reconciled: 'good', Closes: 'good', Manual: 'info', Reference: 'good', AmountDate: 'good', Extension: 'info',
  PartiallyCleared: 'info', Parked: 'info', Warn: 'info', PendingClearing: 'warn', Unmatched: 'warn', WrittenOff: 'warn', Retired: 'warn', Disabled: 'warn',
  Escalated: 'bad', Break: 'bad', 'Halts COB': 'bad', Held: 'info', Delivering: 'info', AwaitingAuthorisation: 'warn', Returned: 'warn', Rerouted: 'info',
  Processing: 'info', Queued: 'info', Pending: 'info', Running: 'info', Open: 'info', EvidenceSubmitted: 'info', Initiated: 'info',
  PendingReview: 'warn', Unauthorised: 'warn', Review: 'warn', Halted: 'warn', CloseOfBusiness: 'warn', HalfOpen: 'warn',
  AmountMismatch: 'warn', MissingInLedger: 'warn', MissingInReport: 'warn', DebitBlock: 'warn', CreditBlock: 'warn', Frozen: 'warn', Skipped: 'warn',
  LIVE: 'good', applied: 'good', INAU: 'warn', pending: 'warn', RJCT: 'bad', failed: 'bad', Required: 'warn', Optional: 'info', Sandbox: 'warn',
  Failed: 'bad', Declined: 'bad', Rejected: 'bad', Lost: 'bad', DeadLettered: 'bad', Decline: 'bad', FullBlock: 'bad', Reversed: 'bad', Expired: 'bad', False: 'bad',
}

const toneBadge = {
  good: 'border-success/30 bg-success/10 text-success',
  warn: 'border-warning/40 bg-warning/15 text-[color-mix(in_oklch,var(--warning),black_35%)] dark:text-warning',
  bad: 'border-destructive/30 bg-destructive/10 text-destructive',
  info: 'border-info/30 bg-info/10 text-info',
}

export function Badge({ value }: { value: string | boolean | null | undefined }) {
  if (value === null || value === undefined) return <ShadBadge variant="outline">—</ShadBadge>
  const text = typeof value === 'boolean' ? (value ? 'True' : 'False') : value
  const tone = tones[text]
  return (
    <ShadBadge variant="outline" className={cn('font-medium', tone && toneBadge[tone])}>
      {tone && <span className="size-1.5 rounded-full bg-current" aria-hidden />}
      {label(text)}
    </ShadBadge>
  )
}

export function ErrorNote({ error }: { error?: Error }) {
  if (!error) return null
  const code = error instanceof ApiError ? error.code : undefined
  return (
    <Alert variant="destructive" className="my-2">
      <AlertCircle />
      <AlertTitle>{code ? label(code) : 'Something went wrong'}</AlertTitle>
      <AlertDescription>{error.message}</AlertDescription>
    </Alert>
  )
}

export function Loading({ what = 'Loading' }: { what?: string }) {
  return (
    <div className="space-y-3 py-2" aria-busy="true" aria-label={what}>
      <Skeleton className="h-4 w-1/3" />
      <Skeleton className="h-4 w-2/3" />
      <Skeleton className="h-4 w-1/2" />
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="empty flex flex-col items-center gap-2 text-muted-foreground">
      <Inbox className="size-5 opacity-60" aria-hidden />
      <p>{children}</p>
    </div>
  )
}

export function Field({ label: name, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="field">
      <span className="mono-label">{name}</span>
      {children}
      {hint && <small className="muted">{hint}</small>}
    </label>
  )
}

const variants = {
  primary: { variant: 'default', className: '' },
  accent: { variant: 'default', className: 'bg-ring text-white hover:bg-ring/90' },
  ghost: { variant: 'outline', className: '' },
  danger: { variant: 'destructive', className: '' },
} as const

export function Button({ children, busy, variant = 'primary', className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { busy?: boolean; variant?: keyof typeof variants }) {
  const v = variants[variant]
  return (
    <ShadButton {...props} variant={v.variant} className={cn(v.className, className)} disabled={busy || props.disabled}>
      {busy && <Loader2 className="animate-spin" />}
      {children}
    </ShadButton>
  )
}

export function Mono({ children }: { children: ReactNode }) {
  return <span className="mono">{children}</span>
}

export function Json({ value }: { value: unknown }) {
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>
}
