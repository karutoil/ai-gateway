import { useEffect, useState, type ReactNode } from 'react'
import { Card, PageHeader, Button, Badge, Icon, SegmentedControl, TableShell, Th, Td, EmptyState, Skeleton, Stat, Progress, type IconName } from '../components/ui'

type Daily = { day: string; tokens: number; cost: number; requests: number }
type TopModel = { model: string; tokens: number; cost: number; requests: number }
type TopKey = { key_prefix: string; tokens: number; cost: number; requests: number }
type ErrorRow = { status: number; count: number; sample?: string }
type Stats = { providers:number; keys:number; requests:number; total_tokens:number; total_cost:number; range:string; daily:Daily[]; top_models:TopModel[]; top_keys:TopKey[]; latency:{p50:number;p95:number;avg:number;count:number}; range_tokens:number; range_cost:number; range_requests:number; range_successful?:number; range_failed?:number; successful?:number; failed?:number; range_ttft_avg?:number; range_tps_avg?:number; ttft?:{avg:number}; tps?:{avg:number}; errors?:ErrorRow[]; cache_hit_rate?:number; cache_read_tokens?:number }

async function fetchStats(range:string):Promise<Stats>{
  const res=await fetch(`/api/stats?range=${range}`,{credentials:'same-origin'}); if(!res.ok) throw new Error(await res.text()); return res.json()
}
async function fetchKeyMap():Promise<Record<string,string>>{
  try{
    const res=await fetch('/api/keys',{credentials:'same-origin'}); if(!res.ok) return {}; const keys=await res.json(); const m:Record<string,string>={}; for(const k of keys) m[k.prefix]=k.name; return m
  }catch{ return {}}
}

const RANGE_DAYS: Record<'24h'|'7d'|'30d', number> = { '24h': 1, '7d': 7, '30d': 30 }
function statusTone(s:number): 'good'|'warn'|'bad' { return s>=500 ? 'bad' : s>=400 ? 'warn' : 'good' }
function isHourly(day: string): boolean { return day.includes('T') }
function bucketLabel(day: string): string {
  if (isHourly(day)) return `${day.slice(8, 10)} ${day.slice(11, 13)}:00`
  return day.slice(5)
}
function bucketTitle(d: Daily): string {
  const when = isHourly(d.day) ? d.day.slice(0, 13).replace('T', ' ') + ':00' : d.day
  return `${when} — ${d.requests} requests · ${d.tokens.toLocaleString()} tokens`
}
function zeroFillDaily(daily: Daily[], range: '24h'|'7d'|'30d'): Daily[] {
  const byKey = new Map(daily.map(d => [d.day, d]))
  const out: Daily[] = []
  const now = new Date()
  if (range === '24h') {
    for (let i = 23; i >= 0; i--) {
      const t = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), now.getUTCHours() - i))
      const key = t.toISOString().slice(0, 13) + ':00:00Z'
      out.push(byKey.get(key) ?? { day: key, tokens: 0, cost: 0, requests: 0 })
    }
    return out
  }
  const days = RANGE_DAYS[range]
  for (let i = days - 1; i >= 0; i--) {
    const day = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i)).toISOString().slice(0, 10)
    out.push(byKey.get(day) ?? { day, tokens: 0, cost: 0, requests: 0 })
  }
  return out
}

function DailyBars({ daily, extract, tip, dim = false }: {
  daily: Daily[]; extract: (d: Daily) => number; tip: (d: Daily) => string; dim?: boolean
}) {
  const max = Math.max(1e-9, ...daily.map(extract))
  return (
    <div className="flex items-end gap-1.5 overflow-x-auto pb-1">
      {daily.map(d => {
        const v = extract(d)
        const pct = v > 0 ? Math.max(4, Math.round((v / max) * 100)) : 2
        return (
          <div key={d.day} title={tip(d)} className="flex-1 min-w-[26px] flex flex-col items-center gap-1.5 group cursor-default">
            <div className="w-full h-36 sm:h-44 flex items-end rounded-lg bg-app border border-stone p-1">
              <div className={`w-full rounded transition-opacity ${v>0 ? (dim ? 'bg-accent/60' : 'bg-accent opacity-90') : 'bg-stone/60'}`}
                style={{ height: `${pct}%` }} />
            </div>
            <span className="font-mono text-[9px] text-muted truncate w-full text-center">{bucketLabel(d.day)}</span>
          </div>
        )
      })}
    </div>
  )
}

export default function Analytics(){
  const [range,setRange]=useState<'24h'|'7d'|'30d'>('7d')
  const [stats,setStats]=useState<Stats|null>(null)
  const [keyMap,setKeyMap]=useState<Record<string,string>>({})
  const [err,setErr]=useState('')
  const [loading,setLoading]=useState(true)
  const [reloadKey,setReloadKey]=useState(0)
  useEffect(()=>{ fetchKeyMap().then(setKeyMap).catch(()=>{})},[])
  useEffect(()=>{
    setLoading(true)
    fetchStats(range).then(s=>{setStats(s);setErr('')}).catch(e=>setErr(String(e.message||e))).finally(()=>setLoading(false))
  },[range, reloadKey])

  const ranges = [{ value:'24h', label:'24h' }, { value:'7d', label:'7d' }, { value:'30d', label:'30d' }] as { value:'24h'|'7d'|'30d'; label:string }[]
  const failPct = stats?.range_requests ? ((((stats as any).range_failed)||0)/stats.range_requests*100).toFixed(1) : '0'
  const filledDaily = zeroFillDaily(stats?.daily ?? [], range)
  const okPct = stats?.range_requests ? Math.round(((stats as any).range_successful||0)/stats.range_requests*100) : 0
  const rangeLabel = range === '24h' ? '24 hours' : range === '7d' ? '7 days' : '30 days'

  // Wider-window probe: when the selected window is empty, check the last
  // 30 days once. If traffic exists there, the window — not the pipeline —
  // is the cause, and a note offers the one-click widen. No polling loop
  // (writes wider, which is not a dep).
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

  return (
    <div className="space-y-6">
      <PageHeader eyebrow="Optimize · Spend" title="Analytics" description="Traffic, spend, reliability and latency across the selected window."
        actions={<SegmentedControl options={ranges} value={range} onChange={setRange}/>} />

      {err && (<Card><EmptyState icon="alert" title="Could not load analytics" hint={err}
        action={<Button variant="secondary" onClick={()=>setReloadKey(k=>k+1)}><Icon name="refresh" size={15}/>Retry</Button>} /></Card>)}

      {loading ? (
        <div className="space-y-4">
          <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3">
            {Array.from({length:6}).map((_,i)=>(
              <Card key={i} className="flex items-start gap-3"><Skeleton className="w-10 h-10 shrink-0 !rounded-xl"/><div className="flex-1 space-y-1.5"><Skeleton className="h-3 w-14"/><Skeleton className="h-6 w-full"/></div></Card>
            ))}
          </div>
          <Card><Skeleton className="h-48 w-full"/></Card>
        </div>
      ) : stats && (
        <>
          {wider != null && wider > 0 && (
            <Card className="border-accent/30 bg-accent/10 flex flex-wrap items-center justify-between gap-3">
              <span className="text-sm text-paper">
                No activity in the last {rangeLabel} — but <b className="tabular-nums">{wider.toLocaleString()} requests</b> exist
                in the last 30 days.
              </span>
              <Button variant="secondary" size="sm" onClick={()=>setRange('30d')}>Show 30 days</Button>
            </Card>
          )}
          <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3">
            <Stat icon="zap" title="Tokens" value={Number(stats.range_tokens||0).toLocaleString()} sub={`${stats.range_requests} requests`} tone="good"/>
            <Stat icon="wallet" title="Spend" value={`$${Number(stats.range_cost||0).toFixed(2)}`} sub="estimated" tone="neutral"/>
            <Stat icon="check" title="Succeeded" value={(stats as any).range_successful ?? 0} sub={`${(stats as any).range_failed ?? 0} failed`} tone="good"/>
            <Stat icon="alert" title="Fail rate" value={`${failPct}%`} sub="of window" tone={Number(failPct) > 5 ? 'bad' : 'warn'}/>
            <Stat icon="gauge" title="P50 · TTFT" value={`${stats.latency?.p50??0}ms`} sub={`TTFT ${(stats as any).range_ttft_avg ? Math.round((stats as any).range_ttft_avg) : 0}ms`} tone="neutral"/>
            <Stat icon="activity" title="Avg · TPS" value={(stats as any).range_tps_avg ? `${(stats as any).range_tps_avg.toFixed(1)} tok/s` : '—'} sub={`p95 ${stats.latency?.p95??0}ms`} tone="neutral"/>
          </div>

          {(stats.range_requests > 0) && (
            <Card>
              <div className="flex flex-wrap items-center justify-between gap-3 border-b-2 border-accent pb-3">
                <div>
                  <h3 className="font-display font-semibold tracking-tight">Reliability</h3>
                  <p className="text-xs text-muted mt-0.5">{okPct}% success across {stats.range_requests.toLocaleString()} requests · {stats.range}</p>
                </div>
                <div className="flex items-center gap-2"><Badge tone="good">{(stats as any).range_successful||0} ok</Badge><Badge tone="bad">{(stats as any).range_failed||0} fail</Badge></div>
              </div>
              <div className="mt-4"><Progress value={okPct} tone={okPct > 95 ? 'accent' : okPct > 85 ? 'amber' : 'red'} /></div>
              {!!stats.cache_read_tokens && (
                <div className="mt-3 flex items-center gap-2 text-xs text-muted">
                  <Icon name="zap" size={13} className="text-accent" />
                  <span><span className="text-accent font-semibold">{((stats.cache_hit_rate ?? 0) * 100).toFixed(1)}%</span> cache hits · {Number(stats.cache_read_tokens).toLocaleString()} cached tokens</span>
                </div>
              )}
            </Card>
          )}

          {(stats.errors?.length ?? 0) > 0 && (
            <Card>
              <div className="flex items-baseline justify-between gap-3">
                <h3 className="font-display font-semibold tracking-tight">Errors by status</h3>
                <span className="text-xs text-muted font-mono">{stats.range}</span>
              </div>
              <div className="mt-4 space-y-2.5">
                {(() => {
                  const maxCount = Math.max(...(stats.errors ?? []).map(e => e.count), 1)
                  return (stats.errors ?? []).map(e => (
                    <div key={e.status} className="flex items-center gap-3">
                      <Badge tone={statusTone(e.status)}>{e.status}</Badge>
                      <div className="flex-1 h-2.5 rounded-full bg-app border border-stone overflow-hidden">
                        <div className="h-full bg-danger/70 rounded-full" style={{ width: `${Math.max(3, Math.round(e.count / maxCount * 100))}%` }}/>
                      </div>
                      <span className="font-mono text-xs tabular-nums text-muted w-16 text-right">{e.count.toLocaleString()}</span>
                      {e.sample && <span className="hidden md:block text-xs text-muted truncate max-w-[280px] font-mono" title={e.sample}>{e.sample}</span>}
                    </div>
                  ))
                })()}
              </div>
            </Card>
          )}

          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
            <Card>
              <div className="flex items-center justify-between gap-3">
                <div><h3 className="font-display font-semibold tracking-tight">Requests</h3><p className="text-[11px] text-muted">Volume per bucket · {stats.range}</p></div>
                <Badge tone="neutral">{filledDaily.reduce((a,d)=>a+d.requests,0).toLocaleString()} total</Badge>
              </div>
              <div className="mt-4">{filledDaily.length===0 ? <EmptyState icon="chart" title="No activity in this range"/> : <DailyBars daily={filledDaily} extract={d=>d.requests} tip={bucketTitle}/>}</div>
            </Card>
            <Card>
              <div className="flex items-center justify-between gap-3">
                <div><h3 className="font-display font-semibold tracking-tight">Spend</h3><p className="text-[11px] text-muted">Estimated cost per bucket · {stats.range}</p></div>
                <Badge tone="good">${filledDaily.reduce((a,d)=>a+d.cost,0).toFixed(2)}</Badge>
              </div>
              <div className="mt-4">{filledDaily.length===0 ? <EmptyState icon="chart" title="No activity in this range"/>
                : <DailyBars daily={filledDaily} dim extract={d=>d.cost} tip={d=>`${bucketTitle(d)} · $${d.cost.toFixed(4)}`}/>}</div>
            </Card>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <div>
              <div className="flex items-baseline justify-between mb-2.5 px-1">
                <h3 className="font-display text-sm font-semibold">Top models</h3><span className="text-xs text-muted font-mono">{stats.range}</span>
              </div>
              <TableShell>
                <thead><tr><Th>Model</Th><Th className="!text-right">Tokens</Th><Th className="!text-right">Cost</Th></tr></thead>
                <tbody>
                  {stats.top_models.map((m, i)=>(
                    <tr key={m.model} className="hover:bg-raised/40 transition-colors">
                      <Td className="font-mono text-xs"><span className="flex items-center gap-2"><span className="w-5 h-5 rounded-md bg-raised border border-stone/60 text-[10px] flex items-center justify-center font-bold text-muted">{i+1}</span><span className="block truncate max-w-[160px] sm:max-w-[200px]" title={m.model}>{m.model}</span></span></Td>
                      <Td className="text-right tabular-nums font-mono text-xs">{m.tokens.toLocaleString()}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs text-accent">${Number(m.cost).toFixed(4)}</Td>
                    </tr>
                  ))}
                  {stats.top_models.length===0 && (<tr><td colSpan={3}><EmptyState icon="box" title="No model usage yet"/></td></tr>)}
                </tbody>
              </TableShell>
            </div>
            <div>
              <div className="flex items-baseline justify-between mb-2.5 px-1">
                <h3 className="font-display text-sm font-semibold">Top keys</h3><span className="text-xs text-muted font-mono">{stats.range}</span>
              </div>
              <TableShell>
                <thead><tr><Th>Key</Th><Th className="!text-right">Tokens</Th><Th className="!text-right">Cost</Th></tr></thead>
                <tbody>
                  {stats.top_keys.map(k=>(
                    <tr key={k.key_prefix} className="hover:bg-raised/40 transition-colors">
                      <Td className="font-mono text-xs">{k.key_prefix}{keyMap[k.key_prefix] && <span className="block text-[11px] text-muted truncate max-w-[140px]" title={keyMap[k.key_prefix]}>{keyMap[k.key_prefix]}</span>}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs">{k.tokens.toLocaleString()}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs text-accent">${Number(k.cost).toFixed(4)}</Td>
                    </tr>
                  ))}
                  {stats.top_keys.length===0 && (<tr><td colSpan={3}><EmptyState icon="key" title="No key usage yet"/></td></tr>)}
                </tbody>
              </TableShell>
            </div>
          </div>
        </>
      )}

      {!loading && !stats && !err && (
        <Card pad={false}><EmptyState icon="chart" title="No analytics data" hint="Requests will show up here once traffic flows through the gateway." action={
          <Button variant="secondary" onClick={()=>{ setLoading(true); fetchStats(range).then(s=>setStats(s)).catch(()=>{}).finally(()=>setLoading(false)) }}><Icon name="refresh" size={15}/>Retry</Button>}/></Card>
      )}
    </div>
  )
}
