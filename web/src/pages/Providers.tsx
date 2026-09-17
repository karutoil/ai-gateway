import { useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import {
  Badge, Button, Card, Confirm, CopyButton, EmptyState, ErrorNote, Field,
  HealthDot, Icon, Input, Modal, PageHeader, Progress, Select, Skeleton, useToast, Eyebrow,
} from '../components/ui'

const PROVIDER_WRITER_ROLES = ['admin', 'support', 'member']

function healthTextCls(status?: string | null): string {
  if (status === 'up') return 'text-accent'
  if (status === 'down') return 'text-danger'
  return 'text-muted'
}

function fmtReset(ms?: number | null, resetTime?: string | null): string {
  if (ms == null || ms <= 0) return resetTime ? fmtResetDate(resetTime) : ''
  const mins = Math.round(ms / 60000)
  if (mins < 60) return `${mins}m`
  if (mins > 48 * 60) return resetTime ? fmtResetDate(resetTime) : `${Math.round(mins / 1440)}d`
  const h = Math.floor(mins / 60)
  const m = mins % 60
  return m ? `${h}h ${m}m` : `${h}h`
}

function fmtResetDate(resetTime?: string | null): string {
  if (!resetTime) return ''
  try {
    const d = new Date(resetTime)
    if (isNaN(d.getTime())) return ''
    return d.toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
  } catch { return '' }
}

function fmtResetAt(unix?: number | null): string {
  if (!unix) return ''
  try {
    const d = new Date(unix * 1000)
    if (isNaN(d.getTime())) return ''
    return d.toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
  } catch { return '' }
}

function QuotaBar({ used }: { used?: number | null }) {
  const v = typeof used === 'number' && isFinite(used) ? Math.max(0, Math.min(100, used)) : null
  const tone = v == null ? 'accent' : v >= 90 ? 'red' : v >= 70 ? 'amber' : 'accent'
  return <Progress value={v ?? 0} tone={tone} />
}

function usedOf(m: any): number {
  if (!m) return -1
  if (m.exhausted) return 100
  return typeof m.used_pct === 'number' && isFinite(m.used_pct) ? m.used_pct : -1
}

function sortByUsed(models: any[]): any[] {
  return [...models].sort((a, b) => {
    const d = usedOf(b) - usedOf(a)
    if (d !== 0) return d
    return String(a.label || a.model_id || '').localeCompare(String(b.label || b.model_id || ''))
  })
}

function ModelQuotaRow({ m }: { m: any }) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-2 text-[11px]">
        <span className="text-muted font-medium truncate" title={m.label || m.model_id}>{m.label || m.model_id}</span>
        <span className="font-mono text-paper shrink-0">
          {m.exhausted ? 'exhausted' : m.used_pct != null ? `${Math.round(m.used_pct)}% used` : m.remaining_pct != null ? `${Math.round(m.remaining_pct)}% left` : '—'}
          {(m.resets_in_ms || m.reset_time) ? ` · ${fmtReset(m.resets_in_ms, m.reset_time)}` : ''}
        </span>
      </div>
      <div className="mt-1"><QuotaBar used={m.exhausted ? 100 : m.used_pct} /></div>
    </div>
  )
}

function OAuthUsage({ providerId, defId, type, name }: { providerId: string; defId?: string; type?: string; name?: string }) {
  const kind = (defId || type || '').toLowerCase()
  const [data, setData] = useState<any>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [modalOpen, setModalOpen] = useState(false)
  useEffect(() => {
    let live = true
    setLoading(true); setError(''); setData(null); setModalOpen(false)
    api.oauth.usage(providerId)
      .then((d) => { if (live) setData(d) })
      .catch((e: any) => { if (live) setError(e?.message || String(e)) })
      .finally(() => { if (live) setLoading(false) })
    return () => { live = false }
  }, [providerId])
  if (loading) return <div className="pt-1"><Skeleton className="h-[52px]" /></div>
  if (error) return <div className="text-[11px] text-muted/80 font-mono leading-relaxed">Usage unavailable — {error}</div>
  if (!data) return null
  if (kind === 'devin') {
    const rows: { label: string; used?: number; remaining?: number; reset?: string }[] = []
    if (!data.hide_daily && (data.daily_used != null || data.daily_remaining != null)) {
      rows.push({ label: 'Daily', used: data.daily_used, remaining: data.daily_remaining, reset: fmtResetAt(data.daily_reset_at) })
    }
    if (!data.hide_weekly && (data.weekly_used != null || data.weekly_remaining != null)) {
      rows.push({ label: 'Weekly', used: data.weekly_used, remaining: data.weekly_remaining, reset: fmtResetAt(data.weekly_reset_at) })
    }
    return (
      <div className="space-y-2 pt-0.5">
        <div className="flex items-center gap-1.5 flex-wrap">
          <span className="text-[11px] font-semibold text-paper">{data.plan || 'Devin'}</span>
          {data.extra_balance_usd > 0 && <Badge tone="neutral">${Number(data.extra_balance_usd).toFixed(2)} extra</Badge>}
        </div>
        {rows.length === 0 && <div className="text-[11px] text-muted">Quota details hidden for this plan.</div>}
        {rows.map((r) => (
          <div key={r.label}>
            <div className="flex items-baseline justify-between gap-2 text-[11px]">
              <span className="text-muted font-medium">{r.label}</span>
              <span className="font-mono text-paper">{r.used != null ? `${Math.round(r.used)}% used` : r.remaining != null ? `${Math.round(r.remaining)}% left` : '—'}{r.reset ? ` · resets ${r.reset}` : ''}</span>
            </div>
            <div className="mt-1"><QuotaBar used={r.used} /></div>
          </div>
        ))}
      </div>
    )
  }
  const models = sortByUsed(Array.isArray(data.models) ? data.models : [])
  const credits = data.prompt_credits
  const preview = models.slice(0, 3)
  return (
    <div className="space-y-2 pt-0.5">
      <div className="flex items-center gap-1.5 flex-wrap">
        {data.plan_type && <span className="text-[11px] font-semibold text-paper">{data.plan_type}</span>}
        {credits && (
          <span className="text-[11px] font-mono text-muted">
            {Math.round(credits.used_pct)}% of {Number(credits.monthly).toLocaleString()} monthly credits used
          </span>
        )}
      </div>
      {credits && <QuotaBar used={credits.used_pct} />}
      {models.length === 0 && <div className="text-[11px] text-muted">No per-model quota reported.</div>}
      {preview.map((m: any) => (
        <ModelQuotaRow key={m.model_id} m={m} />
      ))}
      {models.length > 0 && (
        <button
          type="button"
          onClick={() => setModalOpen(true)}
          className="text-[11px] font-semibold text-accent hover:underline"
        >
          View all {models.length} models
        </button>
      )}
      <Modal open={modalOpen} onClose={() => setModalOpen(false)} title={`${name || 'OAuth'} usage`} width="max-w-lg">
        <div className="space-y-3">
          <div className="flex items-center gap-1.5 flex-wrap">
            {data.plan_type && <Badge tone="neutral">{data.plan_type}</Badge>}
            {credits && (
              <span className="text-xs font-mono text-muted">
                {Math.round(credits.used_pct)}% of {Number(credits.monthly).toLocaleString()} monthly credits used
              </span>
            )}
          </div>
          {credits && <QuotaBar used={credits.used_pct} />}
          <div className="text-[11px] font-semibold uppercase tracking-[0.1em] text-muted">Models · most used first</div>
          <div className="space-y-2.5 max-h-[50vh] overflow-y-auto pr-1">
            {models.map((m: any) => (
              <ModelQuotaRow key={m.model_id} m={m} />
            ))}
          </div>
        </div>
      </Modal>
    </div>
  )
}

type EditState = { id: string; name: string; base_url: string; api_key: string }

const TYPE_META: Record<string, { blurb: string }> = {
  openai: { blurb: 'Official OpenAI endpoint' },
  anthropic: { blurb: 'Official Anthropic endpoint' },
  openai_compatible: { blurb: 'Any OpenAI-style base URL' },
  azure: { blurb: 'Azure OpenAI deployment' },
  antigravity: { blurb: 'Google OAuth flow' },
  devin: { blurb: 'Devin OAuth flow' },
}

export default function Providers({ role = 'admin' }: { role?: string }){
  const canWrite = PROVIDER_WRITER_ROLES.includes(role)
  const [list, setList] = useState<any[]>([])
  const [name, setName] = useState('')
  const [type, setType] = useState('openai')
  const [base, setBase] = useState('')
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [confirmTarget, setConfirmTarget] = useState<any>(null)
  const [deleting, setDeleting] = useState(false)
  const [edit, setEdit] = useState<EditState | null>(null)
  const [editBusy, setEditBusy] = useState(false)
  const [editError, setEditError] = useState('')
  const toast = useToast()

  const [oauthDefs, setOauthDefs] = useState<any[]>([])
  const [oauthSession, setOauthSession] = useState<{ auth_url: string; state: string; provider_id: string; provider_name: string; redirect_uri: string } | null>(null)
  const [pasteUrl, setPasteUrl] = useState('')
  const [oauthBusy, setOauthBusy] = useState(false)
  const [oauthError, setOauthError] = useState('')
  const pollTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const load = () => api.providers.list()
    .then((data)=>{ setList(Array.isArray(data) ? data : []); setLoadError('') })
    .catch((e:any)=> setLoadError(e?.message || String(e)))
    .finally(()=> setLoading(false))
  useEffect(()=>{
    load()
    api.oauth.listDefs().then(setOauthDefs).catch(()=>{})
    try {
      const q = new URLSearchParams(window.location.search)
      if (q.get('oauth') === 'connected') {
        toast.success('OAuth connected')
        q.delete('oauth'); q.delete('provider')
        window.history.replaceState({}, '', window.location.pathname + (q.toString() ? `?${q}` : ''))
        load()
      }
    } catch {}
    return ()=>{ if (pollTimer.current) clearTimeout(pollTimer.current) }
  },[])

  const startOAuth = async (defId: string, providerId?: string, providerName?: string) => {
    setOauthError(''); setOauthBusy(true)
    try {
      const s = await api.oauth.start(defId, providerName, providerId)
      setOauthSession({ auth_url: s.auth_url, state: s.state, provider_id: s.provider_id, provider_name: s.provider_name, redirect_uri: s.redirect_uri })
      setPasteUrl('')
      window.open(s.auth_url, '_blank', 'noopener')
    } catch (e: any) { setOauthError(e?.message || String(e)); toast.error(e?.message || String(e)) }
    finally { setOauthBusy(false) }
  }

  const submitPaste = async () => {
    if (!oauthSession) return
    setOauthError(''); setOauthBusy(true)
    try {
      await api.oauth.exchange({ state: oauthSession.state, provider_id: oauthSession.provider_id, callback_url: pasteUrl })
      toast.success(`Connected ${oauthSession.provider_name}`)
      setOauthSession(null); setPasteUrl('')
      load()
    } catch (e: any) { setOauthError(e?.message || String(e)) }
    finally { setOauthBusy(false) }
  }

  const refreshOAuth = async (id: string) => {
    try { await api.oauth.refresh(id); toast.success('Token refreshed'); load() }
    catch (e: any) { toast.error(e?.message || String(e)) }
  }
  const disconnectOAuth = async (id: string) => {
    try { await api.oauth.disconnect(id); toast.success('OAuth disconnected'); load() }
    catch (e: any) { toast.error(e?.message || String(e)) }
  }

  const pollAfterCreate = (providerId: string) => {
    const started = Date.now()
    const tick = async () => {
      if (Date.now() - started > 30_000) return
      try {
        const data: any = await api.providers.list()
        const rows = Array.isArray(data) ? data : []
        setList(rows)
        const p = rows.find((x: any) => x.id === providerId)
        if (p && (p.health_status === 'up' || p.health_status === 'down')) return
      } catch {}
      pollTimer.current = setTimeout(tick, 4_000)
    }
    pollTimer.current = setTimeout(tick, 4_000)
  }

  const create = async ()=>{
    try{
      setErr('')
      const created: any = await api.providers.create({ name, type, base_url: base, api_key: key })
      setName(''); setKey(''); setBase('')
      toast.success('Provider added')
      toast.info('Discovering models in background — check Models in a minute')
      await load()
      if (created?.id) pollAfterCreate(created.id)
    } catch(e:any){ setErr(e.message); toast.error(e.message || String(e)) }
  }

  const openEdit = (p: any) => { setEdit({ id: p.id, name: p.name || '', base_url: p.base_url || '', api_key: '' }); setEditError('') }
  const saveEdit = async ()=>{
    if (!edit) return
    const trimmed = edit.name.trim()
    if (!trimmed) { setEditError('Name is required.'); return }
    setEditBusy(true); setEditError('')
    try {
      const payload: { name: string; base_url?: string; api_key?: string } = { name: trimmed }
      if (edit.base_url.trim()) payload.base_url = edit.base_url.trim()
      if (edit.api_key.trim()) payload.api_key = edit.api_key.trim()
      await api.providers.update(edit.id, payload)
      toast.success('Provider updated')
      setEdit(null); load()
    } catch (e:any) { setEditError(e?.message || String(e)) }
    finally { setEditBusy(false) }
  }

  const doDelete = async ()=>{
    const p = confirmTarget
    if(!p) return
    setDeleting(true)
    try{ await api.providers.remove(p.id); toast.success('Provider removed'); setConfirmTarget(null); load() }
    catch(e:any){ toast.error(e.message || String(e)) }
    finally{ setDeleting(false) }
  }

  const up = list.filter(p => p.health_status === 'up').length
  const down = list.filter(p => p.health_status === 'down').length

  return (
    <div className="space-y-6">
      <PageHeader eyebrow="Connect · Supply" title="Providers" description="Upstream endpoints behind every bare model name. Health is probed after discovery — down providers are skipped by failover."
        actions={<><Badge tone="good" dot>{list.length} connected</Badge>{up>0 && <Badge tone="neutral">{up} healthy</Badge>}{down>0 && <Badge tone="bad">{down} down</Badge>}</>} />

      {loadError && (
        <Card><EmptyState icon="alert" title="Could not load providers" hint={loadError}
          action={<Button variant="secondary" onClick={load}><Icon name="refresh" size={15}/>Retry</Button>} /></Card>
      )}

      {canWrite && (
        <Card className="overflow-hidden">
          <div className="flex flex-wrap items-start justify-between gap-3 mb-5">
            <div>
              <Eyebrow>New upstream</Eyebrow>
              <h2 className="font-display font-bold text-lg tracking-tight mt-1.5">Add provider</h2>
              <p className="text-xs text-muted mt-1">Models auto-discover in the background after creation.</p>
            </div>
            <Badge tone="neutral">{TYPE_META[type]?.blurb || type}</Badge>
          </div>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <Field label="Name"><Input placeholder="my-openai" value={name} onChange={e=>setName(e.target.value)} /></Field>
            <Field label="Type">
              <Select value={type} onChange={e=>setType(e.target.value)}>
                <option value="openai">openai</option>
                <option value="anthropic">anthropic</option>
                <option value="openai_compatible">openai_compatible</option>
                <option value="azure">azure</option>
                <option value="antigravity">antigravity (OAuth)</option>
                <option value="devin">devin (OAuth)</option>
              </Select>
            </Field>
            <Field label="Base URL" hint="Optional. Leave blank to use the provider's official endpoint.">
              <Input placeholder="https://api.example.com/v1" value={base} onChange={e=>setBase(e.target.value)} className="font-mono text-xs" />
            </Field>
            {type === 'antigravity' || type === 'devin' ? (
              <Field label="API Key" hint="Not needed — this provider connects with OAuth after creation.">
                <Input placeholder="OAuth — no key required" value={key} onChange={e=>setKey(e.target.value)} type="password" autoComplete="off" disabled />
              </Field>
            ) : (
              <Field label="API Key"><Input placeholder="sk-..." value={key} onChange={e=>setKey(e.target.value)} type="password" autoComplete="off" className="font-mono" /></Field>
            )}
          </div>
          {err && <div className="mt-3"><ErrorNote message={err} /></div>}
          <div className="mt-5 flex items-center gap-3">
            <Button variant="primary" onClick={create}><Icon name="plus" size={15} /> Add provider</Button>
            <span className="text-[11px] text-muted">Health settles within ~30s of discovery.</span>
          </div>
        </Card>
      )}

      {oauthDefs.length > 0 && (
        <Card>
          <div className="flex items-center gap-2.5 mb-1.5">
            <span className="w-9 h-9 rounded-xl bg-accent/10 border border-accent/25 text-accent flex items-center justify-center"><Icon name="key" size={16} /></span>
            <div>
              <h2 className="font-display font-semibold tracking-tight">OAuth connections</h2>
              <p className="text-xs text-muted">Sign in instead of pasting keys. One server registration adds future providers.</p>
            </div>
          </div>
          <div className="flex flex-wrap gap-2 mt-3">
            {oauthDefs.map((d: any) => (
              <Button key={d.id} variant="secondary" onClick={()=>startOAuth(d.id)} disabled={oauthBusy || !canWrite}>Connect {d.name || d.id}</Button>
            ))}
          </div>
          {oauthError && <div className="mt-3"><ErrorNote message={oauthError} /></div>}
        </Card>
      )}

      <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
        {loading && Array.from({ length: 6 }).map((_, i)=>(<Skeleton key={i} className="h-[190px]" />))}
        {!loading && list.map(p=> (
          <Card key={p.id} className="flex flex-col group hover:border-muted/60 transition-colors">
            <div className="flex items-start gap-3">
              <div className="w-11 h-11 rounded-xl bg-accent font-display font-bold text-[15px] flex items-center justify-center shrink-0 text-onaccent">
                {(p.name || '?').charAt(0).toUpperCase()}
              </div>
              <div className="min-w-0 flex-1">
                <div className="font-display font-semibold text-[15px] truncate" title={p.name}>{p.name}</div>
                <div className="mt-1.5 flex items-center gap-1.5 flex-wrap">
                  <Badge tone="neutral">{p.type}</Badge>
                  <span className="inline-flex items-center gap-1.5 text-[11px] font-medium"><HealthDot health={p.health_status} /><span className={healthTextCls(p.health_status)}>{p.health_status || 'checking'}</span></span>
                </div>
              </div>
            </div>
            <div className="mt-3.5 rounded-xl border border-stone/60 bg-app/50 px-3 py-2.5 space-y-1.5 text-xs min-h-[76px]">
              {(p.type === 'antigravity' || p.type === 'devin') && (
                <div>{p.oauth_connected ? <Badge tone="good">OAuth {p.oauth_email || 'connected'}</Badge> : <Badge tone="neutral">OAuth not connected</Badge>}</div>
              )}
              {(p.type === 'antigravity' || p.type === 'devin') && p.oauth_connected && (
                <OAuthUsage providerId={p.id} defId={p.oauth_def_id} type={p.type} name={p.name} />
              )}
              {p.base_url ? (
                <div className="flex items-center gap-1">
                  <span className="font-mono text-muted truncate flex-1" title={p.base_url}>{p.base_url}</span>
                  <CopyButton value={p.base_url} />
                </div>
              ) : <div className="text-muted/70 font-mono">official endpoint</div>}
              {p.last_health && <div className="font-mono text-muted/80 truncate" title={p.last_health}>{p.last_health}</div>}
              {p.created_at && !isNaN(new Date(p.created_at).getTime()) && (
                <div className="text-muted/60">since {new Date(p.created_at).toLocaleDateString()}</div>
              )}
            </div>
            {canWrite && (
              <div className="mt-3 pt-3 border-t border-stone/50 flex justify-end gap-1 flex-wrap">
                {(p.type === 'antigravity' || p.type === 'devin') && !p.oauth_connected && (
                  <Button variant="subtle" size="sm" onClick={()=>startOAuth(p.oauth_def_id || p.type, p.id)} disabled={oauthBusy}>Connect</Button>
                )}
                {(p.type === 'antigravity' || p.type === 'devin') && p.oauth_connected && (
                  <><Button variant="ghost" size="sm" onClick={()=>refreshOAuth(p.id)}>Refresh</Button>
                  <Button variant="ghost" size="sm" onClick={()=>disconnectOAuth(p.id)}>Disconnect</Button></>
                )}
                <Button variant="ghost" size="sm" title={`Edit ${p.name}`} onClick={()=>openEdit(p)}><Icon name="pencil" size={14} /> Edit</Button>
                <Button variant="ghost" size="sm" title={`Delete ${p.name}`} onClick={()=>setConfirmTarget(p)} className="hover:!text-danger"><Icon name="trash" size={14} /></Button>
              </div>
            )}
          </Card>
        ))}
        {!loading && !loadError && list.length===0 && (
          <div className="col-span-full"><Card><EmptyState icon="server" title="No providers yet."
            hint={canWrite ? 'Add your first upstream above. Models discover automatically, then routing can fan out.' : 'No upstream providers are connected yet — ask an admin to add one.'} /></Card></div>
        )}
      </div>

      {edit && (
        <Modal open onClose={()=>{ if(!editBusy) setEdit(null) }} title="Edit provider" width="max-w-md">
          <div className="space-y-4">
            {editError && <ErrorNote message={editError} />}
            <Field label="Name"><Input value={edit.name} onChange={e=>setEdit({...edit, name: e.target.value})} autoFocus spellCheck={false} /></Field>
            <Field label="Base URL" hint="Leave blank to use the provider's official endpoint.">
              <Input value={edit.base_url} onChange={e=>setEdit({...edit, base_url: e.target.value})} placeholder="https://api.example.com/v1" spellCheck={false} className="font-mono text-xs" />
            </Field>
            <Field label="New API key" hint="Leave blank to keep the existing key.">
              <Input value={edit.api_key} onChange={e=>setEdit({...edit, api_key: e.target.value})} type="password" placeholder="sk-..." autoComplete="new-password" className="font-mono" />
            </Field>
          </div>
          <div className="flex justify-end gap-2 mt-6">
            <Button variant="ghost" onClick={()=>setEdit(null)} disabled={editBusy}>Cancel</Button>
            <Button variant="primary" onClick={saveEdit} disabled={editBusy || !edit.name.trim()}>{editBusy ? 'Saving…' : 'Save changes'}</Button>
          </div>
        </Modal>
      )}

      <Confirm open={!!confirmTarget} onClose={()=>setConfirmTarget(null)} onConfirm={doDelete} busy={deleting}
        title="Delete provider"
        body={confirmTarget ? `Delete "${confirmTarget.name}"? Models discovered from it will be lost, and routed requests will fail.` : ''} confirmLabel="Delete" />

      {oauthSession && (
        <Modal open onClose={()=>{ if(!oauthBusy) setOauthSession(null) }} title={`Connect ${oauthSession.provider_name}`} width="max-w-md">
          <div className="space-y-4">
            {oauthError && <ErrorNote message={oauthError} />}
            <p className="text-sm text-muted leading-relaxed">Sign-in opened in a new tab. After approving, copy the localhost URL from the address bar and paste it below.</p>
            <Field label="Sign-in URL" hint="Opened automatically — reopen if your popup blocker stopped it.">
              <div className="flex items-center gap-2 rounded-xl border border-stone/60 bg-app/60 px-3 py-2">
                <span className="font-mono text-xs truncate flex-1">{oauthSession.auth_url}</span>
                <CopyButton value={oauthSession.auth_url} />
              </div>
            </Field>
            <Field label="Pasted callback URL" hint={`${oauthSession.redirect_uri}?state=…&code=…`}>
              <Input value={pasteUrl} onChange={e=>setPasteUrl(e.target.value)} placeholder="Paste the localhost URL here" spellCheck={false} autoFocus className="font-mono text-xs" />
            </Field>
          </div>
          <div className="flex justify-end gap-2 mt-6">
            <Button variant="ghost" onClick={()=>setOauthSession(null)} disabled={oauthBusy}>Cancel</Button>
            <Button variant="primary" onClick={submitPaste} disabled={oauthBusy || !pasteUrl.trim()}>{oauthBusy ? 'Connecting…' : 'Connect'}</Button>
          </div>
        </Modal>
      )}
    </div>
  )
}
