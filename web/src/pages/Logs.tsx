import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, logsParamsToSearch } from '../lib/api'
import { useFiltersState } from '../lib/useQueryState'
import { loadSavedViews, saveView, removeSavedView, describeViewParams, type SavedView } from '../lib/savedViews'
import {
  Card, Button, Badge, Icon, Input, PageHeader, SegmentedControl,
  TableShell, Th, Td, TableSkeleton, EmptyState, Modal, CopyButton, Field,
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

/* ---------------- Structured message parsing ---------------- */

type ChatMessage = {
  role: string
  text: string
  toolCalls: { id: string; name: string; args: string }[]
  toolCallId?: string
  images: string[]
  thinking?: string
}

/** Extract readable text from chat content: string or typed-part array. */
function contentToText(v:any): string {
  if (typeof v === 'string') return v
  if (Array.isArray(v)) {
    return v.map((p:any)=>{
      if (typeof p === 'string') return p
      if (p?.type === 'text' || p?.type === 'input_text' || p?.type === 'output_text') return String(p.text ?? '')
      return ''
    }).join('')
  }
  return ''
}

/** Collect image references from content parts for placeholder chips. */
function contentImages(v:any): string[] {
  if (!Array.isArray(v)) return []
  const out: string[] = []
  for (const p of v) {
    const src = p?.image_url?.url || p?.source?.data || (p?.type === 'image_url' ? p?.image_url : null)
    if (typeof src === 'string') {
      const mime = p?.source?.media_type || (src.startsWith('data:') ? src.slice(5, src.indexOf(';')) : 'image')
      out.push(mime)
    }
  }
  return out
}

/** Parse a request body (chat messages / anthropic system+messages) into structured messages. */
function parseRequestMessages(raw:string|undefined|null): ChatMessage[] {
  if (!raw) return []
  let body:any
  try { body = JSON.parse(raw) } catch { return [] }
  const msgs:any[] = Array.isArray(body?.messages) ? body.messages : []
  const out: ChatMessage[] = []
  if (typeof body?.system === 'string' && body.system) {
    out.push({ role: 'system', text: body.system, toolCalls: [], images: [] })
  }
  for (const m of msgs) {
    if (!m || typeof m !== 'object') continue
    out.push({
      role: String(m.role ?? 'user'),
      text: contentToText(m.content),
      toolCalls: Array.isArray(m.tool_calls) ? m.tool_calls.map((tc:any)=>({
        id: String(tc?.id ?? ''), name: String(tc?.function?.name ?? ''), args: String(tc?.function?.arguments ?? ''),
      })) : [],
      toolCallId: typeof m.tool_call_id === 'string' ? m.tool_call_id : undefined,
      images: contentImages(m.content),
    })
  }
  return out
}

/** Parse a response body (chat completion / anthropic message / assembled stream capture) into messages. */
function parseResponseMessages(raw:string|undefined|null): ChatMessage[] {
  if (!raw) return []
  let body:any
  try { body = JSON.parse(raw) } catch { return [] }
  const out: ChatMessage[] = []
  // chat.completion / assembled stream capture
  const msg = body?.choices?.[0]?.message
  if (msg) {
    out.push({
      role: String(msg.role ?? 'assistant'),
      text: contentToText(msg.content),
      toolCalls: Array.isArray(msg.tool_calls) ? msg.tool_calls.map((tc:any)=>({
        id: String(tc?.id ?? ''), name: String(tc?.function?.name ?? ''), args: String(tc?.function?.arguments ?? ''),
      })) : [],
      images: [],
    })
    return out
  }
  // anthropic message
  if (body?.type === 'message' || Array.isArray(body?.content)) {
    let thinking = ''
    let text = ''
    const toolCalls: ChatMessage['toolCalls'] = []
    for (const b of (Array.isArray(body.content) ? body.content : [])) {
      if (b?.type === 'text') text += String(b.text ?? '')
      else if (b?.type === 'thinking') thinking += String(b.thinking ?? '')
      else if (b?.type === 'tool_use') toolCalls.push({ id: String(b.id ?? ''), name: String(b.name ?? ''), args: JSON.stringify(b.input ?? {}) })
    }
    out.push({ role: 'assistant', text, thinking: thinking || undefined, toolCalls, images: [] })
    return out
  }
  // responses API output array
  if (Array.isArray(body?.output)) {
    for (const item of body.output) {
      if (item?.type === 'message') {
        out.push({ role: 'assistant', text: contentToText(item.content), toolCalls: [], images: [] })
      } else if (item?.type === 'function_call') {
        out.push({ role: 'assistant', text: '', toolCalls: [{ id: String(item.call_id ?? item.id ?? ''), name: String(item.name ?? ''), args: String(item.arguments ?? '') }], images: [] })
      }
    }
    return out
  }
  return out
}

const ROLE_TONES: Record<string, 'good'|'warn'|'bad'|'neutral'> = {
  system: 'neutral', user: 'warn', assistant: 'good', tool: 'bad',
}

/** One structured chat message: role badge, text, tool-call cards, image chips. */
function MessageView({ m }: { m: ChatMessage }) {
  const tone = ROLE_TONES[m.role] ?? 'neutral'
  return (
    <div className="rounded-lg border border-stone bg-app p-2.5">
      <div className="flex items-center gap-2">
        <Badge tone={tone}>{m.role}</Badge>
        {m.toolCallId && <span className="font-mono text-[10px] text-muted">tool_call_id: {m.toolCallId}</span>}
        {m.images.map((mime, i) => (
          <span key={i} className="text-[10px] font-mono text-muted border border-stone rounded px-1.5 py-0.5">[image: {mime}]</span>
        ))}
      </div>
      {m.thinking && (
        <pre className="mt-2 font-mono text-[11px] text-muted whitespace-pre-wrap break-words border-l-2 border-stone pl-2">{m.thinking}</pre>
      )}
      {m.text && <pre className="mt-1.5 text-xs whitespace-pre-wrap break-words font-sans">{m.text.length > 4000 ? m.text.slice(0, 4000) + '…' : m.text}</pre>}
      {m.toolCalls.map((tc, i)=>(
        <div key={i} className="mt-2 rounded-md border border-teal/30 bg-teal/5 p-2">
          <div className="flex items-center gap-2 text-[11px] font-mono text-teal">
            <Icon name="route" size={12}/> {tc.name || 'tool_call'}
            {tc.id && <span className="text-muted">{tc.id}</span>}
          </div>
          {tc.args && (
            <pre className="mt-1 font-mono text-[11px] whitespace-pre-wrap break-all text-muted">
              {(() => { try { return JSON.stringify(JSON.parse(tc.args), null, 2) } catch { return tc.args } })()}
            </pre>
          )}
        </div>
      ))}
    </div>
  )
}

/** Structured request+response message panes for the detail modal. */
function MessagePanes({ requestBody, responseBody }: { requestBody?: string; responseBody?: string }) {
  const reqMsgs = parseRequestMessages(requestBody)
  const respMsgs = parseResponseMessages(responseBody)
  if (reqMsgs.length === 0 && respMsgs.length === 0) return <></>
  return (
    <div className="grid md:grid-cols-2 gap-3">
      {reqMsgs.length > 0 && (
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="flex items-center justify-between mb-2">
            <div className="text-xs text-muted">Request messages</div>
            <CopyButton value={requestBody || ''} label="Copy raw"/>
          </div>
          <div className="space-y-2 max-h-80 overflow-y-auto">
            {reqMsgs.map((m, i)=><MessageView key={i} m={m}/>)}
          </div>
        </div>
      )}
      {respMsgs.length > 0 && (
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="flex items-center justify-between mb-2">
            <div className="text-xs text-muted">Response messages</div>
            <CopyButton value={responseBody || ''} label="Copy raw"/>
          </div>
          <div className="space-y-2 max-h-80 overflow-y-auto">
            {respMsgs.map((m, i)=><MessageView key={i} m={m}/>)}
          </div>
        </div>
      )}
    </div>
  )
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

      {/* Usage metadata: finish reason + cache/reasoning token split */}
      {((detail.log?.finish_reason ?? (detail as any).finish_reason) || (detail.log?.cache_read_tokens ?? (detail as any).cache_read_tokens) || (detail.log?.cache_write_tokens ?? (detail as any).cache_write_tokens) || (detail.log?.reasoning_tokens ?? (detail as any).reasoning_tokens)) && (
        <div className="rounded-xl border border-stone bg-raised p-3">
          <div className="text-xs text-muted mb-2">Usage metadata</div>
          <div className="flex flex-wrap gap-2">
            {(detail.log?.finish_reason ?? (detail as any).finish_reason) && (
              <Badge tone="neutral">finish: {detail.log?.finish_reason ?? (detail as any).finish_reason}</Badge>
            )}
            {!!(detail.log?.cache_read_tokens ?? (detail as any).cache_read_tokens) && (
              <Badge tone="good">cache read: {(detail.log?.cache_read_tokens ?? (detail as any).cache_read_tokens).toLocaleString()}</Badge>
            )}
            {!!(detail.log?.cache_write_tokens ?? (detail as any).cache_write_tokens) && (
              <Badge tone="warn">cache write: {(detail.log?.cache_write_tokens ?? (detail as any).cache_write_tokens).toLocaleString()}</Badge>
            )}
            {!!(detail.log?.reasoning_tokens ?? (detail as any).reasoning_tokens) && (
              <Badge tone="neutral">reasoning: {(detail.log?.reasoning_tokens ?? (detail as any).reasoning_tokens).toLocaleString()}</Badge>
            )}
          </div>
          {Array.isArray((detail as any).fallback_chain) && (detail as any).fallback_chain.length > 0 && (
            <div className="mt-2 space-y-1">
              <div className="text-xs text-muted">Fallback chain — attempted before success:</div>
              {(detail as any).fallback_chain.map((a:any, i:number)=>(
                <div key={i} className="font-mono text-[11px] text-muted flex items-center gap-2">
                  <Icon name="alert" size={11}/>{a.name || a.provider_id} <Badge tone={statusTone(Number(a.status)||500)}>{String(a.status)}</Badge>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Structured message view — the actual conversation */}
      <MessagePanes requestBody={detail.log?.request_body || (detail as any).request_body} responseBody={detail.log?.response_body || (detail as any).response_body}/>

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

/** Filter state shape — each field maps 1:1 to a URL query param. */
type LogFilters = {
  q: string
  status: '' | 'failed' | string
  since: '1h' | '24h' | '7d' | '30d' | string
  key_id: string
  provider_id: string
  model: string
  stream: '' | 'true' | 'false'
  min_latency_ms: string
  max_latency_ms: string
  has_error: '' | 'true'
  search_bodies: '' | 'true'
}

const FILTER_DEFAULTS: LogFilters = {
  q: '', status: '', since: '24h', key_id: '', provider_id: '', model: '',
  stream: '', min_latency_ms: '', max_latency_ms: '', has_error: '', search_bodies: '',
}

/** Which filter fields are "advanced" (hidden behind More filters). */
const ADVANCED_FIELDS: (keyof LogFilters)[] = ['min_latency_ms', 'max_latency_ms', 'search_bodies', 'model']

/** True when a field differs from its default (drives chips + active count). */
function filterActive(f: LogFilters, k: keyof LogFilters): boolean {
  return f[k] !== FILTER_DEFAULTS[k]
}

type GroupRowT = {
  group: string
  name?: string
  requests: number
  tokens: number
  cost: number
  failed: number
  avg_latency_ms: number
  p50_latency_ms: number
  p95_latency_ms: number
}

const GROUP_DIMS = [
  { value: 'model', label: 'Model' },
  { value: 'key', label: 'Key' },
  { value: 'provider', label: 'Provider' },
  { value: 'endpoint', label: 'Endpoint' },
  { value: 'status', label: 'Status' },
  { value: 'error', label: 'Errors' },
] as const

export default function Logs(){
  const navigate = useNavigate()
  const [filters, setFilters] = useFiltersState<LogFilters>(FILTER_DEFAULTS)
  const [logs, setLogs] = useState<any[]>([])
  const [total, setTotal] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [limit, setLimit] = useState(100)
  const [offset, setOffset] = useState(0)
  const [keyMap, setKeyMap] = useState<Record<string,string>>({})
  const [keyIdMap, setKeyIdMap] = useState<Record<string,string>>({})
  const [providers, setProviders] = useState<{id:string;name:string}[]>([])
  const [selected, setSelected] = useState<any|null>(null)
  const [detail, setDetail] = useState<any|null>(null)
  const [loadingDetail, setLoadingDetail] = useState(false)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [showReports, setShowReports] = useState(false)
  const [showSaveView, setShowSaveView] = useState(false)
  const [savedViews, setSavedViews] = useState<SavedView[]>(() => loadSavedViews())
  const [viewName, setViewName] = useState('')
  const [exportNote, setExportNote] = useState('')

  // Reports panel state
  const [groupBy, setGroupBy] = useState<string>('model')
  const [groupRows, setGroupRows] = useState<GroupRowT[]>([])
  const [groupLoading, setGroupLoading] = useState(false)

  // Debounced search: q updates the URL 300ms after typing stops.
  const [qInput, setQInput] = useState(filters.q)
  useEffect(() => {
    const t = setTimeout(() => { if (qInput !== filters.q) setFilters({ q: qInput }) }, 300)
    return () => clearTimeout(t)
  }, [qInput]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { setQInput(filters.q) }, [filters.q])

  // Server-side query from URL-synced filters.
  const apiParams = () => {
    const p: Record<string, unknown> = {
      limit, since: filters.since,
    }
    if (filters.q) p.q = filters.q
    if (filters.status) p.status = filters.status
    if (filters.key_id) p.key_id = filters.key_id
    if (filters.provider_id) p.provider_id = filters.provider_id
    if (filters.model) p.model = filters.model
    if (filters.stream) p.stream = filters.stream
    if (filters.has_error) p.has_error = filters.has_error
    if (filters.search_bodies) p.search_bodies = filters.search_bodies
    if (filters.min_latency_ms) p.min_latency_ms = Number(filters.min_latency_ms)
    if (filters.max_latency_ms) p.max_latency_ms = Number(filters.max_latency_ms)
    return p
  }

  const loadLogs = (opts?: { resetOffset?: boolean })=>{
    const nextOffset = opts?.resetOffset ? 0 : offset
    setLoading(true)
    setLoadError('')
    api.logsQuery({ ...apiParams(), offset: nextOffset } as any)
      .then(({ rows, total })=>{
        setLogs(rows)
        setTotal(total)
        if (opts?.resetOffset) setOffset(0)
      })
      .catch((e:any)=> setLoadError(e?.message || String(e)))
      .finally(()=> setLoading(false))
  }

  useEffect(()=>{ loadLogs({ resetOffset: true }) }, [limit, JSON.stringify(filters)])

  useEffect(()=>{
    api.keys.list().then(keys=>{
      const byPrefix:Record<string,string> = {}
      const byId:Record<string,string> = {}
      for(const k of keys){ byPrefix[k.prefix]=k.name; byId[k.id]=k.name }
      setKeyMap(byPrefix); setKeyIdMap(byId)
    }).catch(()=>{})
    api.providers.list().then((list:any)=>{
      const arr = Array.isArray(list) ? list : list?.data ?? []
      setProviders(arr.map((p:any)=>({ id: p.id, name: p.name })))
    }).catch(()=>{})
  },[])

  const loadGroup = () => {
    setGroupLoading(true)
    const { limit: _l, offset: _o, ...rest } = apiParams() as any
    api.logsGroup({ ...rest, group_by: groupBy as any, range: filters.since, limit: 20 })
      .then(r => setGroupRows(r.rows || []))
      .catch(() => setGroupRows([]))
      .finally(() => setGroupLoading(false))
  }
  useEffect(() => { if (showReports) loadGroup() }, [showReports, groupBy, JSON.stringify(filters)]) // eslint-disable-line react-hooks/exhaustive-deps

  const doExport = async () => {
    setExportNote('')
    try {
      const { filename, truncated } = await api.logsExport(apiParams() as any)
      setExportNote(truncated ? `Exported ${filename} (truncated at server cap)` : `Exported ${filename}`)
      setTimeout(() => setExportNote(''), 5000)
    } catch (e:any) { setExportNote(e?.message || 'export failed') }
  }

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

  // Click-through helpers: navigate to this view filtered on a dimension value.
  const applyFilter = (patch: Partial<LogFilters>) => { setFilters(patch); setOffset(0) }

  const activeFilters = (Object.keys(FILTER_DEFAULTS) as (keyof LogFilters)[])
    .filter(k => filterActive(filters, k))

  const saveCurrentView = () => {
    const params = new URLSearchParams(window.location.search)
    params.delete('limit'); params.delete('offset')
    setSavedViews(saveView(viewName || 'Untitled view', params.toString()))
    setViewName(''); setShowSaveView(false)
  }

  const applySavedView = (v: SavedView) => {
    const params = new URLSearchParams(v.params)
    const patch: Partial<LogFilters> = {}
    for (const k of Object.keys(FILTER_DEFAULTS) as (keyof LogFilters)[]) {
      (patch as any)[k] = params.get(k) ?? FILTER_DEFAULTS[k]
    }
    // since/range aliases: saved views store the range under `since`
    setFilters(patch)
  }

  const rawJson = detail ? JSON.stringify(detail || selected, null, 2) : ''

  return (
    <div className="space-y-4">
      <PageHeader
        title="Request Logs"
        description="Every proxied call with status, latency, tokens and cost."
        actions={
          <>
            <Button variant="secondary" onClick={doExport}>
              <Icon name="download" size={15}/>Export CSV
            </Button>
            <Button variant="primary" onClick={()=>loadLogs()} disabled={loading}>
              <Icon name="refresh" size={15}/>Refresh
            </Button>
          </>
        }
      />

      {exportNote && <div className="text-xs text-teal">{exportNote}</div>}

      {/* Saved views chips */}
      {savedViews.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 -mt-1">
          <span className="text-xs text-muted uppercase tracking-wide mr-1">Saved views</span>
          {savedViews.map(v => (
            <span key={v.id} className="inline-flex items-center gap-1 rounded-full border border-stone bg-raised px-2.5 py-1 text-xs group hover:border-teal/50 transition-colors">
              <button onClick={()=>applySavedView(v)} title={describeViewParams(v.params)} className="focus:outline-none">
                <span className="font-medium">{v.name}</span>
                <span className="text-muted ml-1.5">{describeViewParams(v.params)}</span>
              </button>
              <button onClick={()=>setSavedViews(removeSavedView(v.id))} className="text-muted hover:text-red-400 focus:outline-none" title="Delete view">
                <Icon name="x" size={11}/>
              </button>
            </span>
          ))}
        </div>
      )}

      {/* Totals strip — aggregates cover the currently loaded rows only */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted font-mono tabular-nums -mt-2">
        <span>{logs.length} loaded{total != null ? ` of ${total.toLocaleString()} matching` : ''}</span>
        <span title="Sum over the currently loaded rows">{logs.reduce((a,l)=>a+(l.total_tokens||0),0).toLocaleString()} tokens (loaded)</span>
        <span title="Sum over the currently loaded rows">${logs.reduce((a,l)=>a+(l.cost_usd||0),0).toFixed(4)} (loaded)</span>
        <span>avg TTFT {logs.length ? Math.round(logs.reduce((a,l)=>a+(l.ttft_ms||0),0)/logs.length) : 0}ms</span>
      </div>

      {/* Filter bar — every control is server-side and URL-synced */}
      <Card className="!p-3 space-y-2.5">
        <div className="flex flex-col sm:flex-row sm:items-center gap-2">
          <div className="relative flex-1 min-w-[180px]">
            <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={15}/></span>
            <Input value={qInput} onChange={e=>setQInput(e.target.value)} placeholder="Search model, endpoint, error text…" className="pl-9"/>
          </div>
          <select value={filters.status} onChange={e=>applyFilter({ status: e.target.value })}
            className="bg-app border border-stone rounded-lg px-2 h-9 text-sm focus:outline-none focus:border-teal/60">
            <option value="">Any status</option>
            <option value="failed">Failed only</option>
          </select>
          <select value={filters.key_id} onChange={e=>applyFilter({ key_id: e.target.value })}
            className="bg-app border border-stone rounded-lg px-2 h-9 text-sm max-w-[150px] focus:outline-none focus:border-teal/60">
            <option value="">Any key</option>
            {Object.entries(keyIdMap).map(([id,name]) => <option key={id} value={id}>{name}</option>)}
          </select>
          <select value={filters.provider_id} onChange={e=>applyFilter({ provider_id: e.target.value })}
            className="bg-app border border-stone rounded-lg px-2 h-9 text-sm max-w-[150px] focus:outline-none focus:border-teal/60">
            <option value="">Any provider</option>
            {providers.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
          <select value={filters.since} onChange={e=>applyFilter({ since: e.target.value })}
            className="bg-app border border-stone rounded-lg px-2 h-9 text-sm focus:outline-none focus:border-teal/60">
            {SINCE_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SegmentedControl<''|'true'|'false'>
            options={[{value:'',label:'Any'},{value:'true',label:'Stream'},{value:'false',label:'Non-stream'}]}
            value={filters.stream} onChange={v=>applyFilter({ stream: v })}
          />
          <label className="flex items-center gap-1.5 text-xs text-muted cursor-pointer select-none">
            <input type="checkbox" checked={filters.has_error==='true'} onChange={e=>applyFilter({ has_error: e.target.checked ? 'true' : '' })}
              className="w-3.5 h-3.5 accent-teal rounded"/>
            Has error
          </label>
          <label className="flex items-center gap-1.5 text-xs text-muted cursor-pointer select-none">
            <input type="checkbox" checked={filters.search_bodies==='true'} onChange={e=>applyFilter({ search_bodies: e.target.checked ? 'true' : '' })}
              className="w-3.5 h-3.5 accent-teal rounded"/>
            Search bodies
          </label>
          <button onClick={()=>setShowAdvanced(s=>!s)} className="text-xs text-muted hover:text-paper transition-colors flex items-center gap-1 focus:outline-none">
            <Icon name="chevronDown" size={13} className={`transition-transform ${showAdvanced?'rotate-180':''}`}/>
            More filters {activeFilters.some(k=>ADVANCED_FIELDS.includes(k)) ? '•' : ''}
          </button>
          <div className="flex-1"/>
          <Button variant="secondary" size="sm" onClick={()=>setShowSaveView(true)}><Icon name="plus" size={13}/>Save view</Button>
          <Button variant="secondary" size="sm" onClick={()=>{
            navigator.clipboard?.writeText(window.location.href).then(()=>setExportNote('View link copied'), ()=>{})
          }}><Icon name="key" size={13}/>Share</Button>
        </div>
        {showAdvanced && (
          <div className="flex flex-wrap items-center gap-2 pt-1 border-t border-stone">
            <Input value={filters.model} onChange={e=>applyFilter({ model: e.target.value })} placeholder="Model (substring)" className="max-w-[180px] h-8 text-xs"/>
            <Input value={filters.min_latency_ms} onChange={e=>applyFilter({ min_latency_ms: e.target.value.replace(/\D/g,'') })} placeholder="Min latency ms" className="max-w-[120px] h-8 text-xs" inputMode="numeric"/>
            <Input value={filters.max_latency_ms} onChange={e=>applyFilter({ max_latency_ms: e.target.value.replace(/\D/g,'') })} placeholder="Max latency ms" className="max-w-[120px] h-8 text-xs" inputMode="numeric"/>
          </div>
        )}
      </Card>

      {/* Active filter chips */}
      {activeFilters.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 -mt-1">
          <span className="text-xs text-muted">Filtered by</span>
          {activeFilters.map(k => (
            <Badge key={k} tone="info">
              {k === 'key_id' ? (keyIdMap[filters.key_id] ? `key: ${keyIdMap[filters.key_id]}` : 'key') :
               k === 'provider_id' ? (providers.find(p=>p.id===filters.provider_id)?.name || 'provider') :
               k === 'q' ? `search: ${filters.q}` :
               `${k.replace(/_/g,' ')}: ${filters[k]}`}
              <button onClick={()=>applyFilter({ [k]: FILTER_DEFAULTS[k] } as any)} className="ml-1 hover:text-red-300 focus:outline-none"><Icon name="x" size={10}/></button>
            </Badge>
          ))}
          <button onClick={()=>setFilters({...FILTER_DEFAULTS})} className="text-xs text-muted hover:text-paper focus:outline-none">Clear all</button>
        </div>
      )}

      {/* Reports (group-by) panel */}
      <Card className="!p-3">
        <button onClick={()=>setShowReports(s=>!s)} className="w-full flex items-center justify-between text-sm font-medium focus:outline-none">
          <span className="flex items-center gap-2"><Icon name="chart" size={15} className="text-teal"/>Reports</span>
          <Icon name="chevronDown" size={15} className={`text-muted transition-transform ${showReports?'rotate-180':''}`}/>
        </button>
        {showReports && (
          <div className="mt-3 space-y-3">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs text-muted">Group by</span>
              <SegmentedControl<string>
                options={GROUP_DIMS.map(d=>({ value: d.value, label: d.label }))}
                value={groupBy} onChange={setGroupBy}
              />
              <span className="text-xs text-muted ml-auto">window {filters.since} · respects filters above</span>
            </div>
            {groupLoading ? (
              <div className="py-6 text-center text-sm text-muted">Aggregating…</div>
            ) : groupRows.length === 0 ? (
              <div className="py-6 text-center text-sm text-muted">No rows match the current filters.</div>
            ) : (
              <div className="overflow-x-auto rounded-lg border border-stone">
                <table className="w-full text-xs">
                  <thead>
                    <tr>
                      <Th className="text-left">{GROUP_DIMS.find(d=>d.value===groupBy)?.label || groupBy}</Th>
                      <Th className="text-right">Requests</Th>
                      <Th className="text-right">Failed</Th>
                      <Th className="text-right">Tokens</Th>
                      <Th className="text-right">Cost</Th>
                      <Th className="text-right">Avg</Th>
                      <Th className="text-right">p50</Th>
                      <Th className="text-right">p95</Th>
                    </tr>
                  </thead>
                  <tbody>
                    {groupRows.map(g => (
                      <tr key={g.group}
                        onClick={()=>{
                          // Click-through: filter the log list on this group's dimension.
                          if (groupBy === 'model') applyFilter({ model: g.group })
                          else if (groupBy === 'key') { const k = Object.entries(keyMap).find(([,n])=>n===g.name||true); applyFilter({ key_id: g.name ? (Object.entries(keyIdMap).find(([,n])=>n===g.name)?.[0] ?? '') : '' }) }
                          else if (groupBy === 'provider') applyFilter({ provider_id: g.group === '(none)' ? '' : g.group })
                          else if (groupBy === 'status') applyFilter({ status: g.group === '(none)' ? '' : g.group })
                          else if (groupBy === 'error') applyFilter({ has_error: g.group === 'error' ? 'true' : '' })
                          else applyFilter({ model: g.group })
                        }}
                        className="hover:bg-raised/50 cursor-pointer transition-colors">
                        <Td className="font-mono">{g.name || g.group}</Td>
                        <Td className="text-right tabular-nums">{g.requests.toLocaleString()}</Td>
                        <Td className={`text-right tabular-nums ${g.failed>0?'text-red-400':'text-muted'}`}>{g.failed||'—'}</Td>
                        <Td className="text-right tabular-nums">{g.tokens ? g.tokens.toLocaleString() : '—'}</Td>
                        <Td className="text-right tabular-nums">{g.cost ? `$${g.cost.toFixed(4)}` : '—'}</Td>
                        <Td className="text-right tabular-nums">{g.avg_latency_ms ? `${Math.round(g.avg_latency_ms)}ms` : '—'}</Td>
                        <Td className="text-right tabular-nums">{g.p50_latency_ms ? `${g.p50_latency_ms}ms` : '—'}</Td>
                        <Td className="text-right tabular-nums">{g.p95_latency_ms ? `${g.p95_latency_ms}ms` : '—'}</Td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
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
          ) : logs.length===0 ? (
            <tbody>
              <tr><td colSpan={7}>
                <EmptyState
                  icon="logs"
                  title={offset > 0 ? 'No requests on this page' : 'No requests match these filters'}
                  hint={offset > 0
                    ? 'Go back a page, or widen the time range / clear filters.'
                    : 'Adjust the filters, or send your first call from the Playground.'}
                  action={logs.length===0 && offset === 0
                    ? <Button variant="secondary" onClick={()=>setFilters({...FILTER_DEFAULTS})}>Clear filters</Button>
                    : undefined}
                />
              </td></tr>
            </tbody>
          ) : (
            <tbody>
              {logs.map(l=>(
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
            {total != null ? `${total.toLocaleString()} request(s) matching` : ''}
            {' '}· rows {offset + 1}–{offset + logs.length}
          </span>
          <div className="flex items-center gap-1.5">
            <Button variant="secondary" size="sm" disabled={offset === 0 || loading}
              onClick={()=>setOffset(Math.max(0, offset - limit))}>
              <Icon name="chevronLeft" size={13}/>Prev
            </Button>
            <Button variant="secondary" size="sm" disabled={total != null ? offset + logs.length >= total : logs.length < limit}
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

      {/* Save current view modal */}
      <Modal open={showSaveView} onClose={()=>setShowSaveView(false)} title="Save current view" width="max-w-sm">
        <Field label="View name" hint="Shown as a one-click chip above the filter bar.">
          <Input value={viewName} onChange={e=>setViewName(e.target.value)} autoFocus
            placeholder="e.g. prod-key failures 24h"
            onKeyDown={e=>{ if(e.key==='Enter' && viewName.trim()) saveCurrentView() }}/>
        </Field>
        {(() => {
          const active = (Object.keys(FILTER_DEFAULTS) as (keyof LogFilters)[]).filter(k=>filterActive(filters,k))
          return (
            <div className="mt-3 text-xs text-muted">
              Saves: {active.length ? active.map(k=>k.replace(/_/g,' ')).join(', ') + ` · ${filters.since}` : `nothing but the ${filters.since} window`}
            </div>
          )
        })()}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={()=>setShowSaveView(false)}>Cancel</Button>
          <Button variant="primary" onClick={saveCurrentView} disabled={!viewName.trim()}>Save</Button>
        </div>
      </Modal>
    </div>
  )
}
