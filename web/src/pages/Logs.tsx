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

export default function Logs(){
  const navigate = useNavigate()
  const [logs, setLogs] = useState<any[]>([])
  const [loading, setLoading] = useState(true)
  const [query, setQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState<'all'|'ok'|'fail'>('all')
  const [keyMap, setKeyMap] = useState<Record<string,string>>({})
  const [selected, setSelected] = useState<any|null>(null)
  const [detail, setDetail] = useState<any|null>(null)
  const [loadingDetail, setLoadingDetail] = useState(false)

  const loadLogs = ()=>{
    setLoading(true)
    api.logs().then(setLogs).catch(()=>{}).finally(()=>setLoading(false))
  }

  useEffect(()=>{ 
    loadLogs()
    api.keys.list().then(keys=>{
      const m:Record<string,string>={}
      for(const k of keys) m[k.prefix]=k.name
      setKeyMap(m)
    }).catch(()=>{})
  },[])

  const openDetail = async (l:any)=>{
    setSelected(l)
    setLoadingDetail(true)
    try{
      const d:any = await (api as any).logsDetail ? (api as any).logsDetail(l.id) : fetch(`/api/logs/${l.id}`, { credentials: 'same-origin' }).then(r=>r.json())
      setDetail(d)
    }catch{
      setDetail({ log: l })
    }finally{ setLoadingDetail(false)}
  }

  // Also support direct fetch via api if not yet added
  const fetchDetail = async (id:string)=>{
    try{
      const res = await fetch(`/api/logs/${id}`, { credentials: 'same-origin' })
      if(res.ok) return await res.json()
    }catch{}
    return null
  }

  // Patch openDetail to use fetchDetail
  const handleRowClick = async (l:any)=>{
    setSelected(l)
    setDetail(null)
    setLoadingDetail(true)
    const d = await fetchDetail(l.id)
    setDetail(d || { log: l })
    setLoadingDetail(false)
  }

  // Client-side filtering (presentation only — no extra endpoint calls).
  const q = query.trim().toLowerCase()
  const visible = logs.filter(l=>{
    if(statusFilter==='ok' && !(l.status>=200 && l.status<400)) return false
    if(statusFilter==='fail' && !(l.status>=400)) return false
    if(!q) return true
    return [l.model, l.endpoint, keyMap[l.key_prefix], String(l.status??'')]
      .some(v => typeof v==='string' && v.toLowerCase().includes(q))
  })

  const totalCost = logs.reduce((acc,l)=> acc + (l.cost_usd||0), 0)
  const totalTokens = logs.reduce((acc,l)=> acc + (l.total_tokens||0), 0)
  const avgTTFT = logs.length ? Math.round(logs.reduce((a,l)=>a+(l.ttft_ms||0),0)/logs.length) : 0
  const avgTPS = logs.length ? (logs.reduce((a,l)=>a + tpsFor(l),0)/logs.length).toFixed(1) : '0'

  const rawJson = detail ? JSON.stringify(detail || selected, null, 2) : ''

  return (
    <div className="space-y-4">
      <PageHeader
        title="Request Logs"
        description="Every proxied call with status, latency, tokens and cost."
        actions={
          <Button variant="primary" onClick={loadLogs} disabled={loading}>
            <Icon name="refresh" size={15}/>Refresh
          </Button>
        }
      />

      {/* Totals strip */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted font-mono tabular-nums -mt-2">
        <span>{logs.length} requests</span>
        <span>{totalTokens.toLocaleString()} tokens</span>
        <span>${totalCost.toFixed(4)}</span>
        <span>avg TTFT {avgTTFT}ms</span>
        <span>avg TPS {avgTPS}</span>
      </div>

      {/* Filter bar (client-side only) */}
      <Card className="!p-3 flex flex-col sm:flex-row sm:items-center gap-2">
        <div className="relative flex-1 min-w-[180px]">
          <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={15}/></span>
          <Input value={query} onChange={e=>setQuery(e.target.value)} placeholder="Filter by model, endpoint, key…" className="pl-9"/>
        </div>
        <SegmentedControl<'all'|'ok'|'fail'>
          options={[{value:'all',label:'All'},{value:'ok',label:'OK'},{value:'fail',label:'Fail'}]}
          value={statusFilter}
          onChange={setStatusFilter}
        />
      </Card>

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
                title={logs.length===0 ? 'No requests yet' : 'No matching requests'}
                hint={logs.length===0
                  ? 'Send your first call from the Playground and it will appear here instantly.'
                  : 'Try a different search term or status filter.'}
                action={logs.length===0
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
