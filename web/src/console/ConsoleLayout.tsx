import { useEffect, useState, type ComponentType } from 'react'
import { Link, Navigate, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useTheme } from 'next-themes'
import {
  ArrowLeftRight, BookOpen, Building2, CalendarClock, ChevronsUpDown, CircleUserRound, Cog, ExternalLink, FileSearch, KeyRound, Landmark,
  ChartNoAxesCombined, LayoutDashboard, ListChecks, LogOut, Monitor, Moon, Plug, Receipt, Scale, Search, Send, ShieldCheck, Sun, Table2, Users, Wallet, Waypoints, WifiOff,
} from 'lucide-react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Badge as ShadBadge } from '@/components/ui/badge'
import { Breadcrumb, BreadcrumbItem, BreadcrumbList, BreadcrumbPage, BreadcrumbSeparator } from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandSeparator } from '@/components/ui/command'
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator,
  DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Separator } from '@/components/ui/separator'
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupLabel, SidebarHeader, SidebarInset, SidebarMenu, SidebarMenuButton,
  SidebarMenuItem, SidebarProvider, SidebarRail, SidebarTrigger,
} from '@/components/ui/sidebar'
import { api, setSession, SIGNED_OUT, type Session } from './api'
import { day } from './format'
import { ChangePassword } from './Login'
import { SessionContext } from './session'
import type { SystemDates } from './types'
import { Badge, Panel } from './ui'
import './console.css'

const docsUrl = (import.meta.env.VITE_API_DOCS as string | undefined) ?? 'http://localhost:5080/docs'

/** admin: needs the admin permission. analyst: needs the analyst permission, and is all an analyst-only operator sees. */
type NavItem = { to: string; label: string; icon: ComponentType<{ className?: string }>; end?: boolean; admin?: boolean; analyst?: boolean }

const sections: { title: string; items: NavItem[] }[] = [
  {
    title: 'Overview',
    items: [
      { to: '/console', label: 'Dashboard', icon: LayoutDashboard, end: true },
      { to: '/console/analytics', label: 'Business intelligence', icon: ChartNoAxesCombined, analyst: true },
    ],
  },
  {
    title: 'Customers',
    items: [
      { to: '/console/customers', label: 'Customers', icon: Users },
      { to: '/console/accounts', label: 'Accounts', icon: Wallet },
    ],
  },
  {
    title: 'Transactions',
    items: [
      { to: '/console/transact', label: 'Input a transaction', icon: Send },
      { to: '/console/transactions', label: 'Transactions', icon: ArrowLeftRight },
      { to: '/console/enquiries', label: 'Enquiries', icon: FileSearch },
      { to: '/console/payouts', label: 'Payouts', icon: Receipt },
    ],
  },
  {
    title: 'Ledger & controls',
    items: [
      { to: '/console/ledger', label: 'General ledger', icon: BookOpen },
      { to: '/console/controls', label: 'Maker-checker', icon: ShieldCheck },
      { to: '/console/cob', label: 'Close of business', icon: CalendarClock },
      { to: '/console/reconciliation', label: 'Reconciliation', icon: Scale },
      { to: '/console/suspense', label: 'Suspense', icon: ListChecks },
      { to: '/console/nostro', label: 'Nostro', icon: Landmark },
      { to: '/console/vostro', label: 'Vostro', icon: Building2 },
    ],
  },
  {
    title: 'Setup',
    items: [
      { to: '/console/setup/transactions', label: 'Transaction setup', icon: Waypoints },
      { to: '/console/setup/local-tables', label: 'Local tables', icon: Table2 },
      { to: '/console/channels', label: 'Provider channels', icon: Plug },
      { to: '/console/extensions', label: 'Extensions', icon: Cog },
      { to: '/console/operations', label: 'Operations', icon: Monitor },
      { to: '/console/operators', label: 'Operators', icon: KeyRound, admin: true },
    ],
  },
]

/** The page and its section, for the breadcrumb. */
function whereAmI(pathname: string) {
  let best: { section: string; item: NavItem } | undefined
  for (const s of sections) {
    for (const item of s.items) {
      const match = item.end ? pathname === item.to : pathname === item.to || pathname.startsWith(item.to + '/')
      if (match && (!best || item.to.length > best.item.to.length)) best = { section: s.title, item }
    }
  }
  return best
}

export default function ConsoleLayout() {
  const navigate = useNavigate()
  const location = useLocation()
  const [session, setCurrent] = useState<Session>()
  const [changing, setChanging] = useState(false)
  const [dates, setDates] = useState<SystemDates>()
  const [offline, setOffline] = useState(false)
  const [palette, setPalette] = useState(false)

  // Who is signed in; anyone else goes to the sign-in page (and comes back here after).
  useEffect(() => {
    const signIn = () => navigate('/console/login', { replace: true, state: { from: location.pathname } })
    api<Session>('/auth/session')
      .then((s) => { setSession(s); setCurrent(s) })
      .catch(signIn)
    window.addEventListener(SIGNED_OUT, signIn)
    return () => window.removeEventListener(SIGNED_OUT, signIn)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    if (!session) return
    let alive = true
    const refresh = () =>
      api<SystemDates>('/core/dates')
        .then((d) => alive && (setDates(d), setOffline(false)))
        .catch(() => alive && setOffline(true))
    refresh()
    const timer = setInterval(refresh, 10_000)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [session])

  // ⌘K / Ctrl-K opens the command palette.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() === 'k' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        setPalette((open) => !open)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const signedIn = (s: Session) => {
    setSession(s)
    setCurrent(s)
    setChanging(false)
  }
  const signOut = async () => {
    await api('/auth/logout', { method: 'POST' }).catch(() => undefined)
    setSession(undefined)
    navigate('/console/login', { replace: true })
  }

  if (!session) {
    return <div className="flex min-h-svh items-center justify-center text-sm text-muted-foreground">Checking your session…</div>
  }

  // An operator whose only permission is analyst (BI) sees analytics and nothing else; the API
  // refuses them everything else too.
  const analystOnly = session.permissions.length > 0 && session.permissions.every((p) => p === 'analyst')
  const visible = sections
    .map((s) => ({
      ...s,
      items: s.items.filter((i) =>
        analystOnly ? i.analyst : (!i.admin || session.permissions.includes('admin')) && (!i.analyst || session.permissions.includes('analyst'))),
    }))
    .filter((s) => s.items.length)
  const here = whereAmI(location.pathname)
  if (analystOnly && !location.pathname.startsWith('/console/analytics')) return <Navigate to="/console/analytics" replace />

  return (
    <SessionContext.Provider value={session}>
      <SidebarProvider>
        <Sidebar collapsible="icon">
          <SidebarHeader>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton size="lg" asChild>
                  <Link to={analystOnly ? '/console/analytics' : '/console'}>
                    <div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-sidebar-primary font-semibold text-sidebar-primary-foreground">Z</div>
                    <div className="grid flex-1 text-left leading-tight">
                      <span className="truncate font-semibold">Zirnis</span>
                      <span className="truncate text-xs text-sidebar-foreground/60">{analystOnly ? 'Insights' : 'Operations'}</span>
                    </div>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarHeader>
          <SidebarContent>
            {visible.map((section) => (
              <SidebarGroup key={section.title}>
                <SidebarGroupLabel>{section.title}</SidebarGroupLabel>
                <SidebarMenu>
                  {section.items.map((item) => (
                    <SidebarMenuItem key={item.to}>
                      <SidebarMenuButton asChild tooltip={item.label} isActive={here?.item.to === item.to}>
                        <NavLink to={item.to} end={item.end}>
                          <item.icon />
                          <span>{item.label}</span>
                        </NavLink>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  ))}
                </SidebarMenu>
              </SidebarGroup>
            ))}
          </SidebarContent>
          <SidebarFooter>
            <UserMenu session={session} onPassword={() => setChanging(true)} onSignOut={() => void signOut()} />
          </SidebarFooter>
          <SidebarRail />
        </Sidebar>

        <SidebarInset>
          <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-2 border-b bg-background/85 px-4 backdrop-blur">
            <SidebarTrigger className="-ml-1" />
            <Separator orientation="vertical" className="mr-1 data-[orientation=vertical]:h-4" />
            <Breadcrumb className="hidden md:block">
              <BreadcrumbList>
                {here && here.section !== 'Overview' && (
                  <>
                    <BreadcrumbItem>{here.section}</BreadcrumbItem>
                    <BreadcrumbSeparator />
                  </>
                )}
                <BreadcrumbItem>
                  <BreadcrumbPage>{here?.item.label ?? 'Console'}</BreadcrumbPage>
                </BreadcrumbItem>
              </BreadcrumbList>
            </Breadcrumb>
            <div className="ml-auto flex items-center gap-2">
              <Button variant="outline" size="sm" className="hidden w-56 justify-start text-muted-foreground sm:flex" onClick={() => setPalette(true)}>
                <Search />
                Search…
                <kbd className="ml-auto rounded border bg-muted px-1.5 font-mono text-[10px]">⌘K</kbd>
              </Button>
              <Button variant="ghost" size="icon" className="sm:hidden" onClick={() => setPalette(true)} aria-label="Search">
                <Search />
              </Button>
              {session.sandbox && <ShadBadge variant="outline" className="border-warning/50 bg-warning/15">Sandbox</ShadBadge>}
              <div className="hidden items-center gap-2 rounded-md border px-2.5 py-1 text-sm lg:flex" title="Business date (T24 DATES)">
                <CalendarClock className="size-4 text-muted-foreground" />
                <span className="font-medium tabular-nums">{dates ? day(dates.today) : '—'}</span>
                {dates && <Badge value={dates.status} />}
              </div>
            </div>
          </header>

          <main className="mx-auto w-full max-w-[1600px] flex-1 p-4 md:p-6">
            {offline && (
              <Alert variant="destructive" className="mb-4">
                <WifiOff />
                <AlertDescription>The API is unreachable. Start it with <code>ZIRNIS_ENV=Development go run ./cmd/zirnis</code>.</AlertDescription>
              </Alert>
            )}
            {dates?.status === 'CloseOfBusiness' && (
              <Alert className="mb-4">
                <CalendarClock />
                <AlertDescription>Close of business is running: online postings book to {day(dates.nextWorkingDay)}.</AlertDescription>
              </Alert>
            )}
            {session.mustChangePassword || changing ? (
              <Panel title="Your password">
                <ChangePassword forced={session.mustChangePassword} onDone={signedIn} />
              </Panel>
            ) : (
              <Outlet />
            )}
          </main>
        </SidebarInset>
        <Palette open={palette} onOpenChange={setPalette} sections={visible} />
      </SidebarProvider>
    </SessionContext.Provider>
  )
}

function initials(name: string) {
  return name.split(/\s+/).filter(Boolean).slice(0, 2).map((p) => p[0]?.toUpperCase()).join('')
}

function UserMenu({ session, onPassword, onSignOut }: { session: Session; onPassword: () => void; onSignOut: () => void }) {
  const { theme, setTheme } = useTheme()
  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <SidebarMenuButton size="lg" className="data-[state=open]:bg-sidebar-accent">
              <Avatar className="size-8 rounded-lg">
                <AvatarFallback className="rounded-lg bg-sidebar-accent text-xs">{initials(session.name) || <CircleUserRound />}</AvatarFallback>
              </Avatar>
              <div className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-medium">{session.name}</span>
                <span className="truncate text-xs text-sidebar-foreground/60">{session.permissions.join(' · ') || 'enquiry only'}</span>
              </div>
              <ChevronsUpDown className="ml-auto size-4" />
            </SidebarMenuButton>
          </DropdownMenuTrigger>
          <DropdownMenuContent side="top" align="start" className="w-60">
            <DropdownMenuLabel className="font-normal">
              <div className="font-medium">{session.name}</div>
              <div className="text-xs text-muted-foreground">{session.username}</div>
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuSub>
              <DropdownMenuSubTrigger>
                {theme === 'dark' ? <Moon /> : theme === 'light' ? <Sun /> : <Monitor />}
                Theme
              </DropdownMenuSubTrigger>
              <DropdownMenuSubContent>
                <DropdownMenuRadioGroup value={theme ?? 'system'} onValueChange={setTheme}>
                  <DropdownMenuRadioItem value="light">Light</DropdownMenuRadioItem>
                  <DropdownMenuRadioItem value="dark">Dark</DropdownMenuRadioItem>
                  <DropdownMenuRadioItem value="system">System</DropdownMenuRadioItem>
                </DropdownMenuRadioGroup>
              </DropdownMenuSubContent>
            </DropdownMenuSub>
            <DropdownMenuItem onSelect={onPassword}>
              <KeyRound />
              Change password
            </DropdownMenuItem>
            <DropdownMenuItem asChild>
              <a href={docsUrl} target="_blank" rel="noreferrer">
                <ExternalLink />
                API reference
              </a>
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={onSignOut}>
              <LogOut />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}

/** ⌘K: go to any page, or open a transaction or account by its reference. */
function Palette({ open, onOpenChange, sections }: { open: boolean; onOpenChange: (open: boolean) => void; sections: { title: string; items: NavItem[] }[] }) {
  const navigate = useNavigate()
  const [query, setQuery] = useState('')
  const go = (to: string) => {
    onOpenChange(false)
    setQuery('')
    navigate(to)
  }
  const q = query.trim().toUpperCase()
  const reference = /^FT[0-9A-Z]{6,}$/.test(q)
  const accountNumber = /^\d{8,12}$/.test(q)
  return (
    <CommandDialog open={open} onOpenChange={onOpenChange} title="Go to" description="Find a page, a transaction (FT…) or an account number">
      <CommandInput placeholder="A page, FT26… or an account number" value={query} onValueChange={setQuery} />
      <CommandList>
        <CommandEmpty>Nothing matches.</CommandEmpty>
        {(reference || accountNumber) && (
          <>
            <CommandGroup heading="Open">
              {reference && (
                <CommandItem value={`open ${q}`} onSelect={() => go(`/console/transactions/${q}`)}>
                  <ArrowLeftRight />
                  Transaction {q}
                </CommandItem>
              )}
              {accountNumber && (
                <CommandItem value={`open ${q}`} onSelect={() => go(`/console/accounts/${q}`)}>
                  <Wallet />
                  Account {q}
                </CommandItem>
              )}
            </CommandGroup>
            <CommandSeparator />
          </>
        )}
        {sections.map((s) => (
          <CommandGroup key={s.title} heading={s.title}>
            {s.items.map((item) => (
              <CommandItem key={item.to} value={`${s.title} ${item.label}`} onSelect={() => go(item.to)}>
                <item.icon />
                {item.label}
              </CommandItem>
            ))}
          </CommandGroup>
        ))}
      </CommandList>
    </CommandDialog>
  )
}
