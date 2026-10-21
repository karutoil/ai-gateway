/**
 * Request inspector: metrics for the last call, raw request/response payloads
 * and the per-conversation call history.
 */
import { Badge, Button, Icon, useToast } from '../../components/ui'
import { fmtBytes, fmtTime } from '../../lib/playground/format'
import { extractUsageFromObject } from '../../lib/playground/protocol'
import type { Call, Session } from '../../lib/playground/types'

function lastCall(session?: Session): Call | undefined { return session?.calls[0] }
function lastStatus(session?: Session): string { return session?.calls[0]?.status || '' }
function lastLatency(session?: Session): number { return session?.calls[0]?.latencyMs ?? 0 }
function lastTtft(session?: Session): number { return session?.calls[0]?.ttftMs ?? 0 }

export type InspectorProps = {
  tab: 'metrics' | 'request' | 'response' | 'history'
  setTab: (t: 'metrics' | 'request' | 'response' | 'history') => void
  data: { request: any; response: any; headers?: Record<string, string>; at?: number } | null
  running: boolean
  session: Session | undefined
  onClose: () => void
  onPick: (c: Call) => void
  toast: ReturnType<typeof useToast>
  /** False when the selected call was restored without its payload. */
  payloadRetained: boolean
}

export function Inspector({ tab, setTab, data, running, session, onClose, onPick, toast, payloadRetained }: InspectorProps) {
  const res = data?.response
  const usage = extractUsageFromObject(res)
  const calls = session?.calls || []

  const tabs: { id: typeof tab; label: string }[] = [
    { id: 'metrics', label: 'Metrics' },
    { id: 'request', label: 'Request' },
    { id: 'response', label: 'Response' },
    { id: 'history', label: `History${calls.length ? ` (${calls.length})` : ''}` },
  ]

  const copy = (v: any) => navigator.clipboard?.writeText(JSON.stringify(v, null, 2))
    .then(() => toast.success('Copied JSON'), () => toast.error('Copy failed'))

  return (
    <>
      <div className="shrink-0 h-11 px-4 border-b border-stone flex items-center gap-2">
        <span className="text-[13px] font-semibold -tracking-tight">Request inspector</span>
        <Badge tone={running ? 'info' : 'neutral'} dot={running}>{running ? 'running' : 'idle'}</Badge>
        <Button size="sm" variant="ghost" className="ml-auto !px-1.5" onClick={onClose} aria-label="Close inspector">
          <Icon name="x" size={14} />
        </Button>
      </div>

      <div className="shrink-0 flex border-b border-stone overflow-x-auto hide-scrollbar" role="tablist" aria-label="Inspector sections">
        {tabs.map(t => (
          <button key={t.id} onClick={() => setTab(t.id)}
            role="tab" aria-selected={tab === t.id}
            className={`px-3 h-9 text-xs font-medium whitespace-nowrap border-b-2 transition-colors ${
              tab === t.id ? 'border-accent text-paper' : 'border-transparent text-muted hover:text-paper'}`}>
            {t.label}
          </button>
        ))}
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto p-4">
        {tab === 'metrics' && (
          <div className="space-y-4">
            <div className="grid grid-cols-2 gap-2">
              <Stat label="Total tokens" value={usage?.total !== undefined ? usage.total.toLocaleString() : '—'} sub={usage ? 'from response usage' : 'no request yet'} />
              <Stat label="Total latency" value={data?.at ? `${lastLatency(session)}ms` : '—'} sub={data?.at ? 'last call' : 'no request yet'} />
              <Stat label="Time to first token" value={lastTtft(session) ? `${lastTtft(session)}ms` : '—'} sub="streaming only" />
              <Stat label="Status" value={lastStatus(session) || '—'} sub={data?.at ? fmtTime(data.at) : 'no request yet'} />
            </div>

            <div className="h-px bg-stone" />
            <div className="text-[11px] font-semibold uppercase tracking-wider text-muted">Execution</div>
            <KV k="Endpoint" v={lastCall(session)?.path || '—'} />
            <KV k="Model" v={lastCall(session)?.model || '—'} />
            <KV k="Cache" v={lastCall(session)?.cache || 'not reported'} />
            <KV k="Fallback" v={lastCall(session)?.fallback || 'none'} />

            {data?.headers && Object.keys(data.headers).length > 0 && (
              <>
                <div className="h-px bg-stone" />
                <div className="text-[11px] font-semibold uppercase tracking-wider text-muted">Response headers</div>
                <pre className="rounded-lg border border-stone bg-ink text-cream/90 px-3 py-2.5 text-[10px] font-mono overflow-x-auto whitespace-pre-wrap break-all">
                  {Object.entries(data.headers).map(([k, v]) => `${k}: ${v}`).join('\n')}
                </pre>
              </>
            )}

            <div className="h-px bg-stone" />
            <div className="text-[11px] font-semibold uppercase tracking-wider text-muted">Recorded elsewhere</div>
            <p className="text-xs text-muted leading-relaxed">
              Queue time, retries and billed cost are not part of the /v1 response. The gateway records them per request
              under <b>Requests</b> and <b>Costs</b>; open a call from the History tab to inspect its exact payload.
            </p>
          </div>
        )}

        {tab === 'request' && (
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-2">
              <span className="text-[11px] font-semibold uppercase tracking-wider text-muted">Payload</span>
              {data?.request && (
                <Button size="sm" variant="ghost" className="!px-1.5 !text-[11px]" onClick={() => copy(data.request)}>
                  <Icon name="copy" size={11} /> Copy
                </Button>
              )}
            </div>
            <pre className="rounded-lg border border-stone bg-ink text-cream/90 px-3 py-2.5 text-[10px] font-mono overflow-x-auto whitespace-pre max-h-[60vh]">
              {data?.request ? JSON.stringify(redactRequest(data.request), null, 2)
                : payloadRetained ? 'Send a message to capture the request payload.'
                : 'This call was restored from a previous page load — its payload is not retained. Send a new message to capture one.'}
            </pre>
          </div>
        )}

        {tab === 'response' && (
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-2">
              <span className="text-[11px] font-semibold uppercase tracking-wider text-muted">Response</span>
              {data?.response && (
                <Button size="sm" variant="ghost" className="!px-1.5 !text-[11px]" onClick={() => copy(data.response)}>
                  <Icon name="copy" size={11} /> Copy
                </Button>
              )}
            </div>
            <pre className="rounded-lg border border-stone bg-ink text-cream/90 px-3 py-2.5 text-[10px] font-mono overflow-x-auto whitespace-pre max-h-[60vh]">
              {data?.response ? JSON.stringify(data.response, null, 2)
                : payloadRetained ? 'No response captured yet.'
                : 'This call was restored from a previous page load — its response body is not retained.'}
            </pre>
          </div>
        )}

        {tab === 'history' && (
          <div className="space-y-2">
            {calls.length === 0 && <div className="text-xs text-muted">No calls in this conversation yet.</div>}
            {calls.map(c => (
              <button key={c.id} onClick={() => onPick(c)}
                className="w-full text-left rounded-lg border border-stone bg-app/50 hover:border-accent/40 p-2.5 transition-colors">
                <div className="flex items-center gap-2">
                  <Badge tone={c.status === 'ok' ? 'good' : c.status === 'aborted' ? 'warn' : 'bad'}>{c.status}</Badge>
                  <span className="font-mono text-[10px] text-muted ml-auto">{fmtTime(c.at)}</span>
                </div>
                <div className="mt-1.5 font-mono text-[11px] text-paper truncate">{c.model}</div>
                <div className="font-mono text-[10px] text-muted truncate">{c.path}</div>
                <div className="mt-1 flex gap-3 font-mono text-[10px] text-muted">
                  {c.latencyMs !== undefined && <span>{c.latencyMs}ms</span>}
                  {c.ttftMs !== undefined && <span>ttft {c.ttftMs}ms</span>}
                </div>
              </button>
            ))}
          </div>
        )}
      </div>
    </>
  )
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-lg border border-stone bg-app/50 p-2.5">
      <div className="text-[10px] text-muted font-medium">{label}</div>
      <div className="text-base font-semibold tabular-nums -tracking-tight mt-0.5 truncate">{value}</div>
      {sub && <div className="text-[10px] text-muted mt-0.5 truncate">{sub}</div>}
    </div>
  )
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-xs py-0.5">
      <span className="text-muted shrink-0">{k}</span>
      <span className="font-mono text-[11px] text-paper truncate text-right">{v}</span>
    </div>
  )
}

/** Strip base64 blobs so the payload panel stays readable. */
export function redactRequest(req: any): any {
  const clone = (v: any): any => {
    if (Array.isArray(v)) return v.map(clone)
    if (v && typeof v === 'object') {
      const out: any = {}
      for (const [k, val] of Object.entries(v)) out[k] = clone(val)
      return out
    }
    if (typeof v === 'string' && v.startsWith('data:') && v.length > 120) {
      return `${v.slice(0, 48)}…[${fmtBytes(Math.round(v.length * 0.75))} inlined]`
    }
    return v
  }
  return clone(req)
}
