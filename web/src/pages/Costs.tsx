import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, type CostReport, type CostPoint, type AnalyticsWindow, type RangeKey } from '../lib/api'
import { PageHeader, Button, Icon, Card, ErrorNote, TableShell, TableSkeleton, EmptyState, Badge, SegmentedControl } from '../components/ui'
import {
  RangePicker, BreakdownTable, Metric, Panel, BarSeries, useSort, sortRows,
  fmtInt, fmtCompact, fmtMoney, bucketLabel, bucketWhen, TONES,
} from '../components/report'

/** Resolve a bucket's exact UTC instant; daily keys are dates, hourly keys are ISO timestamps. */
function bucketTime(day: unknown): number {
  if (typeof day !== 'string' || !/^\d{4}-\d{2}-\d{2}(?:T\d{2}(?::\d{2})?(?::\d{2})?(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?)?$/.test(day)) return NaN
  return Date.parse(day.includes('T') && !/(?:Z|[+-]\d{2}:?\d{2})$/.test(day) ? `${day}Z` : day)
}

function graphWindow(points: CostPoint[], window: 'all' | '24h' | '7d' | '30d', end: string): CostPoint[] {
  const until = Date.parse(end)
  if (window === 'all' || !Number.isFinite(until)) return points
  const duration = { '24h': 24, '7d': 24 * 7, '30d': 24 * 30 }[window] * 3_600_000
  return points.filter(point => {
    const at = bucketTime(point.day)
    // Include any daily bucket intersecting the time window; hourly buckets
    // are selected by hour. Never compare YYYY-MM-DD strings to full timestamps.
    const bucketDuration = point.day?.includes('T') ? 3_600_000 : 86_400_000
    return Number.isFinite(at) && at < until && at + bucketDuration > until - duration
  })
}

/** Chart viewport options; independent of the report window above. */
const GRAPH_WINDOWS: { value: 'all' | '24h' | '7d' | '30d'; label: string }[] = [
  { value: 'all', label: 'Full window' },
  { value: '24h', label: 'Last 24h' },
  { value: '7d', label: 'Last 7d' },
  { value: '30d', label: 'Last 30d' },
]

/** Spend and cache savings share an API window; the chart also has its own viewport. */
export default function Costs() {
  const navigate = useNavigate()
  const [range, setRange] = useState<RangeKey>('30d')
  const [custom, setCustom] = useState<{ from: string; to: string } | null>(null)
  const [graphRange, setGraphRange] = useState<'all' | '24h' | '7d' | '30d'>('all')
  const [reload, setReload] = useState(0)
  const [data, setData] = useState<CostReport | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  const modelSort = useSort({ key: 'cost', dir: 'desc' })
  const provSort = useSort({ key: 'cost', dir: 'desc' })
  const appSort = useSort({ key: 'cost', dir: 'desc' })
  const clientSort = useSort({ key: 'cost', dir: 'desc' })
  const keySort = useSort({ key: 'cost', dir: 'desc' })
  const cacheSort = useSort({ key: 'cost', dir: 'desc' })

  const win: AnalyticsWindow = custom ? { from: custom.from, to: custom.to } : { range }
  useEffect(() => {
    let cancelled = false
    setLoading(true); setErr(''); setData(null)
    api.analytics.cost(win)
      .then(d => { if (!cancelled) setData(d) })
      .catch((e: unknown) => { if (!cancelled) setErr(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range, custom?.from, custom?.to, reload])

  const t = data?.totals
  const byModel = useMemo(() => sortRows(data?.by_model ?? [], modelSort.sort), [data, modelSort.sort])
  const byProvider = useMemo(() => sortRows(data?.by_provider ?? [], provSort.sort), [data, provSort.sort])
  const byApp = useMemo(() => sortRows(data?.by_app ?? [], appSort.sort), [data, appSort.sort])
  const byClient = useMemo(() => sortRows(data?.by_client ?? [], clientSort.sort), [data, clientSort.sort])
  const byKey = useMemo(() => sortRows(data?.by_key ?? [], keySort.sort), [data, keySort.sort])
  const byCache = useMemo(() => sortRows(data?.by_cache_status ?? [], cacheSort.sort), [data, cacheSort.sort])
  const graph = useMemo(() => graphWindow(data?.daily ?? [], graphRange, data?.to ?? ''), [data, graphRange])
  const graphSpend = graph.reduce((sum, point) => sum + (Number(point.cost) || 0), 0)
  const graphSavings = graph.reduce((sum, point) => sum + (Number(point.cache_savings) || 0), 0)

  const totalInput = t?.prompt_tokens ?? 0
  const cacheShare = totalInput > 0 ? (t?.cache_read_tokens ?? 0) / totalInput : 0
  const savingsShare = t && t.cost_usd + t.cache_savings_usd > 0
    ? t.cache_savings_usd / (t.cost_usd + t.cache_savings_usd) : 0
  const freshInput = Math.max(0, totalInput - (t?.cache_read_tokens ?? 0) - (t?.cache_write_tokens ?? 0))
  const tokenMix = [
    { label: 'Fresh input', value: freshInput, tone: TONES.tokens, note: 'standard input rate' },
    { label: 'Cache read', value: t?.cache_read_tokens ?? 0, tone: TONES.cache, note: 'discounted cache-read rate' },
    { label: 'Cache write', value: t?.cache_write_tokens ?? 0, tone: TONES.latency, note: 'cache-write rate' },
    { label: 'Completion', value: t?.completion_tokens ?? 0, tone: TONES.cost, note: 'output rate' },
  ]

  const go = (params: Record<string, string>) => {
    // Preserve the report's exact start for custom as well as preset windows;
    // using a new relative duration could drift from the viewed report.
    const q = new URLSearchParams(params)
    if (data?.from) q.set('since', data.from)
    navigate(`/logs?${q.toString()}`)
  }

  const columns = [
    { key: 'requests', label: 'Requests', render: (r: any) => fmtInt(r.requests) },
    { key: 'tokens', label: 'Tokens', render: (r: any) => fmtCompact(r.tokens) },
    { key: 'cost', label: 'Cost', render: (r: any) => fmtMoney(r.cost) },
  ]

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Optimize · Cost"
        title="Spend & cache savings"
        description="Track billed spend, quantify cache savings, and find the models and applications driving cost."
        actions={
          <Button variant="secondary" size="sm" onClick={() => setReload(n => n + 1)} disabled={loading} title="Reload cost report">
            <Icon name="refresh" size={14}/> Refresh
          </Button>
        }
      />

      <RangePicker value={range} onChange={setRange} custom={custom} onCustom={setCustom} />
      {err && <ErrorNote message={err} />}
      {loading ? (
        <div className="grid gap-3 lg:grid-cols-2">
          {/* TableSkeleton renders <tbody> rows — it must sit inside TableShell's table. */}
          <TableShell><TableSkeleton rows={5} cols={4} /></TableShell>
          <TableShell><TableSkeleton rows={5} cols={4} /></TableShell>
        </div>
      ) : (
        <>
          {!data && <EmptyState icon="wallet" title="No cost report available" hint="Choose another window or retry loading the report." />}
          {data && <>
          <section aria-label="Spend overview" className="space-y-3">
            <h2 className="text-[15px] font-semibold -tracking-tight text-paper">Spend overview</h2>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <Metric label="Billed spend" value={fmtMoney(t?.cost_usd)} sub={data ? `${data.range} reporting window` : ''} icon="wallet" />
              <Metric label="Cache savings" value={fmtMoney(t?.cache_savings_usd)}
                sub={t ? `${(savingsShare * 100).toFixed(1)}% less than without the cache discount` : ''}
                tone={(t?.cache_savings_usd ?? 0) > 0 ? 'good' : 'neutral'} icon="zap" />
              <Metric label="Cache-read share" value={totalInput > 0 ? `${(cacheShare * 100).toFixed(1)}%` : '—'}
                sub="of recorded input tokens" icon="layers" />
              <Metric label="Zero-cost traffic" value={fmtInt(t?.unpriced_requests)}
                sub={t ? `${fmtCompact(t.unpriced_tokens)} tokens with no recorded cost` : ''}
                tone={(t?.unpriced_requests ?? 0) > 0 ? 'warn' : 'neutral'} icon="alert" />
            </div>
          </section>

          <section aria-label="Spend trend" className="space-y-3">
            <h2 className="text-[15px] font-semibold -tracking-tight text-paper">Spend trend</h2>
            <Panel title="Spend over time" hint="Billed cost and the estimated input-cache discount; chart viewport does not change totals above."
              right={<Badge tone="neutral">{graph.length} buckets</Badge>}>
              <div className="flex flex-wrap items-center gap-2 mb-3" aria-label="Chart time filter">
                <span className="text-xs text-muted mr-1">Show</span>
                <SegmentedControl options={GRAPH_WINDOWS} value={graphRange} onChange={setGraphRange} />
              </div>
              <BarSeries data={graph} labelOf={(point) => bucketLabel(point.day)}
                series={[
                  { key: 'cost', label: 'Billed cost', tone: TONES.cost },
                  { key: 'cache_savings', label: 'Cache savings', tone: TONES.cache },
                ]} format={fmtMoney} />
              {graph.length > 0 && <div className="flex flex-wrap gap-x-5 gap-y-1 border-t border-stone mt-3 pt-3 text-xs text-muted">
                <span>Displayed billed <strong className="font-mono font-medium text-paper">{fmtMoney(graphSpend)}</strong></span>
                <span>Displayed savings <strong className="font-mono font-medium text-paper">{fmtMoney(graphSavings)}</strong></span>
                <span>{bucketWhen(graph[0].day)} → {bucketWhen(graph[graph.length - 1].day)}</span>
              </div>}
            </Panel>
          </section>

          <section aria-label="Caching and pricing" className="space-y-3">
            <h2 className="text-[15px] font-semibold -tracking-tight text-paper">Caching & pricing</h2>
            <div className="grid gap-3 lg:grid-cols-2">
              <Panel title="Token mix" hint="Input buckets and separately billed output tokens.">
                <div className="space-y-2.5">
                  {tokenMix.map(row => {
                    const max = Math.max(1, totalInput, t?.completion_tokens ?? 0)
                    return (
                      <div key={row.label}>
                        <div className="flex items-baseline justify-between text-sm">
                          <span>{row.label}</span>
                          <span className="tabular-nums font-mono text-xs">{fmtCompact(row.value)}</span>
                        </div>
                        <div className="h-1.5 mt-1 rounded-full bg-raised overflow-hidden">
                          <div className="h-full rounded-full" style={{ width: `${row.value > 0 ? Math.max(1, row.value / max * 100) : 0}%`, backgroundColor: row.tone }} />
                        </div>
                        <div className="text-[11px] text-muted mt-0.5">{row.note}</div>
                      </div>
                    )
                  })}
                </div>
                <p className="text-xs text-muted mt-4 pt-3 border-t border-stone">
                  Reasoning tokens: <span className="font-mono text-paper">{fmtCompact(t?.reasoning_tokens)}</span> (included in completion, not added again).
                  Cache reads: <span className="font-mono text-paper">{fmtCompact(t?.cache_read_tokens)}</span> input tokens.
                </p>
              </Panel>
              <Panel title="Response-cache disposition" hint="Request-level response-cache outcomes; distinct from input-token cache reads.">
                <BreakdownTable rows={byCache} sort={cacheSort.sort} onSort={cacheSort.toggle}
                  nameHeader="Disposition" columns={columns} emptyHint="No cache data in this window." />
              </Panel>
            </div>
          </section>

          <section aria-label="Cost drivers" className="space-y-3">
            <h2 className="text-[15px] font-semibold -tracking-tight text-paper">Cost drivers</h2>
            <div className="grid gap-3 lg:grid-cols-2">
              <Panel title="Spend by model" hint="Click a model to inspect its requests.">
                <BreakdownTable rows={byModel} sort={modelSort.sort} onSort={modelSort.toggle}
                  nameHeader="Model" columns={columns} maxRows={15}
                  onRowClick={(r) => go({ model: r.name })} />
              </Panel>
              <Panel title="Spend by provider" hint="Upstream cost concentration and failover impact.">
                <BreakdownTable rows={byProvider} sort={provSort.sort} onSort={provSort.toggle}
                  nameHeader="Provider" columns={columns} maxRows={15} />
              </Panel>
            </div>
          </section>

          <section aria-label="Cost attribution" className="space-y-3">
            <h2 className="text-[15px] font-semibold -tracking-tight text-paper">Cost attribution</h2>
            <div className="grid gap-3 lg:grid-cols-2">
              <Panel title="Spend by application" hint="Chargeback view of apps that declared themselves; click one to inspect its requests.">
                {byApp.length === 0 ? (
                  <EmptyState icon="globe" title="No attributed spend"
                    hint="Attribute requests with X-App-Id, X-Title or HTTP-Referer to see per-application cost. Spend is still broken down by client below." />
                ) : (
                  <BreakdownTable rows={byApp} sort={appSort.sort} onSort={appSort.toggle}
                    nameHeader="Application" columns={columns} maxRows={15}
                    onRowClick={(r) => { if (r.name !== '(unattributed)') go({ app: r.name }) }} />
                )}
              </Panel>
              <Panel title="Spend by client" hint="The harness or SDK that called, from its User-Agent. Click one to inspect its requests.">
                <BreakdownTable rows={byClient} sort={clientSort.sort} onSort={clientSort.toggle}
                  nameHeader="Client" columns={columns} maxRows={15}
                  onRowClick={(r) => { if (r.name !== '(unattributed)') go({ client: r.name }) }} />
              </Panel>
              <Panel title="Spend by key" hint="Which virtual credential is driving cost.">
                <BreakdownTable rows={byKey} sort={keySort.sort} onSort={keySort.toggle}
                  nameHeader="Key prefix" columns={columns} maxRows={15} />
              </Panel>
            </div>
          </section>
          </>}
        </>
      )}
    </div>
  )
}
