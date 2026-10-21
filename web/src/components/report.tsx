import { useState, type ReactNode } from 'react'
import { Button, Card, Icon, Input, SegmentedControl, type IconName } from './ui'
import type { RangeKey } from '../lib/api'

/* ------------------------------------------------------------------ */
/* Formatting                                                          */
/* ------------------------------------------------------------------ */

export const fmtInt = (n: number | null | undefined) => Number(n ?? 0).toLocaleString()

export function fmtCompact(n: number | null | undefined): string {
  const v = Number(n ?? 0)
  if (v >= 1_000_000_000) return `${(v / 1_000_000_000).toFixed(1)}B`
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`
  if (v >= 10_000) return `${(v / 1_000).toFixed(0)}K`
  return v.toLocaleString()
}

export function fmtMoney(v: number | null | undefined): string {
  const n = Number(v ?? 0)
  if (n === 0) return '$0.00'
  if (n >= 100) return `$${n.toFixed(2)}`
  if (n >= 1) return `$${n.toFixed(3)}`
  return `$${n.toFixed(5)}`
}

export const fmtMs = (v: number | null | undefined) => (v ? `${Math.round(Number(v))}ms` : '—')
export const fmtPct = (x: number) => `${(Number(x ?? 0) * 100).toFixed(1)}%`

/**
 * Bucket label: hourly keys carry a "T..Z" ISO fragment, daily ones do not.
 *
 * The value originates from JSON, so it is validated rather than trusted. A
 * shape change once reached `.includes` on a non-string inside a render map
 * and blanked the entire page with "e.includes is not a function"; degrading a
 * single axis label is the correct failure mode.
 */
export function bucketLabel(day: string | null | undefined): string {
  if (typeof day !== 'string' || day === '') return ''
  return day.includes('T') ? `${day.slice(5, 10)} ${day.slice(11, 13)}:00` : day.slice(5)
}

export function bucketWhen(day: string | null | undefined): string {
  if (typeof day !== 'string' || day === '') return ''
  return day.includes('T') ? `${day.slice(0, 13).replace('T', ' ')}:00 UTC` : `${day} UTC`
}

/* ------------------------------------------------------------------ */
/* Range picker                                                        */
/* ------------------------------------------------------------------ */

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: '1h', label: '1h' },
  { value: '6h', label: '6h' },
  { value: '24h', label: '24h' },
  { value: '7d', label: '7d' },
  { value: '30d', label: '30d' },
  { value: '90d', label: '90d' },
  { value: '365d', label: '1y' },
]

/**
 * Window selector. Quick picks cover the common cases; the custom form takes
 * an explicit from/to pair, which is what makes period-over-period comparison
 * and "what happened on the 14th" possible — the fixed 24h/7d/30d triple the
 * rest of the dashboard offers cannot express either.
 */
export function RangePicker({ value, onChange, custom, onCustom, disabled }: {
  value: RangeKey
  onChange: (r: RangeKey) => void
  custom: { from: string; to: string } | null
  onCustom: (c: { from: string; to: string } | null) => void
  disabled?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [err, setErr] = useState('')

  const apply = () => {
    if (!from || !to) { setErr('Both dates are required.'); return }
    const f = new Date(from), t = new Date(to)
    if (Number.isNaN(f.getTime()) || Number.isNaN(t.getTime())) { setErr('Unparseable date.'); return }
    if (f >= t) { setErr('Start must be before end.'); return }
    setErr('')
    onCustom({ from: f.toISOString(), to: t.toISOString() })
    setOpen(false)
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <SegmentedControl<string>
        options={RANGE_OPTIONS.map(o => ({ value: o.value, label: o.label }))}
        value={custom ? '' : value}
        onChange={(v) => { setOpen(false); setErr(''); onCustom(null); onChange(v as RangeKey) }}
      />
      <Button
        size="sm"
        variant={custom ? 'subtle' : 'secondary'}
        onClick={() => setOpen(o => !o)}
        disabled={disabled}
        title="Pick an explicit window">
        <Icon name="clock" size={13}/>
        {custom ? `${custom.from.slice(0, 10)} → ${custom.to.slice(0, 10)}` : 'Custom'}
      </Button>
      {custom && (
        <button onClick={() => onCustom(null)} className="h-7 px-1.5 rounded-md text-xs text-muted hover:text-paper hover:bg-raised transition-colors inline-flex items-center gap-1">
          <Icon name="x" size={11}/> clear
        </button>
      )}
      {open && (
        <div className="w-full flex flex-wrap items-end gap-2 mt-1 p-3 rounded-lg border border-stone bg-raised/40">
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-muted">From</span>
            <Input type="datetime-local" value={from} onChange={e => setFrom(e.target.value)} />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-muted">To</span>
            <Input type="datetime-local" value={to} onChange={e => setTo(e.target.value)} />
          </label>
          <Button variant="primary" onClick={apply}>Apply</Button>
          {err && <span className="text-xs text-danger">{err}</span>}
        </div>
      )}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Charts                                                              */
/* ------------------------------------------------------------------ */

type Series = { key: string; label: string; tone?: string }

/**
 * Grouped vertical bars over time. Deliberately dependency-free: the app
 * ships no charting library, and a bar series with a tooltip covers the
 * reporting pages without adding ~100KB to the embedded bundle.
 *
 * Generic over the datum so labelOf receives the bucket object itself, the
 * same contract as valueOf on LineSeries. Typing it `(d: any)` is what let a
 * string-only formatter be passed here and crash the page at render time.
 */
export function BarSeries<T extends Record<string, any>>({ data, labelOf, series, format, height = 150 }: {
  data: T[]
  labelOf: (d: T) => string
  series: { key: string; label: string; tone: string }[]
  format: (v: number) => string
  height?: number
}) {
  let max = 1e-9
  for (const d of data) for (const s of series) {
    const v = Number(d[s.key] ?? 0)
    if (Number.isFinite(v)) max = Math.max(max, v)
  }

  if (data.length === 0) {
    return <div className="text-xs text-muted py-8 text-center">No data in this window.</div>
  }

  return (
    <div>
      <div className="flex items-end gap-1 overflow-x-auto pb-1" style={{ height }}>
        {data.map((d, i) => (
          <div key={i} className="flex-1 min-w-[18px] h-full flex items-end gap-px group"
            title={`${labelOf(d)}\n${series.map(s => `${s.label}: ${format(Number(d[s.key] ?? 0))}`).join('\n')}`}>
            {series.map(s => {
              const raw = Number(d[s.key] ?? 0)
              const v = Number.isFinite(raw) && raw > 0 ? raw : 0
              const pct = v > 0 ? Math.max(3, Math.round((v / max) * 100)) : 0
              return <div key={s.key} className="flex-1 rounded-t transition-opacity group-hover:opacity-80"
                style={{ height: `${pct}%`, backgroundColor: s.tone }} />
            })}
          </div>
        ))}
      </div>
      <div className="flex justify-between text-[11px] font-mono tabular-nums text-muted mt-1">
        <span>{labelOf(data[0])}</span>
        <span>{labelOf(data[data.length - 1])}</span>
      </div>
      {series.length > 1 && (
        <div className="flex flex-wrap gap-3 mt-2">
          {series.map(s => (
            <span key={s.key} className="inline-flex items-center gap-1.5 text-[11px] text-muted">
              <span className="w-2.5 h-2.5 rounded-sm" style={{ backgroundColor: s.tone }} />{s.label}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * Single-series line chart (SVG). Used where a trend reads better as a line
 * than as bars — error rate, p95 latency.
 */
export function LineSeries<T extends Record<string, any>>({ data, valueOf, labelOf, format, tone = '#6366F1', height = 120 }: {
  data: T[]
  valueOf: (d: T) => number
  labelOf: (d: T) => string
  format: (v: number) => string
  tone?: string
  height?: number
}) {
  let max = 0
  const vals = data.map(d => {
    const v = Number(valueOf(d) ?? 0)
    return Number.isFinite(v) && v > 0 ? v : 0
  })
  for (const v of vals) max = Math.max(max, v)
  const scale = Math.max(max, 1e-9)
  const pts = vals.map((v, i) => {
    const x = vals.length > 1 ? (i / (vals.length - 1)) * 100 : 50
    const y = 100 - (v / scale) * 96
    return `${x},${y}`
  }).join(' ')

  if (data.length === 0) {
    return <div className="text-xs text-muted py-8 text-center">No data in this window.</div>
  }

  return (
    <div>
      <svg viewBox="0 0 100 100" preserveAspectRatio="none" style={{ height }} className="w-full">
        <polyline points={pts} fill="none" stroke={tone} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
      </svg>
      <div className="flex justify-between text-[11px] font-mono tabular-nums text-muted mt-1">
        <span>{labelOf(data[0])}</span>
        <span>peak {format(max)}</span>
        <span>{labelOf(data[data.length - 1])}</span>
      </div>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Sortable table                                                      */
/* ------------------------------------------------------------------ */

export type SortState = { key: string; dir: 'asc' | 'desc' }

export function useSort(initial: SortState) {
  const [sort, setSort] = useState<SortState>(initial)
  const toggle = (key: string) =>
    setSort(s => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: 'desc' }))
  return { sort, toggle }
}

/** Client-side sort over already-fetched rows (rankings, breakdown tables). */
export function sortRows<T extends Record<string, any>>(rows: T[], sort: SortState): T[] {
  const out = [...rows]
  out.sort((a, b) => {
    const av = a[sort.key], bv = b[sort.key]
    let cmp: number
    if (typeof av === 'number' && typeof bv === 'number') cmp = av - bv
    else cmp = String(av ?? '').localeCompare(String(bv ?? ''))
    return sort.dir === 'asc' ? cmp : -cmp
  })
  return out
}

/** Clickable header cell that reports and renders the active sort. */
export function SortTh({ label, sortKey, sort, onSort, align = 'left', className = '' }: {
  label: string
  sortKey: string
  sort: SortState
  onSort: (k: string) => void
  align?: 'left' | 'right'
  className?: string
}) {
  const active = sort.key === sortKey
  return (
    // The click target is a real <button> covering the cell: a bare `th` with
    // onClick is unreachable by keyboard and announced as a plain header.
    // `aria-sort` on the th is what screen readers use to report the order.
    <th
      aria-sort={active ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}
      className={`relative px-4 py-2.5 text-[11px] font-semibold uppercase tracking-wider border-b border-stone bg-raised/60 whitespace-nowrap ${
        align === 'right' ? 'text-right' : 'text-left'
      } ${active ? 'text-accent' : 'text-muted'} ${className}`}>
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        title={`Sort by ${label}`}
        aria-label={`Sort by ${label}${active ? ` (${sort.dir === 'asc' ? 'ascending' : 'descending'})` : ''}`}
        className="absolute inset-0 w-full cursor-pointer transition-colors hover:bg-raised/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/60"
      />
      <span className="pointer-events-none inline-flex items-center gap-1">
        {align === 'right' && active && <SortArrow dir={sort.dir} />}
        {label}
        {align === 'left' && active && <SortArrow dir={sort.dir} />}
      </span>
    </th>
  )
}

function SortArrow({ dir }: { dir: 'asc' | 'desc' }) {
  return (
    <svg width="9" height="9" viewBox="0 0 10 10" className="shrink-0">
      {dir === 'asc' ? <polygon points="5,1 9,8 1,8" fill="currentColor" /> : <polygon points="5,9 1,2 9,2" fill="currentColor" />}
    </svg>
  )
}

/* ------------------------------------------------------------------ */
/* Ranked breakdown                                                    */
/* ------------------------------------------------------------------ */

/**
 * A ranked breakdown table: label, share bar, and a handful of numeric
 * columns. Every reporting page needs this shape, so it lives here rather
 * than being re-implemented per page.
 */
export function BreakdownTable({
  rows, sort, onSort, columns, nameHeader, emptyHint, onRowClick, maxRows,
}: {
  rows: any[]
  sort: SortState
  onSort: (k: string) => void
  columns: { key: string; label: string; align?: 'left' | 'right'; render: (r: any) => ReactNode }[]
  nameHeader: string
  emptyHint?: string
  onRowClick?: (r: any) => void
  maxRows?: number
}) {
  const shown = maxRows ? rows.slice(0, maxRows) : rows
  // Share bars are scaled against the largest value in the table so small
  // contributors stay visible instead of collapsing to a sliver.
  const maxCost = Math.max(1e-12, ...rows.map(r => Number(r.cost ?? 0)))

  if (shown.length === 0) {
    return <div className="text-xs text-muted py-6 text-center">{emptyHint || 'Nothing in this window.'}</div>
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr>
            <SortTh label={nameHeader} sortKey="name" sort={sort} onSort={onSort} />
            {columns.map(c => (
              <SortTh key={c.key} label={c.label} sortKey={c.key} sort={sort} onSort={onSort} align={c.align || 'right'} />
            ))}
          </tr>
        </thead>
        <tbody>
          {shown.map((r, i) => (
            <tr key={`${r.name}-${i}`}
              onClick={onRowClick ? () => onRowClick(r) : undefined}
              onKeyDown={onRowClick ? (e) => {
                if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onRowClick(r) }
              } : undefined}
              tabIndex={onRowClick ? 0 : undefined}
              className={`${onRowClick ? 'cursor-pointer hover:bg-raised/60 focus-visible:outline-none focus-visible:bg-raised/60' : ''} transition-colors`}>
              <td className="px-4 py-2.5 border-b border-stone/60 max-w-[280px]">
                <div className="truncate font-mono text-xs" title={r.name}>{r.name}</div>
                <div className="h-1 mt-1 rounded-full bg-raised overflow-hidden">
                  <div className="h-full rounded-full bg-accent"
                    style={{ width: `${Math.max(2, Math.round((Number(r.cost ?? 0) / maxCost) * 100))}%` }} />
                </div>
              </td>
              {columns.map(c => (
                <td key={c.key} className={`px-4 py-2.5 border-b border-stone/60 font-mono tabular-nums whitespace-nowrap ${c.align === 'left' ? 'text-left' : 'text-right'}`}>
                  {c.render(r)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Small pieces                                                        */
/* ------------------------------------------------------------------ */

/** A labelled metric with an optional sub-note and accent tone. */
export function Metric({ label, value, sub, tone = 'neutral', icon }: {
  label: string
  value: ReactNode
  sub?: ReactNode
  tone?: 'neutral' | 'good' | 'warn' | 'bad'
  icon?: IconName
}) {
  const toneCls = tone === 'good' ? 'text-good' : tone === 'warn' ? 'text-amber' : tone === 'bad' ? 'text-danger' : ''
  return (
    <Card>
      <div className="flex items-center gap-1.5 text-xs font-medium text-muted">
        {icon && <Icon name={icon} size={13} />}{label}
      </div>
      <div className={`text-2xl leading-none font-semibold -tracking-tight mt-1.5 tabular-nums ${toneCls}`}>{value}</div>
      {sub && <div className="text-xs text-muted mt-1.5">{sub}</div>}
    </Card>
  )
}

/** Panel wrapper with a heading and optional right-hand slot. */
export function Panel({ title, hint, right, children }: {
  title: string
  hint?: string
  right?: ReactNode
  children: ReactNode
}) {
  return (
    // min-w-0: panels are grid items, and a grid item defaults to
    // `min-width: auto`, so the whitespace-nowrap numeric cells inside a
    // BreakdownTable stretched the item past its track and pushed the whole
    // page into horizontal scroll on phones.
    <Card className="min-w-0">
      <div className="flex items-start justify-between gap-3 mb-3">
        <div>
          <h3 className="text-[15px] font-semibold -tracking-tight">{title}</h3>
          {hint && <p className="text-xs text-muted mt-0.5">{hint}</p>}
        </div>
        {right}
      </div>
      {children}
    </Card>
  )
}

export const TONES = {
  cost: '#6366F1',
  cache: '#38BDF8',
  error: '#F87171',
  latency: '#FBBF24',
  tokens: '#C084FC',
}
