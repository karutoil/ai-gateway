import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import {
  Card, Button, Badge, Icon, Input, PageHeader, SegmentedControl,
  TableShell, Th, Td, TableSkeleton, EmptyState, Modal, CopyButton,
} from '../components/ui'

function tpsFor(l:any){
  if(!l.total_tokens || !l.latency_ms) return 0
  return l.total_tokens / (l.latency_ms/1000)
}
function ttftFor(l:any){
  return l.ttft_ms || 0
}
/** Canonical status coloring: 2xx/3xx good, 4xx warn, 5xx bad. */
function statusTone(s:number): 'good'|'warn'|'bad' {
  return s>=500 ? 'bad' : s>=400 ? 'warn' : 'good'
}

/** Detail body rendered inside the request modal. */
function DetailBody({ detail, selected, keyMap }: {
  detail: any; selected: any; keyMap: Record<string,string>
}) {
  const raw = JSON.stringify(detail || selected, null, 2)
  const errMsg = String(detail.log?.error || detail.error || selected.error || '')
  const stat = statusTone(detail.log?.status ?? selected.status)
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Status</div>
          <div className="mt-1"><Badge tone={stat} dot>{detail.log?.status ?? selected.status}</Badge></div>
          {(detail.log?.error || detail.error || selected.error) && (
            <div className="mt-2 text-xs text-red-400 break-all line-clamp-3">{errMsg.slice(0,200)}</div>
          )}
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Latency</div>
          <div className="font-mono text-sm mt-1 tabular-nums">{(detail.log?.latency_ms ?? selected.latency_ms)}ms</div>
          <div className="text-xs text-muted tabular-nums">TTFT {((detail.log?.ttft_ms ?? selected.ttft_ms) || '—')}ms</div>
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">TPS</div>
          <div className="font-mono text-sm mt-1 tabular-nums">{detail.tps ? Number(detail.tps).toFixed(1) : tpsFor(detail.log||selected).toFixed(1)}</div>
          <div className="text-xs text-muted tabular-nums">{detail.log?.total_tokens ?? selected.total_tokens} tokens</div>
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Provider</div>
          <div className="text-sm mt-1 truncate" title={String(detail.provider_name || detail.log?.provider_id || selected.provider_id || '')}>
            {detail.provider_name || detail.log?.provider_id || selected.provider_id || '—'}
          </div>
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Key</div>
          <div className="font-mono text-xs mt-1">{detail.key_name ? `${detail.log?.key_prefix} · ${detail.key_name}` : detail.log?.key_prefix || selected.key_prefix}</div>
          {keyMap[selected.key_prefix] && <div className="text-xs text-muted">{keyMap[selected.key_prefix]}</div>}
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Endpoint</div>
          <div className="text-xs mt-1 break-all">{detail.log?.endpoint || selected.endpoint}</div>
          <div className="text-xs text-muted font-mono truncate">{detail.log?.model || selected.model}</div>
        </div>
      </div>

      <div className="grid md:grid-cols-2 gap-3">
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Tokens</div>
          <div className="mt-2 font-mono text-xs space-y-1 tabular-nums">
            <div>prompt: {detail.log?.prompt_tokens ?? selected.prompt_tokens}</div>
            <div>completion: {detail.log?.completion_tokens ?? selected.completion_tokens}</div>
            <div>total: {detail.log?.total_tokens ?? selected.total_tokens}</div>
            <div>cost: ${Number((detail.log?.cost_usd ?? selected.cost_usd) ?? 0).toFixed(6)}</div>
          </div>
        </div>
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted">Timing</div>
          <div className="mt-2 font-mono text-xs space-y-1 tabular-nums">
            <div>latency: {detail.log?.latency_ms ?? selected.latency_ms}ms</div>
            <div>ttft: {(detail.log?.ttft_ms ?? selected.ttft_ms ?? '—')}ms {(detail.log?.is_stream||selected.is_stream) ? '(stream)' : '(non-stream)'}</div>
            <div>tps: {detail.tps ? Number(detail.tps).toFixed(2) : tpsFor(detail.log||selected).toFixed(2)} tok/s</div>
            {detail.log?.ttft_ms && detail.log?.latency_ms && detail.log?.completion_tokens
              ? <div>tps after ttft: {((detail.log.completion_tokens) / ((detail.log.latency_ms - detail.log.ttft_ms)/1000)).toFixed(1)} tok/s</div>
              : null}
          </div>
        </div>
      </div>

      {(detail.log?.error || detail.error || selected.error) && (
        <div className="border border-red-500/30 bg-red-500/10 rounded-xl p-3">
          <div className="flex items-center gap-2 text-xs text-red-400">
            <Icon name="alert" size={14}/>
            <span className="uppercase tracking-wider font-medium">Error — {detail.log?.status || selected.status}</span>
          </div>
          <pre className="mt-2 bg-app border border-red-500/20 rounded-lg p-3 font-mono text-xs whitespace-pre-wrap break-all text-red-400 overflow-x-auto">{errMsg.slice(0,4000)}</pre>
          {(detail.log?.request_body || (detail as any).request_body) && (
            <details className="mt-2">
              <summary className="font-mono text-xs text-muted cursor-pointer select-none">Request body</summary>
              <pre className="mt-1 bg-app border border-stone rounded-lg p-2 font-mono text-xs overflow-x-auto whitespace-pre-wrap break-all">{String(detail.log?.request_body || (detail as any).request_body).slice(0,4000)}</pre>
            </details>
          )}
          {(detail.log?.response_body || (detail as any).response_body) && (
            <details className="mt-2">
              <summary className="font-mono text-xs text-muted cursor-pointer select-none">Response body</summary>
              <pre className="mt-1 bg-app border border-stone rounded-lg p-2 font-mono text-xs overflow-x-auto whitespace-pre-wrap break-all">{String(detail.log?.response_body || (detail as any).response_body).slice(0,4000)}</pre>
            </details>
          )}
        </div>
      )}

      <div className="rounded-xl border border-stone bg-raised p-3">
        <div className="text-xs text-muted mb-2">Raw log</div>
        <pre className="bg-app border border-stone rounded-lg p-3 font-mono text-xs overflow-x-auto whitespace-pre-wrap break-all">{JSON.stringify(detail.log || selected, null, 2)}</pre>
      </div>
      {detail.log && detail.log !== selected && (
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted mb-2">Enriched detail</div>
          <pre className="bg-app border border-stone rounded-lg p-3 font-mono text-xs overflow-x-auto whitespace-pre-wrap break-all">{JSON.stringify(detail, null, 2)}</pre>
        </div>
      )}
    </div>
  )
}

const LIMIT_OPTIONS = [50, 100, 200, 500]
const SINCE_OPTIONS = [
  { value: '1h', label: '1h' },
  { value: '24h', label: '24h' },
  { value: '7d', label: '7d' },
  { value: '30d', label: '30d' },
] as const

export default function Logs(){
  const navigate = useNavigate()
  const [logs, setLogs] = useState<any[]>([])
  const [total, setTotal] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [query, setQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState<'all'|'failed'>('all')
  const [since, setSince] = useState<'1h'|'24h'|'7d'|'30d'>('24h')
  const [limit, setLimit] = useState(100)
  const [offset, setOffset] = useState(0)
  const [keyMap, setKeyMap] = useState<Record<string,string>>({})
  const [selected, setSelected] = useState<any|null>(null)
  const [detail, setDetail] = useState<any|null>(null)
  const [loadingDetail, setLoadingDetail] = useState(false)

  // Server-side pagination + filters (GET /api/logs supports limit/offset/
  // status/since and returns X-Total-Count).
  const loadLogs = (opts?: { resetOffset?: boolean })=>{
    const nextOffset = opts?.resetOffset ? 0 : offset
    setLoading(true)
    setLoadError('')
    api.logsQuery({
      limit,
      offset: nextOffset,
      status: statusFilter === 'failed' ? 'failed' : undefined,
      since,
    })
      .then(({ rows, total })=>{
        setLogs(rows)
        setTotal(total)
        if (opts?.resetOffset) setOffset(0)
      })
      .catch((e:any)=> setLoadError(e?.message || String(e)))
      .finally(()=> setLoading(false))
  }

  useEffect(()=>{ loadLogs({ resetOffset: true }) }, [limit, statusFilter, since])

  useEffect(()=>{
    api.keys.list().then(keys=>{
      const m:Record<string,string>={}
      for(const k of keys) m[k.prefix]=k.name
      setKeyMap(m)
    }).catch(()=>{})
  },[])

  // Also support direct fetch via api if not yet added
  const fetchDetail = async (id:string)=>{
    try{
      const res = await fetch(`/api/logs/${id}`, { credentials: 'same-origin' })
      if(res.ok) return await res.json()
    }catch{}
    return null
  }

  const handleRowClick = async (l:any)=>{
    setSelected(l)
    setDetail(null)
    setLoadingDetail(true)
    const d = await fetchDetail(l.id)
    setDetail(d || { log: l })
    setLoadingDetail(false)
  }

  // Client-side search across the currently loaded page only — the text box
  // narrows what you see; server filters (status/since) govern what is loaded.
  const q = query.trim().toLowerCase()
  const visible = logs.filter(l=>{
    if(!q) return true
    return [l.model, l.endpoint, keyMap[l.key_prefix], String(l.status??'')]
      .some(v => typeof v==='string' && v.toLowerCase().includes(q))
  })

  // Aggregates cover the loaded page only — labeled as such below.
  const totalCost = logs.reduce((acc,l)=> acc + (l.cost_usd||0), 0)
  const totalTokens = logs.reduce((acc,l)=> acc + (l.total_tokens||0), 0)
  const avgTTFT = logs.length ? Math.round(logs.reduce((a,l)=>a+(l.ttft_ms||0),0)/logs.length) : 0
  const avgTPS = logs.length ? (logs.reduce((a,l)=>a + tpsFor(l),0)/logs.length).toFixed(1) : '0'

  const page = Math.floor(offset / limit) + 1
  const hasPrev = offset > 0
  const hasNext = total != null ? offset + logs.length < total : logs.length === limit

  const rawJson = detail ? JSON.stringify(detail || selected, null, 2) : ''

  return (
    <div className="space-y-4">
      <PageHeader
        title="Request Logs"
        description="Every proxied call with status, latency, tokens and cost."
        actions={
          <Button variant="primary" onClick={()=>loadLogs()} disabled={loading}>
            <Icon name="refresh" size={15}/>Refresh
          </Button>
        }
      />

      {/* Totals strip — aggregates cover the currently loaded rows only */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted font-mono tabular-nums -mt-2">
        <span>{logs.length} loaded{total != null ? ` of ${total.toLocaleString()} matching` : ''}</span>
        <span title="Sum over the currently loaded rows">{totalTokens.toLocaleString()} tokens (loaded)</span>
        <span title="Sum over the currently loaded rows">${totalCost.toFixed(4)} (loaded)</span>
        <span>avg TTFT {avgTTFT}ms</span>
        <span>avg TPS {avgTPS}</span>
      </div>

      {/* Filter bar — status + time range + page size hit the server; search is within the loaded page */}
      <Card className="!p-3 flex flex-col sm:flex-row sm:items-center gap-2">
        <div className="relative flex-1 min-w-[180px]">
          <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={15}/></span>
          <Input value={query} onChange={e=>setQuery(e.target.value)} placeholder="Filter loaded rows by model, endpoint, key…" className="pl-9"/>
        </div>
        <SegmentedControl<'all'|'failed'>
          options={[{value:'all',label:'All'},{value:'failed',label:'Failed'}]}
          value={statusFilter}
          onChange={setStatusFilter}
        />
        <SegmentedControl<'1h'|'24h'|'7d'|'30d'>
          options={SINCE_OPTIONS.map(o=>({ value:o.value, label:o.label }))}
          value={since}
          onChange={setSince}
        />
        <label className="flex items-center gap-2 text-xs text-muted shrink-0">
          Rows
          <select
            value={limit}
            onChange={e=>setLimit(Number(e.target.value))}
            className="bg-app border border-stone rounded-lg px-2 h-9 text-sm focus:outline-none focus:border-teal/60"
          >
            {LIMIT_OPTIONS.map(n=> <option key={n} value={n}>{n}</option>)}
          </select>
        </label>
      </Card>

      {loadError && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load request logs"
            hint={loadError}
            action={<Button variant="secondary" onClick={()=>loadLogs()}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {!loadError && (
        <TableShell>
          <thead>
            <tr>
              <Th>Time</Th>
              <Th>Model</Th>
              <Th>Provider</Th>
              <Th className="text-center">Status</Th>
              <Th className="!text-right">Latency</Th>
              <Th className="!text-right">Tokens</Th>
              <Th className="!text-right">Cost</Th>
            </tr>
          </thead>
          {loading ? (
            <TableSkeleton rows={8} cols={7}/>
          ) : visible.length===0 ? (
            <tbody>
              <tr><td colSpan={7}>
                <EmptyState
                  icon="logs"
                  title={logs.length===0
                    ? (offset > 0 ? 'No requests on this page' : 'No requests match these filters')
                    : 'No matching requests on this page'}
                  hint={logs.length===0
                    ? (offset > 0
                        ? 'Go back a page, or widen the time range / clear the failed filter.'
                        : 'Send your first call from the Playground and it will appear here instantly.')
                    : 'Try a different search term.'}
                  action={logs.length===0 && offset === 0
                    ? <Button variant="secondary" onClick={()=>navigate('/playground')}><Icon name="play" size={15}/>Open Playground</Button>
                    : undefined}
                />
              </td></tr>
            </tbody>
          ) : (
            <tbody>
              {visible.map(l=>(
                <tr key={l.id} onClick={()=>handleRowClick(l)}
                    className="hover:bg-raised/50 cursor-pointer transition-colors focus-visible:outline-none focus-visible:bg-raised/50">
                  <Td className="whitespace-nowrap text-xs text-muted tabular-nums">{new Date(l.created_at).toLocaleString()}</Td>
                  <Td><span className="block font-mono text-xs truncate max-w-[160px]" title={`${l.model}${l.endpoint?` · ${l.endpoint}${l.is_stream?' · stream':''}`:''}`}>{l.model}</span></Td>
                  <Td><span className="block font-mono text-xs truncate max-w-[110px] text-muted" title={String(l.provider_id||'')}>{l.provider_id || '—'}</span></Td>
                  <Td className="text-center"><Badge tone={statusTone(l.status)} dot>{l.status}</Badge></Td>
                  <Td className="text-right tabular-nums text-xs">{l.latency_ms}ms</Td>
                  <Td className="text-right tabular-nums text-xs">
                    <span className="block" title={`${l.prompt_tokens ?? 0} prompt · ${l.completion_tokens ?? 0} completion`}>
                      {l.total_tokens ? l.total_tokens.toLocaleString() : '—'}
                    </span>
                  </Td>
                  <Td className="text-right tabular-nums text-xs">{l.cost_usd ? `$${Number(l.cost_usd).toFixed(4)}` : '—'}</Td>
                </tr>
              ))}
            </tbody>
          )}
        </TableShell>
      )}

      {/* Server-side pagination controls */}
      {!loadError && (
        <div className="flex items-center justify-between gap-2 -mt-2">
          <span className="text-xs text-muted tabular-nums">
            Page {page}{total != null ? ` · ${total.toLocaleString()} request(s) matching` : ''}
            {' '}· rows {offset + 1}–{offset + logs.length}
          </span>
          <div className="flex items-center gap-1.5">
            <Button variant="secondary" size="sm" disabled={!hasPrev || loading}
              onClick={()=>setOffset(Math.max(0, offset - limit))}>
              <Icon name="chevronLeft" size={13}/>Prev
            </Button>
            <Button variant="secondary" size="sm" disabled={!hasNext || loading}
              onClick={()=>setOffset(offset + limit)}>
              Next<Icon name="chevronRight" size={13}/>
            </Button>
          </div>
        </div>
      )}

      {/* Request detail modal */}
      <Modal
        open={selected!=null}
        onClose={()=>{ setSelected(null); setDetail(null) }}
        width="max-w-3xl"
        title={`Request ${selected ? selected.id.slice(0,8) : ''}`}
      >
        <div className="max-h-[65vh] overflow-y-auto pr-1">
          {loadingDetail
            ? <div className="py-10 text-center text-muted text-sm flex items-center justify-center gap-2"><Icon name="refresh" size={15} className="animate-spin"/>Loading…</div>
            : detail ? (
              <>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted font-mono pb-3 border-b border-stone">
                  <span>ID {selected.id.slice(0,8)}<CopyButton value={selected.id} label="Copy request id"/></span>
                  <span>{selected.model}</span>
                  <span>{selected.endpoint}{selected.is_stream?' · stream':''}</span>
                  <span className="tabular-nums">{new Date(selected.created_at).toLocaleString()}</span>
                </div>
                <div className="mt-4">
                  <DetailBody detail={detail} selected={selected} keyMap={keyMap}/>
                </div>
              </>
            ) : (
              <EmptyState icon="logs" title="No detail available"/>
            )}
        </div>
        <div className="flex justify-end gap-2 mt-5 pt-4 border-t border-stone">
          <CopyButton size="md" value={rawJson} label="Copy JSON"/>
          <Button variant="secondary" onClick={()=>{setSelected(null); setDetail(null)}}>Close</Button>
        </div>
      </Modal>
    </div>
  )
}
