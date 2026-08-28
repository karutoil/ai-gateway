import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'

type Option = string | { value: string; label: string; group?: string }

type Props = {
  value: string[]
  onChange: (next: string[]) => void
  options: Option[]
  placeholder?: string
  disabled?: boolean
  loading?: boolean
}

// normalizeOptions accepts plain strings (label = value, no group) and
// richer {value,label,group} objects; grouping is presentation-only.
function normalizeOptions(options: Option[]): { value: string; label: string; group: string }[] {
  return options.map(o =>
    typeof o === 'string'
      ? { value: o, label: o, group: '' }
      : { value: o.value, label: o.label || o.value, group: o.group || '' }
  )
}

export default function ModelCombobox({ value, onChange, options, placeholder = 'Search models...', disabled, loading }: Props) {
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const [highlight, setHighlight] = useState(0)
  const rootRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const dropdownRef = useRef<HTMLDivElement>(null)
  const [rect, setRect] = useState<DOMRect | null>(null)

  const opts = normalizeOptions(options)
  const trimmed = query.trim()
  const showAddCustom = trimmed.length > 0 && !value.includes(trimmed)
  const filtered = (() => {
    const q = query.toLowerCase().trim()
    const match = (o: { value: string; label: string; group: string }) =>
      o.value.toLowerCase().includes(q) || o.label.toLowerCase().includes(q) || o.group.toLowerCase().includes(q)
    if (!q) return opts.slice(0, 200)
    return opts.filter(match).slice(0, 200)
  })()

  type Row = { kind: 'add'; value: string } | { kind: 'opt'; value: string; label: string; group: string }
  const rows: Row[] = []
  if (showAddCustom) rows.push({ kind: 'add', value: trimmed })
  for (const o of filtered) rows.push({ kind: 'opt', value: o.value, label: o.label, group: o.group })

  const selectedSet = new Set(value)

  const updateRect = () => {
    if (rootRef.current) setRect(rootRef.current.getBoundingClientRect())
  }
  useEffect(() => {
    if (!open) return
    updateRect()
    const onScroll = () => updateRect()
    const onResize = () => updateRect()
    window.addEventListener('scroll', onScroll, true)
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('scroll', onScroll, true)
      window.removeEventListener('resize', onResize)
    }
  }, [open, query, value.length])

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node
      if (rootRef.current?.contains(target)) return
      if (dropdownRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [])

  useEffect(() => {
    if (highlight >= rows.length) setHighlight(rows.length ? rows.length - 1 : 0)
  }, [rows.length, highlight])

  const add = (v: string) => {
    const t = v.trim()
    if (!t || value.includes(t)) return
    onChange([...value, t])
    setQuery('')
    setOpen(true)
    setHighlight(0)
    requestAnimationFrame(() => inputRef.current?.focus())
  }
  const remove = (v: string) => {
    onChange(value.filter(x => x !== v))
  }
  const toggle = (v: string) => {
    if (selectedSet.has(v)) remove(v)
    else add(v)
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (!open) setOpen(true)
      else setHighlight(h => Math.min(h + 1, rows.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setHighlight(h => Math.max(h - 1, 0))
    } else if (e.key === 'Enter') {
      if (!open) {
        if (trimmed && !value.includes(trimmed)) {
          e.preventDefault()
          add(trimmed)
        }
        return
      }
      if (rows.length === 0) return
      e.preventDefault()
      const cur = rows[highlight]
      if (!cur) return
      if (cur.kind === 'add') add(cur.value)
      else toggle(cur.value)
    } else if (e.key === 'Escape') {
      if (open) {
        e.preventDefault()
        e.stopPropagation()
        setOpen(false)
        return
      }
    } else if (e.key === 'Backspace' && !query && value.length) {
      remove(value[value.length - 1])
    }
  }

  const dropdown = open && rect ? (() => {
    const dropdownHeight = 300
    const spaceBelow = window.innerHeight - rect.bottom - 8
    const spaceAbove = rect.top - 8
    let top: number
    if (spaceBelow < dropdownHeight && spaceAbove > spaceBelow) {
      top = Math.max(8, rect.top - dropdownHeight - 8)
    } else {
      top = rect.bottom + 8
      if (top + dropdownHeight > window.innerHeight - 8) {
        top = Math.max(8, window.innerHeight - dropdownHeight - 8)
      }
    }
    let left = rect.left
    let width = rect.width
    if (left + width > window.innerWidth - 8) {
      left = Math.max(8, window.innerWidth - width - 8)
    }
    return (
      <div
        ref={dropdownRef}
        style={{
          position: 'fixed',
          top,
          left,
          width,
          zIndex: 60,
        }}
        className="bg-surface border border-stone rounded-xl shadow-xl overflow-hidden"
      >
        <div className="max-h-[260px] overflow-auto py-1">
          {rows.length === 0 ? (
            <div className="px-3 py-3 text-xs text-muted text-center">
              {loading ? 'Loading models…' : query ? 'No matches — press Enter to add as wildcard' : 'No models found'}
            </div>
          ) : (
            rows.map((r, idx) => {
              const isAdd = r.kind === 'add'
              const isSelected = !isAdd && selectedSet.has(r.value)
              const isActive = idx === highlight
              // Group header when this row starts a new provider group.
              const prev = idx > 0 ? rows[idx - 1] : null
              const showGroup = !isAdd && !!r.group && (!prev || prev.kind === 'add' || prev.group !== r.group)
              return (
                <div key={`${r.kind}-${r.value}-${idx}`}>
                  {showGroup && (
                    <div className="px-3 pt-2 pb-1 text-[10px] font-semibold uppercase tracking-wider text-amber/80 bg-graphite/40 border-t border-stone/40 first:border-t-0">
                      {r.group}
                    </div>
                  )}
                <button
                  type="button"
                  onMouseEnter={() => setHighlight(idx)}
                  onMouseDown={e => { e.preventDefault(); if (isAdd) add(r.value); else toggle(r.value) }}
                  className={`w-full text-left px-3 py-2 flex items-center justify-between gap-2 text-sm ${isActive ? 'bg-amber/15' : 'hover:bg-graphite/60'} font-mono text-xs`}
                >
                  <span className="flex items-center gap-2 min-w-0">
                    {isAdd ? (
                      <>
                        <span className="w-5 h-5 rounded-full border border-amber/40 bg-amber/10 flex items-center justify-center text-[10px] text-amber">+</span>
                        <span className="truncate">Add <span className="text-amber font-semibold">"{r.value}"</span></span>
                        <span className="hidden sm:inline font-mono text-[10px] text-muted ml-1">(wildcard allowed)</span>
                      </>
                    ) : (
                      <>
                        <span className={`w-5 h-5 rounded-md border flex items-center justify-center text-[10px] leading-none shrink-0 ${isSelected ? 'bg-teal border-teal text-ink' : 'border-stone bg-graphite text-transparent'}`}>
                          {isSelected ? '✓' : ''}
                        </span>
                        <span className="truncate" title={r.value}>{r.label}</span>
                      </>
                    )}
                  </span>
                  {isAdd ? (
                    <span className="font-mono text-[10px] text-muted shrink-0">Enter</span>
                  ) : isSelected ? (
                    <span className="font-mono text-[10px] text-teal shrink-0">selected</span>
                  ) : null}
                </button>
                </div>
              )
            })
          )}
        </div>
        <div className="border-t border-stone/50 px-3 py-2 flex items-center justify-between">
          <span className="font-mono text-[10px] text-muted">{value.length === 0 ? 'Empty = all models allowed' : `${value.length} model${value.length===1?'':'s'} restricted`}</span>
          {value.length > 0 && (
            <button type="button" onMouseDown={e=>{e.preventDefault(); onChange([])}} className="font-mono text-[10px] text-muted hover:text-paper underline">Clear all</button>
          )}
        </div>
      </div>
    )
  })() : null

  return (
    <div ref={rootRef} className="relative">
      <div
        onClick={() => inputRef.current?.focus()}
        className={`flex flex-wrap items-center gap-1.5 min-h-[42px] bg-graphite border rounded-xl px-2 py-2 cursor-text transition-colors ${open ? 'border-amber/50 ring-1 ring-amber/20' : 'border-stone hover:border-stone'} ${disabled ? 'opacity-60 pointer-events-none' : ''}`}
      >
        {value.map(v => (
          <span key={v} className="inline-flex items-center gap-1 bg-surface border border-stone rounded-full pl-2.5 pr-1 py-1 text-xs font-mono">
            <span className="max-w-[180px] truncate" title={v}>{v}</span>
            <button
              type="button"
              onClick={e => { e.stopPropagation(); remove(v) }}
              className="ml-0.5 w-5 h-5 flex items-center justify-center rounded-full hover:bg-stone text-muted hover:text-paper leading-none"
              aria-label={`Remove ${v}`}
            >
              ×
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          value={query}
          onChange={e => { setQuery(e.target.value); setOpen(true); setHighlight(0) }}
          onFocus={() => { setOpen(true); updateRect() }}
          onKeyDown={onKeyDown}
          placeholder={value.length === 0 ? placeholder : 'Add model...'}
          className="flex-1 min-w-[140px] bg-transparent outline-none text-sm placeholder:text-muted/60 px-1 py-0.5"
          disabled={disabled}
          autoComplete="off"
          spellCheck={false}
        />
        {loading && <span className="font-mono text-[10px] text-muted px-1">loading…</span>}
        {!loading && <span className="ml-auto text-muted text-xs px-1 select-none pointer-events-none">{open ? '▴' : '▾'}</span>}
      </div>

      {dropdown ? createPortal(dropdown, document.body) : null}
    </div>
  )
}
