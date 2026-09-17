import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { Card, Button, Badge, Icon, EmptyState, Stat, Eyebrow, PageHeader, CopyButton, type IconName } from '../components/ui'

export default function Dashboard() {
  const navigate = useNavigate()
  const [stats, setStats] = useState<any>(null)
  const [statsError, setStatsError] = useState('')
  const [reloadKey, setReloadKey] = useState(0)
  const [health, setHealth] = useState<any>(null)
  const [catalogStatus, setCatalogStatus] = useState<any>(null)
  const [dismissed, setDismissed] = useState(() => {
    try { return localStorage.getItem('gw_getting_started_dismissed') === '1' } catch { return false }
  })
  useEffect(()=>{
    setStatsError('')
    api.stats().then(setStats).catch((e:any)=>setStatsError(e?.message || String(e)))
    api.health().then(setHealth).catch(()=>setHealth({status:'unknown'}))
    api.catalog.status().then(setCatalogStatus).catch(()=>{})
  },[reloadKey])

  const dismiss = () => {
    setDismissed(true)
    try { localStorage.setItem('gw_getting_started_dismissed', '1') } catch {}
  }

  const cards = [
    { label:'Providers', value: stats?.providers ?? '—', sub:'upstream endpoints', icon:'server' as IconName, tone:'warn' as const },
    { label:'API keys', value: stats?.keys ?? '—', sub:'virtual sk-gw-*', icon:'key' as IconName, tone:'good' as const },
    { label:'Requests', value: stats?.requests ?? '—', sub:'proxied all-time', icon:'pulse' as IconName, tone:'neutral' as const },
    { label:'Models', value: catalogStatus?.count ?? stats?.catalog ?? '—', sub:'in catalog', icon:'box' as IconName, tone:'neutral' as const },
    { label:'Tokens', value: stats?.total_tokens ? Number(stats.total_tokens).toLocaleString() : '0', sub:'total processed', icon:'zap' as IconName, tone:'good' as const },
    { label:'Spend', value: stats?.total_cost ? `$${Number(stats.total_cost).toFixed(2)}` : '$0.00', sub:'estimated', icon:'wallet' as IconName, tone:'neutral' as const },
  ]

  const hour = new Date().getHours()
  const greeting = hour < 12 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening'
  const healthOk = String(health?.status || 'ok') === 'ok'
  const origin = window.location.origin
  const quickStart = `curl ${origin}/v1/chat/completions \\\n  -H "Authorization: Bearer sk-gw-..." \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'`

  const steps = [
    { n: 1, title: 'Connect a provider', desc: 'Add an upstream endpoint and key.', to: '/providers', cta: 'Providers', icon: 'server' as IconName },
    { n: 2, title: 'Discover models', desc: 'Pull its model list + pricing.', to: '/models', cta: 'Models', icon: 'box' as IconName },
    { n: 3, title: 'Issue a key', desc: 'Create a virtual sk-gw-* credential.', to: '/keys', cta: 'API Keys', icon: 'key' as IconName },
    { n: 4, title: 'Send a test call', desc: 'Verify end-to-end in seconds.', to: '/playground', cta: 'Playground', icon: 'play' as IconName },
  ]
  const onboardDone = stats && Number(stats.providers) > 0

  return (
    <div className="space-y-6">
      <PageHeader eyebrow="Operate · Overview" title={`${greeting} — here's your gateway`} description="One domain for every model. Health, spend and the fastest path to first traffic." />

      {/* Hero — ink ledger plate */}
      <div className="rounded-xl bg-ink text-cream shadow-card overflow-hidden">
        <div className="p-6 md:p-8 flex flex-col lg:flex-row lg:items-center gap-6">
          <div className="flex-1 min-w-0">
            <div className="font-mono text-[11px] uppercase tracking-[0.2em] text-accent">
              {healthOk ? `● ${health?.status || 'ok'}` : `● ${health?.status || 'unknown'}`}
              {catalogStatus?.count != null && <span className="opacity-70"> · {catalogStatus.count} models</span>}
              {stats && <span className="opacity-70"> · {stats.requests ?? 0} requests</span>}
            </div>
            <h2 className="font-display text-4xl md:text-5xl font-semibold mt-3">Route. Observe. Govern.</h2>
            <p className="opacity-70 text-sm mt-2 max-w-xl leading-relaxed">Bare model names fan out across your provider groups with failover — keys enforce budgets, every call lands in logs with tokens, latency and cost.</p>
            <div className="mt-5 flex flex-wrap gap-2 items-center">
              <Button variant="primary" onClick={()=>navigate('/playground')}><Icon name="play" size={15}/> Test a call</Button>
              <Button variant="secondary" onClick={()=>navigate('/providers')} className="!bg-transparent !text-cream !border-cream/25 hover:!border-cream/60"><Icon name="server" size={15}/> Add provider</Button>
              <button onClick={()=>navigate('/logs')} className="inline-flex items-center gap-1.5 text-sm font-semibold opacity-80 hover:opacity-100 transition-opacity px-2">View requests <Icon name="arrowRight" size={14}/></button>
            </div>
          </div>
          <div className="lg:w-[300px] shrink-0 grid grid-cols-2 lg:grid-cols-1 gap-px bg-cream/15 rounded-xl overflow-hidden border border-cream/15">
            {[
              { k: 'Tokens', v: stats?.total_tokens ? Number(stats.total_tokens).toLocaleString() : '0' },
              { k: 'Est. spend', v: stats?.total_cost ? `$${Number(stats.total_cost).toFixed(2)}` : '$0.00' },
            ].map(r => (
              <div key={r.k} className="bg-ink px-4 py-3">
                <div className="text-[11px] uppercase tracking-[0.14em] opacity-60">{r.k}</div>
                <div className="font-display text-2xl font-semibold tabular-nums mt-0.5 truncate">{r.v}</div>
              </div>
            ))}
            <button onClick={()=>navigate('/analytics')} className="bg-ink px-4 py-3 text-left hover:bg-cream/5 transition-colors col-span-2 lg:col-span-1">
              <div className="text-[11px] uppercase tracking-[0.14em] text-accent">This week</div>
              <div className="text-sm font-semibold mt-0.5 flex items-center gap-1.5">Open analytics <Icon name="arrowRight" size={13}/></div>
            </button>
          </div>
        </div>
      </div>

      {/* Onboarding */}
      {!statsError && stats && !onboardDone && !dismissed && (
        <Card>
          <div className="flex items-start justify-between gap-3">
            <div>
              <Eyebrow>Getting started</Eyebrow>
              <h2 className="font-display font-semibold text-2xl mt-1.5">First traffic in four steps</h2>
            </div>
            <button onClick={dismiss} aria-label="Dismiss getting started" className="w-8 h-8 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-raised">
              <Icon name="x" size={15} />
            </button>
          </div>
          <div className="mt-4 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3">
            {steps.map(s => (
              <button key={s.n} onClick={()=>navigate(s.to)}
                className="group text-left rounded-xl border border-stone bg-app p-4 hover:border-accent/50 transition-colors">
                <div className="flex items-center justify-between">
                  <span className="font-display text-2xl text-accent font-semibold">{String(s.n).padStart(2, '0')}</span>
                  <Icon name={s.icon} size={16} className="text-muted group-hover:text-accent transition-colors" />
                </div>
                <div className="text-sm font-semibold mt-2">{s.title}</div>
                <div className="text-xs text-muted mt-1 leading-relaxed">{s.desc}</div>
                <div className="text-xs text-accent mt-2.5 font-semibold flex items-center gap-1">{s.cta} <Icon name="arrowRight" size={12}/></div>
              </button>
            ))}
          </div>
        </Card>
      )}

      {/* KPIs */}
      {statsError ? (
        <Card><EmptyState icon="alert" title="Could not load dashboard stats" hint={statsError}
          action={<Button variant="secondary" onClick={()=>setReloadKey(k=>k+1)}><Icon name="refresh" size={15}/>Retry</Button>} /></Card>
      ) : (
        <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3">
          {cards.map(c => <Stat key={c.label} icon={c.icon} title={c.label} value={c.value} sub={c.sub} tone={c.tone} />)}
        </div>
      )}

      {/* Explore + quickstart */}
      <div className="grid grid-cols-1 lg:grid-cols-5 gap-4">
        <div className="lg:col-span-3 grid grid-cols-1 sm:grid-cols-2 gap-4">
          {[
            { to:'/logs', icon:'logs' as IconName, title:'Request inspector', desc:'Status, latency, TTFT, tokens and cost for every proxied call — with full payloads.', cta:'Open requests' },
            { to:'/analytics', icon:'chart' as IconName, title:'Spend analytics', desc:'Traffic, error mix and per-model breakdowns across 24h / 7d / 30d windows.', cta:'Open analytics' },
            { to:'/routing', icon:'route' as IconName, title:'Routing groups', desc:'Balance bare model names across providers with weighted or failover order.', cta:'Open routing' },
            { to:'/keys', icon:'key' as IconName, title:'Keys & budgets', desc:'Virtual credentials with rate limits, allowlists and monthly spend caps.', cta:'Open keys' },
          ].map(l => (
            <Card key={l.to} className="flex flex-col group hover:border-muted/60 transition-colors">
              <div className="w-10 h-10 rounded-xl bg-accent/10 border border-accent/25 text-accent flex items-center justify-center mb-3">
                <Icon name={l.icon} size={18} />
              </div>
              <h3 className="font-display font-semibold tracking-tight">{l.title}</h3>
              <p className="text-muted text-[13px] mt-1 leading-relaxed flex-1">{l.desc}</p>
              <div className="mt-4"><Button variant="secondary" size="sm" onClick={()=>navigate(l.to)}>{l.cta} <Icon name="arrowRight" size={13}/></Button></div>
            </Card>
          ))}
        </div>
        <Card className="lg:col-span-2 flex flex-col overflow-hidden">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="w-8 h-8 rounded-lg bg-raised border border-stone/60 text-accent flex items-center justify-center"><Icon name="terminal" size={15}/></span>
              <h3 className="font-display font-semibold tracking-tight">Quick start</h3>
            </div>
            <CopyButton value={quickStart} label="Copy curl" />
          </div>
          <p className="text-xs text-muted mt-1.5">Drop-in OpenAI-compatible call through your gateway domain.</p>
          <pre className="mt-3 flex-1 rounded-xl border border-stone bg-ink p-4 font-mono text-[11.5px] leading-relaxed overflow-x-auto whitespace-pre text-cream/90">{quickStart}</pre>
          <div className="mt-3 flex items-center gap-2 text-[11px] text-muted">
            <Icon name="lock" size={12}/> Key goes in <code className="font-mono text-accent">Authorization: Bearer</code> — never in the URL.
          </div>
        </Card>
      </div>
    </div>
  )
}
