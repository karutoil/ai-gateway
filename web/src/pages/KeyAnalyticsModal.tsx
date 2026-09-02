import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  Modal, Button, Badge, Icon, SegmentedControl, Skeleton, TableShell, Th, Td, EmptyState,
} from '../components/ui'

type KeyDaily = { day: string; tokens: number; cost: number; requests: number }
type KeyTopModel = { model: string; tokens: number; cost: number; requests: number }
type KeyEndpoint = { endpoint: string; tokens: number; cost: number; requests: number; failed: number }
type KeyErr = { status: number; count: number; sample?: string }
type KeyAnalytics = {
  key_id: string
  prefix: string
  name: string
  range: '24h' | '7d' | '30d'
  all_time: { requests: number; tokens: number; cost: number; failed: number }
  range_requests: number
  range_tokens: number
  range_cost: number
  range_failed: number
  range_successful: number
  daily: KeyDaily[]
  top_models: KeyTopModel[]
  endpoints: KeyEndpoint[]
  latency: { p50: number; p95: number; avg: number; count: number }
  ttft_avg: number
  errors: KeyErr[]
}

type Range = '24h' | '7d' | '30d'

/** Hourly bucket keys look like "2026-01-01T14:00:00Z"; daily are plain dates. */
function bucketLabel(day: string): string {
  if (day.includes('T')) return `${day.slice(8, 10)} ${day.slice(11, 13)}:00`
  return day.slice(5)
}

/** Zero-fill buckets so the chart always spans the selected range. */
function zeroFill(daily: KeyDaily[], range: Range): KeyDaily[] {
  const byKey = new Map(daily.map(d => [d.day, d]))
  const out: KeyDaily[] = []
  const now = new Date()
  if (range === '24h') {
    for (let i = 23; i >= 0; i--) {
      const t = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), now.getUTCHours() - i))
      const key = t.toISOString().slice(0, 13) + ':00:00Z'
      out.push(byKey.get(key) ?? { day: key, tokens: 0, cost: 0, requests: 0 })
    }
    return out
  }
  const days = range === '7d' ? 7 : 30
  for (let i = days - 1; i >= 0; i--) {
    const day = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i)).toISOString().slice(0, 10)
    out.push(byKey.get(day) ?? { day, tokens: 0, cost: 0, requests: 0 })
  }
  return out
}

const fmtInt = (n: number) => n.toLocaleString()
const fmtUsd = (n: number) => n === 0 ? '$0' : n < 0.01 ? '<$0.01' : `$${n.toFixed(2)}`

function KPI({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-stone bg-app px-3 py-2.5">
      <div className="text-[11px] text-muted uppercase tracking-wide">{label}</div>
      <div className="text-lg font-semibold tabular-nums mt-0.5 truncate">{value}</div>
    </div>
  )
}

/** Mini bar chart over the selected range (same visual language as Analytics). */
function MiniBars({ daily, extract, tip }: { daily: KeyDaily[]; extract: (d: KeyDaily) => number; tip: (d: KeyDaily) => string }) {
  const max = Math.max(1e-9, ...daily.map(extract))
  return (
    <div className="flex items-end gap-1 overflow-x-auto pb-1">
      {daily.map(d => {
        const v = extract(d)
        const pct = v > 0 ? Math.max(3, Math.round((v / max) * 100)) : 2
        return (
          <div key={d.day} title={tip(d)} className="flex-1 min-w-[14px] flex flex-col items-center gap-1 group cursor-default">
            <div className="w-full h-20 flex items-end">
              <div
                className={`w-full rounded-t-sm transition-opacity ${v > 0 ? 'bg-teal opacity-80 group-hover:opacity-100' : 'bg-stone/60'}`}
                style={{ height: `${pct}%` }}
              />
            </div>
            <span className="font-mono text-[8px] text-muted truncate w-full text-center">{bucketLabel(d.day)}</span>
          </div>
        )
      })}
    </div>
  )
}

export default function KeyAnalyticsModal({ keyId, onClose }: { keyId: string | null; onClose: () => void }) {
  const [range, setRange] = useState<Range>('7d')
  const [data, setData] = useState<KeyAnalytics | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!keyId) { setData(null); setError(''); return }
    setLoading(true); setError('')
    api.keys.analytics(keyId, range)
      .then(d => { setData(d as KeyAnalytics); setLoading(false) })
      .catch((e: any) => { setError(e?.message || String(e)); setLoading(false) })
  }, [keyId, range])

  return (
    <Modal open={keyId !== null} onClose={onClose} title="Key analytics" width="max-w-2xl">
      {error && (
        <EmptyState icon="alert" title="Could not load analytics" hint={error}
          action={<Button variant="secondary" size="sm" onClick={() => setRange(r => r)}><Icon name="refresh" size={14} />Retry</Button>} />
      )}
      {!error && (
        <div className="space-y-5">
          <div className="flex items-center justify-between gap-3 flex-wrap">
            <SegmentedControl<Range>
              options={[{ value: '24h', label: '24h' }, { value: '7d', label: '7d' }, { value: '30d', label: '30d' }]}
              value={range} onChange={setRange}
            />
            {data && (
              <div className="flex items-center gap-2 text-xs text-muted">
                <code className="font-mono bg-app border border-stone rounded-md px-1.5 py-0.5">{data.prefix}</code>
                <span>All-time: <span className="tabular-nums text-paper">{fmtInt(data.all_time.requests)}</span> reqs · <span className="tabular-nums text-paper">{fmtUsd(data.all_time.cost)}</span></span>
              </div>
            )}
          </div>

          {loading && !data ? (
            <div className="space-y-3">
              <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
                {[0, 1, 2, 3].map(i => <Skeleton key={i} className="h-16" />)}
              </div>
              <Skeleton className="h-28" />
            </div>
          ) : data ? (
            <>
              <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
                <KPI label="Requests" value={fmtInt(data.range_requests)} />
                <KPI label="Tokens" value={fmtInt(data.range_tokens)} />
                <KPI label="Spend" value={fmtUsd(data.range_cost)} />
                <KPI label="Success rate" value={
                  data.range_requests > 0
                    ? `${Math.round((data.range_successful / data.range_requests) * 100)}%`
                    : '—'
                } />
              </div>

              <div>
                <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">Requests over {range}</div>
                <MiniBars
                  daily={zeroFill(data.daily, range)}
                  extract={d => d.requests}
                  tip={d => `${d.day} — ${d.requests} requests · ${d.tokens.toLocaleString()} tokens · ${fmtUsd(d.cost)}`}
                />
              </div>

              <div className="grid sm:grid-cols-2 gap-4">
                <div>
                  <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">Latency</div>
                  <div className="space-y-1.5 text-sm">
                    <div className="flex justify-between"><span className="text-muted">p50</span><span className="tabular-nums">{data.latency.p50 ? `${data.latency.p50} ms` : '—'}</span></div>
                    <div className="flex justify-between"><span className="text-muted">p95</span><span className="tabular-nums">{data.latency.p95 ? `${data.latency.p95} ms` : '—'}</span></div>
                    <div className="flex justify-between"><span className="text-muted">avg</span><span className="tabular-nums">{data.latency.avg ? `${Math.round(data.latency.avg)} ms` : '—'}</span></div>
                    <div className="flex justify-between"><span className="text-muted">avg TTFT</span><span className="tabular-nums">{data.ttft_avg ? `${Math.round(data.ttft_avg)} ms` : '—'}</span></div>
                  </div>
                </div>
                <div>
                  <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">Top models</div>
                  {data.top_models.length === 0 ? (
                    <div className="text-sm text-muted">No requests in range.</div>
                  ) : (
                    <div className="space-y-1.5 text-sm">
                      {data.top_models.slice(0, 5).map(m => (
                        <div key={m.model} className="flex items-center justify-between gap-2">
                          <span className="truncate font-mono text-xs" title={m.model}>{m.model}</span>
                          <span className="tabular-nums text-xs text-muted shrink-0">{fmtInt(m.requests)} req · {fmtInt(m.tokens)} tok</span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </div>

              {data.endpoints.length > 0 && (
                <div>
                  <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">Endpoints</div>
                  <div className="rounded-lg border border-stone overflow-hidden">
                    <table className="w-full text-xs">
                      <thead>
                        <tr>
                          <Th className="text-left">Endpoint</Th>
                          <Th className="text-right">Requests</Th>
                          <Th className="text-right">Tokens</Th>
                          <Th className="text-right">Spend</Th>
                          <Th className="text-right">Failed</Th>
                        </tr>
                      </thead>
                      <tbody>
                        {data.endpoints.map(e => (
                          <tr key={e.endpoint} className="border-t border-stone">
                            <Td className="font-mono">{e.endpoint}</Td>
                            <Td className="text-right tabular-nums">{fmtInt(e.requests)}</Td>
                            <Td className="text-right tabular-nums">{fmtInt(e.tokens)}</Td>
                            <Td className="text-right tabular-nums">{fmtUsd(e.cost)}</Td>
                            <Td className={`text-right tabular-nums ${e.failed > 0 ? 'text-red-400' : 'text-muted'}`}>{fmtInt(e.failed)}</Td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}

              {data.errors.length > 0 && (
                <div>
                  <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">Errors in range</div>
                  <div className="space-y-1.5">
                    {data.errors.map(e => (
                      <div key={e.status} className="flex items-start gap-2 text-sm">
                        <Badge tone={e.status >= 500 ? 'bad' : 'warn'}>{e.status}</Badge>
                        <span className="tabular-nums text-xs text-muted mt-0.5 shrink-0">×{fmtInt(e.count)}</span>
                        {e.sample && <span className="text-xs text-muted truncate" title={e.sample}>{e.sample}</span>}
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {data.range_requests === 0 && (
                <EmptyState icon="chart" title="No usage in this range"
                  hint="Requests made with this key will appear here once traffic flows. Analytics covers requests since per-key attribution shipped." />
              )}
            </>
          ) : null}
        </div>
      )}
      <div className="flex justify-end mt-6">
        <Button variant="ghost" onClick={onClose}>Close</Button>
      </div>
    </Modal>
  )
}
