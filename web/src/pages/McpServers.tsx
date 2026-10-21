import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import { can } from '../lib/permissions'
import {
  Badge, Button, Card, Confirm, CopyButton, EmptyState, ErrorNote, Eyebrow, Field,
  HealthDot, Icon, Input, Modal, PageHeader, Select, Skeleton, Stat, TableShell,
  Td, Textarea, Th, useToast,
} from '../components/ui'

/* ------------------------------------------------------------------ */
/* Types                                                               */
/* ------------------------------------------------------------------ */

type Transport = 'http' | 'sse' | 'stdio'
type AuthType = 'none' | 'bearer' | 'api_key' | 'header'

type McpServer = {
  id: string
  name: string
  slug: string
  transport: Transport
  url?: string
  command?: string
  args?: string[]
  auth_type: AuthType
  auth_header?: string
  enabled: boolean
  health_status: string
  last_error?: string
  last_health_at?: string
  tool_count: number
  created_at: string
  updated_at: string
  has_auth: boolean
  has_env: boolean
  has_headers: boolean
}

type McpTool = {
  server_id: string
  server_slug: string
  server_name: string
  name: string
  namespaced_name: string
  description: string
  input_schema?: unknown
  discovered_at: string
}

type McpCall = {
  ID?: string
  id: string
  server_id: string
  server_slug: string
  tool_name: string
  namespaced_name: string
  key_prefix: string
  endpoint: string
  status: string
  error?: string
  duration_ms: number
  created_at: string
}

type FormState = {
  name: string
  slug: string
  transport: Transport
  url: string
  command: string
  argsText: string
  envText: string
  headersText: string
  authType: AuthType
  authHeader: string
  authValue: string
  enabled: boolean
}

const EMPTY_FORM: FormState = {
  name: '', slug: '', transport: 'http', url: '', command: '', argsText: '',
  envText: '', headersText: '', authType: 'none', authHeader: '', authValue: '', enabled: true,
}

const TRANSPORT_LABEL: Record<Transport, string> = {
  http: 'Streamable HTTP',
  sse: 'SSE (legacy)',
  stdio: 'stdio (local process)',
}

function when(iso?: string | null): string {
  if (!iso) return '—'
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

function healthTone(status: string): 'good' | 'bad' | 'neutral' {
  if (status === 'up') return 'good'
  if (status === 'down') return 'bad'
  return 'neutral'
}

/** Parse a "KEY=value" per line blob into an object. */
function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const eq = trimmed.indexOf('=')
    if (eq <= 0) continue
    out[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim()
  }
  return out
}

function kvToText(obj?: Record<string, string>): string {
  if (!obj) return ''
  return Object.entries(obj).map(([k, v]) => `${k}=${v}`).join('\n')
}

/* ------------------------------------------------------------------ */
/* Server form                                                         */
/* ------------------------------------------------------------------ */

function ServerForm({ initial, busy, onSubmit, onCancel }: {
  initial: FormState
  busy: boolean
  onSubmit: (form: FormState) => void
  onCancel: () => void
}) {
  const [form, setForm] = useState<FormState>(initial)
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setForm((f) => ({ ...f, [k]: v }))
  const isStdio = form.transport === 'stdio'
  const needsAuthValue = form.authType !== 'none'

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <Field label="Display name">
          <Input value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="GitHub" autoFocus />
        </Field>
        <Field label="Tool namespace" hint={form.slug ? `Tools appear as ${form.slug}-<tool>` : 'Derived from the name when left blank.'}>
          <Input value={form.slug} onChange={(e) => set('slug', e.target.value)} placeholder="github" />
        </Field>
      </div>

      <Field label="Transport">
        <Select value={form.transport} onChange={(e) => set('transport', e.target.value as Transport)}>
          {(Object.keys(TRANSPORT_LABEL) as Transport[]).map((t) => (
            <option key={t} value={t}>{TRANSPORT_LABEL[t]}</option>
          ))}
        </Select>
      </Field>

      {!isStdio && (
        <Field label="Endpoint URL" hint="The MCP server's own URL, e.g. https://mcp.example.com/mcp">
          <Input value={form.url} onChange={(e) => set('url', e.target.value)} placeholder="https://mcp.example.com/mcp" />
        </Field>
      )}

      {isStdio && (
        <>
          <Field label="Command" hint="Launched by the gateway process; the binary must exist on the gateway host.">
            <Input value={form.command} onChange={(e) => set('command', e.target.value)} placeholder="npx" />
          </Field>
          <Field label="Arguments" hint="One argument per line.">
            <Textarea value={form.argsText} onChange={(e) => set('argsText', e.target.value)}
              placeholder={'-y\n@modelcontextprotocol/server-everything'} />
          </Field>
          <Field label="Environment" hint="KEY=value per line. Encrypted at rest; never returned by the API.">
            <Textarea value={form.envText} onChange={(e) => set('envText', e.target.value)} placeholder="GITHUB_TOKEN=ghp_…" />
          </Field>
        </>
      )}

      <Field label="Upstream authentication">
        <Select value={form.authType} onChange={(e) => set('authType', e.target.value as AuthType)}>
          <option value="none">None</option>
          <option value="bearer">Bearer token (Authorization: Bearer …)</option>
          <option value="api_key">API key header (X-API-Key)</option>
          <option value="header">Custom header</option>
        </Select>
      </Field>

      {form.authType === 'header' && (
        <Field label="Header name">
          <Input value={form.authHeader} onChange={(e) => set('authHeader', e.target.value)} placeholder="X-Custom-Auth" />
        </Field>
      )}
      {form.authType === 'api_key' && (
        <Field label="Header name (optional)" hint="Defaults to X-API-Key.">
          <Input value={form.authHeader} onChange={(e) => set('authHeader', e.target.value)} placeholder="X-API-Key" />
        </Field>
      )}
      {needsAuthValue && (
        <Field label="Credential" hint={initial.authValue ? 'A credential is already stored. Leave blank to keep it.' : 'AES-256-GCM encrypted with the gateway master key.'}>
          <Input type="password" value={form.authValue} onChange={(e) => set('authValue', e.target.value)} placeholder="••••••••" />
        </Field>
      )}

      {!isStdio && (
        <Field label="Static headers" hint="KEY=value per line, sent on every upstream request. Encrypted at rest.">
          <Textarea value={form.headersText} onChange={(e) => set('headersText', e.target.value)} placeholder="X-Org-Id=acme" />
        </Field>
      )}

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={form.enabled} onChange={(e) => set('enabled', e.target.checked)}
          className="rounded border-stone accent-accent" />
        Enabled (available over /mcp and to LLM mcp references)
      </label>

      <div className="flex justify-end gap-2 pt-1">
        <Button variant="ghost" onClick={onCancel} disabled={busy}>Cancel</Button>
        <Button variant="primary" onClick={() => onSubmit(form)} disabled={busy || !form.name.trim()}>
          {busy ? 'Saving…' : 'Save server'}
        </Button>
      </div>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Key permission editor                                               */
/* ------------------------------------------------------------------ */

function KeyPermissionsModal({ keyId, keyName, onClose }: { keyId: string; keyName: string; onClose: () => void }) {
  const toast = useToast()
  const [data, setData] = useState<any>(null)
  const [tools, setTools] = useState<McpTool[]>([])
  const [state, setState] = useState<Record<string, { granted: boolean; tools: string[] }>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let live = true
    Promise.all([api.mcp.keyPermissions(keyId), api.mcp.allTools().catch(() => [])])
      .then(([perm, allTools]) => {
        if (!live) return
        setData(perm)
        setTools(Array.isArray(allTools) ? allTools : [])
        const next: Record<string, { granted: boolean; tools: string[] }> = {}
        for (const s of perm?.servers ?? []) {
          next[s.server_id] = { granted: !!s.granted, tools: s.allowed_tools ?? [] }
        }
        setState(next)
      })
      .catch((e) => live && setError(e?.message || 'failed to load permissions'))
    return () => { live = false }
  }, [keyId])

  const save = async () => {
    setBusy(true)
    setError('')
    try {
      await api.mcp.setKeyPermissions(keyId, Object.entries(state).map(([server_id, v]) => ({
        server_id, granted: v.granted, allowed_tools: v.tools,
      })))
      toast.success('MCP permissions saved')
      onClose()
    } catch (e: any) {
      setError(e?.message || 'save failed')
    } finally {
      setBusy(false)
    }
  }

  const servers: any[] = data?.servers ?? []

  return (
    <Modal open onClose={onClose} title={`MCP access — ${keyName}`} width="max-w-2xl">
      <div className="space-y-4">
        <p className="text-xs text-muted leading-relaxed">
          Grant this key access to specific MCP servers. Leaving every server unticked keeps the
          default (all enabled servers and tools); ticking at least one switches the key to an
          explicit allowlist. An empty tool list allows every tool on that server.
        </p>
        {error && <ErrorNote message={error} />}
        {!data ? <Skeleton className="h-40" /> : (
          <div className="space-y-2">
            {servers.length === 0 && <div className="text-sm text-muted">No MCP servers registered yet.</div>}
            {servers.map((s) => {
              const cur = state[s.server_id] ?? { granted: false, tools: [] }
              const serverTools = tools.filter((t) => t.server_id === s.server_id)
              return (
                <div key={s.server_id} className="rounded-lg border border-stone bg-surface p-3">
                  <label className="flex items-center gap-2 text-sm font-medium">
                    <input type="checkbox" className="rounded border-stone accent-accent"
                      checked={cur.granted}
                      onChange={(e) => setState((prev) => ({
                        ...prev,
                        [s.server_id]: { granted: e.target.checked, tools: e.target.checked ? cur.tools : [] },
                      }))} />
                    {s.server_name}
                    <span className="text-[11px] font-mono text-muted">{s.server_slug}</span>
                    <Badge tone="neutral">{s.tool_count} tools</Badge>
                  </label>
                  {cur.granted && serverTools.length > 0 && (
                    <div className="mt-2 pl-6">
                      <div className="text-[11px] text-muted mb-1">
                        {cur.tools.length === 0 ? 'All tools on this server are allowed.' : `${cur.tools.length} selected`}
                      </div>
                      <div className="flex flex-wrap gap-1.5">
                        {serverTools.map((t) => {
                          const on = cur.tools.includes(t.name)
                          return (
                            <button key={t.name} type="button"
                              title={t.description || t.name}
                              onClick={() => setState((prev) => {
                                const entry = prev[s.server_id] ?? { granted: true, tools: [] }
                                const has = entry.tools.includes(t.name)
                                return {
                                  ...prev,
                                  [s.server_id]: { granted: true, tools: has ? entry.tools.filter((n) => n !== t.name) : [...entry.tools, t.name] },
                                }
                              })}
                              className={`text-[11px] font-mono px-2 py-1 rounded border transition-colors ${
                                on ? 'bg-accent/10 border-accent/40 text-accent' : 'border-stone text-muted hover:text-paper hover:bg-raised'}`}>
                              {t.name}
                            </button>
                          )
                        })}
                      </div>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" onClick={save} disabled={busy || !data}>{busy ? 'Saving…' : 'Save access'}</Button>
        </div>
      </div>
    </Modal>
  )
}

/* ------------------------------------------------------------------ */
/* Page                                                                */
/* ------------------------------------------------------------------ */

export default function McpServers() {
  const toast = useToast()
  const canWrite = can('mcp:write')
  const canEditKeys = can('keys:limits')

  const [servers, setServers] = useState<McpServer[]>([])
  const [status, setStatus] = useState<any>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [createOpen, setCreateOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<McpServer | null>(null)
  const [editForm, setEditForm] = useState<FormState>(EMPTY_FORM)
  const [saving, setSaving] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<McpServer | null>(null)
  const [deleting, setDeleting] = useState(false)

  const [toolsFor, setToolsFor] = useState<McpServer | null>(null)
  const [tools, setTools] = useState<McpTool[]>([])
  const [toolsBusy, setToolsBusy] = useState(false)
  const [toolsError, setToolsError] = useState('')

  const [testBusy, setTestBusy] = useState<string | null>(null)
  const [testResult, setTestResult] = useState<{ id: string; ok: boolean; message: string } | null>(null)

  const [calls, setCalls] = useState<McpCall[]>([])
  const [keys, setKeys] = useState<any[]>([])
  const [permKey, setPermKey] = useState<{ id: string; name: string } | null>(null)
  const [origin, setOrigin] = useState('')

  const load = async () => {
    setError('')
    try {
      const [list, st] = await Promise.all([api.mcp.list(), api.mcp.status().catch(() => null)])
      setServers(Array.isArray(list) ? list : [])
      setStatus(st)
    } catch (e: any) {
      setError(e?.message || 'failed to load MCP servers')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    setOrigin(window.location.origin)
    load()
    api.mcp.calls().then((c) => setCalls(Array.isArray(c) ? c : [])).catch(() => {})
    api.keys.list().then((k) => setKeys(Array.isArray(k) ? k : [])).catch(() => {})
  }, [])

  // Health/status refresh: the gateway probes MCP servers in the background,
  // so the dashboard polls while this page is open.
  useEffect(() => {
    const t = setInterval(() => {
      api.mcp.list().then((l) => Array.isArray(l) && setServers(l)).catch(() => {})
    }, 15000)
    return () => clearInterval(t)
  }, [])

  const endpoint = useMemo(() => `${origin || ''}/mcp`, [origin])

  const formToPayload = (f: FormState) => {
    const payload: Record<string, unknown> = {
      name: f.name.trim(),
      slug: f.slug.trim(),
      transport: f.transport,
      url: f.url.trim(),
      command: f.command.trim(),
      args: f.argsText.split('\n').map((l) => l.trim()).filter(Boolean),
      env: parseKV(f.envText),
      headers: parseKV(f.headersText),
      auth_type: f.authType,
      auth_header: f.authHeader.trim(),
      enabled: f.enabled,
    }
    if (f.authValue) payload.auth_value = f.authValue
    return payload
  }

  const create = async (form: FormState) => {
    setSaving(true)
    try {
      await api.mcp.create(formToPayload(form))
      toast.success('MCP server added — discovering tools…')
      setCreateOpen(false)
      await load()
      setTimeout(() => { load(); api.mcp.calls().then((c) => setCalls(Array.isArray(c) ? c : [])).catch(() => {}) }, 2500)
    } catch (e: any) {
      toast.error(e?.message || 'create failed')
    } finally {
      setSaving(false)
    }
  }

  const openEdit = (srv: McpServer) => {
    setEditTarget(srv)
    setEditForm({
      name: srv.name,
      slug: srv.slug,
      transport: srv.transport,
      url: srv.url || '',
      command: srv.command || '',
      argsText: (srv.args || []).join('\n'),
      envText: '',
      headersText: '',
      authType: srv.auth_type || 'none',
      authHeader: srv.auth_header || '',
      authValue: '',
      enabled: srv.enabled,
    })
  }

  const saveEdit = async (form: FormState) => {
    if (!editTarget) return
    setSaving(true)
    try {
      await api.mcp.update(editTarget.id, formToPayload(form))
      toast.success('MCP server updated — reconnecting…')
      setEditTarget(null)
      await load()
      setTimeout(load, 2500)
    } catch (e: any) {
      toast.error(e?.message || 'update failed')
    } finally {
      setSaving(false)
    }
  }

  const toggle = async (srv: McpServer) => {
    try {
      if (srv.enabled) await api.mcp.disable(srv.id)
      else await api.mcp.enable(srv.id)
      await load()
    } catch (e: any) {
      toast.error(e?.message || 'toggle failed')
    }
  }

  const remove = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await api.mcp.remove(deleteTarget.id)
      toast.success('MCP server removed')
      setDeleteTarget(null)
      await load()
    } catch (e: any) {
      toast.error(e?.message || 'delete failed')
    } finally {
      setDeleting(false)
    }
  }

  const test = async (srv: McpServer) => {
    setTestBusy(srv.id)
    setTestResult(null)
    try {
      const res: any = await api.mcp.test(srv.id)
      setTestResult({ id: srv.id, ok: !!res?.ok, message: res?.ok ? `${res.tool_count} tools · ${res.latency_ms}ms` : (res?.error || 'failed') })
      await load()
    } catch (e: any) {
      setTestResult({ id: srv.id, ok: false, message: e?.message || 'test failed' })
      await load()
    } finally {
      setTestBusy(null)
    }
  }

  const openTools = async (srv: McpServer) => {
    setToolsFor(srv)
    setToolsBusy(true)
    setToolsError('')
    setTools([])
    try {
      const list = await api.mcp.tools(srv.id)
      setTools(Array.isArray(list) ? list : [])
    } catch (e: any) {
      setToolsError(e?.message || 'failed to load tools')
    } finally {
      setToolsBusy(false)
    }
  }

  const refreshCalls = () => {
    api.mcp.calls().then((c) => setCalls(Array.isArray(c) ? c : [])).catch(() => {})
  }

  const snippet = `{
  "mcpServers": {
    "ai-gateway": {
      "url": "${endpoint}",
      "headers": { "x-litellm-api-key": "Bearer sk-gw-…" }
    }
  }
}`

  return (
    <div className="p-6 max-w-7xl mx-auto animate-fade">
      <PageHeader
        title="MCP Servers"
        eyebrow="Connect · Model Context Protocol"
        description="Register MCP servers once. Every harness connects to a single endpoint and inherits the tools your key is allowed to use."
        actions={
          <>
            <Button onClick={load} title="Refresh"><Icon name="refresh" size={14} />Refresh</Button>
            {canWrite && (
              <Button variant="primary" onClick={() => setCreateOpen(true)}>
                <Icon name="plus" size={14} />Add MCP server
              </Button>
            )}
          </>
        }
      />

      {error && <div className="mb-4"><ErrorNote message={error} /></div>}

      <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-5">
        <Stat icon="server" title="Servers" value={String(status?.servers ?? servers.length)} sub={`${status?.disabled ?? 0} disabled`} />
        <Stat icon="check" title="Healthy" value={String(status?.up ?? 0)} sub={`${status?.down ?? 0} down`} tone={(status?.down ?? 0) > 0 ? 'warn' : 'good'} />
        <Stat icon="box" title="Tools" value={String(status?.tools ?? servers.reduce((n, s) => n + s.tool_count, 0))} sub="namespaced per server" />
        <Stat icon="zap" title="Tool calls" value={String(calls.length)} sub="recent audit rows" />
      </div>

      <Card className="mb-5">
        <div className="flex flex-wrap items-center gap-3">
          <Eyebrow>Single endpoint</Eyebrow>
          <code className="text-xs font-mono text-paper bg-raised px-2 py-1 rounded border border-stone">{endpoint || '/mcp'}</code>
          {origin && <CopyButton value={endpoint} label="Copy endpoint" />}
          <span className="text-xs text-muted">
            Per-server form: <code className="font-mono">{origin || ''}/&lt;server&gt;/mcp</code>
          </span>
        </div>
        <p className="text-xs text-muted mt-2 leading-relaxed">
          Authenticate with a gateway key. MCP clients should send it as
          <code className="font-mono mx-1">x-litellm-api-key: Bearer sk-gw-…</code>
          (Authorization stays free for upstream OAuth), though <code className="font-mono mx-1">Authorization</code>
          and <code className="font-mono mx-1">x-api-key</code> are accepted too.
        </p>
        <pre className="mt-3 text-[11px] leading-relaxed font-mono bg-raised/60 border border-stone rounded-lg p-3 overflow-x-auto">{snippet}</pre>
      </Card>

      {loading ? (
        <div className="space-y-2"><Skeleton className="h-16" /><Skeleton className="h-16" /></div>
      ) : servers.length === 0 ? (
        <Card>
          <EmptyState icon="server" title="No MCP servers yet"
            hint={canWrite
              ? 'Add your first server — Streamable HTTP and SSE endpoints or a locally launched stdio process.'
              : 'An administrator has not registered any MCP servers.'}
            action={canWrite ? <Button variant="primary" onClick={() => setCreateOpen(true)}><Icon name="plus" size={14} />Add MCP server</Button> : undefined} />
        </Card>
      ) : (
        <TableShell minWidth={960}>
          <thead>
            <tr>
              <Th>Server</Th>
              <Th>Transport</Th>
              <Th>Health</Th>
              <Th>Tools</Th>
              <Th>Auth</Th>
              <Th>State</Th>
              <Th className="text-right">Actions</Th>
            </tr>
          </thead>
          <tbody>
            {servers.map((srv) => (
              <tr key={srv.id} className="hover:bg-raised/40 transition-colors">
                <Td>
                  <div className="flex items-center gap-2 min-w-0">
                    <HealthDot health={srv.enabled ? srv.health_status : 'unknown'} />
                    <div className="min-w-0">
                      <div className="font-medium text-paper truncate">{srv.name}</div>
                      <div className="text-[11px] font-mono text-muted truncate" title={srv.url || srv.command}>
                        {srv.slug} · {srv.url || srv.command}
                      </div>
                    </div>
                  </div>
                </Td>
                <Td><Badge tone="neutral">{TRANSPORT_LABEL[srv.transport] || srv.transport}</Badge></Td>
                <Td>
                  <div className="space-y-0.5">
                    <Badge tone={srv.enabled ? healthTone(srv.health_status) : 'neutral'} dot>
                      {srv.enabled ? srv.health_status : 'disabled'}
                    </Badge>
                    {srv.last_error && (
                      <div className="text-[11px] text-danger/90 max-w-[220px] truncate" title={srv.last_error}>{srv.last_error}</div>
                    )}
                    <div className="text-[11px] text-muted">probed {when(srv.last_health_at)}</div>
                  </div>
                </Td>
                <Td>
                  <button className="text-sm font-medium text-accent hover:underline" onClick={() => openTools(srv)}>
                    {srv.tool_count} tools
                  </button>
                </Td>
                <Td>
                  <span className="text-xs text-muted">
                    {srv.auth_type === 'none' ? 'none' : srv.auth_type}
                    {srv.has_headers ? ' + headers' : ''}
                    {srv.has_env ? ' + env' : ''}
                  </span>
                </Td>
                <Td>
                  <Badge tone={srv.enabled ? 'good' : 'neutral'}>{srv.enabled ? 'enabled' : 'disabled'}</Badge>
                </Td>
                <Td>
                  <div className="flex items-center justify-end gap-1">
                    <Button size="sm" variant="ghost" title="Test connection"
                      onClick={() => test(srv)} disabled={testBusy === srv.id}>
                      <Icon name="pulse" size={13} />{testBusy === srv.id ? 'Testing…' : 'Test'}
                    </Button>
                    {canWrite && (
                      <>
                        <Button size="sm" variant="ghost" title={srv.enabled ? 'Disable' : 'Enable'} onClick={() => toggle(srv)}>
                          <Icon name={srv.enabled ? 'lock' : 'check'} size={13} />
                        </Button>
                        <Button size="sm" variant="ghost" title="Edit" onClick={() => openEdit(srv)}>
                          <Icon name="pencil" size={13} />
                        </Button>
                        <Button size="sm" variant="ghost" title="Delete" onClick={() => setDeleteTarget(srv)}>
                          <Icon name="trash" size={13} className="text-danger" />
                        </Button>
                      </>
                    )}
                  </div>
                  {testResult?.id === srv.id && (
                    <div className={`text-[11px] mt-1 text-right ${testResult.ok ? 'text-good' : 'text-danger'}`}>
                      {testResult.message}
                    </div>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </TableShell>
      )}

      {/* Per-key access */}
      <Card className="mt-6">
        <div className="flex items-center justify-between mb-3">
          <div>
            <Eyebrow>Per-key access</Eyebrow>
            <h3 className="text-sm font-semibold mt-1">Which MCP servers each API key may use</h3>
          </div>
        </div>
        {keys.length === 0 ? (
          <p className="text-xs text-muted">No gateway keys yet.</p>
        ) : (
          <div className="divide-y divide-stone">
            {keys.slice(0, 12).map((k) => (
              <div key={k.id} className="flex items-center justify-between gap-3 py-2">
                <div className="min-w-0">
                  <div className="text-sm font-medium truncate">{k.name}</div>
                  <div className="text-[11px] font-mono text-muted">{k.prefix}</div>
                </div>
                <Button size="sm" variant={canEditKeys ? 'secondary' : 'ghost'}
                  onClick={() => setPermKey({ id: k.id, name: k.name })}
                  disabled={!canEditKeys}
                  title={canEditKeys ? 'Edit MCP access' : 'Requires keys:limits'}>
                  <Icon name="sliders" size={13} />MCP access
                </Button>
              </div>
            ))}
          </div>
        )}
      </Card>

      {/* Recent tool calls */}
      <Card className="mt-6" pad={false}>
        <div className="flex items-center justify-between px-4 py-3 border-b border-stone">
          <div>
            <Eyebrow>Audit</Eyebrow>
            <h3 className="text-sm font-semibold mt-1">Recent tool calls</h3>
          </div>
          <Button size="sm" variant="ghost" onClick={refreshCalls}><Icon name="refresh" size={13} />Refresh</Button>
        </div>
        {calls.length === 0 ? (
          <div className="p-4 text-xs text-muted">No tool calls recorded yet.</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-stone">
                  <Th>Tool</Th><Th>Server</Th><Th>Key</Th><Th>Endpoint</Th><Th>Status</Th><Th>Duration</Th><Th>When</Th>
                </tr>
              </thead>
              <tbody>
                {calls.slice(0, 20).map((c, i) => (
                  <tr key={`${c.id || i}`} className="hover:bg-raised/40">
                    <Td><span className="font-mono text-xs">{c.namespaced_name || c.tool_name}</span></Td>
                    <Td><span className="text-xs text-muted">{c.server_slug}</span></Td>
                    <Td><span className="font-mono text-[11px] text-muted">{c.key_prefix || '—'}</span></Td>
                    <Td><span className="font-mono text-[11px] text-muted">{c.endpoint || '—'}</span></Td>
                    <Td>
                      <Badge tone={c.status === 'ok' ? 'good' : c.status === 'denied' ? 'warn' : 'bad'}>{c.status}</Badge>
                      {c.error && <div className="text-[11px] text-danger/90 max-w-[200px] truncate" title={c.error}>{c.error}</div>}
                    </Td>
                    <Td><span className="text-xs tabular-nums text-muted">{c.duration_ms}ms</span></Td>
                    <Td><span className="text-xs text-muted">{when(c.created_at)}</span></Td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {/* Modals */}
      <Modal open={createOpen} onClose={() => setCreateOpen(false)} title="Add MCP server" width="max-w-2xl">
        <ServerForm initial={EMPTY_FORM} busy={saving} onSubmit={create} onCancel={() => setCreateOpen(false)} />
      </Modal>

      {editTarget && (
        <Modal open onClose={() => setEditTarget(null)} title={`Edit ${editTarget.name}`} width="max-w-2xl">
          <ServerForm initial={editForm} busy={saving} onSubmit={saveEdit} onCancel={() => setEditTarget(null)} />
        </Modal>
      )}

      <Confirm open={!!deleteTarget} onClose={() => setDeleteTarget(null)} onConfirm={remove}
        busy={deleting} title="Delete MCP server"
        body={deleteTarget ? `Remove ${deleteTarget.name}? Its cached tools and per-key grants are deleted, and any key that explicitly referenced it loses access. Upstream credentials are destroyed with the row.` : ''} />

      {toolsFor && (
        <Modal open onClose={() => setToolsFor(null)} title={`${toolsFor.name} — tools`} width="max-w-2xl">
          {toolsError && <ErrorNote message={toolsError} />}
          {toolsBusy ? <Skeleton className="h-32" /> : tools.length === 0 ? (
            <div className="text-sm text-muted">
              No tools discovered yet. Use <strong>Test</strong> on the server row to connect and refresh the tool list.
            </div>
          ) : (
            <div className="space-y-2">
              {tools.map((t) => (
                <div key={t.namespaced_name} className="rounded-lg border border-stone p-3">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="font-mono text-xs text-accent">{t.namespaced_name}</span>
                    {origin && <CopyButton value={t.namespaced_name} label="Copy tool name" />}
                  </div>
                  {t.description && <p className="text-xs text-muted mt-1 leading-relaxed">{t.description}</p>}
                  <details className="mt-2">
                    <summary className="text-[11px] text-muted cursor-pointer select-none">Input schema</summary>
                    <pre className="mt-1 text-[11px] font-mono bg-raised/60 border border-stone rounded p-2 overflow-x-auto">
{JSON.stringify(t.input_schema ?? {}, null, 2)}
                    </pre>
                  </details>
                </div>
              ))}
            </div>
          )}
        </Modal>
      )}

      {permKey && (
        <KeyPermissionsModal keyId={permKey.id} keyName={permKey.name} onClose={() => setPermKey(null)} />
      )}
    </div>
  )
}
