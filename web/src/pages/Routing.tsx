import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { LBRule, LBMemberInput, RoutingStrategy } from '../lib/api'
import {
  Badge, Button, Card, Confirm, EmptyState, ErrorNote, Field, HealthDot,
  Icon, PageHeader, SegmentedControl, TableShell, Td, Th, useToast,
} from '../components/ui'
import ModelCombobox from '../components/ModelCombobox'

type ProviderRow = {
  id: string
  name: string
  type: string
  health_status?: string | null
  last_health?: string
}

/** One discovered (provider, model) pair — the unit the picker selects. */
type ProviderModelRow = {
  provider_id?: string
  provider_name?: string
  provider_type?: string
  model_id?: string
  display_name?: string
}

const STRATEGIES: { value: RoutingStrategy; label: string; hint: string }[] = [
  { value: 'failover', label: 'Failover', hint: 'Try the primary model first. If it fails before a response starts, walk the rest of the list in order.' },
  { value: 'round_robin', label: 'Round robin', hint: 'Rotate evenly through the list, one model per request. Order is the rotation order.' },
  { value: 'random', label: 'Random', hint: 'Pick one healthy model from the list at random for each request.' },
  { value: 'weighted', label: 'Weighted', hint: 'Split traffic by each model\u2019s weight (1\u2013100). A weight of 70 against 30 sends about 70% of requests there.' },
]

/** One builder row = one routing option: a specific provider + specific model. */
type BuilderMember = LBMemberInput & { uid: string; name?: string; type?: string; health_status?: string | null }

let uidSeq = 0
const nextUid = () => `m${++uidSeq}`

/** Picker value for a discovered pair. Provider name can contain anything but a newline. */
const pairValue = (providerName: string, modelId: string) => `${providerName}\n${modelId}`

export default function Routing({ role = 'admin' }: { role?: string }){
  // LB rules (read AND write) are admin-only server-side.
  const isAdmin = role === 'admin'
  const [rules, setRules] = useState<LBRule[]>([])
  const [providers, setProviders] = useState<ProviderRow[]>([])
  // Discovered (provider, model) pairs — powers the option picker. Each
  // provider keeps its own model id, so "claude-sonnet" on Anthropic and
  // "claude-3-5-sonnet" on Bedrock are two distinct options.
  const [providerModels, setProviderModels] = useState<ProviderModelRow[]>([])
  const [modelsLoading, setModelsLoading] = useState(true)
  const [discovering, setDiscovering] = useState(false)

  // Builder state. `members` holds ordered option inputs; array order is the
  // member position / failover order sent to PUT /lb/rules/{model}. The same
  // provider may appear multiple times with different models — each row is
  // keyed by uid, not provider id.
  const [model, setModel] = useState('')
  const [strategy, setStrategy] = useState<RoutingStrategy>('failover')
  const [members, setMembers] = useState<BuilderMember[]>([])
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

  // Reference data: all providers + discovered (provider, model) pairs.
  useEffect(()=>{
    ;(async ()=>{
      try{
        const p = await api.providers.list()
        setProviders(Array.isArray(p) ? (p as ProviderRow[]) : [])
      }catch{}
      try{
        const pm = await api.providerModels.list()
        setProviderModels(Array.isArray(pm?.data) ? pm.data : [])
      }catch{}
      finally{ setModelsLoading(false) }
    })()
  }, [])

  const refreshModels = async ()=>{
    setDiscovering(true)
    try{
      await api.providerModels.discoverAll()
      const pm = await api.providerModels.list()
      setProviderModels(Array.isArray(pm?.data) ? pm.data : [])
      toast.success('Models refreshed from provider APIs')
    }catch(e:any){ toast.error(e?.message || String(e)) }
    finally{ setDiscovering(false) }
  }

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

  /** Uniqueness key for an option: (provider, model) pair — mirrors the backend. */
  const optionKey = (providerId: string, override?: string) =>
    `${providerId}\u0000${(override || '').trim().toLowerCase()}`

  // Duplicate options block saving: two identical (provider, model) pairs can
  // never be distinguished at request time.
  const duplicateUids = useMemo(()=>{
    const seen = new Map<string, boolean>()
    const dup = new Map<string, boolean>()
    for(const m of members){
      const k = optionKey(m.provider_id, m.model_override)
      if(seen.get(k)) dup.set(m.uid, true)
      else seen.set(k, false)
    }
    return dup
  }, [members])
  const hasDuplicates = duplicateUids.size > 0
  const missingModel = members.some(m => !(m.model_override || '').trim())

  // One picker entry per discovered pair, grouped by provider — same shape
  // the playground uses. Selecting one fills both the provider and the model
  // id that provider actually serves.
  const comboboxOptions = useMemo(()=>{
    const seen = new Set<string>()
    const out: { value: string; label: string; group: string }[] = []
    for(const pm of providerModels){
      const name = (pm.provider_name || '').trim()
      const id = (pm.model_id || '').trim()
      if(!name || !id) continue
      const key = pairValue(name, id)
      if(seen.has(key)) continue
      seen.add(key)
      out.push({
        value: key,
        label: pm.display_name && pm.display_name !== id ? `${id} — ${pm.display_name}` : id,
        group: name,
      })
    }
    // An existing primary may no longer be in the discovered list. Keep it
    // selectable so the picker still shows what the chain starts with.
    const head = members[0]
    if(head){
      const name = (providerMeta.get(head.provider_id)?.name || head.name || '').trim()
      const id = (head.model_override || '').trim()
      const key = name && id ? pairValue(name, id) : ''
      if(key && !seen.has(key)) out.unshift({ value: key, label: id, group: name })
    }
    return out
  }, [providerModels, members, providerMeta])

  const pairByValue = useMemo(()=>{
    const m = new Map<string, ProviderModelRow>()
    for(const pm of providerModels){
      const name = (pm.provider_name || '').trim()
      const id = (pm.model_id || '').trim()
      if(!name || !id || !pm.provider_id) continue
      if(!m.has(pairValue(name, id))) m.set(pairValue(name, id), pm)
    }
    return m
  }, [providerModels])

  const addOption = (pair: ProviderModelRow)=>{
    const providerId = pair.provider_id || ''
    const modelId = (pair.model_id || '').trim()
    if(!providerId || !modelId) return
    const meta = providerMeta.get(providerId)
    const w = strategy === 'weighted' ? 50 : undefined
    setMembers(prev => [...prev, {
      uid: nextUid(),
      provider_id: providerId,
      model_override: modelId,
      weight: w,
      name: meta?.name || pair.provider_name,
      type: meta?.type || pair.provider_type,
      health_status: meta?.health_status,
    }])
  }

  const onPickPrimary = (next: string[])=>{
    const value = next[next.length - 1]
    if(!value) return
    const pair = pairByValue.get(value)
    if(!pair) return
    const modelId = (pair.model_id || '').trim()
    const providerId = pair.provider_id || ''
    if(!modelId || !providerId) return
    const meta = providerMeta.get(providerId)
    setModel(modelId)
    setMembers(prev => {
      const head: BuilderMember = {
        uid: prev[0]?.uid || nextUid(),
        provider_id: providerId,
        model_override: modelId,
        weight: prev[0]?.weight,
        name: meta?.name || pair.provider_name,
        type: meta?.type || pair.provider_type,
        health_status: meta?.health_status,
      }
      return [head, ...prev.slice(1)]
    })
  }

  const onPickFallback = (next: string[])=>{
    const value = next[next.length - 1]
    if(!value) return
    const pair = pairByValue.get(value)
    if(!pair) return
    addOption(pair)
  }

  const removeMember = (uid:string)=> setMembers(prev => prev.filter(m=> m.uid !== uid))
  const moveMember = (uid:string, dir:-1|1)=>{
    setMembers(prev=>{
      const idx = prev.findIndex(m=> m.uid === uid)
      const to = idx + dir
      if(idx < 0 || to < 0 || to >= prev.length) return prev
      const next = [...prev]
      ;[next[idx], next[to]] = [next[to], next[idx]]
      return next
    })
  }
  const setMemberWeight = (uid:string, weight:number)=>{
    setMembers(prev => prev.map(m=> m.uid === uid ? { ...m, weight } : m))
  }

  const startEdit = (r:LBRule)=>{
    setEditing(r.model)
    setModel(r.model)
    setStrategy(r.strategy || 'round_robin')
    setMembers((r.providers || []).map(m=> ({
      uid: nextUid(),
      provider_id: m.provider_id,
      weight: m.weight || undefined,
      model_override: m.model_override || undefined,
      name: m.name, type: m.type, health_status: m.health_status,
    })))
    setErr('')
    requestAnimationFrame(()=> builderRef.current?.scrollIntoView({ behavior:'smooth', block:'start' }))
  }
  const resetBuilder = ()=>{ setEditing(null); setModel(''); setMembers([]); setStrategy('failover'); setErr('') }

  const save = async ()=>{
    const m = model.trim().toLowerCase()
    if(!m || members.length===0 || missingModel || hasDuplicates) return
    setBusy(true); setErr('')
    try{
      await api.lb.saveRule(m, {
        strategy,
        members: members.map(({ provider_id, model_override, weight })=> ({ provider_id, model_override, weight })),
      })
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
    }
  }

  const canSave = !!model.trim() && members.length > 0 && !hasDuplicates && !missingModel && !busy
  const activeStrategy = STRATEGIES.find(s=> s.value === strategy)

  /** Ordered-member chip controls share one ghost icon-button style. */
  const chipBtnCls =
    'w-6 h-6 rounded-full flex items-center justify-center text-muted hover:text-paper hover:bg-stone transition-colors disabled:opacity-30 disabled:pointer-events-none'

  const chainLabel = (m: BuilderMember) => (m.model_override || '').trim() || m.name || m.provider_id

  const primaryValue = useMemo(()=>{
    const head = members[0]
    if(!head) return ''
    const name = providerMeta.get(head.provider_id)?.name || head.name || ''
    const id = (head.model_override || '').trim()
    if(!name || !id) return ''
    return pairValue(name, id)
  }, [members, providerMeta])

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Connect · Traffic shaping"
        title="Routing"
        description={
          'Pick a primary model, then the other models in the list — the same model on another provider, or a different one. Failover walks the list when one fails; round robin, random, and weighted each pick one model per request. The provider comes with the model. Pin with openai/gpt-4o or X-Provider to bypass.'
        }
        actions={
          <div className="flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={refreshModels} disabled={discovering} title="Fetch model lists from every provider's API">
              <Icon name="zap" size={14}/> {discovering ? 'Discovering…' : 'Refresh models'}
            </Button>
            <Badge tone="good" dot>{rules.length} rules</Badge>
          </div>
        }
      />

      {err && <ErrorNote message={err} />}

      {!isAdmin ? (
        <Card>
          <EmptyState
            icon="route"
            title="Routing is admin-only."
            hint="Load-balancer rules are managed by admins. Bare model names still route through the configured groups — pin a provider with qualified IDs like openai/gpt-4o or an X-Provider header."
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
            hint="Pick a primary model below, then the other models in the list. Choose how traffic is split — failover, round robin, random, or weighted. Requests without a rule or a pin are rejected with model_not_routed."
          />
        </Card>
      ) : (
        <TableShell>
          <table className="w-full text-sm min-w-[640px]">
            <thead>
              <tr>
                <Th>Primary model</Th>
                <Th>Strategy</Th>
                <Th>Models</Th>
                <Th className="text-right">Actions</Th>
              </tr>
            </thead>
            <tbody>
              {rules.map(r=> (
                <tr key={r.model} className={`transition-colors ${editing===r.model ? 'bg-amber/5' : 'hover:bg-app/40'}`}>
                  <Td><span className="font-mono text-sm">{r.model}</span></Td>
                  <Td><Badge tone="neutral">{strategyLabel(r.strategy)}</Badge></Td>
                  <Td>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {(r.providers || []).map((m, i)=> (
                        <span key={`${m.provider_id}-${i}`} className="contents">
                          {i > 0 && <Icon name="chevronDown" size={11} className="-rotate-90 text-muted/60"/>}
                          <span
                            className="inline-flex items-center gap-1.5 bg-app/60 border border-stone rounded-full pl-1 pr-2.5 py-1 text-xs font-mono">
                            <span className={`w-5 h-5 rounded-full text-[10px] flex items-center justify-center shrink-0 ${i===0 ? 'bg-accent text-onaccent' : 'bg-stone'}`}>
                              {i===0 ? '1' : i+1}
                            </span>
                            <HealthDot health={m.health_status} />
                            <span className="max-w-[180px] truncate" title={`${m.name} · ${m.model_override || r.model}`}>
                              {m.model_override || r.model}
                            </span>
                            <span className="text-muted/70 max-w-[90px] truncate">via {m.name}</span>
                            {!!m.weight && r.strategy === 'weighted' && <span className="text-muted">w:{m.weight}</span>}
                          </span>
                        </span>
                      ))}
                      {(r.providers||[]).length===0 && <span className="text-muted text-xs">no models</span>}
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
        <Card className={editing ? 'border-accent' : ''}>
          <div className="flex items-start justify-between gap-3 mb-4">
            <div>
              <h2 className="font-semibold tracking-tight flex items-center gap-2">
                <Icon name="route" size={16} className="text-accent"/>
                {editing ? <>Edit <span className="font-mono">"{editing}"</span></> : 'New route'}
              </h2>
            </div>
            {editing && (
              <Button variant="ghost" size="sm" onClick={resetBuilder} disabled={busy}>
                <Icon name="x" size={14}/> Cancel edit
              </Button>
            )}
          </div>

          <div className="grid gap-4 md:grid-cols-2">
            <Field
              label="Primary model"
              hint="What callers send. The provider comes with the pick. Under failover this is also the first model tried."
              className="max-w-xl"
            >
              <ModelCombobox
                mode="single"
                allowCustom={false}
                value={primaryValue ? [primaryValue] : []}
                onChange={onPickPrimary}
                options={comboboxOptions}
                loading={modelsLoading || discovering}
                placeholder="Search the primary model"
                emptyHint="No models discovered yet. Use Refresh models."
                footer="Clients call this model id"
                chipLabel={v => v.split('\n').pop() || v}
              />
            </Field>
            <Field label="How to choose" hint={activeStrategy?.hint}>
              <SegmentedControl<RoutingStrategy>
                options={STRATEGIES.map(s => ({ value: s.value, label: s.label }))}
                value={strategy}
                onChange={setStrategy}
              />
            </Field>
          </div>

          <div className="mt-5">
            <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">
              {strategy === 'failover'
                ? 'Add a fallback — tried only if the models above fail'
                : 'Add a model — another choice in the pool'}
            </div>
            {providers.length===0 ? (
              <div className="border border-dashed border-stone rounded-xl p-4 text-muted text-sm">No providers yet — add one on the Providers page first.</div>
            ) : (
              <>
                <ModelCombobox
                  mode="single"
                  allowCustom={false}
                  value={[]}
                  onChange={onPickFallback}
                  options={comboboxOptions}
                  loading={modelsLoading || discovering}
                  placeholder={members.length ? (strategy === 'failover' ? 'Search a fallback model' : 'Search another model') : 'Pick the primary model first'}
                  emptyHint="No models discovered yet. Use Refresh models."
                  footer={strategy === 'failover'
                    ? 'Tried in order only after the models above fail'
                    : 'Same model on another provider, or a different model entirely'}
                  disabled={members.length === 0}
                />
                {!modelsLoading && comboboxOptions.length === 0 && (
                  <div className="mt-1.5 flex items-center gap-1.5 text-xs text-amber">
                    <Icon name="alert" size={12}/> No models discovered. Refresh models, or add them on the Models page.
                  </div>
                )}
              </>
            )}
          </div>

          <div className="mt-5">
            <div className="text-xs font-medium text-muted uppercase tracking-wide mb-2">
              {strategy === 'failover' ? 'Fallback chain' : 'Model pool'} ({members.length === 0 ? 'empty' : `${members.length} model${members.length===1?'':'s'}`})
            </div>
            {members.length===0 ? (
              <div className="flex min-h-[36px] items-center rounded-lg bg-app/50 border border-stone px-3 py-1.5 text-muted text-xs">
                {strategy === 'failover'
                  ? 'Pick a primary model above. Fallbacks are optional — without them, a failure is just a failure.'
                  : 'Pick a primary model above, then add the other models this strategy chooses from.'}
              </div>
            ) : (
              <div className="space-y-2">
                {members.map((m, i)=> {
                  const meta = providerMeta.get(m.provider_id) ?? { id: m.provider_id, name: m.name || m.provider_id, type: m.type || '', health_status: m.health_status }
                  const isDup = duplicateUids.get(m.uid) === true
                  const noModel = !(m.model_override || '').trim()
                  return (
                    <div key={m.uid} className={`flex flex-wrap items-center gap-2 rounded-lg bg-app/50 border px-2 py-1.5 ${isDup || noModel ? 'border-red-500/60' : 'border-stone'}`}>
                      <span className={`w-5 h-5 rounded-full text-[10px] flex items-center justify-center shrink-0 ${i===0 ? 'bg-accent text-onaccent' : 'bg-stone'}`}>{i+1}</span>
                      <HealthDot health={meta.health_status} />
                      <span className="min-w-0">
                        <span className="text-xs font-mono">{m.model_override || '—'}</span>
                        <span className="ml-1.5 text-[11px] text-muted">{memberRole(strategy, i)} · via {meta.name}</span>
                      </span>
                      {strategy === 'weighted' && (
                        <label className="inline-flex items-center gap-1 text-xs text-muted">
                          w
                          <input
                            type="number" min={1} max={100} value={m.weight ?? 50}
                            onChange={e=> setMemberWeight(m.uid, Math.max(1, Math.min(100, Number(e.target.value) || 1)))}
                            className="w-14 rounded border border-stone bg-raised px-1.5 py-0.5 text-xs font-mono"
                          />
                        </label>
                      )}
                      {isDup && (
                        <span className="text-[11px] text-red-400">duplicate option — same provider + model</span>
                      )}
                      {noModel && (
                        <span className="text-[11px] text-red-400">no model — remove and pick one from the list</span>
                      )}
                      <span className="flex-1"/>
                      <button type="button" onClick={()=>moveMember(m.uid,-1)} disabled={i===0}
                        aria-label={`Move ${chainLabel(m)} up`} title="Move up" className={chipBtnCls}>
                        <Icon name="chevronDown" size={12} className="rotate-180"/>
                      </button>
                      <button type="button" onClick={()=>moveMember(m.uid,1)} disabled={i===members.length-1}
                        aria-label={`Move ${chainLabel(m)} down`} title="Move down" className={chipBtnCls}>
                        <Icon name="chevronDown" size={12}/>
                      </button>
                      <button type="button" onClick={()=>removeMember(m.uid)} aria-label={`Remove ${chainLabel(m)}`} title="Remove"
                        className={`${chipBtnCls} hover:!text-red-400`}>
                        <Icon name="x" size={12}/>
                      </button>
                    </div>
                  )
                })}
              </div>
            )}
          </div>

          <div className="mt-5 flex flex-wrap items-center gap-3">
            <Button variant="primary" onClick={save} disabled={!canSave}>
              <Icon name="check" size={15}/> {busy ? 'Saving' : editing ? 'Save changes' : 'Create route'}
            </Button>
            {!canSave && !busy && (
              <span className="font-mono text-[11px] text-muted">
                {hasDuplicates
                  ? 'Remove duplicate provider + model options first.'
                  : missingModel
                    ? 'Every option needs a model — remove any that were saved without one and pick again.'
                    : 'Pick a primary model to start the list.'}
              </span>
            )}
          </div>
          {err && <div className="mt-3"><ErrorNote message={err} /></div>}
        </Card>
      </div>

      <Card className="bg-app/40">
        <div className="font-mono text-xs text-muted uppercase tracking-wide">Tip</div>
        <p className="text-xs text-muted mt-1 leading-relaxed">
          The primary model is what callers send. Every other row is another model — the same one on a
          different provider, or a different model — and the strategy decides how the list is used.{' '}
          <span className="text-paper">Failover</span> tries them in order and moves on only when one fails
          before a response starts. <span className="text-paper">Round robin</span> rotates,{' '}
          <span className="text-paper">random</span> picks one per request, and{' '}
          <span className="text-paper">weighted</span> splits traffic by the weights on each row.
          Each pick carries the provider that serves it. Requests pinned to{' '}
          <span className="text-paper">provider/model</span> or via <span className="text-paper">X-Provider</span>{' '}
          skip the list. Use <span className="text-paper">Refresh models</span> to pull each provider's list.
        </p>
      </Card>

      <Confirm
        open={!!pendingDelete}
        onClose={()=>setPendingDelete(null)}
        onConfirm={performRemoveRule}
        title="Delete routing rule"
        body={
          pendingDelete
            ? `Delete routing rule "${pendingDelete.model}"? Requests for this model will be rejected as model_not_routed unless pinned to a provider.`
            : ''
        }
        confirmLabel="Delete"
      />
    </div>
  )
}

function strategyLabel(s: string): string {
  return STRATEGIES.find(x => x.value === s)?.label || s || 'Failover'
}

function memberRole(strategy: RoutingStrategy, index: number): string {
  if (index === 0) return 'primary'
  if (strategy === 'failover') return 'fallback'
  return 'in pool'
}
