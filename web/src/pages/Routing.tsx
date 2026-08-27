import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { LBRule } from '../lib/api'
import {
  Badge, Button, Card, Confirm, EmptyState, ErrorNote, Field, HealthDot,
  Icon, Input, PageHeader, TableShell, Td, Th, useToast,
} from '../components/ui'

type ProviderRow = {
  id: string
  name: string
  type: string
  health_status?: string | null
  last_health?: string
}

type ProviderModelRow = { model_id?: string }

export default function Routing({ role = 'admin' }: { role?: string }){
  // LB rules (read AND write) are admin-only server-side.
  const isAdmin = role === 'admin'
  const [rules, setRules] = useState<LBRule[]>([])
  const [providers, setProviders] = useState<ProviderRow[]>([])
  // Distinct model_ids discovered across providers — suggestions for the model input.
  const [suggestions, setSuggestions] = useState<string[]>([])

  // Builder state. `members` holds ordered provider ids; array order is the
  // round-robin walk order sent to PUT /api/lb/rules/{model}.
  const [model, setModel] = useState('')
  const [members, setMembers] = useState<string[]>([])
  const [editing, setEditing] = useState<string | null>(null) // model being edited; null = creating

  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  // Presentation-only: destructive confirmation for rule deletion.
  const [pendingDelete, setPendingDelete] = useState<LBRule | null>(null)
  const [loadError, setLoadError] = useState('')
  const toast = useToast()
  const builderRef = useRef<HTMLDivElement>(null)

  const loadRules = async ()=>{
    try{
      const r = await api.lb.listRules()
      setRules(Array.isArray(r) ? r : [])
      setLoadError('')
    }catch(e:any){ setLoadError(e?.message || String(e)) }
  }
  useEffect(()=>{ if(isAdmin) loadRules() }, [isAdmin])

  // Reference data: all providers + discovered model ids. Non-fatal on failure.
  useEffect(()=>{
    ;(async ()=>{
      try{
        const p = await api.providers.list()
        setProviders(Array.isArray(p) ? (p as ProviderRow[]) : [])
      }catch{}
      try{
        const pm = await api.providerModels.list()
        const rows: ProviderModelRow[] = Array.isArray(pm) ? pm : []
        const ids = new Set<string>()
        for(const m of rows){
          const id = m?.model_id
          if(typeof id === 'string' && id.trim()) ids.add(id.trim())
        }
        setSuggestions(Array.from(ids).sort((a,b)=> a.localeCompare(b)))
      }catch{}
    })()
  }, [])

  // Model input suggestions: distinct discovered model_ids plus existing rule keys.
  const modelOptions = useMemo(()=>{
    const ids = new Set(suggestions)
    for(const r of rules){ if(r.model) ids.add(r.model) }
    return Array.from(ids).sort((a,b)=> a.localeCompare(b))
  }, [suggestions, rules])

  // Resolve member id → display info. Falls back to data embedded in rules
  // when the provider list doesn't cover every member.
  const providerMeta = useMemo(()=>{
    const m = new Map<string, ProviderRow>()
    for(const p of providers) m.set(p.id, p)
    for(const r of rules){
      for(const mem of r.providers || []){
        if(!mem || !mem.provider_id || m.has(mem.provider_id)) continue
        m.set(mem.provider_id, { id: mem.provider_id, name: mem.name || mem.provider_id, type: mem.type || '', health_status: mem.health_status })
      }
    }
    return m
  }, [providers, rules])

  const metaOf = (id:string): ProviderRow => providerMeta.get(id) ?? { id, name:id, type:'', health_status:null }

  const toggleProvider = (id:string)=>{
    setMembers(prev => prev.includes(id) ? prev.filter(x=> x!==id) : [...prev, id])
  }
  const moveMember = (idx:number, dir:-1|1)=>{
    setMembers(prev=>{
      const next = [...prev]
      const to = idx + dir
      if(to < 0 || to >= next.length) return prev
      ;[next[idx], next[to]] = [next[to], next[idx]]
      return next
    })
  }
  const removeMember = (id:string)=> setMembers(prev => prev.filter(x=> x!==id))

  const startEdit = (r:LBRule)=>{
    setEditing(r.model)
    setModel(r.model)
    setMembers((r.providers || []).map(m=> m.provider_id))
    setErr('')
    requestAnimationFrame(()=> builderRef.current?.scrollIntoView({ behavior:'smooth', block:'start' }))
  }
  const resetBuilder = ()=>{ setEditing(null); setModel(''); setMembers([]); setErr('') }

  const save = async ()=>{
    const m = model.trim().toLowerCase()
    if(!m || members.length===0) return
    setBusy(true); setErr('')
    try{
      await api.lb.saveRule(m, members)
      toast.success('Routing rule saved')
      if(editing && editing !== m){
        // Renamed while editing: retire the old key so the rule moves, not forks.
        try{ await api.lb.deleteRule(editing) }catch{}
      }
      resetBuilder()
      await loadRules()
    }catch(e:any){
      setErr(e.message || String(e))
      toast.error(e.message || String(e))
    }
    finally{ setBusy(false) }
  }

  // Confirm-gated by pendingDelete.
  const performRemoveRule = async ()=>{
    const r = pendingDelete
    if(!r) return
    setPendingDelete(null)
    setErr('')
    try{
      await api.lb.deleteRule(r.model)
      toast.success('Routing rule deleted')
      if(editing === r.model) resetBuilder()
      await loadRules()
    }catch(e:any){
      setErr(e.message || String(e))
      toast.error(e.message || String(e))
    }
  }

  const canSave = !!model.trim() && members.length > 0 && !busy

  /** Ordered-member chip controls share one ghost icon-button style. */
  const chipBtnCls =
    'w-6 h-6 rounded-full flex items-center justify-center text-muted hover:text-paper hover:bg-stone transition-colors disabled:opacity-30 disabled:pointer-events-none'

  return (
    <div className="space-y-6">
      <PageHeader
        title="Model Routing"
        description={
          'Bare model names rotate through their ordered provider group on every request. There is no automatic failover — a failing member keeps returning its own error until you remove or reorder it below. Qualified IDs like openai/gpt-4o or an X-Provider header pin one provider and bypass these groups.'
        }
        actions={<Badge tone="good" dot>{rules.length} rules</Badge>}
      />

      {err && <ErrorNote message={err} />}

      {!isAdmin ? (
        <Card>
          <EmptyState
            icon="route"
            title="Routing is admin-only."
            hint="Load-balancer rules are managed by admins. Bare model names still rotate through the configured groups — pin a provider with qualified IDs like openai/gpt-4o or an X-Provider header."
          />
        </Card>
      ) : loadError ? (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load routing rules"
            hint={loadError}
            action={<Button variant="secondary" onClick={loadRules}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      ) : (
      /* Rules table */
      rules.length===0 ? (
        <Card>
          <EmptyState
            icon="route"
            title="No routing rules yet."
            hint="Build an ordered provider group below for a bare model name. Requests rotate through its members in order; no automatic failover. Pinning via qualified IDs like openai/gpt-4o always wins over these groups."
          />
        </Card>
      ) : (
        <TableShell>
          <table className="w-full text-sm min-w-[640px]">
            <thead>
              <tr>
                <Th>Model</Th>
                <Th>Providers (rotated in order)</Th>
                <Th className="text-right">Actions</Th>
              </tr>
            </thead>
            <tbody>
              {rules.map(r=> (
                <tr key={r.model} className={`transition-colors ${editing===r.model ? 'bg-amber/5' : 'hover:bg-app/40'}`}>
                  <Td><span className="font-mono text-sm">{r.model}</span></Td>
                  <Td>
                    <div className="flex flex-wrap gap-1.5">
                      {(r.providers || []).map((m, i)=> (
                        <span key={`${m.provider_id}-${i}`}
                          className="inline-flex items-center gap-1.5 bg-app/60 border border-stone rounded-full pl-1 pr-2.5 py-1 text-xs font-mono">
                          <span className="w-5 h-5 rounded-full bg-stone text-xs flex items-center justify-center shrink-0">{i+1}</span>
                          <HealthDot health={m.health_status} />
                          <span className="max-w-[180px] truncate">{m.name}</span>
                        </span>
                      ))}
                      {(r.providers||[]).length===0 && <span className="text-muted text-xs">no members</span>}
                    </div>
                  </Td>
                  <Td className="text-right whitespace-nowrap">
                    <div className="inline-flex gap-1.5">
                      <Button variant="secondary" size="sm" onClick={()=>startEdit(r)}>
                        <Icon name="pencil" size={13}/> Edit
                      </Button>
                      <Button variant="ghost" size="sm" onClick={()=>setPendingDelete(r)} title={`Delete rule ${r.model}`}
                        className="hover:text-red-400">
                        <Icon name="trash" size={14}/> Delete
                      </Button>
                    </div>
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableShell>
      )
      )}

      {/* Builder / editor */}
      <div ref={builderRef} className="scroll-mt-24">
        <Card className={editing ? 'border-teal/40' : ''}>
          <div className="flex items-start justify-between gap-3 mb-4">
            <div>
              <h2 className="font-semibold tracking-tight flex items-center gap-2">
                <Icon name="route" size={16} className="text-teal"/>
                {editing ? <>Edit <span className="font-mono">"{editing}"</span></> : 'New route group'}
              </h2>
              <div className="font-mono text-xs text-muted mt-1">Member order below = round-robin walk order across requests.</div>
            </div>
            {editing && (
              <Button variant="ghost" size="sm" onClick={resetBuilder} disabled={busy}>
                <Icon name="x" size={14}/> Cancel edit
              </Button>
            )}
          </div>

          <Field label="Model" className="max-w-xl">
            <Input
              list="routing-model-suggestions"
              value={model}
              onChange={e=>setModel(e.target.value)}
              placeholder="bare model name — e.g. gpt-4o-mini"
              className="font-mono"
            />
            <datalist id="routing-model-suggestions">
              {modelOptions.map(id=> <option key={id} value={id} />)}
            </datalist>
          </Field>

          <div className="mt-5">
            <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">
              Providers — check to add / uncheck to remove
            </div>
            {providers.length===0 ? (
              <div className="border border-dashed border-stone rounded-xl p-4 text-muted text-sm">No providers yet — add one on the Providers page first.</div>
            ) : (
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-2">
                {providers.map(p=> {
                  const checked = members.includes(p.id)
                  return (
                    <label key={p.id} className={`flex items-center gap-2.5 border rounded-lg px-3 py-2 cursor-pointer transition-colors focus-within:ring-2 focus-within:ring-teal/30 ${checked ? 'border-teal/50 bg-teal/5' : 'border-stone hover:bg-app/60'}`}>
                      <input type="checkbox" checked={checked} onChange={()=>toggleProvider(p.id)} className="accent-teal rounded shrink-0"/>
                      <HealthDot health={p.health_status} />
                      <span className="truncate text-sm flex-1 min-w-0">{p.name}</span>
                      {p.type && <Badge tone="neutral">{p.type}</Badge>}
                    </label>
                  )
                })}
              </div>
            )}
          </div>

          <div className="mt-5">
            <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">
              Ordered members ({members.length})
            </div>
            <div className="flex flex-wrap gap-1.5 min-h-[36px] items-center rounded-lg bg-app/50 border border-stone px-2 py-1.5">
              {members.map((id, i)=> {
                const meta = metaOf(id)
                return (
                  <span key={id} className="inline-flex items-center gap-1 bg-raised border border-stone rounded-full pl-1 pr-1 py-1 text-xs font-mono">
                    <span className="w-5 h-5 rounded-full bg-stone text-xs flex items-center justify-center shrink-0">{i+1}</span>
                    <HealthDot health={meta.health_status} />
                    <span className="max-w-[160px] truncate">{meta.name}</span>
                    <button type="button" onClick={()=>moveMember(i,-1)} disabled={i===0}
                      aria-label={`Move ${meta.name} up`} title="Move up" className={chipBtnCls}>
                      <Icon name="chevronDown" size={12} className="rotate-180"/>
                    </button>
                    <button type="button" onClick={()=>moveMember(i,1)} disabled={i===members.length-1}
                      aria-label={`Move ${meta.name} down`} title="Move down" className={chipBtnCls}>
                      <Icon name="chevronDown" size={12}/>
                    </button>
                    <button type="button" onClick={()=>removeMember(id)} aria-label={`Remove ${meta.name}`} title="Remove"
                      className={`${chipBtnCls} hover:!text-red-400`}>
                      <Icon name="x" size={12}/>
                    </button>
                  </span>
                )
              })}
              {members.length===0 && <span className="text-muted text-xs px-1">No providers selected yet.</span>}
            </div>
          </div>

          <div className="mt-5 flex flex-wrap items-center gap-3">
            <Button variant="primary" onClick={save} disabled={!canSave}>
              <Icon name="check" size={15}/> {busy ? 'Saving' : editing ? 'Save changes' : 'Create group'}
            </Button>
            {!canSave && !busy && (
              <span className="font-mono text-[11px] text-muted">Needs a model name and at least one provider.</span>
            )}
          </div>
          {err && <div className="mt-3"><ErrorNote message={err} /></div>}
        </Card>
      </div>

      <Card className="bg-app/40">
        <div className="font-mono text-xs text-muted uppercase tracking-wide">Tip</div>
        <p className="text-xs text-muted mt-1 leading-relaxed">
          Rules are keyed by lowercased model name. Saving an edit under a new name moves the rule.
          Requests pinned to <span className="text-paper">provider/model</span> or via{' '}
          <span className="text-paper">X-Provider</span> skip these groups entirely.
        </p>
      </Card>

      <Confirm
        open={!!pendingDelete}
        onClose={()=>setPendingDelete(null)}
        onConfirm={performRemoveRule}
        title="Delete routing rule"
        body={
          pendingDelete
            ? `Delete routing rule "${pendingDelete.model}"? Requests for this model will stop rotating through its group.`
            : ''
        }
        confirmLabel="Delete"
      />
    </div>
  )
}
