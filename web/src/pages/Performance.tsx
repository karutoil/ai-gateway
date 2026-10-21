import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, type PerformanceReport, type PerfRow, type AnalyticsWindow, type RangeKey } from '../lib/api'
import { PageHeader, Button, Card, ErrorNote, TableShell, TableSkeleton, EmptyState, Badge, HealthDot, Icon } from '../components/ui'
import {
  RangePicker, Panel, Metric, BarSeries, LineSeries, useSort, sortRows,
  SortTh, fmtInt, fmtMs, bucketLabel, TONES,
} from '../components/report'
import { performanceLogsUrl, performancePoints, performanceSummary, problemRows } from './performanceView'

/**
 * How the upstreams actually behaved.
 *
 * Everything here is derived from request_logs, which the gateway has always
 * recorded — latency, TTFT, status, tokens — but never aggregated. Uptime is
 * the success RATIO over the window, not the point-in-time health flag, so a
 * provider that flapped for an hour is visible rather than averaged away.
 */
export default function Performance() {
  const navigate = useNavigate()
  const [range, setRange] = useState<RangeKey>('7d')
  const [custom, setCustom] = useState<{ from: string; to: string } | null>(null)
  const [data, setData] = useState<PerformanceReport | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [tab, setTab] = useState<'providers' | 'models'>('providers')
  const [onlyProblems, setOnlyProblems] = useState(false)
  // Bumped by the Refresh button. Re-running the fetch needs a dependency that
  // changes; clearing `data` alone left the page blank and never refetched.
  const [reload, setReload] = useState(0)

  const sort = useSort({ key: 'requests', dir: 'desc' })
  const win: AnalyticsWindow = custom ? { from: custom.from, to: custom.to } : { range }

  useEffect(() => {
    let cancelled = false
    setLoading(true); setErr('')
    api.analytics.performance(win)
      .then(d => { if (!cancelled) setData(d) })
      .catch((e: any) => { if (!cancelled) setErr(e?.message || String(e)) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range, custom?.from, custom?.to, reload])

  const base: PerfRow[] = tab === 'providers' ? (data?.providers ?? []) : (data?.models ?? [])
  const rows = useMemo(() => sortRows(problemRows(base, onlyProblems), sort.sort), [base, onlyProblems, sort.sort])
  // The endpoint caps rankings at 50, so fleet-wide requests and failures must
  // come from the uncapped time buckets, not the provider/model table.
  const points = useMemo(() => performancePoints(data?.daily ?? []), [data])
  const requests = points.reduce((sum, point) => sum + point.requests, 0)
  const failed = points.reduce((sum, point) => sum + point.failed, 0)
  const successPct = requests > 0 ? (1 - failed / requests) * 100 : null
  const summary = performanceSummary(base)
  const truncated = base.length >= 50

  const go = (model?: string) => {
    if (data) navigate(performanceLogsUrl(data, model))
  }

  if (loading && !data) {
    return (
      <div className="space-y-6">
        <PageHeader eyebrow="Operate · Performance" title="Upstream performance" />
        <TableShell><TableSkeleton rows={6} cols={8} /></TableShell>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Operate · Performance"
        title="Upstream performance"
        description="Uptime, latency percentiles, time-to-first-token and generation speed per provider and model, measured from your own traffic."
        actions={
          <Button variant="secondary" size="sm" onClick={() => setReload(n => n + 1)}>
            <Icon name="refresh" size={14}/> Refresh
          </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-2">
        <RangePicker value={range} onChange={setRange} custom={custom} onCustom={setCustom} />
        <span className="text-xs text-muted ml-auto" role="status">
          {loading ? 'Updating…' : data ? `Window ${new Date(data.from).toLocaleString()} – ${new Date(data.to).toLocaleString()}` : 'No report loaded'}
        </span>
      </div>

      {err && <ErrorNote message={err} />}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Metric label="Requests" value={data ? fmtInt(requests) : '—'} sub="All requests in this window" icon="pulse" />
        <Metric
          label="Success rate"
          value={successPct == null ? '—' : `${successPct.toFixed(2)}%`}
          sub={data ? `${fmtInt(failed)} failed requests` : 'No data'}
          tone={successPct == null ? 'neutral' : successPct >= 99 ? 'good' : successPct >= 95 ? 'warn' : 'bad'}
          icon="check"
        />
        <Metric
          label="Slowest p95"
          value={summary.slowest ? fmtMs(summary.slowest.p95_ms) : '—'}
          sub={summary.slowest ? `${tab === 'providers' ? 'Provider' : 'Model'}: ${summary.slowest.name}` : 'No measured traffic'}
          tone={(summary.slowest?.p95_ms ?? 0) > 10000 ? 'warn' : 'neutral'}
          icon="clock"
        />
        <Metric
          label={tab === 'providers' ? 'Providers ranked' : 'Models ranked'}
          value={fmtInt(base.length)}
          sub={truncated ? 'Top 50 by request volume' : 'With traffic in this window'}
          icon="server"
        />
      </div>

      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="font-display text-[15px] font-semibold -tracking-tight text-paper">Traffic over time</h2>
          <p className="text-xs text-muted mt-1">Buckets are hourly for short windows and daily for longer windows (UTC).</p>
        </div>
        <Badge tone="neutral">{points.length} buckets</Badge>
      </div>
      <Panel title="Requests & failures" hint="Failed requests are part of the request total, not additional traffic.">
        <BarSeries
          data={points}
          labelOf={(d) => bucketLabel(d.day)}
          series={[
            { key: 'requests', label: 'Requests', tone: TONES.cost },
            { key: 'failed', label: 'Failed', tone: TONES.error },
          ]}
          format={fmtInt}
        />
      </Panel>

      <div className="grid gap-4 lg:grid-cols-2">
        <Panel title="Mean latency" hint="Average end-to-end response time per bucket.">
          <LineSeries data={points} valueOf={(d) => d.avg_ms}
            labelOf={(d) => bucketLabel(d.day)} format={fmtMs} tone={TONES.latency} />
        </Panel>
        <Panel title="Time to first token" hint="Streaming requests only — buffered responses have no first-token moment.">
          <LineSeries data={points} valueOf={(d) => d.avg_ttft_ms}
            labelOf={(d) => bucketLabel(d.day)} format={fmtMs} tone={TONES.cache} />
        </Panel>
      </div>

      <div>
        <h2 className="font-display text-[15px] font-semibold -tracking-tight text-paper">Compare upstreams</h2>
        <p className="text-xs text-muted mt-1">Ranked by traffic in the selected window; change the dimension and sort to find regressions.</p>
      </div>
      <Card pad={false}>
        <div className="flex flex-wrap items-center gap-2 p-4 border-b border-stone">
          <div className="inline-flex rounded-lg border border-stone bg-raised p-0.5 gap-0.5" role="group" aria-label="Ranking dimension">
            {(['providers', 'models'] as const).map(t => (
              <button key={t} type="button" aria-pressed={tab === t} onClick={() => setTab(t)}
                className={`px-3 h-7 rounded-md text-xs font-medium capitalize transition-colors duration-150 ${
                  tab === t ? 'bg-ink text-cream shadow-sm' : 'text-muted hover:text-paper'
                }`}>
                {t}
              </button>
            ))}
          </div>
          <label className="inline-flex items-center gap-2 text-xs text-muted cursor-pointer select-none">
            <input type="checkbox" checked={onlyProblems} onChange={e => setOnlyProblems(e.target.checked)} />
            Only rows with failures or &lt;99% uptime
          </label>
          <Badge tone="neutral">{rows.length} shown{truncated ? ' · top 50' : ''}</Badge>
          <span className="text-xs text-muted ml-auto">Model rows open filtered requests; provider rows open the window.</span>
        </div>

        {rows.length === 0 ? (
          <div className="p-6">
            <EmptyState icon="server" title={onlyProblems ? 'No problem rows in this window' : 'No traffic in this window'}
              hint={onlyProblems ? 'Clear the problems filter to see every ranked row.' : 'Send some requests, or widen the window.'} />
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="bg-raised/60 border-b border-stone">
                  <SortTh label={tab === 'providers' ? 'Provider' : 'Model'} sortKey="name" sort={sort.sort} onSort={sort.toggle} />
                  <SortTh label="Uptime" sortKey="uptime_pct" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="Requests" sortKey="requests" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="Failed" sortKey="failed" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="p50" sortKey="p50_ms" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="p95" sortKey="p95_ms" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="p99" sortKey="p99_ms" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="TTFT p50" sortKey="ttft_p50_ms" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="Tok/s" sortKey="avg_tps" sort={sort.sort} onSort={sort.toggle} align="right" />
                  <SortTh label="Streaming" sortKey="stream_share" sort={sort.sort} onSort={sort.toggle} align="right" />
                </tr>
              </thead>
              <tbody>
                {rows.map(r => (
                  <tr key={r.name}
                    onClick={() => go(tab === 'models' ? r.name : undefined)}
                    onKeyDown={tab === 'models' ? (e) => {
                      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); go(r.name) }
                    } : undefined}
                    tabIndex={tab === 'models' ? 0 : undefined}
                    aria-label={tab === 'models' ? `${r.name} — open its requests` : undefined}
                    className="border-b border-stone/60 hover:bg-raised/60 cursor-pointer transition-colors focus-visible:outline-none focus-visible:bg-raised/60">
                    <td className="px-4 py-2.5 max-w-[260px]">
                      <div className="flex items-center gap-2">
                        {tab === 'providers' && r.health && <HealthDot health={r.health} />}
                        <span className="font-mono text-xs truncate" title={r.name}>{r.name}</span>
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">
                      <span className={r.uptime_pct > 99 ? 'text-good' : r.uptime_pct > 95 ? 'text-amber' : 'text-danger'}>
                        {r.uptime_pct.toFixed(2)}%
                      </span>
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{fmtInt(r.requests)}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">
                      {r.failed > 0 ? <span className="text-danger">{fmtInt(r.failed)}</span> : '—'}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{fmtMs(r.p50_ms)}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{fmtMs(r.p95_ms)}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{fmtMs(r.p99_ms)}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{fmtMs(r.ttft_p50_ms)}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{r.avg_tps ? r.avg_tps.toFixed(0) : '—'}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{(r.stream_share * 100).toFixed(0)}%</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Panel title="Reading these numbers" hint="What each column actually measures. Provider health dots reflect the latest probe, not historical success.">
        <dl className="grid gap-3 sm:grid-cols-2 text-xs">
          {[
            ['Uptime', 'Share of requests in the window that did not return 4xx/5xx. A ratio over time, not the current health probe.'],
            ['p50 / p95 / p99', 'End-to-end gateway latency percentiles sampled from requests in the window. Rankings cover the 50 busiest providers or models.'],
            ['TTFT p50', 'Time to the first streamed chunk. Blank for non-streaming traffic — a buffered response has no first token, and recording 0 would drag the average down.'],
            ['Tok/s', 'Completion tokens divided by the post-TTFT streaming window, so the wait for the first token does not understate generation speed.'],
            ['Streaming', 'Share of requests that used SSE. TTFT and throughput only have meaning for these.'],
            ['Failed', 'Requests whose upstream attempt chain ended in an error after any configured retries and failover.'],
          ].map(([k, v]) => (
            <div key={k}>
              <dt className="font-medium">{k}</dt>
              <dd className="text-muted mt-0.5">{v}</dd>
            </div>
          ))}
        </dl>
      </Panel>
    </div>
  )
}
