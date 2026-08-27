import { useEffect, useState } from 'react'
import { Routes, Route, Link, NavLink, useLocation, Navigate } from 'react-router-dom'
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
import Audit from './pages/Audit'
import Profile from './pages/Profile'
import { authenticatePasskey } from './lib/webauthn'
import { extractApiError } from './lib/api'
import {
  Icon, Button, Input, Card, ErrorNote, SegmentedControl,
  useClickOutside, useToastStore, Toaster, type IconName,
} from './components/ui'

type SessionUser = { username: string; role: string }

function useAuth() {
  // Identity lives in React state only. Authentication rides on the HttpOnly
  // "gw_token" session cookie — the JWT is never stored client-side.
  const [user, setUser] = useState<SessionUser|null>(null)
  const [checking, setChecking] = useState(true)
  // One-line message shown above the login form (e.g. after a password change
  // revoked the session, or after a 401 mid-session).
  const [notice, setNotice] = useState('')
  const isAuthed = !!user

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetch('/api/admin/users/me', { credentials: 'same-origin' })
        if (res.ok && !cancelled) {
          const me = await res.json()
          setUser({ username: me.username || '', role: me.role || '' })
        }
      } catch {}
      finally { if (!cancelled) setChecking(false) }
    })()
    return () => { cancelled = true }
  }, [])

  // api.ts dispatches "gw:unauthorized" whenever any request comes back 401
  // (expired/revoked session cookie). Clear identity so the login screen
  // renders — previously this event had no listener and pages kept failing
  // silently behind a stale "signed-in" shell.
  useEffect(() => {
    const onUnauthorized = () => setUser(null)
    window.addEventListener('gw:unauthorized', onUnauthorized)
    return () => window.removeEventListener('gw:unauthorized', onUnauthorized)
  }, [])

  const applyIdentity = (u: Partial<SessionUser>|undefined) => {
    setUser({ username: u?.username || '', role: u?.role || '' })
  }

  const login = async (username: string, pw: string) => {
    const body:any = { password: pw }
    if (username) body.username = username
    const res = await fetch('/api/auth/login', { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify(body)})
    if (!res.ok) {
      const t = await res.text()
      throw new Error(extractApiError(t, 'login failed'))
    }
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
      return 'dark'
    } catch { return 'dark' }
  })
  useEffect(() => {
    document.documentElement.classList.toggle('light', theme === 'light')
    try { localStorage.setItem('gw_theme', theme) } catch {}
  }, [theme])
  return { theme, toggle: () => setTheme(t => t === 'dark' ? 'light' : 'dark') }
}

/* ------------------------------------------------------------------ */
/* Navigation model                                                    */
/* ------------------------------------------------------------------ */

type NavItem = { to: string; label: string; icon: IconName; adminOnly?: boolean }
const NAV_GROUPS: { title: string; items: NavItem[] }[] = [
  {
    title: 'Overview',
    items: [
      { to: '/', label: 'Dashboard', icon: 'pulse' },
      { to: '/analytics', label: 'Analytics', icon: 'chart' },
    ],
  },
  {
    title: 'Control',
    items: [
      { to: '/providers', label: 'Providers', icon: 'server' },
      { to: '/models', label: 'Models', icon: 'box' },
      { to: '/routing', label: 'Routing', icon: 'route' },
    ],
  },
  {
    title: 'Access',
    items: [
      { to: '/keys', label: 'API Keys', icon: 'key' },
      { to: '/teams', label: 'Teams', icon: 'users', adminOnly: true },
      { to: '/users', label: 'Users', icon: 'userCog', adminOnly: true },
      { to: '/audit', label: 'Audit', icon: 'shield', adminOnly: true },
    ],
  },
  {
    title: 'System',
    items: [
      { to: '/playground', label: 'Playground', icon: 'play' },
      { to: '/logs', label: 'Request Logs', icon: 'logs' },
      { to: '/settings', label: 'Settings', icon: 'cog' },
    ],
  },
]

function visibleGroups(role: string) {
  return NAV_GROUPS
    .map((g) => ({ ...g, items: g.items.filter((i) => !i.adminOnly || role === 'admin') }))
    .filter((g) => g.items.length > 0)
}

function pageTitle(pathname: string): string {
  for (const g of NAV_GROUPS) {
    for (const i of g.items) {
      if (i.to === pathname) return i.label
    }
  }
  if (pathname === '/profile') return 'Profile'
  return 'Dashboard'
}

/* ------------------------------------------------------------------ */
/* Sidebar                                                             */
/* ------------------------------------------------------------------ */

function SidebarLink({ item, collapsed, active, onNavigate }: {
  item: NavItem; collapsed: boolean; active: boolean; onNavigate?: () => void
}) {
  return (
    <NavLink
      to={item.to}
      onClick={onNavigate}
      title={collapsed ? item.label : undefined}
      className={`group relative flex items-center rounded-lg text-sm font-medium transition-colors duration-150 outline-none focus-visible:ring-2 focus-visible:ring-teal/50 ${
        collapsed ? 'justify-center h-10 w-10 mx-auto' : 'gap-3 px-3 h-9'
      } ${active ? 'bg-raised text-paper' : 'text-muted hover:text-paper hover:bg-stone/40'}`}
    >
      <span className={`absolute left-0 top-1/2 -translate-y-1/2 w-[3px] h-5 rounded-full bg-teal transition-all duration-200 ${
        active ? 'opacity-100 scale-y-100' : 'opacity-0 scale-y-50'
      }`} />
      <Icon name={item.icon} size={17} className={active ? 'text-teal' : 'group-hover:text-paper transition-colors'} />
      {!collapsed && <span className="truncate">{item.label}</span>}
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
            <div className="px-3 mb-1.5 text-[10px] font-semibold uppercase tracking-[0.14em] text-muted/70">
              {g.title}
            </div>
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
    <div className={`flex items-center gap-2.5 ${collapsed ? 'justify-center' : 'px-1'}`}>
      <div className="w-8 h-8 rounded-lg bg-gradient-to-br from-teal to-amber flex items-center justify-center shrink-0 shadow-glow">
        <Icon name="zap" size={16} className="text-graphite" />
      </div>
      {!collapsed && (
        <div className="leading-tight">
          <div className="font-semibold tracking-tight">Gateway</div>
          <div className="text-[10px] uppercase tracking-[0.14em] text-muted">Unified LLM</div>
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
  const [sidebarOpen, setSidebarOpen] = useState(false)     // mobile drawer
  const [railCollapsed, setRailCollapsed] = useState(() => {
    try { return localStorage.getItem('gw_rail') === '1' } catch { return false }
  })

  useEffect(() => {
    try { localStorage.setItem('gw_rail', railCollapsed ? '1' : '0') } catch {}
  }, [railCollapsed])
  useEffect(() => { setSidebarOpen(false) }, [loc.pathname])

  /* ---------------- Loading / Login screens ---------------- */

  if (checking) {
    return (
      <div className="min-h-screen grid place-items-center bg-app">
        <div className="flex flex-col items-center gap-4">
          <div className="w-10 h-10 rounded-xl bg-gradient-to-br from-teal to-amber flex items-center justify-center shadow-glow animate-pulse-soft">
            <Icon name="zap" size={20} className="text-graphite" />
          </div>
          <div className="text-muted text-xs tracking-widest uppercase">Restoring session</div>
        </div>
      </div>
    )
  }

  if (!isAuthed) {
    return (
      <>
        <LoginScreen
          theme={theme} toggle={toggle} login={login} loginWithToken={loginWithToken}
          notice={notice} onNoticeConsumed={clearNotice}
        />
        {/* Mounted outside the auth branch so login-screen toasts are visible. */}
        <Toaster />
      </>
    )
  }

  /* ---------------- Authenticated shell ---------------- */

  const collapsed = railCollapsed

  const userMenu = (
    <UserMenu accountName={accountName} role={role} onLogout={logout} />
  )

  return (
    <div className="min-h-screen bg-app">
      {/* Desktop sidebar */}
      <aside className={`hidden lg:flex fixed inset-y-0 left-0 z-30 flex-col border-r border-stone bg-app transition-all duration-200 ${
        collapsed ? 'w-[68px]' : 'w-60'
      }`}>
        <div className={`h-14 flex items-center border-b border-stone shrink-0 ${collapsed ? 'justify-center px-0' : 'px-4'}`}>
          <BrandMark collapsed={collapsed} />
        </div>
        <SidebarBody role={role} pathname={loc.pathname} collapsed={collapsed} />
        <div className="border-t border-stone p-2">
          <button
            onClick={() => setRailCollapsed(c => !c)}
            className="w-full h-9 rounded-lg flex items-center justify-center gap-2 text-muted hover:text-paper hover:bg-stone/40 transition-colors text-xs"
            aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
          >
            <Icon name={collapsed ? 'chevronRight' : 'chevronLeft'} size={15} />
            {!collapsed && <span>Collapse</span>}
          </button>
        </div>
      </aside>

      {/* Mobile drawer */}
      {sidebarOpen && (
        <div className="lg:hidden fixed inset-0 z-40" role="dialog" aria-modal="true">
          <div className="absolute inset-0 bg-black/60 backdrop-blur-sm animate-fade" onClick={() => setSidebarOpen(false)} />
          <div className="absolute inset-y-0 left-0 w-64 bg-app border-r border-stone shadow-pop flex flex-col animate-sidebar">
            <div className="h-14 flex items-center justify-between px-4 border-b border-stone">
              <BrandMark />
              <button onClick={() => setSidebarOpen(false)} aria-label="Close menu"
                className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/50">
                <Icon name="x" size={16} />
              </button>
            </div>
            <SidebarBody role={role} pathname={loc.pathname} collapsed={false} onNavigate={() => setSidebarOpen(false)} />
            <div className="border-t border-stone p-3">
              <Link to="/profile" onClick={() => setSidebarOpen(false)}
                className="flex items-center gap-3 px-2 py-2 rounded-lg text-sm text-muted hover:text-paper hover:bg-stone/40">
                <Icon name="shield" size={16} /> Profile
              </Link>
              <button onClick={logout}
                className="w-full flex items-center gap-3 px-2 py-2 rounded-lg text-sm text-muted hover:text-paper hover:bg-stone/40">
                <Icon name="logout" size={16} /> Log out
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Main column */}
      <div className={`transition-[padding] duration-200 ${collapsed ? 'lg:pl-[68px]' : 'lg:pl-60'}`}>
        {/* Topbar */}
        <header className="sticky top-0 z-20 h-14 border-b border-stone bg-app/90 backdrop-blur flex items-center gap-3 px-4 lg:px-6">
          <button onClick={() => setSidebarOpen(true)}
            className="lg:hidden w-9 h-9 -ml-1 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/40"
            aria-label="Open menu">
            <Icon name="menu" size={18} />
          </button>

          <div className="min-w-0 flex items-center gap-2 text-sm">
            <span className="hidden sm:inline text-muted/70">Gateway</span>
            <span className="hidden sm:inline text-muted/40">/</span>
            <span className="font-medium truncate">{pageTitle(loc.pathname)}</span>
          </div>

          <div className="ml-auto flex items-center gap-1.5">
            <button onClick={toggle}
              className="w-9 h-9 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/40 transition-colors"
              aria-label="Toggle theme" title={theme === 'dark' ? 'Switch to light' : 'Switch to dark'}>
              <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={16} />
            </button>
            {userMenu}
          </div>
        </header>

        {/* Page content with route transition */}
        <main key={loc.pathname} className="max-w-[1240px] mx-auto px-4 lg:px-6 py-6 lg:py-8 animate-page min-h-[calc(100vh-56px)]">
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
            <Route path="/users" element={role==='admin' ? <Users /> : <Navigate to="/" replace />} />
            <Route path="/audit" element={role==='admin' ? <Audit /> : <Navigate to="/" replace />} />
            <Route path="/profile" element={<Profile onSessionRevoked={() => logout('Password changed — please sign in again')} />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </main>
      </div>

      <Toaster />
    </div>
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
        className="flex items-center gap-2 h-9 pl-1 pr-2 rounded-lg hover:bg-stone/40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50"
        aria-haspopup="menu" aria-expanded={open}>
        <Avatar name={accountName} />
        <span className="hidden md:block text-sm max-w-[120px] truncate">{accountName || 'admin'}</span>
        <Icon name="chevronDown" size={13} className={`text-muted transition-transform duration-150 ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 top-full mt-2 w-56 rounded-xl border border-stone bg-surface shadow-pop p-1.5 animate-modal origin-top-right">
          <div className="px-3 py-2.5 border-b border-stone mb-1.5">
            <div className="text-sm font-medium truncate">{accountName || 'admin'}</div>
            <div className="flex items-center gap-1.5 mt-0.5">
              <span className="inline-flex items-center gap-1 text-[11px] text-teal">
                <Icon name="shield" size={11} />{role || 'admin'}
              </span>
            </div>
          </div>
          <Link to="/profile" onClick={() => setOpen(false)} role="menuitem"
            className="flex items-center gap-2.5 px-3 py-2 rounded-lg text-sm text-muted hover:text-paper hover:bg-stone/40 transition-colors">
            <Icon name="shield" size={15} /> Profile & security
          </Link>
          <button onClick={() => { setOpen(false); onLogout() }} role="menuitem"
            className="w-full flex items-center gap-2.5 px-3 py-2 rounded-lg text-sm text-muted hover:text-paper hover:bg-stone/40 transition-colors text-left">
            <Icon name="logout" size={15} /> Log out
          </button>
        </div>
      )}
    </div>
  )
}

function Avatar({ name, size = 7 }: { name: string; size?: number }) {
  const letter = (name || 'A')[0].toUpperCase()
  return (
    <span
      style={{ width: `${size * 4}px`, height: `${size * 4}px` }}
      className="rounded-lg bg-gradient-to-br from-teal/80 to-amber/80 flex items-center justify-center text-graphite text-xs font-bold select-none"
    >
      {letter}
    </span>
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
    <div className="min-h-screen grid lg:grid-cols-[1fr_1.1fr] bg-app bg-grid">
      {/* Left: brand story */}
      <div className="hidden lg:flex flex-col justify-between p-12 border-r border-stone/60">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-xl bg-gradient-to-br from-teal to-amber flex items-center justify-center shadow-glow">
            <Icon name="zap" size={18} className="text-graphite" />
          </div>
          <div className="leading-tight">
            <div className="font-semibold tracking-tight">AI Gateway</div>
            <div className="text-[10px] uppercase tracking-[0.16em] text-muted">One domain · every model</div>
          </div>
        </div>
        <div className="max-w-md">
          <h1 className="text-3xl font-semibold tracking-tight leading-snug">
            Route, observe and govern<br/>every LLM call — <span className="text-teal">in one place.</span>
          </h1>
          <p className="text-muted mt-4 leading-relaxed text-sm">
            OpenAI &amp; Anthropic compatible APIs, curated provider routing with load balancing,
            virtual keys, budgets, caching and full request observability.
          </p>
          <div className="mt-6 flex flex-wrap gap-2">
            {['Routing', 'Budgets', 'Caching', 'Observability'].map((f) => (
              <span key={f} className="text-xs text-muted border border-stone rounded-full px-2.5 py-1">{f}</span>
            ))}
          </div>
        </div>
        <div className="text-xs text-muted">Fast Go core · OpenAI / Anthropic / Responses compatible</div>
      </div>

      {/* Right: sign-in card */}
      <div className="flex items-center justify-center p-6">
        <Card className="w-full max-w-md animate-page">
          <div className="mb-6">
            <div className="lg:hidden flex items-center gap-2 mb-5">
              <div className="w-8 h-8 rounded-lg bg-gradient-to-br from-teal to-amber flex items-center justify-center">
                <Icon name="zap" size={15} className="text-graphite" />
              </div>
              <span className="font-semibold tracking-tight">AI Gateway</span>
            </div>
            <h2 className="text-xl font-semibold tracking-tight">Sign in</h2>
            <p className="text-muted text-sm mt-1">Manage providers, routing, keys and spend.</p>
          </div>

          <SegmentedControl
            value={mode}
            onChange={setMode}
            options={[
              { value: 'password', label: 'Password' },
              { value: 'passkey', label: 'Passkey' },
              { value: 'recovery', label: 'Recovery' },
            ]}
          />

          {notice && (
            <div className="mt-4 flex items-start gap-2 rounded-lg border border-teal/30 bg-teal/10 px-3 py-2.5 text-sm text-teal">
              <Icon name="check" size={15} className="mt-0.5 shrink-0" />
              <span>{notice}</span>
            </div>
          )}

          <div className="mt-5 space-y-4">
            {mode !== 'recovery' && (
              <Input placeholder="Username" value={username} onChange={e=>setUsername(e.target.value)} autoComplete="username" />
            )}
            {mode==='password' && (
              <>
                <Input placeholder="Username" value={username} onChange={e=>setUsername(e.target.value)} autoComplete="username" />
                <Input placeholder="Password" type="password" value={pw} onChange={e=>setPw(e.target.value)}
                  autoComplete="current-password" onKeyDown={e=>{ if(e.key==='Enter') doLogin() }} />
                <ErrorNote message={err} />
                <Button variant="primary" disabled={busy} onClick={doLogin} className="w-full">
                  {busy ? 'Signing in…' : 'Sign in'}
                </Button>
                <p className="text-[11px] text-muted leading-relaxed text-center">
                  Default sign-in: username <code className="font-mono text-muted">admin</code> with the{' '}
                  <code className="font-mono text-muted">ADMIN_PASSWORD</code> you configured (default{' '}
                  <code className="font-mono text-muted">admin123</code>). Change it after first login.
                </p>
              </>
            )}
            {mode==='passkey' && (
              <>
                <ErrorNote message={err} />
                <Button variant="primary" disabled={busy} onClick={doPasskeyLogin} className="w-full">
                  {busy ? 'Waiting for authenticator…' : 'Continue with passkey'}
                </Button>
                <p className="text-xs text-muted text-center">
                  Requires passkey enrollment.{' '}
                  <button onClick={()=>setMode('recovery')} className="text-teal underline underline-offset-2 hover:brightness-110">
                    Lost passkey? Use recovery code
                  </button>
                </p>
              </>
            )}
            {mode==='recovery' && (
              <>
                <Input placeholder="Recovery code (XXXX-XXXX-XXXX-XXXX)" value={recoveryCode}
                  onChange={e=>setRecoveryCode(e.target.value)} className="font-mono" />
                <ErrorNote message={err} />
                <Button variant="primary" disabled={busy} onClick={doRecovery} className="w-full">
                  {busy ? 'Verifying…' : 'Verify recovery code'}
                </Button>
                <p className="text-xs text-muted text-center">
                  Shown once when a passkey is enabled.
                </p>
              </>
            )}
          </div>

          <div className="mt-6 pt-4 border-t border-stone flex items-center justify-between">
            <span className="text-xs text-muted">Admins create accounts under Users.</span>
            <button onClick={toggle} aria-label="Toggle theme"
              className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/40">
              <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={15} />
            </button>
          </div>
        </Card>
      </div>
    </div>
  )
}
