import { useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import {
  Badge, Button, Card, Confirm, CopyButton, EmptyState, ErrorNote, Field,
  HealthDot, Icon, Input, Modal, PageHeader, Select, Skeleton, useToast,
} from '../components/ui'

/** Roles allowed to mutate providers (mirrors backend RequireRole). */
const PROVIDER_WRITER_ROLES = ['admin', 'support', 'member']

function healthTextCls(status?: string | null): string {
  if (status === 'up') return 'text-teal'
  if (status === 'down') return 'text-red-400'
  return 'text-muted'
}

type EditState = { id: string; name: string; base_url: string; api_key: string }

export default function Providers({ role = 'admin' }: { role?: string }){
  const canWrite = PROVIDER_WRITER_ROLES.includes(role)
  const [list, setList] = useState<any[]>([])
  const [name, setName] = useState('')
  const [type, setType] = useState('openai')
  const [base, setBase] = useState('')
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')

  // Presentation-only: skeleton grid, delete confirmation, edit modal.
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [confirmTarget, setConfirmTarget] = useState<any>(null)
  const [deleting, setDeleting] = useState(false)
  const [edit, setEdit] = useState<EditState | null>(null)
  const [editBusy, setEditBusy] = useState(false)
  const [editError, setEditError] = useState('')
  const toast = useToast()

  // OAuth connections (generic registry; Antigravity ships built-in).
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
    } catch (e: any) {
      setOauthError(e?.message || String(e))
      toast.error(e?.message || String(e))
    } finally { setOauthBusy(false) }
  }

  const submitPaste = async () => {
    if (!oauthSession) return
    setOauthError(''); setOauthBusy(true)
    try {
      await api.oauth.exchange({ state: oauthSession.state, provider_id: oauthSession.provider_id, callback_url: pasteUrl })
      toast.success(`Connected ${oauthSession.provider_name}`)
      setOauthSession(null); setPasteUrl('')
      load()
    } catch (e: any) {
      setOauthError(e?.message || String(e))
    } finally { setOauthBusy(false) }
  }

  const refreshOAuth = async (id: string) => {
    try { await api.oauth.refresh(id); toast.success('Token refreshed'); load() }
    catch (e: any) { toast.error(e?.message || String(e)) }
  }

  const disconnectOAuth = async (id: string) => {
    try { await api.oauth.disconnect(id); toast.success('OAuth disconnected'); load() }
    catch (e: any) { toast.error(e?.message || String(e)) }
  }

  /**
   * After adding a provider the backend auto-discovers its models in a
   * background goroutine — failures would be invisible. Poll the provider
   * list for ~30s so the health/status line settles without a manual refresh.
   */
  const pollAfterCreate = (providerId: string) => {
    const started = Date.now()
    const tick = async () => {
      if (Date.now() - started > 30_000) return
      try {
        const data: any = await api.providers.list()
        const rows = Array.isArray(data) ? data : []
        setList(rows)
        const p = rows.find((x: any) => x.id === providerId)
        // Stop early once a health verdict (post-discovery) has landed.
        if (p && (p.health_status === 'up' || p.health_status === 'down')) return
      } catch { /* transient — keep polling */ }
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
      toast.info('Discovering models in background — check the Providers page in a minute')
      await load()
      if (created?.id) pollAfterCreate(created.id)
    } catch(e:any){
      setErr(e.message)
      toast.error(e.message || String(e))
    }
  }

  const openEdit = (p: any) => {
    setEdit({ id: p.id, name: p.name || '', base_url: p.base_url || '', api_key: '' })
    setEditError('')
  }

  const saveEdit = async ()=>{
    if (!edit) return
    const trimmed = edit.name.trim()
    if (!trimmed) { setEditError('Name is required.'); return }
    setEditBusy(true); setEditError('')
    try {
      // api_key is optional — rotate only when a new key was typed.
      const payload: { name: string; base_url?: string; api_key?: string } = { name: trimmed }
      if (edit.base_url.trim()) payload.base_url = edit.base_url.trim()
      if (edit.api_key.trim()) payload.api_key = edit.api_key.trim()
      await api.providers.update(edit.id, payload)
      toast.success('Provider updated')
      setEdit(null)
      load()
    } catch (e:any) {
      setEditError(e?.message || String(e))
    } finally { setEditBusy(false) }
  }

  const doDelete = async ()=>{
    const p = confirmTarget
    if(!p) return
    setDeleting(true)
    try{
      await api.providers.remove(p.id)
      toast.success('Provider removed')
      setConfirmTarget(null)
      load()
    } catch(e:any){ toast.error(e.message || String(e)) }
    finally{ setDeleting(false) }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Providers"
        description="Upstream AI providers. Connect an endpoint, watch its health, and remove stale entries."
        actions={<Badge tone="good" dot>{list.length} connected</Badge>}
      />

      {loadError && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load providers"
            hint={loadError}
            action={<Button variant="secondary" onClick={load}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {/* Create form */}
      {canWrite && (
        <Card>
          <div className="flex items-center gap-2 mb-4">
            <Icon name="server" size={16} className="text-teal" />
            <h2 className="font-semibold tracking-tight">Add provider</h2>
          </div>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <Field label="Name">
              <Input placeholder="my-openai" value={name} onChange={e=>setName(e.target.value)} />
            </Field>
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
              <Input placeholder="https://api.example.com/v1" value={base} onChange={e=>setBase(e.target.value)} />
            </Field>
            {type === 'antigravity' || type === 'devin' ? (
              <Field label="API Key" hint="Not needed — this provider connects with OAuth after creation.">
                <Input placeholder="OAuth — no key required" value={key} onChange={e=>setKey(e.target.value)} type="password" autoComplete="off" disabled />
              </Field>
            ) : (
              <Field label="API Key">
                <Input placeholder="sk-..." value={key} onChange={e=>setKey(e.target.value)} type="password" autoComplete="off" />
              </Field>
            )}
          </div>
          {err && <div className="mt-3"><ErrorNote message={err} /></div>}
          <div className="mt-4">
            <Button variant="primary" onClick={create}>
              <Icon name="plus" size={15} /> Add Provider
            </Button>
          </div>
        </Card>
      )}

      {/* OAuth connections */}
      {oauthDefs.length > 0 && (
        <Card>
          <div className="flex items-center gap-2 mb-2">
            <Icon name="key" size={16} className="text-teal" />
            <h2 className="font-semibold tracking-tight">OAuth connections</h2>
          </div>
          <p className="text-sm text-muted mb-4">
            Sign in with your account instead of pasting keys. Adding another OAuth provider later is one server registration — no UI changes.
          </p>
          <div className="flex flex-wrap gap-2">
            {oauthDefs.map((d: any) => (
              <Button key={d.id} variant="secondary" onClick={()=>startOAuth(d.id)} disabled={oauthBusy || !canWrite}>
                Connect {d.name || d.id}
              </Button>
            ))}
          </div>
          {oauthError && <div className="mt-3"><ErrorNote message={oauthError} /></div>}
        </Card>
      )}

      {/* Provider cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3 md:gap-4">
        {loading && Array.from({ length: 6 }).map((_, i)=>(
          <Skeleton key={i} className="h-[168px]" />
        ))}
        {!loading && list.map(p=> (
          <Card key={p.id} className="flex flex-col">
            <div className="flex items-start gap-3">
              <div className="w-10 h-10 rounded-lg bg-teal/10 border border-teal/25 text-teal font-semibold text-sm uppercase flex items-center justify-center shrink-0">
                {(p.name || '?').charAt(0)}
              </div>
              <div className="min-w-0 flex-1">
                <div className="font-mono text-sm font-medium truncate" title={p.name}>{p.name}</div>
                <div className="mt-1"><Badge tone="neutral">{p.type}</Badge></div>
              </div>
            </div>

            <div className="mt-3 space-y-1.5 text-xs">
              <div className="flex items-center gap-1.5">
                <HealthDot health={p.health_status} />
                <span className={healthTextCls(p.health_status)}>{p.health_status || 'checking'}</span>
              </div>
              {(p.type === 'antigravity' || p.type === 'devin') && (
                <div className="flex items-center gap-1.5">
                  {p.oauth_connected
                    ? <Badge tone="good">OAuth {p.oauth_email || 'connected'}</Badge>
                    : <Badge tone="neutral">OAuth not connected</Badge>}
                </div>
              )}
              {p.created_at && !isNaN(new Date(p.created_at).getTime()) && (
                <div className="text-muted">
                  created {new Date(p.created_at).toLocaleDateString()}
                </div>
              )}
              {p.last_health && <div className="font-mono text-muted truncate" title={p.last_health}>{p.last_health}</div>}
              {p.base_url && (
                <div className="flex items-center gap-1 -ml-1">
                  <span className="font-mono text-muted truncate flex-1" title={p.base_url}>{p.base_url}</span>
                  <CopyButton value={p.base_url} />
                </div>
              )}
            </div>

            {canWrite && (
              <div className="mt-auto pt-3 flex justify-end gap-1 flex-wrap">
                {(p.type === 'antigravity' || p.type === 'devin') && !p.oauth_connected && (
                  <Button variant="ghost" size="sm" onClick={()=>startOAuth(p.oauth_def_id || p.type, p.id)} disabled={oauthBusy}>
                    Connect
                  </Button>
                )}
                {(p.type === 'antigravity' || p.type === 'devin') && p.oauth_connected && (
                  <>
                    <Button variant="ghost" size="sm" onClick={()=>refreshOAuth(p.id)}>Refresh</Button>
                    <Button variant="ghost" size="sm" onClick={()=>disconnectOAuth(p.id)}>Disconnect</Button>
                  </>
                )}
                <Button variant="ghost" size="sm"
                  title={`Edit ${p.name}`}
                  onClick={()=>openEdit(p)}>
                  <Icon name="pencil" size={14} /> Edit
                </Button>
                <Button variant="ghost" size="sm"
                  title={`Delete ${p.name}`}
                  onClick={()=>setConfirmTarget(p)}>
                  <Icon name="trash" size={14} /> Remove
                </Button>
              </div>
            )}
          </Card>
        ))}
        {!loading && !loadError && list.length===0 && (
          <div className="col-span-full">
            <Card><EmptyState
              icon="server"
              title="No providers yet."
              hint={canWrite
                ? 'Add your first upstream provider above. Models are discovered from it on the Models page.'
                : 'No upstream providers are connected yet — ask an admin to add one.'}
            /></Card>
          </div>
        )}
      </div>

      {/* Edit provider modal */}
      {edit && (
        <Modal open onClose={()=>{ if(!editBusy) setEdit(null) }} title="Edit provider" width="max-w-md">
          <div className="space-y-4">
            {editError && <ErrorNote message={editError} />}
            <Field label="Name">
              <Input value={edit.name} onChange={e=>setEdit({...edit, name: e.target.value})} autoFocus spellCheck={false} />
            </Field>
            <Field label="Base URL" hint="Leave blank to use the provider's official endpoint.">
              <Input value={edit.base_url} onChange={e=>setEdit({...edit, base_url: e.target.value})} placeholder="https://api.example.com/v1" spellCheck={false} />
            </Field>
            <Field label="New API key" hint="Leave blank to keep the existing key.">
              <Input value={edit.api_key} onChange={e=>setEdit({...edit, api_key: e.target.value})} type="password" placeholder="sk-..." autoComplete="new-password" />
            </Field>
          </div>
          <div className="flex justify-end gap-2 mt-6">
            <Button variant="ghost" onClick={()=>setEdit(null)} disabled={editBusy}>Cancel</Button>
            <Button variant="primary" onClick={saveEdit} disabled={editBusy || !edit.name.trim()}>
              {editBusy ? 'Saving…' : 'Save changes'}
            </Button>
          </div>
        </Modal>
      )}

      <Confirm
        open={!!confirmTarget}
        onClose={()=>setConfirmTarget(null)}
        onConfirm={doDelete}
        busy={deleting}
        title="Delete provider"
        body={
          confirmTarget
            ? `Delete "${confirmTarget.name}"? Models discovered from this provider will be lost, and requests routed through it will fail.`
            : ''
        }
        confirmLabel="Delete"
      />

      {/* OAuth paste modal (headless/loopback callback) */}
      {oauthSession && (
        <Modal open onClose={()=>{ if(!oauthBusy) setOauthSession(null) }} title={`Connect ${oauthSession.provider_name}`} width="max-w-md">
          <div className="space-y-4">
            {oauthError && <ErrorNote message={oauthError} />}
            <p className="text-sm text-muted">
              Sign-in opened in a new tab. After approving, your browser lands on a localhost URL that cannot load —
              copy that full URL from the address bar and paste it below.
            </p>
            <Field label="Sign-in URL" hint="Opened automatically — reopen if your popup blocker stopped it.">
              <div className="flex items-center gap-2">
                <span className="font-mono text-xs truncate flex-1">{oauthSession.auth_url}</span>
                <CopyButton value={oauthSession.auth_url} />
              </div>
            </Field>
            <Field label="Pasted callback URL" hint={`${oauthSession.redirect_uri}?state=…&code=…`}>
              <Input value={pasteUrl} onChange={e=>setPasteUrl(e.target.value)} placeholder="Paste the localhost URL here" spellCheck={false} autoFocus />
            </Field>
          </div>
          <div className="flex justify-end gap-2 mt-6">
            <Button variant="ghost" onClick={()=>setOauthSession(null)} disabled={oauthBusy}>Cancel</Button>
            <Button variant="primary" onClick={submitPaste} disabled={oauthBusy || !pasteUrl.trim()}>
              {oauthBusy ? 'Connecting…' : 'Connect'}
            </Button>
          </div>
        </Modal>
      )}
    </div>
  )
}
