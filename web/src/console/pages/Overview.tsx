import { Link } from 'react-router-dom'
import { ArrowLeftRight, BadgeCheck, CircleAlert, Plug, RefreshCw, Send, ShieldCheck, Users, Wallet } from 'lucide-react'
import { Bar, BarChart, CartesianGrid, LabelList, XAxis, YAxis } from 'recharts'
import { Button as ShadButton } from '@/components/ui/button'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { api } from '../api'
import { dateTime, day, label, money } from '../format'
import { useLoad } from '../hooks'
import type { JournalEntryView, ProviderStatus, SystemDates, TrialBalanceLine } from '../types'
import { Badge, Empty, ErrorNote, Loading, Mono, PageHeader, Panel, Stat } from '../ui'

type Dashboard = {
  customers: { total: number; byType: Record<string, number>; new24h: number }
  accounts: { open: number; sub: number; closed: number; byCurrency: Record<string, number> }
  transactions: {
    last24h: number
    byStatus: Record<string, number>
    byFlow: Record<string, number>
    value24h: Record<string, number>
    hourly: { hour: string; completed: number; failed: number; other: number }[]
  }
  pending: { transactions: number; changes: number; payoutReviews: number; escalatedPayouts: number }
}

type OverviewData = {
  dates: SystemDates
  board: Dashboard
  trial: TrialBalanceLine[]
  integrity: { healthy: boolean; drift: unknown[] }
  providers: ProviderStatus[]
  journal: JournalEntryView[]
  loadedAt: string
}

async function loadOverview(): Promise<OverviewData> {
  const dates = await api<SystemDates>('/core/dates')
  const [board, trial, integrity, providers, journal] = await Promise.all([
    api<Dashboard>('/ops/dashboard'),
    api<TrialBalanceLine[]>('/ops/gl/trial-balance'),
    api<{ healthy: boolean; drift: unknown[] }>('/ops/gl/integrity'),
    api<ProviderStatus[]>('/ops/providers'),
    api<JournalEntryView[]>(`/ops/gl/journal?date=${dates.onlineBookingDate}&limit=8`),
  ])
  return { dates, board, trial, integrity, providers, journal, loadedAt: new Date().toISOString() }
}

const number = new Intl.NumberFormat('en-KE')
const compact = new Intl.NumberFormat('en-KE', { notation: 'compact', maximumFractionDigits: 1 })

const volumeConfig = {
  completed: { label: 'Completed', color: 'var(--chart-ok)' },
  other: { label: 'In progress', color: 'var(--chart-pending)' },
  failed: { label: 'Failed', color: 'var(--chart-failed)' },
} satisfies ChartConfig

const flowConfig = { count: { label: 'Transactions', color: 'var(--chart-1)' } } satisfies ChartConfig
const currencyConfig = { count: { label: 'Open accounts', color: 'var(--chart-1)' } } satisfies ChartConfig

export default function Overview() {
  const { data, error, loading, reload } = useLoad(loadOverview)

  if (loading && !data) {
    return (
      <>
        <PageHeader module="OVERVIEW" title="Dashboard" />
        <div className="stat-grid">{Array.from({ length: 6 }, (_, i) => <Panel key={i}><Loading /></Panel>)}</div>
      </>
    )
  }
  if (error) return <ErrorNote error={error} />
  if (!data) return null

  const { dates, board, trial, integrity, providers, journal, loadedAt } = data
  const balanced = trial.every((t) => t.balanced) && integrity.healthy
  const degraded = providers.filter((p) => p.circuit !== 'Closed' || p.simulatedOutage || p.notRoutable)
  const tx = board.transactions
  const completed = tx.byStatus.Completed ?? 0
  const failed = tx.byStatus.Failed ?? 0
  const finished = completed + failed
  const successRate = finished ? (100 * completed) / finished : undefined
  const awaiting = board.pending.transactions + board.pending.changes + board.pending.payoutReviews + board.pending.escalatedPayouts
  const hourly = tx.hourly.map((h) => ({ ...h, label: new Date(h.hour).toLocaleTimeString('en-KE', { hour: '2-digit', minute: '2-digit' }) }))
  const flows = Object.entries(tx.byFlow).map(([flow, count]) => ({ flow: label(flow), count })).sort((a, b) => b.count - a.count)
  const currencies = Object.entries(board.accounts.byCurrency).map(([currency, count]) => ({ currency, count })).sort((a, b) => b.count - a.count)
  const values = Object.entries(tx.value24h).sort((a, b) => b[1] - a[1])

  return (
    <>
      <PageHeader module="OVERVIEW" title="Dashboard" description={<>Business date {day(dates.today)} · the last 24 hours, as of {dateTime(loadedAt)}</>}>
        <ShadButton variant="outline" size="sm" onClick={reload}><RefreshCw />Refresh</ShadButton>
        <ShadButton size="sm" asChild><Link to="/console/transact"><Send />Input a transaction</Link></ShadButton>
      </PageHeader>

      <div className="stat-grid">
        <Stat label="Customers" icon={<Users />} value={number.format(board.customers.total)}
          hint={<>{Object.entries(board.customers.byType).map(([t, n]) => `${number.format(n)} ${label(t).toLowerCase()}`).join(' · ')} · +{number.format(board.customers.new24h)} in 24h</>} />
        <Stat label="Open accounts" icon={<Wallet />} value={number.format(board.accounts.open)}
          hint={`${number.format(board.accounts.sub)} sub-accounts · ${number.format(board.accounts.closed)} closed`} />
        <Stat label="Transactions, 24h" icon={<ArrowLeftRight />} value={number.format(tx.last24h)}
          hint={successRate === undefined ? 'None finished yet' : `${successRate.toFixed(1)}% completed of ${number.format(finished)} finished`}
          tone={successRate !== undefined && successRate < 90 ? 'warn' : undefined} />
        <Stat label="Value moved, 24h" icon={<BadgeCheck />} value={values[0] ? `${values[0][0]} ${compact.format(values[0][1] / 100)}` : '—'}
          hint={values.slice(1, 4).map(([c, v]) => `${c} ${compact.format(v / 100)}`).join(' · ') || 'Completed transactions'} />
        <Stat label="Awaiting a person" icon={<ShieldCheck />} value={number.format(awaiting)} tone={awaiting ? 'warn' : undefined}
          hint={<Link to="/console/controls" className="hover:underline">{board.pending.transactions} transactions · {board.pending.changes} changes · {board.pending.payoutReviews} payouts to review · {board.pending.escalatedPayouts} outcome unknown</Link>} />
        <Stat label="Books" icon={balanced ? <BadgeCheck /> : <CircleAlert />} value={balanced ? 'In balance' : 'Out of balance'} tone={balanced ? 'good' : 'bad'}
          hint={`${trial.length} currencies · ${integrity.drift.length} drifted accounts`} />
      </div>

      <div className="grid-main-side">
        <Panel title="Transactions by hour" aside={<span className="muted">last 24 hours</span>}>
          {tx.last24h === 0 ? (
            <Empty>No transactions in the last 24 hours.</Empty>
          ) : (
            <ChartContainer config={volumeConfig} className="aspect-auto h-72 w-full">
              <BarChart data={hourly} margin={{ left: 0, right: 8, top: 8 }} barCategoryGap={3}>
                <CartesianGrid vertical={false} strokeOpacity={0.5} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} tickMargin={8} minTickGap={24} />
                <YAxis tickLine={false} axisLine={false} width={44} tickFormatter={(v: number) => compact.format(v)} />
                <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent />} />
                <ChartLegend content={<ChartLegendContent />} />
                <Bar dataKey="completed" stackId="v" fill="var(--color-completed)" stroke="var(--card)" strokeWidth={2} radius={[0, 0, 4, 4]} />
                <Bar dataKey="other" stackId="v" fill="var(--color-other)" stroke="var(--card)" strokeWidth={2} />
                <Bar dataKey="failed" stackId="v" fill="var(--color-failed)" stroke="var(--card)" strokeWidth={2} radius={[4, 4, 0, 0]} />
              </BarChart>
            </ChartContainer>
          )}
        </Panel>

        <Panel title="Providers" aside={<Link to="/console/operations" className="panel-link">Operations →</Link>}>
          <ul className="flex flex-col divide-y">
            {providers.map((p) => (
              <li key={p.name} className="flex items-center justify-between gap-3 py-2.5 text-sm">
                <span className="flex items-center gap-2">
                  <Plug className="size-4 text-muted-foreground" />
                  <span className="font-medium">{p.name}</span>
                  {!p.live && <span className="tag">simulated</span>}
                </span>
                <Badge value={p.simulatedOutage ? 'Failed' : p.circuit === 'Closed' ? 'Online' : p.circuit} />
              </li>
            ))}
          </ul>
          <p className="mt-3 text-xs text-muted-foreground">
            {degraded.length ? `${degraded.length} degraded: payments fail over to the next provider.` : 'Every circuit is closed.'}
          </p>
        </Panel>
      </div>

      <div className="grid-2">
        <Panel title="By flow" aside={<span className="muted">last 24 hours</span>}>
          {flows.length === 0 ? <Empty>Nothing yet.</Empty> : (
            <ChartContainer config={flowConfig} className="aspect-auto w-full" style={{ height: Math.max(120, flows.length * 36) }}>
              <BarChart data={flows} layout="vertical" margin={{ left: 0, right: 40 }}>
                <XAxis type="number" hide />
                <YAxis type="category" dataKey="flow" tickLine={false} axisLine={false} width={110} />
                <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent hideLabel />} />
                <Bar dataKey="count" fill="var(--color-count)" radius={[0, 4, 4, 0]} barSize={18}>
                  <LabelList dataKey="count" position="right" offset={8} className="fill-muted-foreground" fontSize={12} formatter={(v: unknown) => number.format(Number(v))} />
                </Bar>
              </BarChart>
            </ChartContainer>
          )}
        </Panel>

        <Panel title="Open accounts by currency" aside={<Link to="/console/accounts" className="panel-link">Accounts →</Link>}>
          {currencies.length === 0 ? <Empty>No accounts yet.</Empty> : (
            <ChartContainer config={currencyConfig} className="aspect-auto w-full" style={{ height: Math.max(120, currencies.length * 36) }}>
              <BarChart data={currencies} layout="vertical" margin={{ left: 0, right: 48 }}>
                <XAxis type="number" hide />
                <YAxis type="category" dataKey="currency" tickLine={false} axisLine={false} width={48} />
                <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent hideLabel />} />
                <Bar dataKey="count" fill="var(--color-count)" radius={[0, 4, 4, 0]} barSize={18}>
                  <LabelList dataKey="count" position="right" offset={8} className="fill-muted-foreground" fontSize={12} formatter={(v: unknown) => number.format(Number(v))} />
                </Bar>
              </BarChart>
            </ChartContainer>
          )}
        </Panel>
      </div>

      <div className="grid-2">
        <Panel title="Trial balance by currency" aside={<Link to="/console/ledger" className="panel-link">General ledger →</Link>}>
          <table>
            <thead>
              <tr><th>Currency</th><th className="num">Debit-normal</th><th className="num">Credit-normal</th><th>Status</th></tr>
            </thead>
            <tbody>
              {trial.filter((t) => t.debitNormalTotal || t.creditNormalTotal).map((t) => (
                <tr key={t.currency}>
                  <td><Mono>{t.currency}</Mono></td>
                  <td className="num">{money(t.debitNormalTotal, t.currency)}</td>
                  <td className="num">{money(t.creditNormalTotal, t.currency)}</td>
                  <td><Badge value={t.balanced ? 'Matched' : 'Failed'} /></td>
                </tr>
              ))}
            </tbody>
          </table>
          {trial.every((t) => !t.debitNormalTotal && !t.creditNormalTotal) && <Empty>No postings yet.</Empty>}
        </Panel>

        <Panel title={`Latest postings · ${day(dates.onlineBookingDate)}`} aside={<span className="muted">latest {journal.length}</span>}>
          {journal.length === 0 ? (
            <Empty>No entries booked today.</Empty>
          ) : (
            <ul className="journal-feed">
              {journal.slice().reverse().map((t) => (
                <li key={t.id}>
                  <Mono>{t.transactionReference}</Mono>
                  <span className="truncate">{t.description}</span>
                  <span className="text-xs text-muted-foreground">{dateTime(t.postedAt)}</span>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>
    </>
  )
}
