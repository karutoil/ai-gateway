import { useEffect, useState } from 'react'
import { Routes, Route, Link, NavLink, useLocation, useNavigate, Navigate } from 'react-router-dom'
import Dashboard from './pages/Dashboard'
import Providers from './pages/Providers'
import Routing from './pages/Routing'
import Keys from './pages/Keys'
import Playground from './pages/Playground'
import Logs from './pages/Logs'
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
import { setCurrentPermissions, can, type Perm } from './lib/permissions'
import {
  Icon, Button, Input, Card, ErrorNote, SegmentedControl, Badge, Avatar,
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
        setUser({ username: me.username || data.username || '', role: me.role || data.role || '' })
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
    ],
  },
  {
    title: 'Connect', caption: 'Upstream supply',
    items: [
      { to: '/providers', label: 'Providers', icon: 'server', hint: 'Endpoints & health', perm: 'providers:read' },
      { to: '/models', label: 'Models', icon: 'box', hint: 'Catalog & pricing', perm: 'catalog:read' },
      { to: '/routing', label: 'Routing', icon: 'route', hint: 'Failover & balancing', perm: 'routing:read' },
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
      { to: '/settings', label: 'Settings', icon: 'cog', hint: 'Catalog & pricing' },
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
        className={`group relative flex items-center justify-center h-11 w-11 mx-auto rounded-lg transition-colors duration-150 ${
          active ? 'bg-accent text-onaccent' : 'text-muted hover:text-paper hover:bg-raised'}`}>
        <Icon name={item.icon} size={18} />
      </NavLink>
    )
  }
  return (
    <NavLink to={item.to} onClick={onNavigate}
      className={`group flex items-center gap-3 px-2.5 py-2 rounded-lg text-sm transition-colors duration-150 border ${
        active ? 'bg-raised border-stone text-paper' : 'border-transparent text-muted hover:text-paper hover:bg-raised/60'}`}>
      <span className={`w-8 h-8 rounded-md flex items-center justify-center shrink-0 transition-colors ${active ? 'bg-accent text-onaccent' : 'bg-raised text-muted group-hover:text-paper border border-stone'}`}>
        <Icon name={item.icon} size={16} />
      </span>
      <span className="min-w-0 flex-1">
        <span className={`block leading-none ${active ? 'font-semibold' : 'font-medium'}`}>{item.label}</span>
        <span className="block text-[11px] text-muted/80 mt-1 leading-none truncate">{item.hint}</span>
      </span>
      {active && <span className="w-1.5 h-1.5 rounded-full bg-accent shrink-0" />}
    </NavLink>
  )
}

function SidebarBody({ role, pathname, collapsed, onNavigate }: {
  role: string; pathname: string; collapsed: boolean; onNavigate?: () => void
}) {
  return (
    <nav className="flex-1 overflow-y-auto px-3 py-4 space-y-6">
      {visibleGroups(role).map((g) => (
        <div key={g.title}>
          {!collapsed && (
            <div className="px-2 mb-2 flex items-baseline justify-between">
              <span className="text-[10px] font-bold uppercase tracking-[0.18em] text-muted/70">{g.title}</span>
              <span className="text-[10px] text-muted/50">{g.caption}</span>
            </div>
          )}
          <div className={collapsed ? 'space-y-1.5' : 'space-y-1'}>
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
      <div className="w-9 h-9 rounded-lg bg-accent flex items-center justify-center shrink-0">
        <Icon name="zap" size={17} className="text-onaccent" />
      </div>
      {!collapsed && (
        <div className="leading-tight min-w-0">
          <div className="font-display font-semibold text-[17px]">AI Gateway</div>
          <div className="text-[9px] font-semibold uppercase tracking-[0.22em] text-muted">Control plane</div>
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

  useEffect(() => {
    try { localStorage.setItem('gw_rail', railCollapsed ? '1' : '0') } catch {}
  }, [railCollapsed])
  useEffect(() => { setSidebarOpen(false) }, [loc.pathname])

  if (checking) {
    return (
      <div className="min-h-screen grid place-items-center bg-app">
        <div className="flex flex-col items-center gap-4">
          <div className="w-12 h-12 rounded-xl bg-accent flex items-center justify-center animate-pulse-soft">
            <Icon name="zap" size={22} className="text-onaccent" />
          </div>
          <div className="font-display text-lg font-semibold">AI Gateway</div>
          <div className="text-muted text-[11px] tracking-[0.2em] uppercase">Restoring session</div>
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
      <aside className={`hidden lg:flex fixed inset-y-0 left-0 z-30 flex-col border-r border-stone bg-surface transition-all duration-200 ${collapsed ? 'w-[76px]' : 'w-[272px]'}`}>
        <div className={`h-16 flex items-center border-b border-stone shrink-0 ${collapsed ? 'justify-center px-0' : 'px-4'}`}>
          <BrandMark collapsed={collapsed} />
        </div>
        {!collapsed && (
          <div className="px-3 pt-3 shrink-0">
            <div className="relative">
              <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={14} /></span>
              <input value={navFilter} onChange={e => setNavFilter(e.target.value)} placeholder="Jump to…"
                className="w-full bg-app border border-stone rounded-lg pl-9 pr-8 h-9 text-[13px] placeholder:text-muted/50 focus:outline-none focus:border-accent/60 focus:ring-2 focus:ring-accent/15" />
              <span className="absolute right-2.5 top-1/2 -translate-y-1/2 text-[10px] font-mono text-muted/60 border border-stone rounded px-1.5 py-0.5">/</span>
            </div>
          </div>
        )}
        <div className="flex-1 min-h-0 flex flex-col">
          <FilteredSidebar role={role} pathname={loc.pathname} collapsed={collapsed} filter={navFilter} />
        </div>
        <div className="border-t border-stone p-2.5 space-y-2 shrink-0">
          {!collapsed && (
            <button onClick={() => navigate('/playground')}
              className="w-full rounded-lg bg-accent text-onaccent text-[13px] font-semibold h-9 flex items-center justify-center gap-2 hover:bg-accent/90 transition-colors">
              <Icon name="play" size={14} /> New test call
            </button>
          )}
          <button onClick={() => setRailCollapsed(c => !c)}
            className="w-full h-9 rounded-lg flex items-center justify-center gap-2 text-muted hover:text-paper hover:bg-raised transition-colors text-xs font-medium"
            aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}>
            <Icon name={collapsed ? 'chevronRight' : 'chevronLeft'} size={15} />
            {!collapsed && <span>Collapse</span>}
          </button>
        </div>
      </aside>

      {sidebarOpen && (
        <div className="lg:hidden fixed inset-0 z-40" role="dialog" aria-modal="true">
          <div className="absolute inset-0 bg-black/60 animate-fade" onClick={() => setSidebarOpen(false)} />
          <div className="absolute inset-y-0 left-0 w-[300px] bg-surface border-r border-stone shadow-pop flex flex-col animate-sidebar">
            <div className="h-16 flex items-center justify-between px-4 border-b border-stone">
              <BrandMark />
              <button onClick={() => setSidebarOpen(false)} aria-label="Close menu"
                className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised">
                <Icon name="x" size={16} />
              </button>
            </div>
            <SidebarBody role={role} pathname={loc.pathname} collapsed={false} onNavigate={() => setSidebarOpen(false)} />
            <div className="border-t border-stone p-3 space-y-1">
              <Link to="/profile" onClick={() => setSidebarOpen(false)}
                className="flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised">
                <Icon name="shield" size={16} /> Profile
              </Link>
              <button onClick={() => logout()}
                className="w-full flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised">
                <Icon name="logout" size={16} /> Log out
              </button>
            </div>
          </div>
        </div>
      )}

      <div className={`transition-[padding] duration-200 ${collapsed ? 'lg:pl-[76px]' : 'lg:pl-[272px]'}`}>
        <header className="sticky top-0 z-20 h-16 border-b border-stone bg-surface flex items-center gap-3 px-4 lg:px-6">
          <button onClick={() => setSidebarOpen(true)}
            className="lg:hidden w-9 h-9 -ml-1 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised" aria-label="Open menu">
            <Icon name="menu" size={18} />
          </button>
          <div className="min-w-0 flex items-center gap-2.5">
            <span className="w-8 h-8 rounded-md bg-raised border border-stone hidden sm:flex items-center justify-center text-accent">
              <Icon name={meta.icon} size={16} />
            </span>
            <div className="min-w-0 leading-tight">
              <div className="flex items-center gap-2 text-[13px]">
                <span className="text-muted/60">{meta.group}</span>
                <Icon name="chevronRight" size={12} className="text-muted/40" />
                <span className="font-semibold truncate">{meta.label}</span>
              </div>
              <div className="hidden md:flex items-center gap-2 mt-0.5">
                <span className="flex items-center gap-1.5 text-[11px] text-accent font-medium">
                  <span className="w-1.5 h-1.5 rounded-full bg-accent animate-pulse-soft" /> Live
                </span>
                <span className="text-[11px] text-muted/60 font-mono">{loc.pathname}</span>
              </div>
            </div>
          </div>
          <div className="ml-auto flex items-center gap-1.5">
            <button onClick={() => navigate('/logs')} title="Search requests"
              className="hidden md:flex items-center gap-2 h-9 pl-3 pr-2 rounded-lg border border-stone bg-app text-muted hover:text-paper hover:border-muted/60 text-[13px] transition-colors">
              <Icon name="search" size={14} /><span className="text-muted/70">Search requests…</span>
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

        <main key={loc.pathname} className="max-w-[1280px] mx-auto px-4 lg:px-8 py-6 lg:py-8 animate-page min-h-[calc(100vh-64px)]">
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/providers" element={<Providers role={role} />} />
            <Route path="/routing" element={<Routing role={role} />} />
            <Route path="/keys" element={<Keys role={role} />} />
            <Route path="/models" element={<Models role={role} />} />
            <Route path="/playground" element={<Playground />} />
            <Route path="/logs" element={<Logs />} />
            <Route path="/analytics" element={<Analytics />} />
            <Route path="/settings" element={<Settings role={role} />} />
            <Route path="/teams" element={<Teams role={role} />} />
            <Route path="/webhooks" element={<WebhooksPage />} />
            <Route path="/users" element={role==='admin' ? <Users /> : <Navigate to="/" replace />} />
            <Route path="/audit" element={role==='admin' ? <Audit /> : <Navigate to="/" replace />} />
            <Route path="/profile" element={<Profile onSessionRevoked={() => logout('Password changed — please sign in again')} />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
          <footer className="mt-10 pt-5 border-t border-stone flex flex-wrap items-center gap-3 text-[11px] text-muted/70">
            <span className="flex items-center gap-1.5"><span className="w-5 h-5 rounded bg-accent flex items-center justify-center"><Icon name="zap" size={11} className="text-onaccent" /></span> AI Gateway control plane</span>
            <span className="font-mono">OpenAI · Anthropic · Responses compatible</span>
            <span className="ml-auto font-mono">Go core · {role || 'session'}</span>
          </footer>
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
    <nav className="flex-1 overflow-y-auto px-3 py-4 space-y-5">
      {groups.map(g => (
        <div key={g.title}>
          <div className="px-2 mb-1.5 text-[10px] font-bold uppercase tracking-[0.18em] text-muted/70">{g.title}</div>
          <div className="space-y-1">
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
        className="flex items-center gap-2 h-10 pl-1.5 pr-2.5 rounded-lg border border-stone bg-app hover:border-muted/60 transition-colors"
        aria-haspopup="menu" aria-expanded={open}>
        <Avatar name={accountName} size={7} />
        <span className="hidden md:block text-sm font-medium max-w-[120px] truncate">{accountName || 'admin'}</span>
        <span className="hidden md:inline-flex text-[10px] font-bold uppercase tracking-wider text-accent bg-accent/10 border border-accent/25 rounded-full px-2 py-0.5">{role || 'admin'}</span>
        <Icon name="chevronDown" size={13} className={`text-muted transition-transform duration-150 ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 top-full mt-2 w-60 rounded-xl border border-stone bg-surface shadow-pop p-2 animate-modal origin-top-right">
          <div className="px-3 py-3 border-b border-stone mb-1.5 flex items-center gap-2.5">
            <Avatar name={accountName} size={9} />
            <div className="min-w-0">
              <div className="text-sm font-semibold truncate">{accountName || 'admin'}</div>
              <div className="text-[11px] text-muted capitalize">{role || 'admin'} access</div>
            </div>
          </div>
          <Link to="/profile" onClick={() => setOpen(false)} role="menuitem"
            className="flex items-center gap-2.5 px-3 py-2.5 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors">
            <Icon name="shield" size={15} /> Profile & security
          </Link>
          <Link to="/settings" onClick={() => setOpen(false)} role="menuitem"
            className="flex items-center gap-2.5 px-3 py-2.5 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors">
            <Icon name="cog" size={15} /> Workspace settings
          </Link>
          <button onClick={() => { setOpen(false); onLogout() }} role="menuitem"
            className="w-full flex items-center gap-2.5 px-3 py-2.5 rounded-lg text-sm text-muted hover:text-paper hover:bg-raised transition-colors text-left">
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
      {/* Left: ink ledger panel */}
      <div className="hidden lg:flex flex-col justify-between p-12 bg-ink text-cream">
        <div className="flex items-center gap-3">
          <div className="w-10 h-10 rounded-lg bg-accent flex items-center justify-center">
            <Icon name="zap" size={19} className="text-onaccent" />
          </div>
          <div className="leading-tight">
            <div className="font-display font-semibold text-[17px]">AI Gateway</div>
            <div className="text-[10px] font-semibold uppercase tracking-[0.22em] opacity-60">LLM control plane</div>
          </div>
        </div>
        <div className="max-w-lg">
          <div className="font-mono text-[11px] uppercase tracking-[0.2em] text-accent">Ledger · Vol. I</div>
          <h1 className="font-display text-[52px] leading-[1.02] font-semibold mt-3">
            Every model,<br />one bill,<br />no surprises.
          </h1>
          <p className="opacity-70 mt-4 leading-relaxed text-[15px]">
            Route bare model names across providers with failover, enforce
            budgets on virtual keys, and account for every token.
          </p>
          <dl className="mt-8 border-t border-cream/15">
            {[
              ['01', 'Routing', 'Round-robin, weighted, failover'],
              ['02', 'Budgets', 'Per-key caps, rotation, allowlists'],
              ['03', 'Ledger', 'TTFT, tokens and cost per call'],
            ].map(([n, t, d]) => (
              <div key={n} className="flex items-baseline gap-4 py-3.5 border-b border-cream/15">
                <span className="font-mono text-xs text-accent">{n}</span>
                <span className="font-semibold text-[15px] w-24">{t}</span>
                <span className="text-sm opacity-60">{d}</span>
              </div>
            ))}
          </dl>
        </div>
        <div className="font-mono text-[11px] opacity-50">Fast Go core · OpenAI / Anthropic / Responses</div>
      </div>

      {/* Right: sign-in form */}
      <div className="flex items-center justify-center p-6">
        <Card className="w-full max-w-md animate-page">
          <div className="mb-6">
            <div className="lg:hidden flex items-center gap-2 mb-5">
              <div className="w-9 h-9 rounded-lg bg-accent flex items-center justify-center">
                <Icon name="zap" size={16} className="text-onaccent" />
              </div>
              <div className="font-display font-semibold">AI Gateway</div>
            </div>
            <h2 className="font-display text-3xl font-semibold">Welcome back</h2>
            <p className="text-muted text-sm mt-1">Sign in to open the ledger.</p>
          </div>

          <SegmentedControl value={mode} onChange={setMode}
            options={[{ value: 'password', label: 'Password' }, { value: 'passkey', label: 'Passkey' }, { value: 'recovery', label: 'Recovery' }]} />

          {notice && (
            <div className="mt-4 flex items-start gap-2 rounded-lg border border-accent/30 bg-accent/10 px-3.5 py-3 text-sm text-accent">
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
                  onChange={e=>setRecoveryCode(e.target.value)} className="font-mono" />
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
