import { useEffect, useState, type ReactNode } from 'react'
import {
  Card, PageHeader, Button, Badge, Icon, SegmentedControl,
  TableShell, Th, Td, EmptyState, Skeleton,
  type IconName,
} from '../components/ui'
type Daily = { day: string; tokens: number; cost: number; requests: number }
type TopModel = { model: string; tokens: number; cost: number; requests: number }
type TopKey = { key_prefix: string; tokens: number; cost: number; requests: number }
type Stats = { providers:number; keys:number; requests:number; total_tokens:number; total_cost:number; range:string; daily:Daily[]; top_models:TopModel[]; top_keys:TopKey[]; latency:{p50:number;p95:number;avg:number;count:number}; range_tokens:number; range_cost:number; range_requests:number; range_successful?:number; range_failed?:number; successful?:number; failed?:number; range_ttft_avg?:number; range_tps_avg?:number; ttft?:{avg:number}; tps?:{avg:number} }

async function fetchStats(range:string):Promise<Stats>{
  // Auth rides on the HttpOnly session cookie sent with same-origin requests.
  const res=await fetch(`/api/stats?range=${range}`,{credentials:'same-origin'}); if(!res.ok) throw new Error(await res.text()); return res.json()
}
async function fetchKeyMap():Promise<Record<string,string>>{
  try{
    const res=await fetch('/api/keys',{credentials:'same-origin'}); if(!res.ok) return {}; const keys=await res.json(); const m:Record<string,string>={}; for(const k of keys) m[k.prefix]=k.name; return m
  }catch{ return {}}
}

const RANGE_DAYS: Record<'24h'|'7d'|'30d', number> = { '24h': 1, '7d': 7, '30d': 30 }

/**
 * Zero-fill missing days so the bar charts always span the selected range —
 * days without traffic render as 0-height bars instead of silently shifting
 * the axis. Bucket keys are UTC day strings, matching the backend grouping.
 */
function zeroFillDaily(daily: Daily[], range: '24h'|'7d'|'30d'): Daily[] {
  const byDay = new Map(daily.map(d => [d.day, d]))
  const days = RANGE_DAYS[range]
  const out: Daily[] = []
  const now = new Date()
  for (let i = days - 1; i >= 0; i--) {
    const day = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i)).toISOString().slice(0, 10)
    out.push(byDay.get(day) ?? { day, tokens: 0, cost: 0, requests: 0 })
  }
  return out
}

/** Small KPI tile: Card + icon chip + value. */
function Stat({ icon, title, value, sub, tone = 'neutral' }: {
  icon: IconName; title: string; value: ReactNode; sub?: string
  tone?: 'neutral' | 'good' | 'warn' | 'bad'
}) {
  const tones: Record<'neutral' | 'good' | 'warn' | 'bad', string> = {
    neutral: 'bg-stone/50 text-muted',
    good: 'bg-teal/10 text-teal',
    warn: 'bg-amber/10 text-amber',
    bad: 'bg-red-500/10 text-red-400',
  }
  return (
    <Card className="flex items-start gap-3">
      <div className={`w-9 h-9 rounded-lg flex items-center justify-center shrink-0 ${tones[tone]}`}>
        <Icon name={icon} size={17} />
      </div>
      <div className="min-w-0">
        <div className="text-xs text-muted">{title}</div>
        <div className="text-xl font-semibold tracking-tight tabular-nums mt-0.5 truncate">{value}</div>
        {sub && <div className="text-xs text-muted mt-0.5">{sub}</div>}
      </div>
    </Card>
  )
}

/** Minimal CSS bar chart — hover tooltip via title attr, label under each bar. */
function DailyBars({ daily, extract, tip, dim = false }: {
  daily: Daily[]; extract: (d: Daily) => number; tip: (d: Daily) => string
  /** dim = secondary series (spend) rendered at reduced intensity */
  dim?: boolean
}) {
  const max = Math.max(1e-9, ...daily.map(extract))
  return (
    <div className="flex items-end gap-1.5 overflow-x-auto pb-1">
      {daily.map(d => {
        const v = extract(d)
        const pct = v > 0 ? Math.max(3, Math.round((v / max) * 100)) : 2
        return (
          <div key={d.day} title={tip(d)} className="flex-1 min-w-[26px] flex flex-col items-center gap-1.5 group cursor-default">
            <div className="w-full h-32 sm:h-40 flex items-end">
              <div
                className={`w-full rounded-t-sm group-hover:opacity-100 transition-opacity ${dim ? 'bg-teal/50 opacity-70 hover:bg-teal/70' : 'bg-teal opacity-90 hover:bg-teal'}`}
                style={{ height: `${pct}%` }}
              />
            </div>
            <span className="font-mono text-[9px] text-muted truncate w-full text-center">{d.day.slice(5)}</span>
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

  const ranges = [
    { value:'24h', label:'24h' },
    { value:'7d', label:'7d' },
    { value:'30d', label:'30d' },
  ] as { value:'24h'|'7d'|'30d'; label:string }[]

  const failPct = stats?.range_requests ? ((((stats as any).range_failed)||0)/stats.range_requests*100).toFixed(1) : '0'
  // Zero-filled series: every day of the selected range gets a bar.
  const filledDaily = zeroFillDaily(stats?.daily ?? [], range)

  return (
    <div className="space-y-6">
      <PageHeader
        title="Analytics"
        description="Traffic, spend and performance across your gateway."
        actions={<SegmentedControl options={ranges} value={range} onChange={setRange}/>}
      />

      {err && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load analytics"
            hint={err}
            action={<Button variant="secondary" onClick={()=>setReloadKey(k=>k+1)}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {loading ? (
        <div className="space-y-4">
          <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-3">
            {Array.from({length:6}).map((_,i)=>(
              <Card key={i} className="flex items-start gap-3">
                <Skeleton className="w-9 h-9 shrink-0"/>
                <div className="flex-1 space-y-1.5"><Skeleton className="h-3 w-14"/><Skeleton className="h-5 w-full"/></div>
              </Card>
            ))}
          </div>
          <Card><Skeleton className="h-40 w-full"/></Card>
        </div>
      ) : stats && (
        <>
          {/* KPI grid */}
          <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-3">
            <Stat icon="zap" title="Tokens" value={Number(stats.range_tokens||0).toLocaleString()} sub={`${stats.range_requests} req`} tone="good"/>
            <Stat icon="chart" title="Cost" value={`$${Number(stats.range_cost||0).toFixed(4)}`} sub="estimated" tone="neutral"/>
            <Stat icon="check" title="Success" value={(stats as any).range_successful ?? 0} sub={`${(stats as any).range_failed ?? 0} failed`} tone="good"/>
            <Stat icon="alert" title="Fail rate" value={`${failPct}%`} sub={`${(stats as any).range_failed ?? 0} failed in range`} tone="warn"/>
            <Stat icon="pulse" title="P50 / TTFT" value={`${stats.latency?.p50??0}ms`} sub={`TTFT ${(stats as any).range_ttft_avg ? Math.round((stats as any).range_ttft_avg) : 0}ms`} tone="neutral"/>
            <Stat icon="route" title="P95 / TPS" value={`${stats.latency?.p95??0}ms`} sub={`TPS ${(stats as any).range_tps_avg ? (stats as any).range_tps_avg.toFixed(1) : '—'}`} tone="neutral"/>
          </div>

          {(stats.range_requests > 0) && (
            <Card>
              <div className="flex items-baseline justify-between gap-3">
                <h3 className="text-sm font-semibold tracking-tight">Success vs Failure</h3>
                <span className="text-xs text-muted">{stats.range}</span>
              </div>
              <div className="mt-3 flex gap-2 items-center">
                <div className="flex-1 h-4 rounded-full overflow-hidden flex bg-app border border-stone">
                  <div className="bg-teal h-full" style={{width: `${stats.range_requests ? (((stats as any).range_successful||0)/stats.range_requests*100) : 0}%`}} />
                  <div className="bg-red-500 h-full" style={{width: `${stats.range_requests ? (((stats as any).range_failed||0)/stats.range_requests*100) : 0}%`}} />
                </div>
                <Badge tone="good">{(stats as any).range_successful||0} ok</Badge>
                <Badge tone="bad">{(stats as any).range_failed||0} fail</Badge>
              </div>
            </Card>
          )}

          {/* Daily charts */}
          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
            <Card>
              <div className="flex items-baseline justify-between gap-3">
                <h3 className="text-sm font-semibold tracking-tight">Requests / day</h3>
                <span className="text-xs text-muted">{stats.range}</span>
              </div>
              <div className="mt-4">
                {filledDaily.length===0
                  ? <EmptyState icon="chart" title="No activity in this range"/>
                  : <DailyBars daily={filledDaily} extract={d=>d.requests} tip={d=>`${d.day} — ${d.requests} requests · ${d.tokens.toLocaleString()} tokens`}/>}
              </div>
            </Card>
            <Card>
              <div className="flex items-baseline justify-between gap-3">
                <h3 className="text-sm font-semibold tracking-tight">Spend / day</h3>
                <span className="text-xs text-muted">{stats.range}</span>
              </div>
              <div className="mt-4">
                {filledDaily.length===0
                  ? <EmptyState icon="chart" title="No activity in this range"/>
                  : <DailyBars daily={filledDaily} dim extract={d=>d.cost} tip={d=>`${d.day} — $${d.cost.toFixed(4)} · ${d.requests} requests`}/>}
              </div>
            </Card>
          </div>

          {/* Top tables */}
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <div>
              <div className="flex items-baseline justify-between mb-2 px-1">
                <h3 className="text-sm font-semibold">Top models</h3>
                <span className="text-xs text-muted">{stats.range}</span>
              </div>
              <TableShell>
                <thead><tr><Th>Model</Th><Th className="!text-right">Tokens</Th><Th className="!text-right">Cost</Th></tr></thead>
                <tbody>
                  {stats.top_models.map(m=>(
                    <tr key={m.model}>
                      <Td className="font-mono text-xs"><span className="block truncate max-w-[140px] sm:max-w-[180px]" title={m.model}>{m.model}</span></Td>
                      <Td className="text-right tabular-nums font-mono text-xs">{m.tokens.toLocaleString()}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs">${Number(m.cost).toFixed(4)}</Td>
                    </tr>
                  ))}
                  {stats.top_models.length===0 && (
                    <tr><td colSpan={3}><EmptyState icon="box" title="No model usage yet"/></td></tr>
                  )}
                </tbody>
              </TableShell>
            </div>
            <div>
              <div className="flex items-baseline justify-between mb-2 px-1">
                <h3 className="text-sm font-semibold">Top keys</h3>
                <span className="text-xs text-muted">{stats.range}</span>
              </div>
              <TableShell>
                <thead><tr><Th>Key</Th><Th className="!text-right">Tokens</Th><Th className="!text-right">Cost</Th></tr></thead>
                <tbody>
                  {stats.top_keys.map(k=>(
                    <tr key={k.key_prefix}>
                      <Td className="font-mono text-xs">{k.key_prefix}{keyMap[k.key_prefix] && <span className="block text-[11px] text-muted truncate max-w-[120px]" title={keyMap[k.key_prefix]}>{keyMap[k.key_prefix]}</span>}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs">{k.tokens.toLocaleString()}</Td>
                      <Td className="text-right tabular-nums font-mono text-xs">${Number(k.cost).toFixed(4)}</Td>
                    </tr>
                  ))}
                  {stats.top_keys.length===0 && (
                    <tr><td colSpan={3}><EmptyState icon="key" title="No key usage yet"/></td></tr>
                  )}
                </tbody>
              </TableShell>
            </div>
          </div>

          {/* Last days summary retained from the previous numeric strip */}
          {filledDaily.slice(-6).reverse().length > 0 && (
            <div className="grid grid-cols-1 sm:grid-cols-3 md:grid-cols-6 gap-2">
              {filledDaily.slice(-6).reverse().map(d=>(
                <Card key={d.day} className="p-3">
                  <div className="font-mono text-xs text-paper">{d.day}</div>
                  <div className="font-mono text-[11px] tabular-nums text-teal mt-1">{d.tokens.toLocaleString()} tok</div>
                  <div className="font-mono text-[11px] tabular-nums text-muted">${d.cost.toFixed(4)}</div>
                </Card>
              ))}
            </div>
          )}
        </>
      )}

      {!loading && !stats && !err && (
        <Card pad={false}>
          <EmptyState icon="chart" title="No analytics data" hint="Requests will show up here once traffic flows through the gateway." action={
            <Button variant="secondary" onClick={()=>{ setLoading(true); fetchStats(range).then(s=>setStats(s)).catch(()=>{}).finally(()=>setLoading(false)) }}>
              <Icon name="refresh" size={15}/>Retry
            </Button>
          }/>
        </Card>
      )}
    </div>
  )
}
