import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import ModelCombobox from '../components/ModelCombobox'
import {
  PageHeader, Card, Button, Input, Field, Badge, Icon, CopyButton,
  TableShell, Th, Td, TableSkeleton, EmptyState, ErrorNote, Modal, Confirm, useToast,
} from '../components/ui'

/** Roles allowed to mutate keys (mirrors backend RequireRole). */
const KEY_WRITER_ROLES = ['admin', 'support', 'member']

type EditModalState = {
  id: string
  prefix: string
  name: string
  rpm: string
  rph: string
  rpd: string
  tpm: string
  allowed: string[]
}

type CreateForm = { name: string; rpm: string; rph: string; rpd: string; tpm: string; allowed: string[] }

const EMPTY_CREATE: CreateForm = { name: '', rpm: '', rph: '', rpd: '', tpm: '', allowed: [] }

const CREATED_COL = 7

function ago(iso?: string | null): string {
  if (!iso) return '—'
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d ago`
  return new Date(iso).toLocaleDateString()
}

export default function Keys({ role = 'admin' }: { role?: string }){
  const canWrite = KEY_WRITER_ROLES.includes(role)
  const [list, setList] = useState<any[]>([])
  const [name, setName] = useState('')
  const [newKey, setNewKey] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [editingId, setEditingId] = useState<string | null>(null)
  const [editingName, setEditingName] = useState('')
  const [busy, setBusy] = useState(false)
  const [editModal, setEditModal] = useState<EditModalState | null>(null)
  const [saving, setSaving] = useState(false)
  const [availableModels, setAvailableModels] = useState<string[]>([])
  const [modelsLoading, setModelsLoading] = useState(false)

  // presentation-only state (design system): loading skeleton, modals, sticky errors
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState<CreateForm>(EMPTY_CREATE)
  const [createBusy, setCreateBusy] = useState(false)
  const [createError, setCreateError] = useState('')
  const [confirmRevoke, setConfirmRevoke] = useState<{ ids: string[]; label: string } | null>(null)
  const [editError, setEditError] = useState('')

  const toast = useToast()

  const load = () => api.keys.list()
    .then((r) => { setList(r as any[]); setLoadError(''); setLoading(false) })
    .catch((e:any) => { setLoadError(e?.message || String(e)); setLoading(false) })
  useEffect(() => { load() }, [])

  const loadModels = async ()=>{
    if (availableModels.length > 0 || modelsLoading) return
    setModelsLoading(true)
    try{
      const providerRes = await api.providerModels.list().catch(()=>null)
      const providerIds: string[] = []
      if(providerRes){
        const arr2 = (providerRes as any).data ?? providerRes
        if(Array.isArray(arr2)) for(const m of arr2) {
          const modelId = m?.model_id || m?.id
          if(!modelId) continue
          // provider/model display to disambiguate duplicates
          // Prefer provider_name, fallback to provider_type or provider_id
          const providerLabel = m?.provider_name || m?.provider_type || (m?.provider_id ? String(m.provider_id).slice(0,8) : '')
          const display = providerLabel ? `${providerLabel}/${String(modelId)}` : String(modelId)
          providerIds.push(display)
        }
      }
      // Merge catalog ids so keys can restrict to models not yet discovered
      // from a provider (e.g. freshly synced catalog entries).
      const catalogRes = await api.catalog.list(undefined, undefined, undefined, 200).catch(()=>null)
      const catalogArr = (catalogRes as any)?.data ?? catalogRes
      if(Array.isArray(catalogArr)) for(const m of catalogArr) {
        const id = m?.model_id || m?.id
        if(id) providerIds.push(String(id))
      }
      const merged = Array.from(new Set(providerIds)).sort((a,b)=> a.localeCompare(b))
      setAvailableModels(merged)
    } finally { setModelsLoading(false) }
  }

  useEffect(()=>{ loadModels() }, [])

  /* ---------------- create key (modal; same POST /api/keys endpoint) --------------- */

  const parseOpt = (v: string): number | undefined | 'invalid' => {
    const s = v.trim()
    if (!s) return undefined
    const n = parseInt(s, 10)
    return Number.isNaN(n) ? 'invalid' : n
  }

  const openCreate = () => {
    loadModels()
    setCreateForm(EMPTY_CREATE); setCreateError(''); setCreateOpen(true)
  }

  const create = async ()=>{
    const trimmed = name.trim()
    if(!trimmed){ setCreateError('Name is required.'); return }
    setCreateError('')
    const opts: Record<string, unknown> = {}
    for (const [field, key] of [['rpm','rate_limit_rpm'],['rph','rate_limit_rph'],['rpd','rate_limit_rpd'],['tpm','rate_limit_tpm']] as const) {
      const parsed = parseOpt(createForm[field])
      if (parsed === 'invalid') { setCreateError(`${field.toUpperCase()} must be a number.`); return }
      if (typeof parsed === 'number') opts[key] = parsed
    }
    const allowed = Array.from(new Set(createForm.allowed))
    if (allowed.length > 0) opts['allowed_models'] = allowed
    setCreateBusy(true)
    try{
      const k = await api.keys.create(trimmed, Object.keys(opts).length ? opts : undefined)
      setNewKey(k.key)
      setName('')
      setCreateForm(EMPTY_CREATE)
      setCreateOpen(false)
      toast.success(`Key "${trimmed}" created`)
      load()
    }catch(e:any){
      setCreateError(e.message || String(e))
      toast.error('Could not create key')
    } finally { setCreateBusy(false) }
  }

  const toggleSelect = (id:string)=>{
    setSelected(prev=>{
      const next = new Set(prev)
      if(next.has(id)) next.delete(id); else next.add(id)
      return next
    })
  }
  const toggleAll = ()=>{
    if(selected.size === list.length) setSelected(new Set())
    else setSelected(new Set(list.map(k=>k.id)))
  }
  const clearSelection = ()=> setSelected(new Set())

  const bulkRevoke = async()=>{
    if(selected.size===0 || !confirmRevoke) return
    setBusy(true)
    try{
      await api.keys.bulkRemove(Array.from(confirmRevoke.ids))
      toast.success(`Revoked ${confirmRevoke.ids.length} key(s)`)
      clearSelection()
      setConfirmRevoke(null)
      load()
    }catch(e:any){ toast.error(e.message || 'Failed to revoke keys') } finally{ setBusy(false)}
  }

  /* ---------------- rename (pencil → modal; same PUT endpoint) --------------- */

  const startEdit = (k:any)=>{
    if(selected.size>0) return
    setEditingId(k.id)
    setEditingName(k.name)
  }
  const saveEdit = async()=>{
    if(!editingId) return
    const trimmed = editingName.trim()
    if(!trimmed){ setEditingId(null); return}
    try{
      await api.keys.update(editingId, {name: trimmed})
      toast.success('Key renamed')
      setEditingId(null)
      load()
    }catch(e:any){ toast.error(e.message || 'Rename failed')}
  }
  const cancelEdit = ()=>{ setEditingId(null); setEditingName('') }

  /* ---------------- limits & allowlist editor (same PUT /limits + PUT name) --------------- */

  const openEditModal = (k:any)=>{
    loadModels()
    setEditModal({
      id: k.id,
      prefix: k.prefix || '',
      name: k.name || '',
      rpm: k.rate_limit_rpm != null ? String(k.rate_limit_rpm) : '',
      rph: k.rate_limit_rph ? String(k.rate_limit_rph) : '',
      rpd: k.rate_limit_rpd ? String(k.rate_limit_rpd) : '',
      tpm: k.rate_limit_tpm ? String(k.rate_limit_tpm) : '',
      allowed: Array.isArray(k.allowed_models) ? [...k.allowed_models] : [],
    })
    setEditError('')
  }
  const closeEditModal = ()=>{ if(!saving) setEditModal(null) }

  const saveEditModal = async()=>{
    if(!editModal) return
    const trimmed = editModal.name.trim()
    if(!trimmed){ setEditError('Name is required.'); return }
    setSaving(true)
    try{
      const original = list.find(x=> x.id===editModal.id)
      if(original && trimmed !== original.name){
        await api.keys.update(editModal.id, { name: trimmed })
      }
      // Empty string = leave the stored value untouched (omit from the PUT
      // payload). Sending 0 for RPM would reset it to the 60/min default.
      const parseIntField = (v:string): number | undefined =>{
        const s = v.trim()
        if(!s) return undefined
        const n = parseInt(s,10)
        return Number.isNaN(n) ? undefined : n
      }
      const payload: Record<string, unknown> = {}
      const rpm = parseIntField(editModal.rpm)
      if(rpm !== undefined) payload['rate_limit_rpm'] = rpm
      const rph = parseIntField(editModal.rph)
      if(rph !== undefined) payload['rate_limit_rph'] = rph
      const rpd = parseIntField(editModal.rpd)
      if(rpd !== undefined) payload['rate_limit_rpd'] = rpd
      const tpm = parseIntField(editModal.tpm)
      if(tpm !== undefined) payload['rate_limit_tpm'] = tpm
      payload['allowed_models'] = editModal.allowed
      await api.keys.setLimits(editModal.id, payload)
      setEditModal(null)
      toast.success('Key updated')
      load()
    }catch(e:any){
      toast.error(e.message || String(e))
    } finally { setSaving(false) }
  }

  const revokeOne = async ()=>{
    if(!confirmRevoke) return
    try{
      await Promise.all(confirmRevoke.ids.map(id => api.keys.remove(id)))
      toast.success('Key revoked')
      setConfirmRevoke(null)
      load()
    }catch(e:any){ toast.error(e.message || 'Failed to revoke key') }
  }

  const isSelectMode = selected.size>0
  const limitChips = (k:any) => [
    { label: `rpm ${k.rate_limit_rpm ?? 60}`, on: true },
    { label: `rph ${k.rate_limit_rph}`, on: !!k.rate_limit_rph },
    { label: `rpd ${k.rate_limit_rpd}`, on: !!k.rate_limit_rpd },
    { label: `tpm ${k.rate_limit_tpm}`, on: !!k.rate_limit_tpm },
  ].filter(c => c.on)

  return (
    <div className="space-y-6">
      <PageHeader
        title="API Keys"
        description="Virtual sk-gw-* credentials that authenticate requests to the /v1/* gateway endpoints."
        actions={
          <>
            <Badge tone="neutral">{list.length} {list.length === 1 ? 'key' : 'keys'}</Badge>
            {canWrite && (
              <Button variant="primary" onClick={openCreate}><Icon name="plus" size={15} /> Create key</Button>
            )}
          </>
        }
      />

      {loadError && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load API keys"
            hint={loadError}
            action={<Button variant="secondary" onClick={load}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {/* show-once secret — unmissable, secure-feeling */}
      {newKey && (
        <div className="rounded-xl border border-teal/30 bg-teal/10 p-4">
          <div className="flex items-start gap-3">
            <div className="w-9 h-9 rounded-lg bg-teal/15 text-teal flex items-center justify-center shrink-0">
              <Icon name="lock" size={17} />
            </div>
            <div className="min-w-0 flex-1">
              <div className="text-sm font-semibold text-teal">New key created — copy it now</div>
              <div className="text-xs text-muted mt-0.5">Store it now — shown only once. We never display the full secret again.</div>
              <div className="mt-2.5 flex items-center gap-2 rounded-lg border border-teal/30 bg-graphite/60 px-3 py-2">
                <code className="font-mono text-sm text-paper break-all min-w-0 flex-1">{newKey}</code>
                <CopyButton value={newKey} label="Copy key" />
              </div>
            </div>
            <Button variant="ghost" size="sm" onClick={()=>setNewKey(null)} title="Dismiss"><Icon name="x" size={14} /></Button>
          </div>
        </div>
      )}

      {isSelectMode && (
        <Card className="border-amber/30 bg-amber/5 flex flex-wrap items-center justify-between gap-3 !py-3.5">
          <span className="flex items-center gap-2 text-sm text-amber font-medium">
            <Icon name="alert" size={15} />
            {selected.size} key(s) selected
          </span>
          <div className="flex items-center gap-2">
            <Button variant="danger" size="sm" disabled={busy}
              onClick={()=>setConfirmRevoke({ ids: Array.from(selected), label: `${selected.size} selected key(s)` })}>
              Revoke selected
            </Button>
            <Button variant="ghost" size="sm" onClick={clearSelection}>Clear</Button>
          </div>
        </Card>
      )}

      <TableShell>
        <table className="w-full text-sm min-w-[860px]">
          <thead>
            <tr>
              <Th className="w-10">
                <input type="checkbox" checked={list.length>0 && selected.size===list.length} onChange={toggleAll}
                  className="w-4 h-4 accent-teal cursor-pointer rounded" aria-label="Select all keys" />
              </Th>
              <Th>Name</Th>
              <Th>Prefix</Th>
              <Th>Limits</Th>
              <Th>Created</Th>
              <Th>Last used</Th>
              <Th className="text-right">Actions</Th>
            </tr>
          </thead>
          {loading ? (
            <TableSkeleton rows={4} cols={CREATED_COL} />
          ) : (
            <tbody>
              {list.map(k=> (
                <tr key={k.id} className={`${isSelectMode && selected.has(k.id) ? 'bg-teal/5' : ''} hover:bg-stone/20 transition-colors`}>
                  <Td className="text-center">
                    <input type="checkbox" checked={selected.has(k.id)} onChange={()=>toggleSelect(k.id)}
                      className="w-4 h-4 accent-teal cursor-pointer rounded" aria-label={`Select ${k.name}`} />
                  </Td>
                  <Td>
                    <div className="group flex items-center gap-1.5 max-w-[220px]">
                      <span className="font-medium truncate" title={k.name}>{k.name}</span>
                      {canWrite && !isSelectMode && (
                        <button onClick={()=>startEdit(k)} title="Rename"
                          className="shrink-0 text-muted hover:text-paper transition-colors focus-visible:text-paper focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50 rounded">
                          <Icon name="pencil" size={13} />
                        </button>
                      )}
                    </div>
                  </Td>
                  <Td>
                    <div className="flex items-center gap-1 -ml-1.5">
                      <code className="font-mono text-xs text-muted bg-app border border-stone rounded-md px-1.5 py-0.5">{k.prefix}</code>
                      <CopyButton value={String(k.prefix ?? '')} label="Copy prefix" />
                    </div>
                  </Td>
                  <Td>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {limitChips(k).map(c => (
                        <Badge key={c.label} tone="neutral"><span className="tabular-nums">{c.label}</span></Badge>
                      ))}
                    </div>
                    <div className="mt-1">
                      <Badge tone={k.allowed_models?.length ? 'info' : 'neutral'} dot={!!k.allowed_models?.length}>
                        {k.allowed_models?.length ? (
                          <><span className="tabular-nums">{k.allowed_models.length}</span> models allowed</>
                        ) : 'All models'}
                      </Badge>
                    </div>
                  </Td>
                  <Td>
                    <span className="text-xs text-muted whitespace-nowrap" title={k.created_at ? new Date(k.created_at).toLocaleString() : ''}>{ago(k.created_at)}</span>
                  </Td>
                  <Td>
                    <span className="text-xs text-muted whitespace-nowrap" title={k.last_used_at ? new Date(k.last_used_at).toLocaleString() : 'never used'}>
                      {ago(k.last_used_at)}
                    </span>
                  </Td>
                  <Td>
                    {canWrite ? (
                      <div className="flex items-center justify-end gap-1">
                        <Button variant="secondary" size="sm" onClick={()=>openEditModal(k)} title="Edit limits & models">
                          <Icon name="pencil" size={13} /> Edit
                        </Button>
                        <Button variant="ghost" size="sm" className="text-red-400 hover:text-red-300 hover:bg-red-500/10"
                          title="Revoke key"
                          onClick={()=>setConfirmRevoke({ ids: [k.id], label: `"${k.name}"` })}>
                          <Icon name="trash" size={14} />
                        </Button>
                      </div>
                    ) : (
                      <span className="text-xs text-muted/50">read-only</span>
                    )}
                  </Td>
                </tr>
              ))}
              {list.length===0 && (
                <tr>
                  <td colSpan={CREATED_COL}>
                    <EmptyState icon="key" title="No API keys yet"
                      hint={canWrite
                        ? 'Generate a key and give it a nickname — nicknames appear in logs and analytics as prefix → nickname.'
                        : 'No keys exist yet — ask an admin or support user to create one.'}
                      action={canWrite ? <Button variant="primary" onClick={openCreate}><Icon name="plus" size={15} /> Create key</Button> : undefined} />
                  </td>
                </tr>
              )}
            </tbody>
          )}
        </table>
      </TableShell>

      {!loading && list.length > 0 && (
        <p className="flex items-start gap-2 text-xs text-muted -mt-2">
          <Icon name="zap" size={13} className="mt-0.5 shrink-0 text-muted" />
          <span>Click a name to rename · use Edit to manage rate limits &amp; the model allowlist · tick checkboxes to revoke in bulk. Full secrets are never stored for display — only the prefix survives.</span>
        </p>
      )}

      {/* create-key modal */}
      <Modal open={createOpen} onClose={()=>{ if(!createBusy) setCreateOpen(false) }} title="Create API key" width="max-w-xl">
        <div className="space-y-5">
          {createError && <ErrorNote message={createError} />}
          <Field label="Name / nickname">
            <Input value={name} onChange={e=>setName(e.target.value)} onKeyDown={e=>{ if(e.key==='Enter') create() }}
              placeholder="e.g. prod-frontend" autoFocus spellCheck={false} />
          </Field>

          <div>
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
              {([['rpm','RPM','requests / minute'],['rph','RPH','requests / hour'],['rpd','RPD','requests / day'],['tpm','TPM','tokens / minute']] as const).map(([f,label,hint]) => (
                <Field key={f} label={label} hint={hint}>
                  <Input value={createForm[f]} onChange={e=>setCreateForm({...createForm, [f]: e.target.value})}
                    placeholder="—" inputMode="numeric" className="tabular-nums" autoComplete="off" />
                </Field>
              ))}
            </div>
            <p className="text-xs text-muted mt-2">Optional rate limits — blank = unlimited.</p>
          </div>

          <div>
            <div className="flex items-center justify-between mb-1.5">
              <span className="text-xs font-medium text-muted uppercase tracking-wide">Allowed models</span>
              <span className="text-[11px] text-muted tabular-nums">{createForm.allowed.length ? `${createForm.allowed.length} restricted` : 'All models allowed'}</span>
            </div>
            <ModelCombobox
              value={createForm.allowed}
              onChange={v=> setCreateForm(prev=> ({...prev, allowed: v}))}
              options={availableModels}
              loading={modelsLoading}
              placeholder="Search models — e.g. gpt-4o-mini"
            />
            <p className="text-[11px] text-muted mt-2 leading-relaxed">
              Empty = gateway allows every model. Exact id or prefix* wildcard (gpt-4*, Muse-*, openai/gpt-4o-mini). Type a custom wildcard and press Enter.
            </p>
          </div>
        </div>
        <div className="flex justify-end gap-2 mt-6">
          <Button variant="ghost" onClick={()=>setCreateOpen(false)} disabled={createBusy}>Cancel</Button>
          <Button variant="primary" onClick={create} disabled={createBusy || !name.trim()}>
            {createBusy ? 'Creating…' : <> <Icon name="key" size={15} /> Create key </>}
          </Button>
        </div>
      </Modal>

      {/* rename modal */}
      <Modal open={editingId !== null} onClose={()=>{ cancelEdit() }} title="Rename key" width="max-w-sm">
        <Field label="Name / nickname" hint="Nicknames appear in logs & analytics (prefix → nickname).">
          <Input value={editingName} onChange={e=>setEditingName(e.target.value)} autoFocus spellCheck={false}
            onKeyDown={e=>{ if(e.key==='Enter') saveEdit(); }} placeholder="e.g. prod-frontend" />
        </Field>
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={cancelEdit}>Cancel</Button>
          <Button variant="primary" onClick={saveEdit} disabled={!editingName.trim()}>Save</Button>
        </div>
      </Modal>

      {/* limits editor modal (pre-filled) */}
      {editModal && (
        <Modal open onClose={closeEditModal} title="Edit API key" width="max-w-xl">
          <div className="space-y-5 max-h-[70vh] overflow-y-auto pr-1">
            {editError && <ErrorNote message={editError} />}
            <div className="rounded-lg bg-app border border-stone px-3 py-2 text-xs text-muted font-mono">
              {editModal.prefix} <span className="text-paper">·</span> {editModal.id.slice(0,8)}…
            </div>

            <Field label="Name / nickname">
              <Input value={editModal.name} onChange={e=> setEditModal(prev=> prev ? {...prev, name: e.target.value} : prev)}
                placeholder="e.g. prod-frontend" spellCheck={false} />
            </Field>

            <div>
              <div className="text-xs font-medium text-muted mb-2 uppercase tracking-wide">Rate limits</div>
              <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
                <Field label="RPM" hint="per minute">
                  <Input value={editModal.rpm} onChange={e=> setEditModal(prev=> prev ? {...prev, rpm: e.target.value} : prev)}
                    inputMode="numeric" className="tabular-nums" autoComplete="off" />
                </Field>
                <Field label="RPH" hint="per hour">
                  <Input value={editModal.rph} onChange={e=> setEditModal(prev=> prev ? {...prev, rph: e.target.value} : prev)}
                    inputMode="numeric" className="tabular-nums" autoComplete="off" />
                </Field>
                <Field label="RPD" hint="per day">
                  <Input value={editModal.rpd} onChange={e=> setEditModal(prev=> prev ? {...prev, rpd: e.target.value} : prev)}
                    inputMode="numeric" className="tabular-nums" autoComplete="off" />
                </Field>
                <Field label="TPM" hint="tokens / min">
                  <Input value={editModal.tpm} onChange={e=> setEditModal(prev=> prev ? {...prev, tpm: e.target.value} : prev)}
                    inputMode="numeric" className="tabular-nums" autoComplete="off" />
                </Field>
              </div>
              <p className="text-xs text-muted mt-2">Leave a field empty to keep its current value. RPM unset/0 falls back to the 60/min default; RPH / RPD / TPM 0 = unlimited.</p>
            </div>

            <div>
              <div className="flex items-center justify-between mb-1.5">
                <span className="text-xs font-medium text-muted uppercase tracking-wide">Allowed models</span>
                <span className="text-[11px] text-muted tabular-nums">{editModal.allowed.length ? `${editModal.allowed.length} restricted` : 'All models allowed'}</span>
              </div>
              <ModelCombobox
                value={editModal.allowed}
                onChange={v=> setEditModal(prev=> prev ? {...prev, allowed: v} : prev)}
                options={availableModels}
                loading={modelsLoading}
                placeholder="Search models — e.g. gpt-4o-mini"
              />
              <p className="text-[11px] text-muted mt-2 leading-relaxed">
                Empty = gateway allows every model. Add entries to restrict — exact id or prefix* wildcard (gpt-4*, Muse-*, openai/gpt-4o-mini). Type a custom wildcard and press Enter.
              </p>
            </div>
          </div>
          <div className="flex justify-end gap-2 mt-6 pt-4 border-t border-stone">
            <Button variant="ghost" onClick={closeEditModal} disabled={saving}>Cancel</Button>
            <Button variant="primary" onClick={saveEditModal} disabled={saving}>{saving ? 'Saving…' : 'Save changes'}</Button>
          </div>
        </Modal>
      )}

      {/* destructive confirmation */}
      <Confirm
        open={confirmRevoke !== null}
        onClose={()=>setConfirmRevoke(null)}
        onConfirm={confirmRevoke && confirmRevoke.ids.length > 1 ? bulkRevoke : revokeOne}
        busy={busy}
        title="Revoke key?"
        confirmLabel="Revoke"
        body={`Permanently revoke ${confirmRevoke?.label ?? ''}? Existing requests using this key will start failing immediately, and this cannot be undone.`}
      />
    </div>
  )
}
