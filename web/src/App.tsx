import { useEffect, useRef, useState } from 'react'
import { Routes, Route, Link, NavLink, useLocation, useNavigate, Navigate } from 'react-router-dom'
import Dashboard from './pages/Dashboard'
import Providers from './pages/Providers'
import Routing from './pages/Routing'
import McpServers from './pages/McpServers'
import Keys from './pages/Keys'
import Playground from './pages/Playground'
import Logs from './pages/Logs'
import Attribution from './pages/Attribution'
import Costs from './pages/Costs'
import Performance from './pages/Performance'
import Models from './pages/Models'
import Analytics from './pages/Analytics'
import Settings from './pages/Settings'
import Teams from './pages/Teams'
import Users from './pages/Users'
import WebhooksPage from './pages/Webhooks'
import Audit from './pages/Audit'
import Profile from './pages/Profile'
import { authenticatePasskey } from './lib/webauthn'
import { extractApiError } from './lib/api'
import { requestSearchFocus } from './lib/searchFocus'
import { setCurrentPermissions, can, type Perm } from './lib/permissions'
import {
  Icon, Button, Input, Card, ErrorNote, SegmentedControl, Avatar,
  useClickOutside, useToastStore, Toaster, type IconName,
} from './components/ui'

type SessionUser = { username: string; role: string; permissions?: string[] }

function useAuth() {
  const [user, setUser] = useState<SessionUser|null>(null)
  const [checking, setChecking] = useState(true)
  const [notice, setNotice] = useState('')
  const isAuthed = !!user

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetch('/api/admin/users/me', { credentials: 'same-origin' })
        if (res.ok && !cancelled) {
          const me = await res.json()
          setUser({ username: me.username || '', role: me.role || '', permissions: me.permissions })
          setCurrentPermissions((me.permissions as Perm[] | undefined) ?? null, me.role || '')
        }
      } catch {}
      finally { if (!cancelled) setChecking(false) }
    })()
    return () => { cancelled = true }
  }, [])

  useEffect(() => {
    const onUnauthorized = () => { setUser(null); setCurrentPermissions(null, '') }
    window.addEventListener('gw:unauthorized', onUnauthorized)
    return () => window.removeEventListener('gw:unauthorized', onUnauthorized)
  }, [])

  const applyIdentity = (u: Partial<SessionUser>|undefined) => {
    setUser({ username: u?.username || '', role: u?.role || '', permissions: u?.permissions })
    setCurrentPermissions((u?.permissions as Perm[] | undefined) ?? null, u?.role || '')
  }

  const login = async (username: string, pw: string) => {
    const body:any = { password: pw }
    if (username) body.username = username
    const res = await fetch('/api/auth/login', { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify(body)})
    if (!res.ok) throw new Error(extractApiError(await res.text(), 'login failed'))
    const data = await res.json()
    try {
      const meRes = await fetch('/api/admin/users/me', { credentials: 'same-origin' })
      if (meRes.ok) {
        const me = await meRes.json()
        // Apply the full identity (permissions included): setUser alone
        // leaves the permission store empty, hiding every permission-gated
        // nav link until a hard refresh re-ran the /me fetch.
        applyIdentity({ username: me.username || data.username, role: me.role || data.role, permissions: me.permissions ?? data.permissions })
        return
      }
    } catch {}
    applyIdentity(data)
  }

  const loginWithToken = (_tok?:string, extra?:{username?:string, role?:string})=>{
    applyIdentity(extra)
    fetch('/api/admin/users/me', { credentials: 'same-origin' })
      .then(r => r.ok ? r.json() : null)
      .then(me => { if (me && me.username !== undefined) applyIdentity(me) })
      .catch(()=>{})
  }

  const logout = (message?: string) => {
    fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' }).catch(()=>{})
    setUser(null)
    setNotice(message || '')
  }
  const clearNotice = () => setNotice('')
  return { isAuthed, checking, login, loginWithToken, logout, clearNotice, notice, role: user?.role||'', username: user?.username||'' }
}

function useTheme() {
  const [theme, setTheme] = useState<string>(() => {
    try {
      const saved = localStorage.getItem('gw_theme')
      if (saved === 'light' || saved === 'dark') return saved
      return 'light'
    } catch { return 'light' }
  })
  useEffect(() => {
    document.documentElement.classList.toggle('light', theme === 'light')
    try { localStorage.setItem('gw_theme', theme) } catch {}
  }, [theme])
  return { theme, toggle: () => setTheme(t => t === 'dark' ? 'light' : 'dark') }
}

/* ------------------------------------------------------------------ */
/* Navigation model — ordered by operator flow                         */
/* ------------------------------------------------------------------ */

type NavItem = { to: string; label: string; icon: IconName; hint: string; adminOnly?: boolean; perm?: Perm; permAny?: Perm[] }
const NAV_GROUPS: { title: string; caption: string; items: NavItem[] }[] = [
  {
    title: 'Operate', caption: 'Daily traffic',
    items: [
      { to: '/', label: 'Overview', icon: 'pulse', hint: 'Health, spend, onboarding', permAny: ['logs:read', 'keys:read_own'] },
      { to: '/playground', label: 'Playground', icon: 'play', hint: 'Live test calls' },
      { to: '/logs', label: 'Requests', icon: 'logs', hint: 'Every proxied call', permAny: ['logs:read', 'keys:read_own'] },
      { to: '/performance', label: 'Performance', icon: 'gauge', hint: 'Uptime, latency, throughput', permAny: ['logs:read', 'keys:read_own'] },
    ],
  },
  {
    title: 'Connect', caption: 'Upstream supply',
    items: [
      { to: '/providers', label: 'Providers', icon: 'server', hint: 'Endpoints & health', perm: 'providers:read' },
      { to: '/models', label: 'Models', icon: 'box', hint: 'Catalog & pricing', perm: 'catalog:read' },
      { to: '/routing', label: 'Routing', icon: 'route', hint: 'Failover & balancing', perm: 'routing:read' },
      { to: '/mcp', label: 'MCP Servers', icon: 'layers', hint: 'One endpoint for every harness', perm: 'mcp:read' },
      { to: '/settings', label: 'Settings', icon: 'cog', hint: 'Catalog & pricing' },
    ],
  },
  {
    title: 'Govern', caption: 'Access & trust',
    items: [
      { to: '/keys', label: 'API Keys', icon: 'key', hint: 'Virtual credentials', perm: 'keys:read_own' },
      { to: '/teams', label: 'Teams', icon: 'users', hint: 'Orgs & members', permAny: ['orgs:read', 'orgs:write'] as Perm[] },
      { to: '/users', label: 'Users', icon: 'userCog', hint: 'Roles & passkeys', perm: 'users:read' },
      { to: '/webhooks', label: 'Webhooks', icon: 'zap', hint: 'Event delivery', perm: 'settings:write' },
      { to: '/audit', label: 'Audit', icon: 'shield', hint: 'Privileged trail', perm: 'audit:read' },
    ],
  },
  {
    title: 'Optimize', caption: 'Spend & tuning',
    items: [
      { to: '/analytics', label: 'Analytics', icon: 'chart', hint: 'Usage & cost trends', permAny: ['analytics:read', 'keys:read_own'] },
      { to: '/attribution', label: 'Attribution', icon: 'globe', hint: 'Apps & end users', permAny: ['logs:read', 'keys:read_own'] },
      { to: '/costs', label: 'Costs', icon: 'wallet', hint: 'Spend & cache savings', permAny: ['logs:read', 'keys:read_own'] },
    ],
  },
]

function visibleGroups(role: string) {
  return NAV_GROUPS
    .map((g) => ({
      ...g,
      items: g.items.filter((i) => {
        if (i.adminOnly && role !== 'admin') return false
        if (i.permAny && !i.permAny.some((p) => can(p as Perm))) return false
        if (i.perm && !can(i.perm)) return false
        return true
      }),
    }))
    .filter((g) => g.items.length > 0)
}

function pageMeta(pathname: string): { label: string; icon: IconName; group: string } {
  for (const g of NAV_GROUPS) for (const i of g.items) if (i.to === pathname) return { label: i.label, icon: i.icon, group: g.title }
  if (pathname === '/profile') return { label: 'Profile', icon: 'shield', group: 'Account' }
  return { label: 'Overview', icon: 'pulse', group: 'Operate' }
}

/* ------------------------------------------------------------------ */
/* Sidebar                                                             */
/* ------------------------------------------------------------------ */

function SidebarLink({ item, collapsed, active, onNavigate }: {
  item: NavItem; collapsed: boolean; active: boolean; onNavigate?: () => void
}) {
  if (collapsed) {
    return (
      <NavLink to={item.to} onClick={onNavigate} title={`${item.label} — ${item.hint}`}
        className={`group relative flex items-center justify-center h-9 w-9 mx-auto rounded-lg transition-colors duration-150 ${
          active ? 'bg-raised text-accent' : 'text-muted hover:text-paper hover:bg-raised/70'}`}>
        <Icon name={item.icon} size={17} />
      </NavLink>
    )
  }
  return (
    <NavLink to={item.to} onClick={onNavigate} title={item.hint}
      className={`group flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm transition-colors duration-150 ${
        active ? 'bg-raised text-paper font-medium' : 'text-muted hover:text-paper hover:bg-raised/70'}`}>
      <Icon name={item.icon} size={16} className={active ? 'text-accent' : 'text-muted group-hover:text-paper'} />
      <span className="min-w-0 flex-1 truncate">{item.label}</span>
      {active && <span className="w-1.5 h-1.5 rounded-full bg-accent shrink-0" />}
    </NavLink>
  )
}

function SidebarBody({ role, pathname, collapsed, onNavigate }: {
  role: string; pathname: string; collapsed: boolean; onNavigate?: () => void
}) {
  return (
    <nav className="flex-1 overflow-y-auto px-3 py-4 space-y-5">
      {visibleGroups(role).map((g) => (
        <div key={g.title}>
          {!collapsed && (
            <div className="px-2.5 mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-muted">{g.title}</div>
          )}
          <div className={collapsed ? 'space-y-1' : 'space-y-0.5'}>
            {g.items.map((i) => (
              <SidebarLink key={i.to} item={i} collapsed={collapsed} active={pathname === i.to} onNavigate={onNavigate} />
            ))}
          </div>
        </div>
      ))}
    </nav>
  )
}

function BrandMark({ collapsed }: { collapsed?: boolean }) {
  return (
    <div className={`flex items-center gap-2.5 ${collapsed ? 'justify-center' : ''}`}>
      <div className="w-8 h-8 rounded-lg bg-accent flex items-center justify-center shrink-0">
        <Icon name="zap" size={16} className="text-onaccent" />
      </div>
      {!collapsed && (
        <div className="leading-tight min-w-0">
          <div className="font-semibold text-[15px] -tracking-tight">AI Gateway</div>
          <div className="text-[9px] font-semibold uppercase tracking-[0.18em] text-muted">Control plane</div>
        </div>
      )}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* App                                                                 */
/* ------------------------------------------------------------------ */

export default function App() {
  const { isAuthed, checking, login, loginWithToken, logout, clearNotice, notice, role, username: accountName } = useAuth()
  const { theme, toggle } = useTheme()
  const loc = useLocation()
  const navigate = useNavigate()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [railCollapsed, setRailCollapsed] = useState(() => {
    try { return localStorage.getItem('gw_rail') === '1' } catch { return false }
  })
  const [navFilter, setNavFilter] = useState('')
  const navFilterRef = useRef<HTMLInputElement>(null)
  const searchTriggerRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    try { localStorage.setItem('gw_rail', railCollapsed ? '1' : '0') } catch {}
  }, [railCollapsed])
  useEffect(() => { setSidebarOpen(false) }, [loc.pathname])

  // ⌘K / Ctrl-K jumps to the request search. The destination page mounts after
  // this fires, so hand the focus request across rather than leaving the
  // keyboard on <body>.
  useEffect(() => {
    if (!isAuthed) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        navigate('/logs')
        requestSearchFocus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [isAuthed, navigate])

  // `/` focuses the sidebar jump box — the shortcut the "Jump to…" chip has
  // advertised since it was added (pressing it previously did nothing).
  useEffect(() => {
    if (!isAuthed) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return
      const t = e.target as HTMLElement | null
      const tag = t?.tagName?.toLowerCase()
      if (tag === 'input' || tag === 'textarea' || tag === 'select' || t?.isContentEditable) return
      if (!navFilterRef.current) return
      e.preventDefault()
      setRailCollapsed(false)
      requestAnimationFrame(() => navFilterRef.current?.focus())
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [isAuthed])

  if (checking) {
    return (
      <div className="min-h-screen grid place-items-center bg-app">
        <div className="flex flex-col items-center gap-4">
          <div className="w-11 h-11 rounded-xl bg-accent flex items-center justify-center animate-pulse-soft">
            <Icon name="zap" size={21} className="text-onaccent" />
          </div>
          <div className="font-semibold text-lg -tracking-tight">AI Gateway</div>
          <div className="text-muted text-[11px] font-mono uppercase tracking-[0.2em]">Restoring session</div>
        </div>
      </div>
    )
  }

  if (!isAuthed) {
    return (
      <>
        <LoginScreen theme={theme} toggle={toggle} login={login} loginWithToken={loginWithToken} notice={notice} onNoticeConsumed={clearNotice} />
        <Toaster />
      </>
    )
  }

  const collapsed = railCollapsed
  const meta = pageMeta(loc.pathname)

  return (
    <div className="min-h-screen bg-app">
      <aside className={`hidden lg:flex fixed inset-y-0 left-0 z-30 flex-col border-r border-stone bg-surface transition-all duration-200 ${collapsed ? 'w-[68px]' : 'w-[260px]'}`}>
        <div className={`h-14 flex items-center border-b border-stone shrink-0 ${collapsed ? 'justify-center px-0' : 'px-4'}`}>
          <BrandMark collapsed={collapsed} />
        </div>
        {!collapsed && (
          <div className="px-3 pt-3 shrink-0">
            <div className="relative">
              <span className="absolute left-2.5 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={14} /></span>
              <input ref={navFilterRef} value={navFilter} onChange={e => setNavFilter(e.target.value)} placeholder="Jump to…"
                aria-label="Jump to a page"
                onKeyDown={e => {
                  if (e.key === 'Escape') { setNavFilter(''); e.currentTarget.blur() }
                  if (e.key === 'Enter') {
                    const first = document.querySelector<HTMLElement>('nav a[href]')
                    first?.click()
                  }
                }}
                className="w-full bg-app border border-stone rounded-lg pl-8 pr-8 h-8 text-[13px] placeholder:text-muted focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/15" />
              <span className="absolute right-2 top-1/2 -translate-y-1/2 text-[10px] font-mono text-muted border border-stone rounded px-1 py-px">/</span>
            </div>
          </div>
        )}
        <div className="flex-1 min-h-0 flex flex-col">
          <FilteredSidebar role={role} pathname={loc.pathname} collapsed={collapsed} filter={navFilter} />
        </div>
        <div className="border-t border-stone p-2.5 space-y-1.5 shrink-0">
          {!collapsed && (
            <button onClick={() => navigate('/playground')}
              className="w-full rounded-lg bg-accent text-onaccent text-[13px] font-medium h-9 flex items-center justify-center gap-2 hover:opacity-90 transition-colors">
              <Icon name="play" size={14} /> New test call
            </button>
          )}
          <button onClick={() => setRailCollapsed(c => !c)}
            className="w-full h-8 rounded-lg flex items-center justify-center gap-2 text-muted hover:text-paper hover:bg-raised transition-colors text-xs font-medium"
            aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}>
            <Icon name={collapsed ? 'chevronRight' : 'chevronLeft'} size={14} />
            {!collapsed && <span>Collapse</span>}
          </button>
        </div>
      </aside>

      {sidebarOpen && (
        <div className="lg:hidden fixed inset-0 z-40" role="dialog" aria-modal="true">
          <div className="absolute inset-0 bg-black/60 animate-fade" onClick={() => setSidebarOpen(false)} />
          <div className="absolute inset-y-0 left-0 w-[280px] bg-surface border-r border-stone shadow-pop flex flex-col animate-sidebar">
            <div className="h-14 flex items-center justify-between px-4 border-b border-stone">
              <BrandMark />
              <button onClick={() => setSidebarOpen(false)} aria-label="Close menu"
                className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised">
                <Icon name="x" size={16} />
              </button>
            </div>
            <SidebarBody role={role} pathname={loc.pathname} collapsed={false} onNavigate={() => setSidebarOpen(false)} />
            <div className="border-t border-stone p-3 space-y-1">
              <Link to="/profile" onClick={() => setSidebarOpen(false)}
                className="flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised">
                <Icon name="shield" size={16} /> Profile
              </Link>
              <button onClick={() => logout()}
                className="w-full flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised">
                <Icon name="logout" size={16} /> Log out
              </button>
            </div>
          </div>
        </div>
      )}

      <div className={`transition-[padding] duration-200 ${collapsed ? 'lg:pl-[68px]' : 'lg:pl-[260px]'}`}>
        <header className="sticky top-0 z-20 h-14 border-b border-stone bg-surface flex items-center gap-3 px-4 lg:px-6">
          <button onClick={() => setSidebarOpen(true)}
            className="lg:hidden w-9 h-9 -ml-1 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised" aria-label="Open menu">
            <Icon name="menu" size={18} />
          </button>
          <div className="min-w-0 flex items-center gap-2">
            <span className="w-7 h-7 rounded-md bg-raised border border-stone hidden sm:flex items-center justify-center text-accent">
              <Icon name={meta.icon} size={14} />
            </span>
            <div className="flex items-center gap-1.5 text-[13px] min-w-0">
              <span className="text-muted truncate hidden sm:block">{meta.group}</span>
              <Icon name="chevronRight" size={12} className="text-muted/40 hidden sm:block" />
              <span className="font-semibold truncate -tracking-tight">{meta.label}</span>
            </div>
          </div>
          <div className="ml-auto flex items-center gap-1.5">
            <button ref={searchTriggerRef} onClick={() => { navigate('/logs'); requestSearchFocus() }} title="Search requests (⌘K)"
              className="hidden md:flex items-center gap-2 h-9 pl-3 pr-2 rounded-lg border border-stone bg-app text-muted hover:text-paper hover:border-muted/50 text-[13px] transition-colors">
              <Icon name="search" size={14} /><span className="text-muted">Search requests…</span>
              <span className="font-mono text-[10px] border border-stone rounded px-1.5 py-0.5">⌘K</span>
            </button>
            <button onClick={toggle}
              className="w-9 h-9 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised transition-colors"
              aria-label="Toggle theme" title={theme === 'dark' ? 'Switch to light' : 'Switch to dark'}>
              <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={16} />
            </button>
            <UserMenu accountName={accountName} role={role} onLogout={logout} />
          </div>
        </header>

        {/* The playground is a full-height chat surface: it manages its own
            width and padding, so the shared max-width/px wrapper would only
            strand it in dead space on wide screens. */}
        <main key={loc.pathname} className={`animate-page min-h-[calc(100vh-56px)] ${loc.pathname === '/playground' ? '' : 'max-w-[1380px] mx-auto px-5 lg:px-8 py-6 lg:py-8'}`}>
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/providers" element={<Providers role={role} />} />
            <Route path="/routing" element={<Routing role={role} />} />
            <Route path="/mcp" element={<McpServers />} />
            <Route path="/keys" element={<Keys role={role} />} />
            <Route path="/models" element={<Models role={role} />} />
            <Route path="/playground" element={<Playground />} />
            <Route path="/logs" element={<Logs />} />
            <Route path="/analytics" element={<Analytics />} />
            <Route path="/attribution" element={<Attribution />} />
            <Route path="/costs" element={<Costs />} />
            <Route path="/performance" element={<Performance />} />
            <Route path="/settings" element={<Settings role={role} />} />
            <Route path="/teams" element={<Teams role={role} />} />
            <Route path="/webhooks" element={<WebhooksPage />} />
            <Route path="/users" element={role==='admin' ? <Users /> : <Navigate to="/" replace />} />
            <Route path="/audit" element={role==='admin' ? <Audit /> : <Navigate to="/" replace />} />
            <Route path="/profile" element={<Profile onSessionRevoked={() => logout('Password changed — please sign in again')} />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
          {/* The playground owns the whole viewport (a fixed-height chat), so
              the shared footer would only push a scrollbar onto it. */}
          {loc.pathname !== '/playground' && (
            <footer className="mt-10 pt-4 border-t border-stone flex flex-wrap items-center gap-x-4 gap-y-2 text-[11px] text-muted">
              <span className="flex items-center gap-1.5"><span className="w-4 h-4 rounded bg-accent flex items-center justify-center"><Icon name="zap" size={10} className="text-onaccent" /></span> AI Gateway control plane</span>
              <span className="font-mono">OpenAI · Anthropic · Responses compatible</span>
              <span className="ml-auto font-mono">Go core · {role || 'session'}</span>
            </footer>
          )}
        </main>
      </div>
      <Toaster />
    </div>
  )
}

function FilteredSidebar({ role, pathname, collapsed, filter }: { role: string; pathname: string; collapsed: boolean; filter: string }) {
  const q = filter.trim().toLowerCase()
  if (!q || collapsed) return <SidebarBody role={role} pathname={pathname} collapsed={collapsed} />
  const groups = visibleGroups(role)
    .map(g => ({ ...g, items: g.items.filter(i => i.label.toLowerCase().includes(q) || i.hint.toLowerCase().includes(q)) }))
    .filter(g => g.items.length > 0)
  if (groups.length === 0) return <div className="p-4 text-sm text-muted">No matches for “{filter}”.</div>
  return (
    <nav className="flex-1 overflow-y-auto px-3 py-4 space-y-4">
      {groups.map(g => (
        <div key={g.title}>
          <div className="px-2.5 mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-muted">{g.title}</div>
          <div className="space-y-0.5">
            {g.items.map(i => <SidebarLink key={i.to} item={i} collapsed={false} active={pathname === i.to} />)}
          </div>
        </div>
      ))}
    </nav>
  )
}

/* ------------------------------------------------------------------ */
/* User menu                                                           */
/* ------------------------------------------------------------------ */

function UserMenu({ accountName, role, onLogout }: { accountName: string; role: string; onLogout: () => void }) {
  const [open, setOpen] = useState(false)
  const ref = useClickOutside(() => setOpen(false))
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])
  return (
    <div className="relative" ref={ref}>
      <button onClick={() => setOpen(o => !o)}
        className="flex items-center gap-2 h-9 pl-1 pr-2 rounded-lg border border-stone bg-app hover:bg-raised transition-colors"
        aria-haspopup="menu" aria-expanded={open}>
        <Avatar name={accountName} size={6} />
        <span className="hidden md:block text-[13px] font-medium max-w-[120px] truncate">{accountName || 'admin'}</span>
        <Icon name="chevronDown" size={13} className={`text-muted transition-transform duration-150 ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 top-full mt-2 w-56 rounded-xl border border-stone bg-surface shadow-pop p-1.5 animate-modal origin-top-right">
          <div className="px-2.5 py-2.5 mb-1 flex items-center gap-2.5 border-b border-stone">
            <Avatar name={accountName} size={8} />
            <div className="min-w-0">
              <div className="text-sm font-semibold truncate">{accountName || 'admin'}</div>
              <div className="text-[11px] text-muted capitalize">{role || 'admin'} access</div>
            </div>
          </div>
          <Link to="/profile" onClick={() => setOpen(false)} role="menuitem"
            className="flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors">
            <Icon name="shield" size={15} /> Profile & security
          </Link>
          <Link to="/settings" onClick={() => setOpen(false)} role="menuitem"
            className="flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors">
            <Icon name="cog" size={15} /> Workspace settings
          </Link>
          <button onClick={() => { setOpen(false); onLogout() }} role="menuitem"
            className="w-full flex items-center gap-2.5 px-2.5 h-9 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors text-left">
            <Icon name="logout" size={15} /> Log out
          </button>
        </div>
      )}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Login                                                               */
/* ------------------------------------------------------------------ */

function LoginScreen({ theme, toggle, login, loginWithToken, notice, onNoticeConsumed }: {
  theme: string; toggle: () => void
  login: (u: string, p: string) => Promise<void>
  loginWithToken: (tok?: string, extra?: { username?: string; role?: string }) => void
  notice?: string; onNoticeConsumed?: () => void
}) {
  const [username, setUsername] = useState('')
  const [pw, setPw] = useState('')
  const [recoveryCode, setRecoveryCode] = useState('')
  const [mode, setMode] = useState<'password'|'recovery'|'passkey'>('password')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const toastErr = useToastStore((s) => s.push)

  const doLogin = async () => {
    onNoticeConsumed?.()
    setErr(''); setBusy(true)
    try { await login(username, pw) }
    catch (e:any){ const m = e.message||String(e); setErr(m); toastErr('error','Login failed') }
    finally{ setBusy(false) }
  }

  const doPasskeyLogin = async()=>{
    setErr(''); setBusy(true)
    try{
      const r = await fetch('/api/auth/passkey/login/begin', { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify({ username })})
      if(!r.ok) throw new Error(extractApiError(await r.text(), 'passkey login failed'))
      const begin = await r.json()
      const {session, credential} = await authenticatePasskey(begin)
      const r2 = await fetch('/api/auth/passkey/login/finish', { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify({ session, credential })})
      if(!r2.ok) throw new Error(extractApiError(await r2.text(), 'passkey login failed'))
      const data = await r2.json()
      loginWithToken(data.token, {username: data.username, role: data.role})
    }catch(e:any){ const m = e.message||String(e); setErr(m); toastErr('error','Passkey login failed') }
    finally{ setBusy(false) }
  }

  const doRecovery = async()=>{
    setErr(''); setBusy(true)
    try{
      const r = await fetch('/api/auth/recovery/verify', { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify({ username, code: recoveryCode })})
      if(!r.ok) throw new Error(extractApiError(await r.text(), 'recovery failed'))
      const data = await r.json()
      loginWithToken(data.token, {username: data.username, role: data.role})
    }catch(e:any){ const m = e.message||String(e); setErr(m); toastErr('error','Recovery failed') }
    finally{ setBusy(false) }
  }

  return (
    <div className="min-h-screen grid lg:grid-cols-[1.1fr_1fr] bg-app">
      {/* Left: dark brand plate (dark in both themes) */}
      <div className="hidden lg:flex flex-col justify-between p-12 bg-ink text-cream border-r border-cream/10">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-accent flex items-center justify-center">
            <Icon name="zap" size={17} className="text-onaccent" />
          </div>
          <div className="leading-tight">
            <div className="font-semibold text-[15px] -tracking-tight">AI Gateway</div>
            <div className="text-[9px] font-semibold uppercase tracking-[0.18em] text-cream/60">LLM control plane</div>
          </div>
        </div>
        <div className="max-w-lg">
          <div className="font-mono text-[11px] uppercase tracking-[0.2em] text-indigo-300">Control plane · v2</div>
          <h1 className="text-[42px] leading-[1.06] font-semibold -tracking-[0.02em] mt-4">
            Route. Observe.<br />Govern — every<br />model, one bill.
          </h1>
          <p className="text-cream/70 mt-4 leading-relaxed text-[15px]">
            Route bare model names across providers with failover, enforce
            budgets on virtual keys, and account for every token.
          </p>
          <dl className="mt-8 border-t border-cream/15">
            {[
              ['01', 'Routing', 'Round-robin, weighted, failover'],
              ['02', 'Budgets', 'Per-key caps, rotation, allowlists'],
              ['03', 'Ledger', 'TTFT, tokens and cost per call'],
            ].map(([n, t, d]) => (
              <div key={n} className="flex items-baseline gap-4 py-3 border-b border-cream/15">
                <span className="font-mono text-xs text-indigo-300">{n}</span>
                <span className="font-semibold text-[14px] w-24">{t}</span>
                <span className="text-sm text-cream/60">{d}</span>
              </div>
            ))}
          </dl>
        </div>
        <div className="font-mono text-[11px] text-cream/50">Fast Go core · OpenAI / Anthropic / Responses</div>
      </div>

      {/* Right: sign-in form */}
      <div className="flex items-center justify-center p-6">
        <Card className="w-full max-w-md animate-page" >
          <div className="mb-6">
            <div className="lg:hidden flex items-center gap-2.5 mb-5">
              <div className="w-9 h-9 rounded-lg bg-accent flex items-center justify-center">
                <Icon name="zap" size={16} className="text-onaccent" />
              </div>
              <div className="font-semibold -tracking-tight">AI Gateway</div>
            </div>
            <h2 className="text-2xl font-semibold -tracking-tight">Welcome back</h2>
            <p className="text-muted text-sm mt-1">Sign in to open the control plane.</p>
          </div>

          <SegmentedControl value={mode} onChange={setMode}
            options={[{ value: 'password', label: 'Password' }, { value: 'passkey', label: 'Passkey' }, { value: 'recovery', label: 'Recovery' }]} />

          {notice && (
            <div className="mt-4 flex items-start gap-2 rounded-lg border border-good/30 bg-good/10 px-3.5 py-3 text-sm text-good">
              <Icon name="check" size={15} className="mt-0.5 shrink-0" /><span>{notice}</span>
            </div>
          )}

          <div className="mt-5 space-y-4">
            {mode==='password' && (
              <>
                <Input placeholder="Username" value={username} onChange={e=>setUsername(e.target.value)} autoComplete="username" />
                <Input placeholder="Password" type="password" value={pw} onChange={e=>setPw(e.target.value)}
                  autoComplete="current-password" onKeyDown={e=>{ if(e.key==='Enter') doLogin() }} />
                <ErrorNote message={err} />
                <Button variant="primary" size="lg" disabled={busy} onClick={doLogin} className="w-full">
                  {busy ? 'Signing in…' : <>Sign in <Icon name="arrowRight" size={15} /></>}
                </Button>
              </>
            )}
            {mode==='passkey' && (
              <>
                <ErrorNote message={err} />
                <Button variant="primary" size="lg" disabled={busy} onClick={doPasskeyLogin} className="w-full">
                  {busy ? 'Waiting for authenticator…' : 'Continue with passkey'}
                </Button>
                <p className="text-xs text-muted text-center">
                  Requires passkey enrollment.{' '}
                  <button onClick={()=>setMode('recovery')} className="text-accent underline underline-offset-2">Lost passkey? Use recovery code</button>
                </p>
              </>
            )}
            {mode==='recovery' && (
              <>
                <Input placeholder="Username" value={username} onChange={e=>setUsername(e.target.value)} autoComplete="username" />
                <Input placeholder="Recovery code (XXXX-XXXX-XXXX-XXXX)" value={recoveryCode}
                  onChange={e=>setRecoveryCode(e.target.value)} className="font-mono"
                  onKeyDown={e=>{ if(e.key==='Enter') doRecovery() }} />
                <ErrorNote message={err} />
                <Button variant="primary" size="lg" disabled={busy} onClick={doRecovery} className="w-full">
                  {busy ? 'Verifying…' : 'Verify recovery code'}
                </Button>
                <p className="text-xs text-muted text-center">Shown once when a passkey is enabled.</p>
              </>
            )}
          </div>

          <div className="mt-6 pt-4 border-t border-stone flex items-center justify-between">
            <span className="text-[11px] text-muted font-mono">HttpOnly session · no token in storage</span>
            <button onClick={toggle} aria-label="Toggle theme"
              className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised">
              <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={15} />
            </button>
          </div>
        </Card>
      </div>
    </div>
  )
}
