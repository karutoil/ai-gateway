import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  PageHeader, Card, Button, Badge, Icon, Input, TableShell, Th, Td,
  TableSkeleton, EmptyState, ErrorNote,
} from '../components/ui'

type AuditRow = {
  id: string
  actor: string
  action: string
  target_type?: string
  target_id?: string
  meta?: string
  created_at: string
}

const PAGE_SIZES = [25, 50, 100, 250]

/** Render the audit `meta` JSON blob compactly, falling back to the raw string. */
function MetaCell({ meta }: { meta?: string }) {
  if (!meta) return <span className="text-muted/50">—</span>
  let text = meta
  try {
    const j = JSON.parse(meta)
    text = JSON.stringify(j)
  } catch { /* keep raw */ }
  return <span className="font-mono text-[11px] text-muted block truncate max-w-[280px]" title={text}>{text}</span>
}

export default function Audit(){
  const [rows, setRows] = useState<AuditRow[]>([])
  const [total, setTotal] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [limit, setLimit] = useState(50)
  const [offset, setOffset] = useState(0)
  const [actor, setActor] = useState('')
  const [actorInput, setActorInput] = useState('')

  const load = (opts?: { resetOffset?: boolean })=>{
    const nextOffset = opts?.resetOffset ? 0 : offset
    setLoading(true)
    setLoadError('')
    api.audit.list({ limit, offset: nextOffset, actor: actor || undefined })
      .then(({ rows, total })=>{
        setRows(rows)
        setTotal(total)
        if (opts?.resetOffset) setOffset(0)
      })
      .catch((e:any)=> setLoadError(e?.message || String(e)))
      .finally(()=> setLoading(false))
  }

  useEffect(()=>{ load({ resetOffset: true }) }, [limit, actor])

  const page = Math.floor(offset / limit) + 1
  const totalPages = total != null ? Math.max(1, Math.ceil(total / limit)) : null
  const hasPrev = offset > 0
  const hasNext = total != null ? offset + rows.length < total : rows.length === limit

  return (
    <div className="space-y-4">
      <PageHeader
        title="Audit"
        description="Admin-side trail of privileged actions — who did what, to which target, and when."
        actions={
          <Button variant="primary" onClick={()=>load()} disabled={loading}>
            <Icon name="refresh" size={15}/>Refresh
          </Button>
        }
      />

      {loadError && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load the audit trail"
            hint={loadError}
            action={<Button variant="secondary" onClick={()=>load()}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {!loadError && (
        <>
          {/* Filters + pagination controls */}
          <Card className="!p-3 flex flex-col sm:flex-row sm:items-center gap-2">
            <form
              className="relative flex-1 min-w-[180px]"
              onSubmit={(e)=>{ e.preventDefault(); setActor(actorInput.trim()) }}
            >
              <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted pointer-events-none"><Icon name="search" size={15}/></span>
              <Input value={actorInput} onChange={e=>setActorInput(e.target.value)} placeholder="Filter by actor (username)…" className="pl-9"/>
            </form>
            <label className="flex items-center gap-2 text-xs text-muted">
              Rows
              <select
                value={limit}
                onChange={e=>setLimit(Number(e.target.value))}
                className="bg-app border border-stone rounded-lg px-2 h-9 text-sm focus:outline-none focus:border-teal/60"
              >
                {PAGE_SIZES.map(n=> <option key={n} value={n}>{n}</option>)}
              </select>
            </label>
            <div className="flex items-center gap-1.5">
              <Button variant="secondary" size="sm" disabled={!hasPrev || loading} onClick={()=>setOffset(Math.max(0, offset - limit))}>
                <Icon name="chevronLeft" size={13}/> Prev
              </Button>
              <span className="text-xs text-muted tabular-nums px-1">
                {total != null ? <>page {page} / {totalPages} · {total.toLocaleString()} events</> : <>page {page}</>}
              </span>
              <Button variant="secondary" size="sm" disabled={!hasNext || loading} onClick={()=>setOffset(offset + limit)}>
                Next <Icon name="chevronRight" size={13}/>
              </Button>
            </div>
          </Card>

          <TableShell>
            <table className="w-full text-sm min-w-[720px]">
              <thead>
                <tr>
                  <Th>When</Th>
                  <Th>Actor</Th>
                  <Th>Action</Th>
                  <Th>Target</Th>
                  <Th>Meta</Th>
                </tr>
              </thead>
              {loading ? (
                <TableSkeleton rows={10} cols={5}/>
              ) : rows.length===0 ? (
                <tbody>
                  <tr><td colSpan={5}>
                    <EmptyState
                      icon="logs"
                      title={offset > 0 ? 'No events on this page' : 'No audit events yet'}
                      hint={offset > 0
                        ? 'Go back a page or clear the actor filter.'
                        : 'Privileged actions (provider/key/user changes, settings edits) will appear here.'}
                    />
                  </td></tr>
                </tbody>
              ) : (
                <tbody>
                  {rows.map(r=>(
                    <tr key={r.id} className="hover:bg-raised/50 transition-colors">
                      <Td className="whitespace-nowrap text-xs text-muted tabular-nums">{new Date(r.created_at).toLocaleString()}</Td>
                      <Td className="font-mono text-xs">{r.actor || '—'}</Td>
                      <Td><Badge tone="neutral">{r.action}</Badge></Td>
                      <Td className="font-mono text-xs">
                        {r.target_type
                          ? <span title={`${r.target_type}:${r.target_id ?? ''}`} className="block truncate max-w-[220px]">{r.target_type}:{String(r.target_id ?? '').slice(0,12)}</span>
                          : <span className="text-muted/50">—</span>}
                      </Td>
                      <Td><MetaCell meta={r.meta}/></Td>
                    </tr>
                  ))}
                </tbody>
              )}
            </table>
          </TableShell>

          {!loading && rows.length > 0 && (
            <p className="text-xs text-muted -mt-2">
              Showing {rows.length} event(s) starting at #{offset + 1}{total != null ? ` of ${total.toLocaleString()}` : ''} (admin-only endpoint).
            </p>
          )}
        </>
      )}
    </div>
  )
}
