import { useEffect, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import {
  Card, Button, Badge, Icon, EmptyState,
  type IconName,
} from '../components/ui'

/** Small KPI tile: Card + icon chip + value. */
function Stat({ icon, title, value, sub, tone = 'neutral' }: {
  icon: IconName; title: string; value: ReactNode; sub?: string
  tone?: 'neutral' | 'good' | 'warn'
}) {
  const tones: Record<'neutral' | 'good' | 'warn', string> = {
    neutral: 'bg-stone/50 text-muted',
    good: 'bg-teal/10 text-teal',
    warn: 'bg-amber/10 text-amber',
  }
  return (
    <Card className="flex items-start gap-3">
      <div className={`w-9 h-9 rounded-lg flex items-center justify-center shrink-0 ${tones[tone]}`}>
        <Icon name={icon} size={17} />
      </div>
      <div className="min-w-0">
        <div className="text-xs text-muted">{title}</div>
        <div className="text-2xl font-semibold tracking-tight tabular-nums mt-0.5 truncate">{value}</div>
        {sub && <div className="text-xs text-muted mt-0.5">{sub}</div>}
      </div>
    </Card>
  )
}

export default function Dashboard() {
  const navigate = useNavigate()
  const [stats, setStats] = useState<any>(null)
  const [statsError, setStatsError] = useState('')
  const [reloadKey, setReloadKey] = useState(0)
  const [health, setHealth] = useState<any>(null)
  const [catalogStatus, setCatalogStatus] = useState<any>(null)
  const [gettingStartedDismissed, setGettingStartedDismissed] = useState(() => {
    try { return localStorage.getItem('gw_getting_started_dismissed') === '1' } catch { return false }
  })
  useEffect(()=>{
    setStatsError('')
    api.stats().then(setStats).catch((e:any)=>setStatsError(e?.message || String(e)))
    api.health().then(setHealth).catch(()=>setHealth({status:'unknown'}))
    api.catalog.status().then(setCatalogStatus).catch(()=>{})
  },[reloadKey])

  const dismissGettingStarted = () => {
    setGettingStartedDismissed(true)
    try { localStorage.setItem('gw_getting_started_dismissed', '1') } catch {}
  }

  // Data (values + formatting) identical to the previous page.
  const cards = [
    { label:'Providers', value: stats?.providers ?? '—', sub:'upstream', icon:'server' as IconName, tone:'warn' as const },
    { label:'Keys', value: stats?.keys ?? '—', sub:'sk-gw-*', icon:'key' as IconName, tone:'good' as const },
    { label:'Requests', value: stats?.requests ?? '—', sub:'proxied', icon:'pulse' as IconName, tone:'neutral' as const },
    { label:'Models', value: catalogStatus?.count ?? stats?.catalog ?? '—', sub:'catalog', icon:'box' as IconName, tone:'warn' as const },
    { label:'Tokens', value: stats?.total_tokens ? Number(stats.total_tokens).toLocaleString() : '0', sub:'total', icon:'zap' as IconName, tone:'good' as const },
    { label:'Cost', value: stats?.total_cost ? `$${Number(stats.total_cost).toFixed(4)}` : '$0.00', sub:'estimated', icon:'chart' as IconName, tone:'neutral' as const },
  ]

  const hour = new Date().getHours()
  const greeting = hour < 12 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening'
  const healthOk = String(health?.status || 'ok') === 'ok'

  const links = [
    { to:'/logs', icon:'logs' as IconName, title:'Request Logs', desc:'Every proxied call with status, latency, tokens and cost.' },
    { to:'/analytics', icon:'chart' as IconName, title:'Analytics', desc:'Usage trends, spend and per-model breakdowns over time.' },
  ]

  // Build the Quick Start example from where the dashboard is actually served.
  const origin = window.location.origin
  const quickStart = `curl ${origin}/v1/chat/completions \\
  -H "Authorization: Bearer sk-gw-..." \\
  -H "Content-Type: application/json" \\
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'
`

  const steps = [
    { n: 1, title: 'Add a provider', desc: 'Connect an upstream endpoint.', to: '/providers', cta: 'Providers' },
    { n: 2, title: 'Discover its models', desc: 'Pull the model list from the provider.', to: '/models', cta: 'Models' },
    { n: 3, title: 'Create a gateway key', desc: 'Issue a virtual sk-gw-* key.', to: '/keys', cta: 'API Keys' },
    { n: 4, title: 'Send a request from the Playground', desc: 'Test end-to-end without leaving the dashboard.', to: '/playground', cta: 'Playground' },
  ]

  return (
    <div className="space-y-6">
      {/* Hero greeting */}
      <Card className="relative overflow-hidden">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <div className="text-xs uppercase tracking-wider text-muted">AI Gateway</div>
            <h1 className="text-2xl md:text-3xl font-bold tracking-tight mt-1">{greeting}</h1>
            <p className="text-muted text-sm mt-1">One domain for every model.</p>
          </div>
          <div className="flex flex-wrap gap-2 items-center">
            <Badge tone={healthOk ? 'good' : 'warn'} dot>{health?.status || 'ok'}</Badge>
            {catalogStatus?.count != null && <Badge tone="neutral">{catalogStatus.count} models</Badge>}
          </div>
        </div>
      </Card>

      {/* Getting started — shown until the first provider is connected (and not dismissed) */}
      {!statsError && stats && Number(stats.providers) === 0 && !gettingStartedDismissed && (
        <Card className="border-teal/30 bg-teal/5">
          <div className="flex items-start justify-between gap-3">
            <div className="flex items-center gap-2">
              <Icon name="zap" size={16} className="text-teal" />
              <h2 className="font-semibold tracking-tight">Getting started</h2>
            </div>
            <button onClick={dismissGettingStarted} aria-label="Dismiss getting started"
              className="w-8 h-8 -mr-1 -mt-1 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/40">
              <Icon name="x" size={15} />
            </button>
          </div>
          <div className="mt-3 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3">
            {steps.map(s => (
              <button key={s.n} onClick={()=>navigate(s.to)}
                className="text-left rounded-lg border border-stone bg-surface p-3 hover:border-teal/40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50">
                <div className="flex items-center gap-2">
                  <span className="w-5 h-5 rounded-full bg-teal/15 text-teal text-[11px] font-bold flex items-center justify-center shrink-0">{s.n}</span>
                  <span className="text-sm font-medium truncate">{s.title}</span>
                </div>
                <div className="text-xs text-muted mt-1.5 leading-relaxed">{s.desc}</div>
                <div className="text-xs text-teal mt-2 font-medium">{s.cta} →</div>
              </button>
            ))}
          </div>
        </Card>
      )}

      {/* KPI grid — same data points as before (or an honest error state) */}
      {statsError ? (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load dashboard stats"
            hint={statsError}
            action={<Button variant="secondary" onClick={()=>setReloadKey(k=>k+1)}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      ) : (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3 md:gap-4">
          {cards.map(c => (
            <Stat key={c.label} icon={c.icon} title={c.label} value={c.value} sub={c.sub} tone={c.tone} />
          ))}
        </div>
      )}

      {/* Link-out area */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {links.map(l => (
          <Card key={l.to} className="flex flex-col">
            <div className="w-10 h-10 rounded-lg bg-teal/10 text-teal flex items-center justify-center mb-3">
              <Icon name={l.icon} size={18} />
            </div>
            <h3 className="font-semibold tracking-tight">{l.title}</h3>
            <p className="text-muted text-sm mt-1 leading-relaxed flex-1">{l.desc}</p>
            <div className="mt-4">
              <Button variant="secondary" onClick={()=>navigate(l.to)}>
                <Icon name="external" size={15}/>Open
              </Button>
            </div>
          </Card>
        ))}
      </div>

      {/* Quick start */}
      <Card>
        <details>
          <summary className="text-sm font-semibold cursor-pointer select-none flex items-center gap-2">
            <Icon name="play" size={14} className="text-teal"/>Quick start
          </summary>
          <pre className="mt-3 rounded-lg border border-stone bg-app p-3 font-mono text-xs overflow-x-auto whitespace-pre">{quickStart}</pre>
        </details>
      </Card>
    </div>
  )
}
