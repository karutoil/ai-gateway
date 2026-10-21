import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, type AttributionReport, type AnalyticsWindow, type RangeKey } from '../lib/api'
import { PageHeader, Button, Icon, Card, ErrorNote, TableShell, TableSkeleton, EmptyState, Badge } from '../components/ui'
import {
  RangePicker, BreakdownTable, Metric, Panel, useSort, sortRows,
  fmtInt, fmtCompact, fmtMoney, fmtMs,
} from '../components/report'

/**
 * Who used the gateway.
 *
 * Two attribution dimensions, deliberately presented differently:
 *
 *   Applications  a bounded set (an operator decides how many apps attribute),
 *                 so this is a true breakdown and can be grouped freely.
 *   End users     unbounded by construction — one row per person or agent.
 *                 This page therefore RANKS them (top N by spend) rather than
 *                 grouping, which is the same filter/rank-versus-group split
 *                 the API enforces by rejecting group_by=end_user.
 *
 * Clients is the third, and the only one that is populated by default: app and
 * end_user both need the caller to opt in (X-App-Id / X-Title / Referer, or a
 * body `user`), while the User-Agent product token is always on the wire. It
 * leads the page because an empty Applications panel with a populated Clients
 * panel is the honest reading — "nobody declared an app, here is who called".
 *
 * All three link into Requests with the matching filter applied, so a name here
 * is a starting point for drilling in rather than a dead end.
 */
export default function Attribution() {
  const navigate = useNavigate()
  const [range, setRange] = useState<RangeKey>('7d')
  const [custom, setCustom] = useState<{ from: string; to: string } | null>(null)
  const [data, setData] = useState<AttributionReport | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  // Bumped by Refresh. Clearing `data` alone (or calling setRange(r => r)) is a
  // no-op for React and left the button doing nothing at all.
  const [reload, setReload] = useState(0)

  const appsSort = useSort({ key: 'cost', dir: 'desc' })
  const usersSort = useSort({ key: 'cost', dir: 'desc' })
  const clientsSort = useSort({ key: 'cost', dir: 'desc' })

  const win: AnalyticsWindow = custom ? { from: custom.from, to: custom.to } : { range }

  useEffect(() => {
    let cancelled = false
    setLoading(true); setErr('')
    api.analytics.attribution(win)
      .then(d => { if (!cancelled) setData(d) })
      .catch((e: any) => { if (!cancelled) setErr(e?.message || String(e)) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range, custom?.from, custom?.to, reload])

  const apps = useMemo(() => sortRows(data?.apps ?? [], appsSort.sort), [data, appsSort.sort])
  const users = useMemo(() => sortRows(data?.end_users ?? [], usersSort.sort), [data, usersSort.sort])
  const clients = useMemo(() => sortRows(data?.clients ?? [], clientsSort.sort), [data, clientsSort.sort])

  const cov = data?.coverage
  const share = (n: number | undefined) => (cov && cov.total_requests > 0 ? (n ?? 0) / cov.total_requests : 0)
  const clientPct = share(cov?.with_client)
  const userPct = share(cov?.with_end_user)
  // "Identified" counts any identity, which is what makes a thin ranking
  // readable: 0% identified means the request carried nothing at all, while
  // 100% identified with 0 end users means clients are recorded and people
  // simply are not named.
  const identifiedPct = share(cov?.identified)

  const go = (params: Record<string, string>) => {
    const q = new URLSearchParams({ since: range, ...params })
    navigate(`/logs?${q.toString()}`)
  }

  const rankColumns = [
    { key: 'requests', label: 'Requests', render: (r: any) => fmtInt(r.requests) },
    { key: 'tokens', label: 'Tokens', render: (r: any) => fmtCompact(r.tokens) },
    { key: 'cost', label: 'Cost', render: (r: any) => fmtMoney(r.cost) },
    { key: 'failed', label: 'Failed', render: (r: any) => r.failed > 0 ? <span className="text-danger">{fmtInt(r.failed)}</span> : '—' },
    { key: 'identities', label: 'Reach', render: (r: any) => r.identities ? fmtInt(r.identities) : '—' },
  ]

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Optimize · Attribution"
        title="Who is using the gateway"
        description="Who called: the client software (from the User-Agent, always present), the application and the end user each request declares. So spend maps to a product or a person instead of just a key prefix."
        actions={
          <Button variant="secondary" size="sm" onClick={() => setReload(n => n + 1)} disabled={loading} title="Reload">
            <Icon name="refresh" size={14} className={loading ? 'animate-spin' : ''}/> Refresh
          </Button>
        }
      />

      <RangePicker value={range} onChange={setRange} custom={custom} onCustom={setCustom} />

      {err && <ErrorNote message={err} />}

      {/* Coverage first: without it a thin ranking is indistinguishable from
          an integration that never sends attribution at all. */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label="Requests"
          value={loading ? '—' : fmtInt(cov?.total_requests)}
          sub={cov ? `${fmtInt(cov.unattributed)} with no identity at all` : 'in window'}
          tone={(cov?.unattributed ?? 0) > 0 ? 'warn' : 'neutral'}
          icon="pulse"
        />
        <Metric
          label="Clients"
          value={loading ? '—' : `${(clientPct * 100).toFixed(1)}%`}
          sub={cov ? `${fmtInt(cov.with_client)} of ${fmtInt(cov.total_requests)} name a client` : ''}
          tone={clientPct > 0.5 ? 'good' : clientPct > 0 ? 'warn' : 'neutral'}
          icon="terminal"
        />
        <Metric
          label="End users"
          value={loading ? '—' : `${(userPct * 100).toFixed(1)}%`}
          sub={cov ? `${fmtInt(cov.with_end_user)} of ${fmtInt(cov.total_requests)} name a person` : ''}
          tone={userPct > 0.5 ? 'good' : userPct > 0 ? 'warn' : 'neutral'}
          icon="users"
        />
        <Metric
          label="Unpriced"
          value={loading ? '—' : fmtInt(cov?.unpriced_requests)}
          sub={cov ? `${fmtCompact(cov.unpriced_token_share)} tokens billed at $0` : ''}
          tone={(cov?.unpriced_requests ?? 0) > 0 ? 'bad' : 'good'}
          icon="wallet"
        />
      </div>

      {(cov?.unpriced_requests ?? 0) > 0 && (
        <Card className="border-danger/30 bg-danger/10">
          <div className="flex items-start gap-2.5 text-sm">
            <Icon name="alert" size={15} className="text-danger mt-0.5 shrink-0" />
            <div>
              <p className="font-medium text-danger">{fmtInt(cov?.unpriced_requests)} requests served tokens with no recorded cost.</p>
              <p className="text-xs text-muted mt-1">
                Their model is missing from the catalog and no fallback pricing is set, so they consume no budget.
                Set <span className="font-mono">price_fallback_input_usd_per_1m</span> /{' '}
                <span className="font-mono">..._output_...</span> in Settings, or sync the catalog.
                Alert on <span className="font-mono">gateway_zero_cost_requests_total&#123;reason="catalog_miss"&#125;</span>.
              </p>
            </div>
          </div>
        </Card>
      )}

      {loading ? (
        <div className="grid gap-3 lg:grid-cols-2">
          {/* TableSkeleton renders <tbody> rows — it must sit inside TableShell's table. */}
          <TableShell><TableSkeleton rows={5} cols={5} /></TableShell>
          <TableShell><TableSkeleton rows={5} cols={5} /></TableShell>
        </div>
      ) : (
        <div className="space-y-3">
          {/* Clients leads: it is the dimension populated without any
              client-side plumbing, so it answers "who is calling" even when
              nothing declares an app or an end user. */}
          <Panel
            title="Clients"
            hint="Derived from the User-Agent product token (or an X-Client header). Click a row to see its requests."
            right={<Badge tone="neutral">{clients.length}</Badge>}>
            {clients.length === 0 ? (
              <EmptyState icon="terminal" title="No clients recorded"
                hint={<>Every request carries a <span className="font-mono">User-Agent</span>, so this should never be empty. If it is, the request reached the gateway without one, or the rows predate this dimension.</>} />
            ) : (
              <BreakdownTable
                rows={clients} sort={clientsSort.sort} onSort={clientsSort.toggle}
                nameHeader="Client" columns={rankColumns}
                maxRows={15}
                onRowClick={(r) => go({ client: r.name })}
              />
            )}
          </Panel>

          <div className="grid gap-3 lg:grid-cols-2">
            <Panel
              title="Applications"
              hint="Declared by the caller (X-App-Id, X-Title or HTTP-Referer). Bounded set — groupable."
              right={<Badge tone="neutral">{apps.length}</Badge>}>
              {apps.length === 0 ? (
                <EmptyState icon="globe" title="No application declared itself"
                  hint={<>Optional: send <span className="font-mono">X-App-Id</span>, <span className="font-mono">X-Title</span> or an <span className="font-mono">HTTP-Referer</span> header. Traffic is still attributed to a client above.</>} />
              ) : (
                <BreakdownTable
                  rows={apps} sort={appsSort.sort} onSort={appsSort.toggle}
                  nameHeader="Application" columns={rankColumns}
                  maxRows={15}
                  onRowClick={(r) => go({ app: r.name })}
                />
              )}
            </Panel>

            <Panel
              title="End users"
              hint="Ranked by spend, not grouped — identities are unbounded, so only the top N are materialised."
              right={<Badge tone="neutral">{users.length}</Badge>}>
              {users.length === 0 ? (
                <EmptyState icon="users" title="No end users named"
                  hint={<>The OpenAI <span className="font-mono">user</span> field, Anthropic <span className="font-mono">metadata.user_id</span>, or an <span className="font-mono">X-End-User</span> header names the person the call was made for.</>} />
              ) : (
                <BreakdownTable
                  rows={users} sort={usersSort.sort} onSort={usersSort.toggle}
                  nameHeader="End user" columns={rankColumns}
                  maxRows={15}
                  onRowClick={(r) => go({ end_user: r.name })}
                />
              )}
            </Panel>
          </div>
        </div>
      )}

      {!loading && data && data.end_users.length > 0 && (
        <Panel title="Last seen" hint="Most recent attributed activity per end user, newest first.">
          <div className="flex flex-wrap gap-2">
            {[...data.end_users]
              .filter(u => u.last_seen)
              .sort((a, b) => String(b.last_seen).localeCompare(String(a.last_seen)))
              .slice(0, 12)
              .map(u => (
                <button key={u.name} onClick={() => go({ end_user: u.name })}
                  className="inline-flex items-center gap-2 text-xs px-2.5 py-1.5 rounded-lg border border-stone bg-surface hover:border-accent/50 hover:bg-raised transition-colors">
                  <span className="font-mono truncate max-w-[150px]" title={u.name}>{u.name}</span>
                  <span className="text-muted font-mono tabular-nums">{new Date(u.last_seen!).toLocaleString()}</span>
                </button>
              ))}
          </div>
        </Panel>
      )}
    </div>
  )
}
