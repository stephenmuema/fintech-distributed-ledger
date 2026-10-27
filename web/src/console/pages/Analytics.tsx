import { useState, type ReactNode } from 'react'
import { ArrowDownRight, ArrowLeftRight, ArrowUpRight, BadgeCheck, Coins, Download, Minus, Receipt, Store, UserPlus, Users } from 'lucide-react'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, LabelList, XAxis, YAxis } from 'recharts'
import { Button as ShadButton } from '@/components/ui/button'
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { api } from '../api'
import { amount, dateTime, day, exponent, label } from '../format'
import { useLoad } from '../hooks'
import type { SystemDates } from '../types'
import { Empty, ErrorNote, Loading, PageHeader, Panel, Stat } from '../ui'

type KPIs = {
  transactions: number
  completed: number
  failed: number
  successRate: number
  value: number
  averageTicket: number
  fees: number
  activeCustomers: number
  newCustomers: number
  newAccounts: number
}
type Bucket = { bucket: string; completed: number; failed: number; other: number; value: number; fees: number }
type Breakdown = { key: string; count: number; completed: number; failed: number; value: number }
type Analytics = {
  from: string
  to: string
  previousFrom: string
  previousTo: string
  currency: string
  currencies: string[]
  granularity: 'day' | 'hour'
  kpis: KPIs
  previous: KPIs
  series: Bucket[]
  byFlow: Breakdown[]
  byChannel: Breakdown[]
  byType: Breakdown[]
  bySegment: Breakdown[]
  byProvider: Breakdown[]
  declines: Breakdown[]
  topMerchants: { customerId: string; name: string; count: number; value: number }[]
  customers: { total: number; byType: Record<string, number>; byKyc: Record<string, number>; byTier: Record<string, number>; new: { bucket: string; count: number }[] }
  accounts: { byCurrency: Record<string, number>; sub: number; multiCurrency: number }
  asOf: string
}

const presets = [
  { value: '1', label: 'Today' },
  { value: '7', label: 'Last 7 days' },
  { value: '30', label: 'Last 30 days' },
  { value: '90', label: 'Last 90 days' },
  { value: '365', label: 'Last 365 days' },
  { value: 'custom', label: 'Custom range' },
]
const currencies = ['KES', 'USD', 'EUR', 'GBP', 'UGX', 'TZS', 'NGN']

const number = new Intl.NumberFormat('en-KE')
const compact = new Intl.NumberFormat('en-KE', { notation: 'compact', maximumFractionDigits: 1 })
const percent = (v: number) => `${(100 * v).toFixed(1)}%`

/** Minor units → major units, for axes and compact figures. */
const major = (minor: number, currency: string) => minor / 10 ** exponent(currency)

const isoDay = (d: Date) => d.toISOString().slice(0, 10)
const minusDays = (iso: string, n: number) => {
  const d = new Date(`${iso}T00:00:00Z`)
  d.setUTCDate(d.getUTCDate() - n)
  return isoDay(d)
}

const outcomeConfig = {
  completed: { label: 'Completed', color: 'var(--chart-ok)' },
  other: { label: 'In progress', color: 'var(--chart-pending)' },
  failed: { label: 'Failed', color: 'var(--chart-failed)' },
} satisfies ChartConfig

/** Downloads rows as a CSV file. */
function downloadCsv(name: string, rows: Record<string, string | number>[]) {
  if (!rows.length) return
  const columns = Object.keys(rows[0])
  const cell = (v: string | number) => (typeof v === 'number' ? String(v) : `"${v.replace(/"/g, '""')}"`)
  const csv = [columns.join(','), ...rows.map((r) => columns.map((c) => cell(r[c] ?? '')).join(','))].join('\n')
  const url = URL.createObjectURL(new Blob([csv], { type: 'text/csv' }))
  const a = document.createElement('a')
  a.href = url
  a.download = `${name}.csv`
  a.click()
  URL.revokeObjectURL(url)
}

function CsvButton({ name, rows }: { name: string; rows: Record<string, string | number>[] }) {
  return (
    <ShadButton variant="ghost" size="sm" className="h-7 text-muted-foreground" disabled={!rows.length} onClick={() => downloadCsv(name, rows)}>
      <Download />
      CSV
    </ShadButton>
  )
}

/** The change against the previous period, in ink: whether up is good depends on the figure. */
function Delta({ now, before, rate }: { now: number; before: number; rate?: boolean }) {
  if (rate) {
    const points = 100 * (now - before)
    if (!before && !now) return <span>no earlier data</span>
    const Icon = points > 0.05 ? ArrowUpRight : points < -0.05 ? ArrowDownRight : Minus
    return <span className="inline-flex items-center gap-0.5"><Icon className="size-3.5" />{Math.abs(points).toFixed(1)} pts vs previous period</span>
  }
  if (!before) return <span>{now ? 'new this period' : 'no earlier data'}</span>
  const change = (now - before) / before
  const Icon = change > 0.0005 ? ArrowUpRight : change < -0.0005 ? ArrowDownRight : Minus
  return <span className="inline-flex items-center gap-0.5"><Icon className="size-3.5" />{Math.abs(100 * change).toFixed(1)}% vs previous period</span>
}

/** A horizontal bar per category: one series, so one colour, with its value labelled. */
function Bars({ rows, dataKey, name, format, color = 'var(--chart-1)', width = 120 }: {
  rows: Record<string, string | number>[]
  dataKey: string
  name: string
  format: (v: number) => string
  color?: string
  width?: number
}) {
  const config = { [dataKey]: { label: name, color } } satisfies ChartConfig
  return (
    <ChartContainer config={config} className="aspect-auto w-full" style={{ height: Math.max(96, rows.length * 34) }}>
      <BarChart data={rows} layout="vertical" margin={{ left: 0, right: 64 }}>
        <XAxis type="number" hide />
        <YAxis type="category" dataKey="name" tickLine={false} axisLine={false} width={width} />
        <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent hideLabel={false} formatter={(v) => format(Number(v))} />} />
        <Bar dataKey={dataKey} fill={`var(--color-${dataKey})`} radius={[0, 4, 4, 0]} barSize={16}>
          <LabelList dataKey={dataKey} position="right" offset={8} className="fill-muted-foreground" fontSize={12} formatter={(v: unknown) => format(Number(v))} />
        </Bar>
      </BarChart>
    </ChartContainer>
  )
}

/** Completed, in progress and failed per category, stacked. */
function OutcomeBars({ rows, width = 120 }: { rows: Breakdown[]; width?: number }) {
  const data = rows.map((r) => ({ name: label(r.key), completed: r.completed, failed: r.failed, other: r.count - r.completed - r.failed }))
  return (
    <ChartContainer config={outcomeConfig} className="aspect-auto w-full" style={{ height: Math.max(120, data.length * 38 + 40) }}>
      <BarChart data={data} layout="vertical" margin={{ left: 0, right: 16 }}>
        <XAxis type="number" hide />
        <YAxis type="category" dataKey="name" tickLine={false} axisLine={false} width={width} />
        <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent />} />
        <ChartLegend content={<ChartLegendContent />} />
        <Bar dataKey="completed" stackId="o" fill="var(--color-completed)" stroke="var(--card)" strokeWidth={2} barSize={18} radius={[4, 0, 0, 4]} />
        <Bar dataKey="other" stackId="o" fill="var(--color-other)" stroke="var(--card)" strokeWidth={2} barSize={18} />
        <Bar dataKey="failed" stackId="o" fill="var(--color-failed)" stroke="var(--card)" strokeWidth={2} barSize={18} radius={[0, 4, 4, 0]} />
      </BarChart>
    </ChartContainer>
  )
}

const breakdownRows = (rows: Breakdown[], currency: string) =>
  rows.map((r) => ({ key: r.key, transactions: r.count, completed: r.completed, failed: r.failed, [`value_${currency}`]: major(r.value, currency) }))

const mapRows = (m: Record<string, number>) => Object.entries(m).map(([name, count]) => ({ name: label(name), count })).sort((a, b) => b.count - a.count)

export default function Analytics() {
  const [preset, setPreset] = useState('30')
  const [custom, setCustom] = useState<{ from: string; to: string }>()
  const [currency, setCurrency] = useState('KES')

  const { data, error, loading } = useLoad(async () => {
    const dates = await api<SystemDates>('/core/dates')
    const to = preset === 'custom' && custom ? custom.to : dates.today
    const from = preset === 'custom' && custom ? custom.from : minusDays(dates.today, Number(preset) - 1)
    return api<Analytics>(`/ops/analytics?from=${from}&to=${to}&currency=${currency}`)
  }, [preset, custom?.from, custom?.to, currency])

  const controls = (
    <div className="flex flex-wrap items-end gap-2">
      <Select value={preset} onValueChange={(v) => {
        if (v === 'custom' && !custom && data) setCustom({ from: data.from, to: data.to })
        setPreset(v)
      }}>
        <SelectTrigger size="sm" className="w-40" aria-label="Period"><SelectValue /></SelectTrigger>
        <SelectContent>{presets.map((p) => <SelectItem key={p.value} value={p.value}>{p.label}</SelectItem>)}</SelectContent>
      </Select>
      {preset === 'custom' && custom && (
        <>
          <Input type="date" className="h-8 w-38" aria-label="From" value={custom.from} max={custom.to} onChange={(e) => e.target.value && setCustom({ ...custom, from: e.target.value })} />
          <Input type="date" className="h-8 w-38" aria-label="To" value={custom.to} min={custom.from} onChange={(e) => e.target.value && setCustom({ ...custom, to: e.target.value })} />
        </>
      )}
      <Select value={currency} onValueChange={setCurrency}>
        <SelectTrigger size="sm" className="w-24" aria-label="Currency"><SelectValue /></SelectTrigger>
        <SelectContent>{currencies.map((c) => <SelectItem key={c} value={c}>{c}</SelectItem>)}</SelectContent>
      </Select>
    </div>
  )

  const header = (description?: ReactNode) => (
    <PageHeader module="INSIGHTS" title="Business intelligence" description={description}>{controls}</PageHeader>
  )

  if (error) return <>{header()}<ErrorNote error={error} /></>
  if (!data) {
    return (
      <>
        {header()}
        <div className="stat-grid">{Array.from({ length: 4 }, (_, i) => <Panel key={i}><Loading /></Panel>)}</div>
      </>
    )
  }

  const { kpis, previous, currency: ccy } = data
  const hourly = data.granularity === 'hour'
  const bucketLabel = (b: string) =>
    hourly
      ? new Date(b).toLocaleTimeString('en-KE', { hour: '2-digit', minute: '2-digit' })
      : new Date(`${b}T00:00:00`).toLocaleDateString('en-KE', { day: 'numeric', month: 'short' })
  const series = data.series.map((b) => ({ ...b, label: bucketLabel(b.bucket), major: major(b.value, ccy) }))
  const newCustomers = data.customers.new.map((b) => ({ ...b, label: bucketLabel(b.bucket) }))
  const money = (minor: number) => `${ccy} ${compact.format(major(minor, ccy))}`
  const range = data.from === data.to ? day(data.from) : `${day(data.from)} – ${day(data.to)}`
  const providers = data.byProvider.map((p) => ({ name: p.key, rate: p.completed + p.failed ? (p.completed / (p.completed + p.failed)) * 100 : 0, count: p.count }))
  const otherCurrencies = data.currencies.filter((c) => c !== ccy)
  const volumeConfig = { major: { label: `Value (${ccy})`, color: 'var(--chart-1)' } } satisfies ChartConfig
  const newConfig = { count: { label: 'New customers', color: 'var(--chart-1)' } } satisfies ChartConfig

  return (
    <>
      {header(<>{range} · {hourly ? 'by hour of input' : 'by booking date'} · values in {ccy}{loading ? ' · updating…' : ''} · as of {dateTime(data.asOf)}</>)}

      <div className="stat-grid xl:grid-cols-4 2xl:grid-cols-4">
        <Stat label="Transactions" icon={<ArrowLeftRight />} value={number.format(kpis.transactions)} hint={<Delta now={kpis.transactions} before={previous.transactions} />} />
        <Stat label={`Value moved (${ccy})`} icon={<Coins />} value={money(kpis.value)} hint={<Delta now={kpis.value} before={previous.value} />} />
        <Stat label="Success rate" icon={<BadgeCheck />} value={kpis.completed + kpis.failed ? percent(kpis.successRate) : '—'}
          hint={<Delta now={kpis.successRate} before={previous.successRate} rate />} />
        <Stat label="Average ticket" icon={<Receipt />} value={kpis.averageTicket ? money(kpis.averageTicket) : '—'} hint={<Delta now={kpis.averageTicket} before={previous.averageTicket} />} />
        <Stat label={`Fee revenue (${ccy})`} icon={<Coins />} value={money(kpis.fees)} hint={<Delta now={kpis.fees} before={previous.fees} />} />
        <Stat label="Active customers" icon={<Users />} value={number.format(kpis.activeCustomers)} hint={<Delta now={kpis.activeCustomers} before={previous.activeCustomers} />} />
        <Stat label="New customers" icon={<UserPlus />} value={number.format(kpis.newCustomers)}
          hint={<>{number.format(kpis.newAccounts)} new accounts · <Delta now={kpis.newCustomers} before={previous.newCustomers} /></>} />
        <Stat label="Top merchant" icon={<Store />} value={data.topMerchants[0] ? money(data.topMerchants[0].value) : '—'}
          hint={data.topMerchants[0]?.name ?? 'No card or M-Pesa collections yet'} />
      </div>

      <Panel title="Volume" aside={<CsvButton name={`volume-${data.from}-${data.to}`} rows={data.series.map((b) => ({
        [hourly ? 'hour' : 'booking_date']: b.bucket, completed: b.completed, in_progress: b.other, failed: b.failed, [`value_${ccy}`]: major(b.value, ccy), [`fees_${ccy}`]: major(b.fees, ccy),
      }))} />}>
        {kpis.transactions === 0 ? <Empty>No transactions in this period.</Empty> : (
          <Tabs defaultValue="count">
            <TabsList>
              <TabsTrigger value="count">Transactions</TabsTrigger>
              <TabsTrigger value="value">Value</TabsTrigger>
              <TabsTrigger value="table">Table</TabsTrigger>
            </TabsList>
            <TabsContent value="count">
              <ChartContainer config={outcomeConfig} className="aspect-auto h-72 w-full">
                <BarChart data={series} margin={{ left: 0, right: 8, top: 8 }} barCategoryGap={3}>
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
            </TabsContent>
            <TabsContent value="value">
              <ChartContainer config={volumeConfig} className="aspect-auto h-72 w-full">
                <AreaChart data={series} margin={{ left: 0, right: 8, top: 8 }}>
                  <CartesianGrid vertical={false} strokeOpacity={0.5} />
                  <XAxis dataKey="label" tickLine={false} axisLine={false} tickMargin={8} minTickGap={24} />
                  <YAxis tickLine={false} axisLine={false} width={52} tickFormatter={(v: number) => compact.format(v)} />
                  <ChartTooltip cursor={{ strokeOpacity: 0.5 }} content={<ChartTooltipContent formatter={(v) => `${ccy} ${number.format(Number(v))}`} />} />
                  <Area dataKey="major" type="monotone" stroke="var(--color-major)" strokeWidth={2} fill="var(--color-major)" fillOpacity={0.15}
                    dot={series.length <= 2 ? { r: 4 } : false} activeDot={{ r: 5, stroke: 'var(--card)', strokeWidth: 2 }} />
                </AreaChart>
              </ChartContainer>
              {otherCurrencies.length > 0 && <p className="text-xs text-muted-foreground">Also active in {otherCurrencies.join(', ')}: pick a currency above to value them.</p>}
            </TabsContent>
            <TabsContent value="table">
              <div className="max-h-80 overflow-auto">
                <table>
                  <thead><tr><th>{hourly ? 'Hour' : 'Booking date'}</th><th className="num">Completed</th><th className="num">In progress</th><th className="num">Failed</th><th className="num">Value</th><th className="num">Fees</th></tr></thead>
                  <tbody>
                    {series.map((b) => (
                      <tr key={b.bucket}>
                        <td>{hourly ? b.label : day(b.bucket)}</td>
                        <td className="num">{number.format(b.completed)}</td>
                        <td className="num">{number.format(b.other)}</td>
                        <td className="num">{number.format(b.failed)}</td>
                        <td className="num">{amount(b.value, ccy)}</td>
                        <td className="num">{amount(b.fees, ccy)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </TabsContent>
          </Tabs>
        )}
      </Panel>

      <div className="grid-2">
        <Panel title="By flow" aside={<CsvButton name="by-flow" rows={breakdownRows(data.byFlow, ccy)} />}>
          {data.byFlow.length ? <OutcomeBars rows={data.byFlow} /> : <Empty>Nothing in this period.</Empty>}
        </Panel>
        <Panel title="Provider success rate" aside={<CsvButton name="by-provider" rows={breakdownRows(data.byProvider, ccy)} />}>
          {providers.length ? (
            <>
              <Bars rows={providers} dataKey="rate" name="Completed of finished" format={(v) => `${v.toFixed(1)}%`} />
              <p className="mt-2 text-xs text-muted-foreground">Completed out of completed and failed, per provider: {providers.map((p) => `${p.name} ${number.format(p.count)}`).join(' · ')} transactions.</p>
            </>
          ) : <Empty>No provider traffic in this period.</Empty>}
        </Panel>
      </div>

      <div className="grid-2">
        <Panel title="Top decline reasons" aside={<CsvButton name="declines" rows={data.declines.map((d) => ({ reason: d.key, transactions: d.count }))} />}>
          {data.declines.length
            ? <Bars rows={data.declines.map((d) => ({ name: label(d.key), count: d.count }))} dataKey="count" name="Declined" format={(v) => number.format(v)} color="var(--chart-failed)" width={170} />
            : <Empty>Nothing declined in this period.</Empty>}
        </Panel>
        <Panel title="By customer segment" aside={<CsvButton name="by-segment" rows={breakdownRows(data.bySegment, ccy)} />}>
          {data.bySegment.length ? (
            <>
              <OutcomeBars rows={data.bySegment} width={90} />
              <table className="mt-2">
                <thead><tr><th>Segment</th><th className="num">Transactions</th><th className="num">Value ({ccy})</th></tr></thead>
                <tbody>
                  {data.bySegment.map((s) => <tr key={s.key}><td>{label(s.key)}</td><td className="num">{number.format(s.count)}</td><td className="num">{amount(s.value, ccy)}</td></tr>)}
                </tbody>
              </table>
            </>
          ) : <Empty>Nothing in this period.</Empty>}
        </Panel>
      </div>

      <div className="grid-2">
        <Panel title="Top merchants by collections" aside={<CsvButton name="top-merchants" rows={data.topMerchants.map((m, i) => ({ rank: i + 1, merchant: m.name, collections: m.count, [`value_${ccy}`]: major(m.value, ccy) }))} />}>
          {data.topMerchants.length ? (
            <table>
              <thead><tr><th>#</th><th>Merchant</th><th className="num">Collections</th><th className="num">Value ({ccy})</th><th className="w-1/4">Share of top 10</th></tr></thead>
              <tbody>
                {data.topMerchants.map((m, i) => {
                  const share = m.value / data.topMerchants.reduce((s, x) => s + x.value, 0)
                  return (
                    <tr key={m.customerId}>
                      <td className="text-muted-foreground tabular-nums">{i + 1}</td>
                      <td className="font-medium">{m.name}</td>
                      <td className="num">{number.format(m.count)}</td>
                      <td className="num">{amount(m.value, ccy)}</td>
                      <td>
                        <div className="flex items-center gap-2">
                          <div className="h-2 flex-1 rounded-full bg-muted"><div className="h-2 rounded-full bg-chart-1" style={{ width: `${(100 * share).toFixed(1)}%` }} /></div>
                          <span className="w-12 text-right text-xs tabular-nums text-muted-foreground">{percent(share)}</span>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          ) : <Empty>No completed collections in {ccy} in this period.</Empty>}
        </Panel>
        <Panel title="By channel and transaction type" aside={<CsvButton name="by-channel-and-type" rows={[...breakdownRows(data.byChannel, ccy).map((r) => ({ dimension: 'channel', ...r })), ...breakdownRows(data.byType, ccy).map((r) => ({ dimension: 'type', ...r }))]} />}>
          <table>
            <thead><tr><th>Channel</th><th className="num">Transactions</th><th className="num">Success</th><th className="num">Value ({ccy})</th></tr></thead>
            <tbody>
              {data.byChannel.map((c) => (
                <tr key={c.key}><td>{label(c.key)}</td><td className="num">{number.format(c.count)}</td>
                  <td className="num">{c.completed + c.failed ? percent(c.completed / (c.completed + c.failed)) : '—'}</td><td className="num">{amount(c.value, ccy)}</td></tr>
              ))}
            </tbody>
          </table>
          <table className="mt-4">
            <thead><tr><th>Transaction type</th><th className="num">Transactions</th><th className="num">Success</th><th className="num">Value ({ccy})</th></tr></thead>
            <tbody>
              {data.byType.map((c) => (
                <tr key={c.key}><td className="mono">{c.key}</td><td className="num">{number.format(c.count)}</td>
                  <td className="num">{c.completed + c.failed ? percent(c.completed / (c.completed + c.failed)) : '—'}</td><td className="num">{amount(c.value, ccy)}</td></tr>
              ))}
            </tbody>
          </table>
          {!data.byChannel.length && <Empty>Nothing in this period.</Empty>}
        </Panel>
      </div>

      <div className="grid-2">
        <Panel title="New customers" aside={<span className="muted">{number.format(data.customers.total)} on record</span>}>
          {kpis.newCustomers === 0 ? <Empty>No new customers in this period.</Empty> : (
            <ChartContainer config={newConfig} className="aspect-auto h-56 w-full">
              <BarChart data={newCustomers} margin={{ left: 0, right: 8, top: 8 }} barCategoryGap={3}>
                <CartesianGrid vertical={false} strokeOpacity={0.5} />
                <XAxis dataKey="label" tickLine={false} axisLine={false} tickMargin={8} minTickGap={24} />
                <YAxis tickLine={false} axisLine={false} width={44} tickFormatter={(v: number) => compact.format(v)} />
                <ChartTooltip cursor={{ fillOpacity: 0.4 }} content={<ChartTooltipContent />} />
                <Bar dataKey="count" fill="var(--color-count)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ChartContainer>
          )}
        </Panel>
        <Panel title="Customer and account mix" aside={<CsvButton name="customer-mix" rows={[
          ...mapRows(data.customers.byType).map((r) => ({ dimension: 'type', ...r })),
          ...mapRows(data.customers.byKyc).map((r) => ({ dimension: 'kyc', ...r })),
          ...mapRows(data.customers.byTier).map((r) => ({ dimension: 'tier', ...r })),
          ...mapRows(data.accounts.byCurrency).map((r) => ({ dimension: 'open_accounts_by_currency', ...r })),
        ]} />}>
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <p className="mono-label mb-1">Customers by type</p>
              <Bars rows={mapRows(data.customers.byType)} dataKey="count" name="Customers" format={(v) => number.format(v)} width={84} />
            </div>
            <div>
              <p className="mono-label mb-1">By KYC tier</p>
              <Bars rows={mapRows(data.customers.byTier)} dataKey="count" name="Customers" format={(v) => number.format(v)} width={84} />
            </div>
            <div>
              <p className="mono-label mb-1">Open accounts by currency</p>
              <Bars rows={mapRows(data.accounts.byCurrency)} dataKey="count" name="Open accounts" format={(v) => number.format(v)} width={48} />
            </div>
            <div className="space-y-3 text-sm">
              <p className="mono-label">Relationships</p>
              <p><span className="text-2xl font-semibold tabular-nums">{number.format(data.accounts.multiCurrency)}</span><br /><span className="text-muted-foreground">customers hold accounts in more than one currency</span></p>
              <p><span className="text-2xl font-semibold tabular-nums">{number.format(data.accounts.sub)}</span><br /><span className="text-muted-foreground">family and staff sub-accounts</span></p>
            </div>
          </div>
        </Panel>
      </div>
    </>
  )
}
