import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { ModelGroup, LBMemberInput, RoutingStrategy, LBMember } from '../lib/api'
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

type ProviderModel = {
  provider_id: string
  provider_name?: string
  model_id: string
}

const STRATEGIES: { value: RoutingStrategy; label: string; hint: string }[] = [
  { value: 'round_robin', label: 'Round robin', hint: 'Rotate evenly across members, one provider per request.' },
  { value: 'random', label: 'Random', hint: 'Pick a healthy member at random per request.' },
  { value: 'weighted', label: 'Weighted', hint: 'Split traffic proportionally to each member\u2019s weight (1\u2013100).' },
  { value: 'failover', label: 'Failover', hint: 'Always use the first healthy member in order; later members only on failure.' },
]

type BuilderMember = LBMemberInput & { name?: string; type?: string; health_status?: string | null; provider_name?: string }

export default function ModelGroups({ role = 'admin' }: { role?: string }){
  const isAdmin = role === 'admin'
  const [groups, setGroups] = useState<ModelGroup[]>([])
  const [providers, setProviders] = useState<ProviderRow[]>([])
  const [providerModels, setProviderModels] = useState<ProviderModel[]>([])
  const [discovering, setDiscovering] = useState(false)

  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [strategy, setStrategy] = useState<RoutingStrategy>('failover')
  const [members, setMembers] = useState<BuilderMember[]>([])
  const [editing, setEditing] = useState<string | null>(null)
  const [selectedProvider, setSelectedProvider] = useState<string | null>(null)

  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [pendingDelete, setPendingDelete] = useState<ModelGroup | null>(null)
  const toast = useToast()
  const builderRef = useRef<HTMLDivElement>(null)

  const loadGroups = async () => {
    try {
      const g = await api.lb.listGroups()
      setGroups(Array.isArray(g) ? g : [])
      setLoadError('')
    } catch (e: any) { setLoadError(e?.message || String(e)) }
  }
  useEffect(() => { if (isAdmin) loadGroups() }, [isAdmin])

  useEffect(() => {
    ;(async () => {
      try { const p = await api.providers.list(); setProviders(Array.isArray(p) ? p : []) } catch {}
      try { const pm = await api.providerModels.list(); setProviderModels(Array.isArray(pm) ? pm : []) } catch {}
    })()
  }, [])

  const providerMeta = useMemo(() => {
    const m = new Map<string, ProviderRow>()
    for (const p of providers) m.set(p.id, p)
    for (const g of groups) {
      for (const mem of g.members || []) {
        if (!mem || !mem.provider_id || m.has(mem.provider_id)) continue
        m.set(mem.provider_id, { id: mem.provider_id, name: mem.name || mem.provider_id, type: mem.type || '', health_status: mem.health_status })
      }
    }
    return m
  }, [providers, groups])

  const modelsByProvider = useMemo(() => {
    const map = new Map<string, string[]>()
    for (const pm of providerModels) {
      const arr = map.get(pm.provider_id) || []
      arr.push(pm.model_id)
      map.set(pm.provider_id, arr)
    }
    for (const arr of map.values()) arr.sort((a, b) => a.localeCompare(b))
    return map
  }, [providerModels])

  const addMember = (providerId: string, modelId: string) => {
    if (members.some(m => m.provider_id === providerId && m.model_override === modelId)) return
    const meta = providerMeta.get(providerId)
    const w = strategy === 'weighted' ? 50 : undefined
    setMembers(prev => [...prev, {
      provider_id: providerId,
      model_override: modelId,
      weight: w,
      name: meta?.name,
      type: meta?.type,
      health_status: meta?.health_status,
      provider_name: meta?.name,
    }])
  }
  const removeMember = (idx: number) => setMembers(prev => prev.filter((_, i) => i !== idx))
  const moveMember = (idx: number, dir: -1 | 1) => {
    setMembers(prev => {
      const next = [...prev]
      const to = idx + dir
      if (to < 0 || to >= next.length) return prev
      ;[next[idx], next[to]] = [next[to], next[idx]]
      return next
    })
  }
  const setMemberWeight = (idx: number, weight: number) => {
    setMembers(prev => prev.map((m, i) => i === idx ? { ...m, weight } : m))
  }

  const refreshModels = async () => {
    setDiscovering(true)
    try {
      await api.providerModels.discoverAll()
      const pm = await api.providerModels.list()
      setProviderModels(Array.isArray(pm) ? pm : [])
      toast.success('Models refreshed from provider APIs')
    } catch (e: any) { toast.error(e?.message || String(e)) }
    finally { setDiscovering(false) }
  }

  const startEdit = (g: ModelGroup) => {
    setEditing(g.name)
    setName(g.name)
    setDisplayName(g.display_name || '')
    setStrategy((g.strategy || 'failover') as RoutingStrategy)
    setMembers((g.members || []).map(m => ({
      provider_id: m.provider_id,
      model_override: m.model_override || '',
      weight: m.weight,
      name: m.name, type: m.type, health_status: m.health_status,
      provider_name: m.name,
    })))
    setErr('')
    requestAnimationFrame(() => builderRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
  }
  const resetBuilder = () => { setEditing(null); setName(''); setDisplayName(''); setMembers([]); setStrategy('failover'); setSelectedProvider(null); setErr('') }

  const validate = () => {
    const n = name.trim().toLowerCase()
    if (!n) return 'Group name required.'
    if (!/^[a-z0-9._/-]{1,64}$/.test(n)) return 'Group name may only contain a-z, 0-9, \'.,\', \'_\', \'-\', \'/\'.'
    if (members.length === 0) return 'Add at least one provider/model pair.'
    return ''
  }

  const save = async () => {
    const v = validate()
    if (v) { setErr(v); return }
    setBusy(true); setErr('')
    try {
      await api.lb.saveGroup(name.trim().toLowerCase(), {
        display_name: displayName.trim() || undefined,
        strategy,
        members: members.map(({ provider_id, model_override, weight }) => ({
          provider_id,
          model_override: model_override?.trim() || undefined,
          weight,
        })),
      })
      toast.success('Model group saved')
      if (editing && editing !== name.trim().toLowerCase()) {
        try { await api.lb.deleteGroup(editing) } catch {}
      }
      resetBuilder()
      await loadGroups()
    } catch (e: any) {
      setErr(e.message || String(e))
      toast.error(e.message || String(e))
    } finally { setBusy(false) }
  }

  const performDelete = async () => {
    const g = pendingDelete
    if (!g) return
    setPendingDelete(null)
    try {
      await api.lb.deleteGroup(g.name)
      toast.success('Model group deleted')
      if (editing === g.name) resetBuilder()
      await loadGroups()
    } catch (e: any) { setErr(e.message || String(e)) }
  }

  const canSave = !!name.trim() && members.length > 0 && !busy
  const activeStrategy = STRATEGIES.find(s => s.value === strategy)

  const chipBtnCls =
    'w-6 h-6 rounded-full flex items-center justify-center text-muted hover:text-paper hover:bg-stone transition-colors disabled:opacity-30 disabled:pointer-events-none'

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="Connect · Traffic shaping"
        title="Model groups"
        description={
          'User-created groups like "subagent-dispatcher" map one name to an ordered list of provider/model members. When one member rate-limits or fails, the gateway falls back to the next.'
        }
        actions={
          <div className="flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={refreshModels} disabled={discovering} title="Fetch model lists from every provider's API">
              <Icon name="zap" size={14}/> {discovering ? 'Discovering…' : 'Refresh models'}
            </Button>
            <Badge tone="good" dot>{groups.length} group{groups.length === 1 ? '' : 's'}</Badge>
          </div>
        }
      />

      {err && <ErrorNote message={err} />}

      {!isAdmin ? (
        <Card>
          <EmptyState
            icon="route"
            title="Model groups are admin-only."
            hint="Admins create groups here; clients send the group name as the model in chat completions."
          />
        </Card>
      ) : loadError ? (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load model groups"
            hint={loadError}
            action={<Button variant="secondary" onClick={loadGroups}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      ) : groups.length === 0 ? (
        <Card>
          <EmptyState
            icon="route"
            title="No model groups yet."
            hint="Create a group below so clients can send a single name like subagent-dispatcher and the gateway will fan out across providers."
          />
        </Card>
      ) : (
        <TableShell>
          <table className="w-full text-sm min-w-[640px]">
            <thead>
              <tr>
                <Th>Group name</Th>
                <Th>Display name</Th>
                <Th>Strategy</Th>
                <Th>Members (in order)</Th>
                <Th className="text-right">Actions</Th>
              </tr>
            </thead>
            <tbody>
              {groups.map(g => (
                <tr key={g.name} className={`transition-colors ${editing === g.name ? 'bg-amber/5' : 'hover:bg-app/40'}`}>
                  <Td><span className="font-mono text-sm">{g.name}</span></Td>
                  <Td>{g.display_name || <span className="text-muted">—</span>}</Td>
                  <Td><Badge tone="neutral">{g.strategy || 'round_robin'}</Badge></Td>
                  <Td>
                    <div className="flex flex-wrap gap-1.5">
                      {(g.members || []).map((m, i) => (
                        <span key={`${m.provider_id}-${i}`}
                          className="inline-flex items-center gap-1.5 bg-app/60 border border-stone rounded-full pl-1 pr-2.5 py-1 text-xs font-mono">
                          <span className="w-5 h-5 rounded-full bg-stone text-xs flex items-center justify-center shrink-0">{i + 1}</span>
                          <HealthDot health={m.health_status} />
                          <span className="max-w-[180px] truncate">{m.name}</span>
                          <span className="text-muted">→ {m.model_override}</span>
                          {!!m.weight && strategyAllowsWeight(g.strategy || 'round_robin') && <span className="text-muted">w:{m.weight}</span>}
                        </span>
                      ))}
                      {(g.members || []).length === 0 && <span className="text-muted text-xs">no members</span>}
                    </div>
                  </Td>
                  <Td className="text-right whitespace-nowrap">
                    <div className="inline-flex gap-1.5">
                      <Button variant="secondary" size="sm" onClick={() => startEdit(g)}>
                        <Icon name="pencil" size={13}/> Edit
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setPendingDelete(g)} title={`Delete group ${g.name}`}
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
      )}

      <div ref={builderRef} className="scroll-mt-24">
        <Card className={editing ? 'border-accent' : ''}>
          <div className="flex items-start justify-between gap-3 mb-4">
            <div>
              <h2 className="font-semibold tracking-tight flex items-center gap-2">
                <Icon name="route" size={16} className="text-accent"/>
                {editing ? <>Edit <span className="font-mono">"{editing}"</span></> : 'New model group'}
              </h2>
              <div className="font-mono text-xs text-muted mt-1">{activeStrategy?.hint}</div>
            </div>
            {editing && (
              <Button variant="ghost" size="sm" onClick={resetBuilder} disabled={busy}>
                <Icon name="x" size={14}/> Cancel edit
              </Button>
            )}
          </div>

          <div className="grid gap-4 md:grid-cols-3">
            <Field label="Group name" className="max-w-xl">
              <Input
                value={name}
                onChange={e => setName(e.target.value)}
                disabled={!!editing}
                placeholder="subagent-dispatcher"
                className="font-mono"
              />
            </Field>
            <Field label="Display name">
              <Input
                value={displayName}
                onChange={e => setDisplayName(e.target.value)}
                placeholder="Subagent dispatcher"
              />
            </Field>
            <Field label="Strategy">
              <select
                value={strategy}
                onChange={e => setStrategy(e.target.value as RoutingStrategy)}
                className="w-full max-w-xs rounded-lg border border-stone bg-raised px-3 py-2 text-sm"
              >
                {STRATEGIES.map(s => <option key={s.value} value={s.value}>{s.label}</option>)}
              </select>
            </Field>
          </div>

          <div className="mt-6 grid gap-4 lg:grid-cols-2">
            {/* Left: provider picker */}
            <div className="space-y-3">
              <div className="text-xs font-medium text-muted uppercase tracking-wide">1. Select a provider</div>
              {providers.length === 0 ? (
                <div className="border border-dashed border-stone rounded-xl p-4 text-muted text-sm">No providers yet — add one on the Providers page first.</div>
              ) : (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
                  {providers.map(p => {
                    const selected = selectedProvider === p.id
                    const count = members.filter(m => m.provider_id === p.id).length
                    return (
                      <button
                        key={p.id}
                        type="button"
                        onClick={() => setSelectedProvider(selected ? null : p.id)}
                        className={`flex items-center gap-2.5 border rounded-lg px-3 py-2 text-left transition-colors focus-within:ring-2 focus-within:ring-accent/30 ${selected ? 'border-accent/50 bg-accent/5' : 'border-stone hover:bg-app/60'}`}>
                        <HealthDot health={p.health_status} />
                        <span className="truncate text-sm flex-1 min-w-0">{p.name}</span>
                        {p.type && <Badge tone="neutral">{p.type}</Badge>}
                        {count > 0 && <Badge tone="good">{count}</Badge>}
                      </button>
                    )
                  })}
                </div>
              )}

              {selectedProvider && (
                <div className="space-y-2">
                  <div className="text-xs font-medium text-muted uppercase tracking-wide">
                    2. Pick a model for {providerMeta.get(selectedProvider)?.name || selectedProvider}
                  </div>
                  {(() => {
                    const models = modelsByProvider.get(selectedProvider) || []
                    return models.length === 0 ? (
                      <div className="border border-dashed border-stone rounded-xl p-3 text-muted text-sm">
                        No discovered models. Use Refresh models or type a model id below.
                      </div>
                    ) : (
                      <div className="grid grid-cols-1 sm:grid-cols-2 gap-1.5 max-h-64 overflow-y-auto pr-1">
                        {models.map(mid => {
                          const already = members.some(m => m.provider_id === selectedProvider && m.model_override === mid)
                          return (
                            <button
                              key={mid}
                              type="button"
                              onClick={() => addMember(selectedProvider, mid)}
                              disabled={already}
                              className={`text-left text-xs font-mono border rounded-lg px-2.5 py-1.5 transition-colors ${already ? 'opacity-40 cursor-not-allowed border-stone bg-app/30' : 'border-stone hover:bg-accent/5 hover:border-accent/30'}`}>
                              {mid}
                            </button>
                          )
                        })}
                      </div>
                    )
                  })()}
                  <div className="flex items-center gap-2">
                    <Input
                      placeholder="type model id"
                      onKeyDown={e => {
                        if (e.key !== 'Enter') return
                        const v = (e.target as HTMLInputElement).value.trim()
                        if (v && selectedProvider) { addMember(selectedProvider, v); (e.target as HTMLInputElement).value = '' }
                      }}
                      className="font-mono text-xs flex-1"
                    />
                    <span className="text-muted text-xs">Press Enter</span>
                  </div>
                </div>
              )}
            </div>

            {/* Right: selected members */}
            <div className="space-y-3">
              <div className="text-xs font-medium text-muted uppercase tracking-wide">
                Selected members ({members.length})
              </div>
              {members.length === 0 ? (
                <div className="flex min-h-[120px] items-center justify-center rounded-lg bg-app/50 border border-stone px-3 py-1.5 text-muted text-sm">
                  Click a provider, then pick a model. Selected pairs appear here in failover order.
                </div>
              ) : (
                <div className="space-y-2">
                  {members.map((m, i) => {
                    const meta = providerMeta.get(m.provider_id) ?? { id: m.provider_id, name: m.provider_name || m.provider_id, type: m.type || '', health_status: m.health_status }
                    return (
                      <div key={`${m.provider_id}-${m.model_override}-${i}`} className="flex flex-wrap items-center gap-2 rounded-lg bg-app/50 border border-stone px-2 py-1.5">
                        <span className="w-5 h-5 rounded-full bg-stone text-xs flex items-center justify-center shrink-0">{i + 1}</span>
                        <HealthDot health={meta.health_status} />
                        <span className="max-w-[160px] truncate text-xs font-mono">{meta.name}</span>
                        <span className="text-xs text-muted">→</span>
                        <span className="text-xs font-mono">{m.model_override}</span>
                        {strategy === 'weighted' && (
                          <label className="inline-flex items-center gap-1 text-xs text-muted">
                            w
                            <input
                              type="number" min={1} max={100} value={m.weight ?? 50}
                              onChange={e => setMemberWeight(i, Math.max(1, Math.min(100, Number(e.target.value) || 1)))}
                              className="w-14 rounded border border-stone bg-raised px-1.5 py-0.5 text-xs font-mono"
                            />
                          </label>
                        )}
                        <span className="flex-1"/>
                        <button type="button" onClick={() => moveMember(i, -1)} disabled={i === 0} aria-label="Move up" title="Move up" className={chipBtnCls}>
                          <Icon name="chevronDown" size={12} className="rotate-180"/>
                        </button>
                        <button type="button" onClick={() => moveMember(i, 1)} disabled={i === members.length - 1} aria-label="Move down" title="Move down" className={chipBtnCls}>
                          <Icon name="chevronDown" size={12}/>
                        </button>
                        <button type="button" onClick={() => removeMember(i)} aria-label="Remove" title="Remove" className={`${chipBtnCls} hover:!text-red-400`}>
                          <Icon name="x" size={12}/>
                        </button>
                      </div>
                    )
                  })}
                </div>
              )}
            </div>
          </div>

          <div className="mt-5 flex flex-wrap items-center gap-3">
            <Button variant="primary" onClick={save} disabled={!canSave}>
              <Icon name="check" size={15}/> {busy ? 'Saving' : editing ? 'Save changes' : 'Create group'}
            </Button>
            {!canSave && !busy && (
              <span className="font-mono text-[11px] text-muted">Needs a group name and at least one provider/model pair.</span>
            )}
          </div>
        </Card>
      </div>

      <Card className="bg-app/40">
        <div className="font-mono text-xs text-muted uppercase tracking-wide">Tip</div>
        <p className="text-xs text-muted mt-1 leading-relaxed">
          Groups are keyed by lowercase name and are resolved before aliases. Clients send the group name as the model in <span className="text-paper">/v1/chat/completions</span>. Failover is the usual choice for subagent dispatchers: if the first provider/model hits a rate limit, the next member takes over.
        </p>
      </Card>

      <Confirm
        open={!!pendingDelete}
        onClose={() => setPendingDelete(null)}
        onConfirm={performDelete}
        title="Delete model group"
        body={pendingDelete ? `Delete model group "${pendingDelete.name}"? Clients sending this name will get model_not_routed.` : ''}
        confirmLabel="Delete"
      />
    </div>
  )
}

function strategyAllowsWeight(s: RoutingStrategy): boolean {
  return s === 'weighted'
}
