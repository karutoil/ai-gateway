import { useEffect, useState, type ReactNode } from 'react'
import { Card, PageHeader, Button, Badge, Icon, SegmentedControl, EmptyState, Skeleton, Stat, Progress } from '../components/ui'

/* ------------------------------------------------------------------ */
/* Types                                                               */
/* ------------------------------------------------------------------ */

type Daily = { day: string; tokens: number; cost: number; requests: number; cache_hits?: number }
type TopModel = { model: string; tokens: number; cost: number; requests: number }
type TopKey = { key_prefix: string; tokens: number; cost: number; requests: number }
type ErrorRow = { status: number; count: number; sample?: string }
type Stats = {
  providers: number; keys: number; requests: number; total_tokens: number; total_cost: number
  range: string
  daily: Daily[]; top_models: TopModel[]; top_keys: TopKey[]
  latency: { p50: number; p95: number; avg: number; count: number }
  range_tokens: number; range_cost: number; range_requests: number
  range_successful?: number; range_failed?: number
  successful?: number; failed?: number
  range_ttft_avg?: number; range_tps_avg?: number
  ttft?: { avg: number }; tps?: { avg: number }
  errors?: ErrorRow[]
  cache_hit_rate?: number; cache_read_tokens?: number
  range_cache_hits?: number; gateway_cache_hit_rate?: number
  range_cache_eligible?: number; range_cache_bypassed?: number
  range_prompt_tokens?: number; range_completion_tokens?: number
}

type Range = '24h' | '7d' | '30d'
type Metric = 'requests' | 'tokens' | 'cost' | 'cache_hits'

/* ------------------------------------------------------------------ */
/* Data                                                                */
/* ------------------------------------------------------------------ */

async function fetchStats(range: Range): Promise<Stats> {
  const res = await fetch(`/api/stats?range=${range}`, { credentials: 'same-origin' })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

async function fetchKeyMap(): Promise<Record<string, string>> {
  try {
    const res = await fetch('/api/keys', { credentials: 'same-origin' })
    if (!res.ok) return {}
    const keys = await res.json()
    const m: Record<string, string> = {}
    for (const k of keys) m[k.prefix] = k.name
    return m
  } catch { return {} }
}

/* ------------------------------------------------------------------ */
/* Formatting helpers                                                  */
/* ------------------------------------------------------------------ */

const fmtInt = (n: number | null | undefined) => Number(n ?? 0).toLocaleString()

function fmtCompact(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 10_000) return `${(n / 1_000).toFixed(0)}K`
  return n.toLocaleString()
}

function fmtMoney(v: number): string {
  const n = Number(v ?? 0)
  if (n >= 100) return `$${n.toFixed(2)}`
  if (n === 0) return '$0.00'
  return `$${n.toFixed(4)}`
}

const fmtPct = (x: number) => `${(x * 100).toFixed(1)}%`
const statusTone = (s: number): 'good' | 'warn' | 'bad' => (s >= 500 ? 'bad' : s >= 400 ? 'warn' : 'good')

/* ------------------------------------------------------------------ */
/* Daily buckets                                                       */
/* ------------------------------------------------------------------ */

const RANGE_DAYS: Record<Range, number> = { '24h': 1, '7d': 7, '30d': 30 }

function isHourly(day: string): boolean { return day.includes('T') }
function bucketLabel(day: string): string {
  if (isHourly(day)) return `${day.slice(8, 10)} ${day.slice(11, 13)}:00`
  return day.slice(5)
}
function bucketWhen(day: string): string {
  return isHourly(day) ? day.slice(0, 13).replace('T', ' ') + ':00' : day
}

function zeroFillDaily(daily: Daily[], range: Range): Daily[] {
  const byKey = new Map(daily.map(d => [d.day, d]))
  const out: Daily[] = []
  const now = new Date()
  if (range === '24h') {
    for (let i = 23; i >= 0; i--) {
      const t = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), now.getUTCHours() - i))
      const key = t.toISOString().slice(0, 13) + ':00:00Z'
      out.push(byKey.get(key) ?? { day: key, tokens: 0, cost: 0, requests: 0, cache_hits: 0 })
    }
    return out
  }
  const days = RANGE_DAYS[range]
  for (let i = days - 1; i >= 0; i--) {
    const day = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i)).toISOString().slice(0, 10)
    out.push(byKey.get(day) ?? { day, tokens: 0, cost: 0, requests: 0, cache_hits: 0 })
  }
  return out
}

/** Vertical bars for one extracted metric, labeled per bucket. */
function DailyBars({ daily, extract, format, dim = false }: {
  daily: Daily[]; extract: (d: Daily) => number; format: (d: Daily) => string; dim?: boolean
}) {
  const max = Math.max(1e-9, ...daily.map(extract))
  return (
    <div className="flex items-end gap-1.5 overflow-x-auto pb-1">
      {daily.map(d => {
        const v = extract(d)
        const pct = v > 0 ? Math.max(4, Math.round((v / max) * 100)) : 2
        return (
          <div key={d.day} title={format(d)} className="flex-1 min-w-[26px] flex flex-col items-center gap-1.5 group cursor-default">
            <div className="w-full h-36 sm:h-44 flex items-end rounded-lg bg-app border border-stone p-1">
              <div className={`w-full rounded transition-opacity group-hover:opacity-100 ${v > 0 ? (dim ? 'bg-accent/60' : 'bg-accent opacity-90') : 'bg-stone/60'}`}
                style={{ height: `${pct}%` }} />
            </div>
            <span className="font-mono text-[9px] text-muted truncate w-full text-center">{bucketLabel(d.day)}</span>
          </div>
        )
      })}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Section header                                                      */
/* ------------------------------------------------------------------ */

function SectionHead({ title, hint, right }: { title: string; hint?: string; right?: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <div>
        <h3 className="font-display font-semibold tracking-tight">{title}</h3>
        {hint && <p className="text-[11px] text-muted mt-0.5">{hint}</p>}
      </div>
      {right}
    </div>
  )
}

/** One labeled metric row with a proportional bar (for Speed / Caching cards). */
function MetricBar({ label, value, share, mono = true, tone = 'accent' }: {
  label: string; value: string; share: number; mono?: boolean; tone?: 'accent' | 'amber'
}) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-muted">{label}</span>
        <span className={`${mono ? 'font-mono tabular-nums' : ''} text-paper font-medium`}>{value}</span>
      </div>
      <div className="mt-1.5"><Progress value={Math.min(100, share * 100)} tone={tone} /></div>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Page                                                                */
/* ------------------------------------------------------------------ */

const RANGES: { value: Range; label: string }[] = [{ value: '24h', label: '24h' }, { value: '7d', label: '7d' }, { value: '30d', label: '30d' }]
const METRICS: { value: Metric; label: string }[] = [
  { value: 'requests', label: 'Requests' },
  { value: 'tokens', label: 'Tokens' },
  { value: 'cost', label: 'Spend' },
  { value: 'cache_hits', label: 'Cache hits' },
]

const metricExtract: Record<Metric, (d: Daily) => number> = {
  requests: d => d.requests,
  tokens: d => d.tokens,
  cost: d => d.cost,
  cache_hits: d => d.cache_hits ?? 0,
}

export default function Analytics() {
  const [range, setRange] = useState<Range>('7d')
  const [metric, setMetric] = useState<Metric>('requests')
  const [stats, setStats] = useState<Stats | null>(null)
  const [keyMap, setKeyMap] = useState<Record<string, string>>({})
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => { fetchKeyMap().then(setKeyMap).catch(() => {}) }, [])
  useEffect(() => {
    setLoading(true)
    fetchStats(range).then(s => { setStats(s); setErr('') }).catch(e => setErr(String(e.message || e))).finally(() => setLoading(false))
  }, [range, reloadKey])

  const filledDaily = zeroFillDaily(stats?.daily ?? [], range)
  const rangeLabel = range === '24h' ? '24 hours' : range === '7d' ? '7 days' : '30 days'

  // Derived KPI values (guarded against divide-by-zero on quiet windows).
  const requests = stats?.range_requests ?? 0
  const ok = stats?.range_successful ?? 0
  const fail = stats?.range_failed ?? 0
  const failRate = requests > 0 ? fail / requests : 0
  const okRate = requests > 0 ? ok / requests : 0
  const tokens = stats?.range_tokens ?? 0
  const promptTok = stats?.range_prompt_tokens ?? 0
  const completionTok = stats?.range_completion_tokens ?? 0
  const cost = stats?.range_cost ?? 0
  const costPer1M = tokens > 0 ? (cost / tokens) * 1_000_000 : 0
  const tokPerReq = requests > 0 ? tokens / requests : 0
  const gwHits = stats?.range_cache_hits ?? 0
  const gwEligible = stats?.range_cache_eligible ?? 0
  const gwBypassed = stats?.range_cache_bypassed ?? 0
  const gwMisses = Math.max(0, gwEligible - gwHits)
  const gwRate = stats?.gateway_cache_hit_rate ?? 0
  const promptCacheRate = stats?.cache_hit_rate ?? 0
  const promptCacheTok = stats?.cache_read_tokens ?? 0

  // Wider-window probe: when the selected window is empty, check the last
  // 30 days once. If traffic exists there, the window — not the pipeline —
  // is the cause, and a note offers the one-click widen.
  const [wider, setWider] = useState<number | null>(null)
  useEffect(() => {
    setWider(null)
    if (loading || err || !stats || stats.range_requests > 0 || range === '30d') return
    let cancelled = false
    fetchStats('30d')
      .then(s => { if (!cancelled) setWider(s.range_requests) })
      .catch(() => { if (!cancelled) setWider(null) })
    return () => { cancelled = true }
  }, [loading, err, stats, range])

  const errors = stats?.errors ?? []
  const maxErrorCount = Math.max(1, ...errors.map(e => e.count))
  const latency = stats?.latency
  const p95 = latency?.p95 ?? 0
  const maxLatency = Math.max(p95, latency?.p50 ?? 0, latency?.avg ?? 0, 1)

  const totalTokens = stats?.total_tokens ?? 0
  const modelShare = (m: TopModel) => (totalTokens > 0 ? m.tokens / totalTokens : 0)
  const topModelMax = Math.max(1, ...(stats?.top_models ?? []).map(m => m.tokens))
  const topKeyMax = Math.max(1, ...(stats?.top_keys ?? []).map(k => k.tokens))

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Optimize · Spend"
        title="Analytics"
        description="Traffic, spend, reliability and latency for the selected window — top to bottom."
        actions={
          <div className="flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={() => setReloadKey(k => k + 1)} disabled={loading} title="Reload analytics">
              <Icon name="refresh" size={14} className={loading ? 'animate-spin' : ''}/> Refresh
            </Button>
            <SegmentedControl options={RANGES} value={range} onChange={setRange} />
          </div>
        }
      />

      {err && (
        <Card>
          <EmptyState icon="alert" title="Could not load analytics" hint={err}
            action={<Button variant="secondary" onClick={() => setReloadKey(k => k + 1)}><Icon name="refresh" size={15} />Retry</Button>} />
        </Card>
      )}

      {loading ? (
        <div className="space-y-4">
          <div className="grid grid-cols-2 xl:grid-cols-4 gap-3">
            {Array.from({ length: 4 }).map((_, i) => (
              <Card key={i} className="flex items-start gap-3">
                <Skeleton className="w-10 h-10 shrink-0 !rounded-xl" />
                <div className="flex-1 space-y-1.5"><Skeleton className="h-3 w-14" /><Skeleton className="h-6 w-full" /></div>
              </Card>
            ))}
          </div>
          <Card><Skeleton className="h-56 w-full" /></Card>
          <div className="grid lg:grid-cols-3 gap-3">
            {Array.from({ length: 3 }).map((_, i) => <Card key={i}><Skeleton className="h-40 w-full" /></Card>)}
          </div>
        </div>
      ) : stats && requests > 0 ? (
        <>
          {wider != null && wider > 0 && (
            <Card className="border-accent/30 bg-accent/10 flex flex-wrap items-center justify-between gap-3">
              <span className="text-sm text-paper">
                No activity in the last {rangeLabel} — but <b className="tabular-nums">{wider.toLocaleString()} requests</b> exist
                in the last 30 days.
              </span>
              <Button variant="secondary" size="sm" onClick={() => setRange('30d')}>Show 30 days</Button>
            </Card>
          )}

          {/* ---------- Overview KPIs ---------- */}
          <div className="grid grid-cols-2 xl:grid-cols-4 gap-3">
            <Stat
              icon="pulse" title="Requests"
              value={fmtInt(requests)}
              sub={`${fmtInt(ok)} succeeded · ${fmtInt(fail)} failed`}
              tone={failRate === 0 ? 'good' : failRate > 0.05 ? 'bad' : 'warn'}
            />
            <Stat
              icon="layers" title="Tokens"
              value={fmtCompact(tokens)}
              sub={promptTok || completionTok ? `${fmtCompact(promptTok)} in · ${fmtCompact(completionTok)} out` : 'no usage reported'}
              delta={tokPerReq ? `≈${Math.round(tokPerReq)}/req` : undefined}
            />
            <Stat
              icon="wallet" title="Spend"
              value={fmtMoney(cost)}
              sub={costPer1M ? `${fmtMoney(costPer1M)} per 1M tokens` : 'no tokens to price'}
            />
            <Stat
              icon="zap" title="Cache hits"
              value={gwRate ? fmtPct(gwRate) : '0%'}
              sub={gwEligible
                ? `${fmtInt(gwHits)} of ${fmtInt(gwEligible)} eligible served without an upstream call`
                : `${fmtInt(gwHits)} served without an upstream call`}
              tone={gwHits > 0 ? 'good' : 'neutral'}
            />
          </div>

          {/* ---------- Traffic over time ---------- */}
          <Card>
            <SectionHead
              title="Traffic"
              hint={`Per ${range === '24h' ? 'hour' : 'day'} · last ${rangeLabel} · hover bars for detail`}
              right={<SegmentedControl options={METRICS} value={metric} onChange={setMetric} />}
            />
            <div className="mt-4">
              <DailyBars
                daily={filledDaily}
                extract={metricExtract[metric]}
                dim={metric === 'cost'}
                format={d => `${bucketWhen(d.day)} — ${
                  metric === 'cost' ? fmtMoney(d.cost)
                  : metric === 'tokens' ? `${fmtInt(d.tokens)} tokens`
                  : metric === 'cache_hits' ? `${fmtInt(d.cache_hits ?? 0)} cache hits · ${fmtInt(d.requests)} requests`
                  : `${fmtInt(d.requests)} requests`
                }`}
              />
            </div>
            <div className="mt-4 pt-3 border-t border-stone grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
              {([
                ['Requests', fmtInt(requests)],
                ['Tokens', fmtInt(tokens)],
                ['Spend', fmtMoney(cost)],
                ['Cache hits', fmtInt(gwHits)],
              ] as [string, string][]).map(([label, value]) => (
                <div key={label} className="flex items-baseline justify-between sm:justify-start sm:gap-2 border-stone">
                  <span className="text-muted">{label}</span>
                  <span className="font-mono tabular-nums font-medium">{value}</span>
                </div>
              ))}
            </div>
          </Card>

          {/* ---------- Quality: reliability / speed / caching ---------- */}
          <div className="grid lg:grid-cols-3 gap-4">
            <Card>
              <SectionHead title="Reliability" hint={`${fmtPct(okRate)} success across ${fmtInt(requests)} requests`} />
              <div className="mt-3"><Progress value={okRate * 100} tone={okRate > 0.95 ? 'accent' : okRate > 0.85 ? 'amber' : 'red'} /></div>
              <div className="mt-2.5 flex items-center gap-2">
                <Badge tone="good">{fmtInt(ok)} ok</Badge>
                <Badge tone="bad">{fmtInt(fail)} failed</Badge>
              </div>
              <div className="mt-4 space-y-2.5">
                {errors.length === 0 ? (
                  <div className="flex items-center gap-2 text-xs text-muted border border-stone rounded-lg px-3 py-2.5">
                    <Icon name="check" size={13} className="text-accent" /> No failed requests in this window.
                  </div>
                ) : errors.map(e => (
                  <div key={e.status} className="flex items-center gap-3">
                    <Badge tone={statusTone(e.status)}>{e.status}</Badge>
                    <div className="flex-1 h-2.5 rounded-full bg-app border border-stone overflow-hidden">
                      <div className="h-full bg-danger/70 rounded-full" style={{ width: `${Math.max(3, Math.round(e.count / maxErrorCount * 100))}%` }} />
                    </div>
                    <span className="font-mono text-xs tabular-nums text-muted w-12 text-right">{e.count.toLocaleString()}</span>
                    {e.sample && <span className="hidden xl:block text-[11px] text-muted truncate max-w-[160px] font-mono" title={e.sample}>{e.sample}</span>}
                  </div>
                ))}
              </div>
            </Card>

            <Card>
              <SectionHead title="Speed" hint="Latency to full response, first byte, and generation rate" />
              <div className="mt-4 space-y-4">
                <MetricBar label="p50 latency" value={`${latency?.p50 ?? 0}ms`} share={(latency?.p50 ?? 0) / maxLatency} />
                <MetricBar label="p95 latency" value={`${p95}ms`} share={p95 / maxLatency} tone="amber" />
                <MetricBar label="avg latency" value={`${Math.round(latency?.avg ?? 0)}ms`} share={(latency?.avg ?? 0) / maxLatency} />
                <div className="pt-1 border-t border-stone grid grid-cols-2 gap-3 text-xs">
                  <div>
                    <div className="text-muted">Avg TTFT</div>
                    <div className="font-mono tabular-nums text-paper font-medium mt-0.5">
                      {stats?.range_ttft_avg ? `${Math.round(stats.range_ttft_avg)}ms` : '—'}
                    </div>
                  </div>
                  <div>
                    <div className="text-muted">Avg throughput</div>
                    <div className="font-mono tabular-nums text-paper font-medium mt-0.5">
                      {stats?.range_tps_avg ? `${stats.range_tps_avg.toFixed(1)} tok/s` : '—'}
                    </div>
                  </div>
                </div>
              </div>
            </Card>

            <Card>
              <SectionHead title="Caching" hint="Two caches, two jobs — gateway responses and upstream prompts" />
              <div className="mt-4 space-y-5">
                <div>
                  <div className="flex items-baseline justify-between gap-3 text-xs">
                    <span className="text-muted">Gateway response cache</span>
                    <span className="font-mono tabular-nums text-paper font-medium">{gwEligible ? fmtPct(gwRate) : '—'}</span>
                  </div>
                  <div className="mt-1.5"><Progress value={gwRate * 100} tone={gwHits > 0 ? 'accent' : 'amber'} /></div>
                  <p className="text-[11px] text-muted mt-1.5">
                    {fmtInt(gwHits)} hit{gwHits === 1 ? '' : 's'} · {fmtInt(gwMisses)} miss{gwMisses === 1 ? '' : 'es'}
                    {gwBypassed > 0 && <> · {fmtInt(gwBypassed)} bypassed (not cache-eligible)</>}
                    {gwHits > 0 && <> — hits skip the upstream call and the token spend entirely.</>}
                  </p>
                </div>
                <div className="pt-1 border-t border-stone">
                  <div className="flex items-baseline justify-between gap-3 text-xs">
                    <span className="text-muted">Upstream prompt cache</span>
                    <span className="font-mono tabular-nums text-paper font-medium">{promptCacheTok ? fmtPct(promptCacheRate) : '—'}</span>
                  </div>
                  <div className="mt-1.5"><Progress value={promptCacheRate * 100} tone={promptCacheTok > 0 ? 'accent' : 'amber'} /></div>
                  <p className="text-[11px] text-muted mt-1.5">
                    {fmtInt(promptCacheTok)} prompt tokens read from provider KV cache — billed at reduced cache-read rates.
                  </p>
                </div>
              </div>
            </Card>
          </div>

          {/* ---------- Leaderboards ---------- */}
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <Card>
              <SectionHead title="Top models" hint={`By tokens · ${rangeLabel}`} right={<Badge tone="neutral">{stats.top_models.length}</Badge>} />
              <div className="mt-3 space-y-2">
                {stats.top_models.map((m, i) => (
                  <div key={m.model} className="flex items-center gap-3">
                    <span className="w-5 h-5 rounded-md bg-raised border border-stone/60 text-[10px] flex items-center justify-center font-bold text-muted shrink-0">{i + 1}</span>
                    <div className="min-w-0 flex-1">
                      <div className="flex items-baseline justify-between gap-3">
                        <span className="font-mono text-xs truncate" title={m.model}>{m.model}</span>
                        <span className="font-mono text-xs tabular-nums text-muted shrink-0">{fmtCompact(m.tokens)} tok · {fmtMoney(m.cost)}</span>
                      </div>
                      <div className="mt-1 h-1.5 rounded-full bg-app border border-stone overflow-hidden">
                        <div className="h-full bg-accent/80 rounded-full" style={{ width: `${Math.max(3, Math.round((m.tokens / topModelMax) * 100))}%` }} />
                      </div>
                    </div>
                  </div>
                ))}
                {stats.top_models.length === 0 && <EmptyState icon="box" title="No model usage yet" />}
              </div>
            </Card>

            <Card>
              <SectionHead title="Top keys" hint={`By tokens · ${rangeLabel}`} right={<Badge tone="neutral">{stats.top_keys.length}</Badge>} />
              <div className="mt-3 space-y-2">
                {stats.top_keys.map(k => (
                  <div key={k.key_prefix} className="flex items-center gap-3">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-baseline justify-between gap-3">
                        <span className="font-mono text-xs truncate" title={keyMap[k.key_prefix] ? `${keyMap[k.key_prefix]} (${k.key_prefix})` : k.key_prefix}>
                          {keyMap[k.key_prefix] || k.key_prefix}
                        </span>
                        <span className="font-mono text-xs tabular-nums text-muted shrink-0">{fmtCompact(k.tokens)} tok · {fmtMoney(k.cost)}</span>
                      </div>
                      <div className="mt-1 h-1.5 rounded-full bg-app border border-stone overflow-hidden">
                        <div className="h-full bg-accent/80 rounded-full" style={{ width: `${Math.max(3, Math.round((k.tokens / topKeyMax) * 100))}%` }} />
                      </div>
                    </div>
                  </div>
                ))}
                {stats.top_keys.length === 0 && <EmptyState icon="key" title="No key usage yet" />}
              </div>
            </Card>
          </div>

          <p className="text-[11px] text-muted/70 text-right font-mono">
            window {range} · {fmtInt(requests)} requests · estimated spend {fmtMoney(cost)}
          </p>
        </>
      ) : stats ? (
        <Card pad={false}>
          <EmptyState
            icon="chart"
            title={`No analytics data in the last ${rangeLabel}`}
            hint={wider ? 'Traffic exists in the last 30 days — widen the window above.' : 'Requests will show up here once traffic flows through the gateway.'}
            action={wider ? <Button variant="primary" onClick={() => setRange('30d')}>Show last 30 days</Button> : undefined}
          />
        </Card>
      ) : null}
    </div>
  )
}
