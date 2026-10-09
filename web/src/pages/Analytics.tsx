import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Card, PageHeader, Button, Badge, Icon, SegmentedControl, EmptyState, Skeleton, Stat, Progress } from '../components/ui'
import { api, extractApiError, type RangeKey } from '../lib/api'
import { BarSeries, bucketWhen, fmtCompact, fmtInt, fmtMoney, fmtPct, TONES } from '../components/report'

type Daily = { day: string; tokens: number; cost: number; requests: number; cache_hits?: number }
type TopModel = { model: string; tokens: number; cost: number; requests: number }
type TopKey = { key_prefix: string; tokens: number; cost: number; requests: number }
type ErrorRow = { status: number; count: number; sample?: string }
type Stats = {
  daily: Daily[]; top_models: TopModel[]; top_keys: TopKey[]
  latency: { p50: number; p95: number; avg: number; count: number }
  range_tokens: number; range_cost: number; range_requests: number
  range_successful: number; range_failed: number
  range_ttft_avg: number; range_tps_avg: number
  errors: ErrorRow[]
  cache_hit_rate: number; cache_read_tokens: number
  range_cache_hits: number; gateway_cache_hit_rate: number
  range_cache_eligible: number; range_cache_bypassed: number
  range_prompt_tokens: number; range_completion_tokens: number
}
type Metric = 'requests' | 'tokens' | 'cost' | 'cache_hits'
const OVERVIEW_RANGES: { value: RangeKey; label: string }[] = [
  { value: '24h', label: '24h' }, { value: '7d', label: '7d' }, { value: '30d', label: '30d' },
]

const METRICS: { value: Metric; label: string }[] = [
  { value: 'requests', label: 'Requests' },
  { value: 'tokens', label: 'Tokens' },
  { value: 'cost', label: 'Spend' },
  { value: 'cache_hits', label: 'Cache hits' },
]
const EMPTY_DAILY = { tokens: 0, cost: 0, requests: 0, cache_hits: 0 }

async function fetchStats(range: RangeKey, signal: AbortSignal): Promise<Stats> {
  // The legacy overview endpoint accepts 24h, 7d and 30d only. The reporting
  // endpoints power the longer windows in the other pages; do not silently
  // render a seven-day overview under a 90-day label.
  const res = await fetch(`/api/stats?range=${range}`, { credentials: 'same-origin', signal })
  if (!res.ok) {
    if (res.status === 401) window.dispatchEvent(new CustomEvent('gw:unauthorized'))
    throw new Error(extractApiError(await res.text(), `analytics request failed (${res.status})`))
  }
  return res.json()
}

async function fetchKeyMap(): Promise<Record<string, string>> {
  try {
    const keys = await api.keys.list() as { prefix: string; name: string }[]
    return Object.fromEntries((Array.isArray(keys) ? keys : []).map(k => [k.prefix, k.name]))
  } catch { return {} }
}

/** Match the server's UTC bucket keys, and include the partial boundary bucket. */
function fillDaily(daily: Daily[], range: RangeKey, now: Date): Daily[] {
  const byKey = new Map((Array.isArray(daily) ? daily : []).filter(d => typeof d.day === 'string').map(d => [d.day, d]))
  const hourly = range === '24h'
  const count = hourly ? 25 : range === '7d' ? 8 : 31
  const out: Daily[] = []
  for (let i = count - 1; i >= 0; i--) {
    const time = hourly
      ? new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), now.getUTCHours() - i))
      : new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i))
    const day = hourly ? `${time.toISOString().slice(0, 13)}:00:00Z` : time.toISOString().slice(0, 10)
    const bucket = byKey.get(day)
    out.push({ day, ...EMPTY_DAILY, ...bucket })
  }
  return out
}

function Section({ id, title, hint, children }: {
  id: string; title: string; hint?: string; children: ReactNode
}) {
  return (
    <section id={id} aria-label={title} className="scroll-mt-6 space-y-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="font-display text-[15px] font-semibold -tracking-tight text-paper">{title}</h2>
          {hint && <p className="text-xs text-muted mt-0.5">{hint}</p>}
        </div>
      </div>
      {children}
    </section>
  )
}

function MetricBar({ label, value, share, tone = 'accent' }: {
  label: string; value: string; share: number; tone?: 'accent' | 'amber'
}) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-muted">{label}</span>
        <span className="font-mono tabular-nums text-paper font-medium">{value}</span>
      </div>
      <div className="mt-1.5"><Progress value={Math.min(100, share * 100)} tone={tone} /></div>
    </div>
  )
}

export default function Analytics() {
  const [range, setRange] = useState<RangeKey>('7d')
  const [metric, setMetric] = useState<Metric>('requests')
  const [stats, setStats] = useState<{ range: RangeKey; data: Stats } | null>(null)
  const [keyMap, setKeyMap] = useState<Record<string, string>>({})
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const [reloadKey, setReloadKey] = useState(0)
  const [fetchedAt, setFetchedAt] = useState(() => new Date())
  const [wider, setWider] = useState<{ range: RangeKey; count: number } | null>(null)

  useEffect(() => { fetchKeyMap().then(setKeyMap).catch(() => {}) }, [])
  useEffect(() => {
    // /api/stats currently has no absolute or >30d window contract. Keep
    // overview selections truthful; detailed reports offer those windows.
    const controller = new AbortController()
    setLoading(true)
    setErr('')
    setWider(null)
    fetchStats(range, controller.signal)
      .then(s => { if (!controller.signal.aborted) { setStats({ range, data: s }); setFetchedAt(new Date()) } })
      .catch(e => { if (!controller.signal.aborted) setErr(String(e?.message || e)) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [range, reloadKey])

  useEffect(() => {
    if (loading || err || !stats || stats.range !== range || stats.data.range_requests > 0 || range === '30d') return
    const controller = new AbortController()
    fetchStats('30d', controller.signal)
      .then(s => { if (!controller.signal.aborted) setWider({ range, count: s.range_requests }) })
      .catch(() => {})
    return () => controller.abort()
  }, [loading, err, stats, range])

  const currentStats = stats?.range === range ? stats.data : null
  const filledDaily = useMemo(() => fillDaily(currentStats?.daily ?? [], range, fetchedAt), [currentStats, range, fetchedAt])
  const rangeLabel = range === '24h' ? '24 hours' : range === '7d' ? '7 days' : '30 days'
  const requests = currentStats?.range_requests ?? 0
  const ok = currentStats?.range_successful ?? 0
  const fail = currentStats?.range_failed ?? 0
  const failRate = requests > 0 ? fail / requests : 0
  const okRate = requests > 0 ? ok / requests : 0
  const tokens = currentStats?.range_tokens ?? 0
  const promptTok = currentStats?.range_prompt_tokens ?? 0
  const completionTok = currentStats?.range_completion_tokens ?? 0
  const cost = currentStats?.range_cost ?? 0
  const costPer1M = tokens > 0 ? cost / tokens * 1_000_000 : 0
  const tokPerReq = requests > 0 ? tokens / requests : 0
  const gwHits = currentStats?.range_cache_hits ?? 0
  const gwEligible = currentStats?.range_cache_eligible ?? 0
  const gwBypassed = currentStats?.range_cache_bypassed ?? 0
  const gwMisses = Math.max(0, gwEligible - gwHits)
  const gwRate = currentStats?.gateway_cache_hit_rate ?? 0
  const promptCacheRate = currentStats?.cache_hit_rate ?? 0
  const promptCacheTok = currentStats?.cache_read_tokens ?? 0
  const errors = currentStats?.errors ?? []
  const maxErrorCount = Math.max(1, ...errors.map(e => e.count))
  const latency = currentStats?.latency
  const p95 = latency?.p95 ?? 0
  const maxLatency = Math.max(p95, latency?.p50 ?? 0, latency?.avg ?? 0, 1)
  const modelMax = Math.max(1, ...(currentStats?.top_models ?? []).map(m => m.tokens))
  const keyMax = Math.max(1, ...(currentStats?.top_keys ?? []).map(k => k.tokens))

  const chartFormat = metric === 'cost' ? fmtMoney : fmtInt
  const chartSeries = [{ key: metric, label: METRICS.find(m => m.value === metric)?.label ?? metric, tone: metric === 'cache_hits' ? TONES.cache : TONES.cost }]

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Observe · Overview"
        title="Analytics"
        description="A windowed view of traffic, usage, reliability, speed, and caching. Explore detailed reports for deeper breakdowns."
        actions={<Button variant="secondary" size="sm" onClick={() => setReloadKey(k => k + 1)} disabled={loading} title="Reload analytics">
          <Icon name="refresh" size={14} className={loading ? 'animate-spin' : ''}/> Refresh
        </Button>}
      />

      <Card pad={false} className="px-4 py-3 flex flex-wrap items-center gap-x-3 gap-y-2">
        <span className="text-xs font-medium text-muted shrink-0">Reporting window</span>
        <SegmentedControl options={OVERVIEW_RANGES} value={range} onChange={setRange} />
        <p className="text-xs text-muted leading-relaxed sm:ml-auto sm:max-w-sm">Overview supports the last 24 hours, 7 days, or 30 days. For longer and custom periods, use the detailed reports below.</p>
      </Card>

      {err && <Card><EmptyState icon="alert" title="Could not load analytics" hint={err}
        action={<Button variant="secondary" onClick={() => setReloadKey(k => k + 1)}><Icon name="refresh" size={15} />Retry</Button>} /></Card>}

      {loading ? (
        <div className="space-y-4">
          <div className="grid grid-cols-2 xl:grid-cols-4 gap-3">
            {Array.from({ length: 4 }).map((_, i) => <Card key={i} className="space-y-3"><Skeleton className="h-8 w-10" /><Skeleton className="h-6 w-full" /></Card>)}
          </div>
          <Card><Skeleton className="h-56 w-full" /></Card>
          <div className="grid lg:grid-cols-3 gap-3">{Array.from({ length: 3 }).map((_, i) => <Card key={i}><Skeleton className="h-40 w-full" /></Card>)}</div>
        </div>
      ) : err ? null : currentStats && requests > 0 ? (
        <>
          <Section id="analytics-overview" title="Overview" hint={`Last ${rangeLabel} · totals use the exact rolling window`}>
            <div className="grid grid-cols-2 xl:grid-cols-4 gap-3">
              <Stat icon="pulse" title="Requests" value={fmtInt(requests)} sub={`${fmtInt(ok)} succeeded · ${fmtInt(fail)} failed`}
                tone={failRate === 0 ? 'good' : failRate > 0.05 ? 'bad' : 'warn'} />
              <Stat icon="layers" title="Tokens" value={fmtCompact(tokens)}
                sub={promptTok || completionTok ? `${fmtCompact(promptTok)} in · ${fmtCompact(completionTok)} out` : 'no usage reported'}
                delta={tokPerReq ? `≈${Math.round(tokPerReq)}/req` : undefined} />
              <Stat icon="wallet" title="Spend" value={fmtMoney(cost)}
                sub={tokens > 0 ? `${fmtMoney(costPer1M)} per 1M tokens` : 'no tokens to price'} />
              <Stat icon="zap" title="Gateway cache hit rate" value={gwEligible ? fmtPct(gwRate) : '—'}
                sub={gwEligible ? `${fmtInt(gwHits)} of ${fmtInt(gwEligible)} eligible requests` : 'no cache-eligible requests'} tone={gwHits > 0 ? 'good' : 'neutral'} />
            </div>
          </Section>

          <Section id="analytics-traffic" title="Traffic over time" hint={`UTC ${range === '24h' ? 'hourly' : 'daily'} buckets · boundary buckets may include traffic outside the exact rolling window`}>
            <Card>
              <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                <span className="text-xs text-muted">Hover a bar for its bucket value</span>
                <SegmentedControl options={METRICS} value={metric} onChange={setMetric} />
              </div>
              <BarSeries data={filledDaily} labelOf={d => bucketWhen(d.day)} series={chartSeries} format={chartFormat} height={176} />
              <div className="mt-4 pt-3 border-t border-stone grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
                {([['Requests', fmtInt(requests)], ['Tokens', fmtInt(tokens)], ['Spend', fmtMoney(cost)], ['Gateway cache hits', fmtInt(gwHits)]] as [string, string][]).map(([label, value]) => (
                  <div key={label} className="flex flex-col gap-0.5"><span className="text-muted">{label}</span><span className="font-mono tabular-nums font-medium">{value}</span></div>
                ))}
              </div>
            </Card>
          </Section>

          <Section id="analytics-quality" title="Service quality" hint="Failures, response times, and the two distinct caching layers">
            <div className="grid lg:grid-cols-3 gap-3">
              <Card>
                <h3 className="text-sm font-semibold -tracking-tight">Reliability</h3>
                <p className="text-xs text-muted mt-0.5">{fmtPct(okRate)} success across {fmtInt(requests)} requests</p>
                <div className="mt-3"><Progress value={okRate * 100} tone={okRate > 0.95 ? 'accent' : okRate > 0.85 ? 'amber' : 'red'} /></div>
                <div className="mt-2.5 flex flex-wrap items-center gap-2"><Badge tone="good">{fmtInt(ok)} succeeded</Badge><Badge tone="bad">{fmtInt(fail)} failed</Badge></div>
                <div className="mt-4 space-y-2.5">
                  {errors.length === 0 ? <p className="text-xs text-muted">No error status breakdown in this window.</p> : errors.map(e => (
                    <div key={e.status} className="flex items-center gap-3" title={e.sample}>
                      <Badge tone={e.status >= 500 ? 'bad' : 'warn'}>{e.status}</Badge>
                      <div className="flex-1 h-2.5 rounded-full bg-app border border-stone overflow-hidden">
                        <div className="h-full bg-danger/70 rounded-full" style={{ width: `${Math.round(e.count / maxErrorCount * 100)}%` }} />
                      </div>
                      <span className="font-mono text-xs tabular-nums text-muted w-12 text-right">{fmtInt(e.count)}</span>
                    </div>
                  ))}
                </div>
              </Card>
              <Card>
                <h3 className="text-sm font-semibold -tracking-tight">Speed</h3>
                <p className="text-xs text-muted mt-0.5">Response latency and generation rate</p>
                <div className="mt-4 space-y-4">
                  <MetricBar label="p50 latency" value={latency?.count ? `${latency.p50}ms` : '—'} share={(latency?.p50 ?? 0) / maxLatency} />
                  <MetricBar label="p95 latency" value={latency?.count ? `${p95}ms` : '—'} share={p95 / maxLatency} tone="amber" />
                  <MetricBar label="avg latency" value={latency?.count ? `${Math.round(latency.avg)}ms` : '—'} share={(latency?.avg ?? 0) / maxLatency} />
                  <div className="pt-2 border-t border-stone grid grid-cols-2 gap-3 text-xs">
                    <div><div className="text-muted">Avg TTFT</div><div className="font-mono tabular-nums mt-0.5">{currentStats.range_ttft_avg > 0 ? `${Math.round(currentStats.range_ttft_avg)}ms` : '—'}</div></div>
                    <div><div className="text-muted">Avg output rate</div><div className="font-mono tabular-nums mt-0.5">{currentStats.range_tps_avg > 0 ? `${currentStats.range_tps_avg.toFixed(1)} tok/s` : '—'}</div></div>
                  </div>
                </div>
              </Card>
              <Card>
                <h3 className="text-sm font-semibold -tracking-tight">Caching</h3>
                <p className="text-xs text-muted mt-0.5">Gateway responses and provider-side prompt tokens</p>
                <div className="mt-4 space-y-5">
                  <div>
                    <div className="flex items-baseline justify-between gap-3 text-xs"><span className="text-muted">Gateway response hits</span><span className="font-mono tabular-nums">{gwEligible ? fmtPct(gwRate) : '—'}</span></div>
                    <div className="mt-1.5"><Progress value={gwRate * 100} /></div>
                    <p className="text-[11px] text-muted mt-1.5">{fmtInt(gwHits)} hits · {fmtInt(gwMisses)} misses{gwBypassed > 0 ? ` · ${fmtInt(gwBypassed)} bypassed` : ''}. Hits skip the upstream call.</p>
                  </div>
                  <div className="pt-2 border-t border-stone">
                    <div className="flex items-baseline justify-between gap-3 text-xs"><span className="text-muted">Provider prompt cache</span><span className="font-mono tabular-nums">{promptTok > 0 ? fmtPct(promptCacheRate) : '—'}</span></div>
                    <div className="mt-1.5"><Progress value={promptCacheRate * 100} /></div>
                    <p className="text-[11px] text-muted mt-1.5">{fmtInt(promptCacheTok)} of {fmtInt(promptTok)} input tokens read from the provider's cache; provider rates may be discounted.</p>
                  </div>
                </div>
              </Card>
            </div>
          </Section>

          <Section id="analytics-contributors" title="Top contributors" hint="Model and key usage ranked by tokens">
            <div className="grid md:grid-cols-2 gap-3">
              <Card>
                <div className="flex items-center justify-between gap-2"><h3 className="text-sm font-semibold -tracking-tight">Models</h3><Badge tone="neutral">{currentStats.top_models.length}</Badge></div>
                <div className="mt-3 space-y-3">
                  {currentStats.top_models.map((m, i) => <div key={m.model} className="flex items-start gap-2">
                    <span className="text-xs text-muted w-4 tabular-nums">{i + 1}</span>
                    <div className="min-w-0 flex-1"><div className="flex flex-wrap items-baseline justify-between gap-1 text-xs"><span className="font-mono break-all">{m.model}</span><span className="font-mono text-muted">{fmtCompact(m.tokens)} tok · {fmtMoney(m.cost)}</span></div>
                      <div className="mt-1 h-1.5 rounded-full bg-app overflow-hidden"><div className="h-full bg-accent rounded-full" style={{ width: `${m.tokens / modelMax * 100}%` }} /></div></div>
                  </div>)}
                  {currentStats.top_models.length === 0 && <EmptyState icon="box" title="No model usage yet" />}
                </div>
              </Card>
              <Card>
                <div className="flex items-center justify-between gap-2"><h3 className="text-sm font-semibold -tracking-tight">Keys</h3><Badge tone="neutral">{currentStats.top_keys.length}</Badge></div>
                <div className="mt-3 space-y-3">
                  {currentStats.top_keys.map(k => <div key={k.key_prefix}>
                    <div className="flex flex-wrap items-baseline justify-between gap-1 text-xs"><span className="font-mono break-all" title={k.key_prefix}>{keyMap[k.key_prefix] || k.key_prefix}</span><span className="font-mono text-muted">{fmtCompact(k.tokens)} tok · {fmtMoney(k.cost)}</span></div>
                    <div className="mt-1 h-1.5 rounded-full bg-app overflow-hidden"><div className="h-full bg-accent rounded-full" style={{ width: `${k.tokens / keyMax * 100}%` }} /></div>
                  </div>)}
                  {currentStats.top_keys.length === 0 && <EmptyState icon="key" title="No key usage yet" />}
                </div>
              </Card>
            </div>
          </Section>
        </>
      ) : currentStats ? (
        <Card pad={false}><EmptyState icon="chart" title={`No analytics data in the last ${rangeLabel}`}
          hint={wider?.range === range && wider.count > 0 ? `${fmtInt(wider.count)} requests exist in the last 30 days.` : 'Requests will show up here once traffic flows through the gateway.'}
          action={wider?.range === range && wider.count > 0 ? <Button variant="primary" onClick={() => setRange('30d')}>Show last 30 days</Button> : undefined} /></Card>
      ) : null}

      <Section id="analytics-reports" title="Explore detailed reports" hint="Longer and custom windows, attribution, pricing, and upstream performance">
        <div className="grid gap-3 md:grid-cols-3">
          {[
            { to: '/costs', title: 'Costs', description: 'Spend, cache savings, and unpriced usage.', icon: 'wallet' as const },
            { to: '/performance', title: 'Performance', description: 'Provider latency, reliability, and stream health.', icon: 'pulse' as const },
            { to: '/attribution', title: 'Attribution', description: 'Usage by application and end user.', icon: 'layers' as const },
          ].map(report => <Link key={report.to} to={report.to} className="block rounded-lg border border-stone bg-surface p-4 hover:border-accent/50 hover:bg-raised/40 transition-colors">
            <span className="flex items-center gap-2 font-medium"><Icon name={report.icon} size={16} className="text-accent" />{report.title}</span>
            <span className="block text-xs text-muted mt-1">{report.description}</span>
          </Link>)}
        </div>
      </Section>
    </div>
  )
}
