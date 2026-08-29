// Optional in-memory Bearer override for programmatic/explicit-token callers.
// Nothing in this app writes to it: the browser automatically sends the
// HttpOnly "gw_token" session cookie set by POST /api/auth/login instead.
let memoryBearer: string | null = null

/**
 * Explicitly attach a Bearer token to subsequent requests (in-memory only,
 * never persisted). Leave unset/null to rely purely on the session cookie.
 */
export function setBearerToken(token: string | null) { memoryBearer = token }

function getApiBase(): string {
  try {
    // @ts-ignore - Vite env
    const env: any = (import.meta as any).env || {}
    const base = env.VITE_API_URL || env.VITE_PUBLIC_URL || env.VITE_GATEWAY_URL || ''
    if (base) return String(base).replace(/\/$/, '')
    return ''
  } catch {
    return ''
  }
}

function apiUrl(path: string): string {
  const base = getApiBase()
  if (!base) return path
  // If path already absolute, return as is
  if (path.startsWith('http://') || path.startsWith('https://')) return path
  return base + path
}

async function req(path: string, opts: RequestInit = {}) {
  const headers: Record<string,string> = {
    'Content-Type':'application/json',
    ...(opts.headers as any),
  }
  // Attach a Bearer token only when explicitly provided by the caller
  // (or via the in-memory override). Regular dashboard auth rides on the
  // HttpOnly session cookie, which same-origin fetches send automatically.
  const hasExplicitAuth = Object.keys(headers).some(h => h.toLowerCase() === 'authorization')
  if (!hasExplicitAuth && memoryBearer) headers['Authorization'] = `Bearer ${memoryBearer}`
  const res = await fetch(apiUrl(path), { ...opts, headers, credentials: 'same-origin' })
  if (!res.ok) {
    const text = await res.text()
    // auto-logout on 401 (cookie invalid/expired)
    if (res.status === 401) {
      setBearerToken(null)
      // don't redirect automatically to avoid loops, but emit event
      window.dispatchEvent(new CustomEvent('gw:unauthorized'))
    }
    let msg = text || res.statusText
    try {
      const j = JSON.parse(text)
      // Error envelopes seen in the wild: {"error":"str"}, {"error":{message}},
      // {"message"}. Pick the first string we can find so callers always get a
      // readable message (an object here would render as "[object Object]").
      if (typeof j?.error === 'string') msg = j.error
      else if (j?.error && typeof j.error.message === 'string') msg = j.error.message
      else if (typeof j?.message === 'string') msg = j.message
    } catch {}
    throw new Error(msg)
  }
  if (res.status === 204) return null
  const ct = res.headers.get('Content-Type') || ''
  if (ct.includes('application/json')) return res.json()
  try { return await res.json() } catch { return null }
}

/**
 * Extract a readable error message from a raw fetch response body — mirrors
 * the envelope handling in req() so callers using raw fetch() (login,
 * passkey flows) render the same clean messages instead of raw JSON.
 */
export function extractApiError(text: string, fallback = 'request failed'): string {
  const t = (text || '').trim()
  if (!t) return fallback
  try {
    const j = JSON.parse(t)
    if (typeof j?.error === 'string') return j.error
    else if (j?.error && typeof j.error.message === 'string') return j.error.message
    else if (typeof j?.message === 'string') return j.message
  } catch {}
  return t.length > 400 ? t.slice(0, 400) + '…' : t
}

/** Query params accepted by GET /api/logs. */
export type LogsQuery = {
  limit?: number
  offset?: number
  model?: string
  key?: string
  endpoint?: string
  /** "failed" or an HTTP status code as a string */
  status?: string
  /** e.g. "1h", "24h", "7d", "30d" */
  since?: string
}

// Load-balancer routing rules: per-model provider groups with a strategy.
// Non-failover strategies serve each request with ONE member; failover walks
// members in position order on retriable failures. Qualified model ids
// ("openai/gpt-4o") and X-Provider headers bypass these rules.
export type RoutingStrategy = 'round_robin' | 'random' | 'weighted' | 'failover'
export type LBMember = {
  provider_id: string
  name: string
  type: string
  weight?: number // weighted strategy: relative traffic share (1-100)
  model_override?: string // optional model id sent upstream for this member
  health_status?: string | null // "up" | "down" | null/absent = unknown
}
export type LBRule = {
  model: string
  strategy: RoutingStrategy
  providers: LBMember[] // array order = member position / failover order
}

// Write-path member shape for saveRule.
export type LBMemberInput = {
  provider_id: string
  model_override?: string
  weight?: number
}

export const api = {
  login: (password: string, username?: string) => fetch(apiUrl('/api/auth/login'), { method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: JSON.stringify(username ? {username, password} : {password})}).then(r=>r.json()),
  providers: {
    list: () => req('/api/providers'),
    create: (data: any) => req('/api/providers', { method:'POST', body: JSON.stringify(data)}),
    update: (id: string, data: { name?: string; base_url?: string; api_key?: string }) =>
      req(`/api/providers/${encodeURIComponent(id)}`, { method:'PUT', body: JSON.stringify(data)}),
    remove: (id: string) => req(`/api/providers/${id}`, { method:'DELETE'}),
    discover: (id: string) => req(`/api/providers/${id}/discover`, { method:'POST'}),
  },
  lb: {
    listRules: (): Promise<LBRule[]> => req('/api/lb/rules'),
    // PUT is an upsert: replaces the member set and strategy for the model.
    saveRule: (model: string, opts: { strategy?: RoutingStrategy; members: LBMemberInput[] }): Promise<LBRule> =>
      req(`/api/lb/rules/${encodeURIComponent(model)}`, { method:'PUT', body: JSON.stringify({ strategy: opts.strategy, members: opts.members })}),
    deleteRule: (model: string): Promise<null> =>
      req(`/api/lb/rules/${encodeURIComponent(model)}`, { method:'DELETE'}),
  },
  keys: {
    list: () => req('/api/keys'),
    create: (name: string, opts?: Record<string, unknown>) => req('/api/keys', { method:'POST', body: JSON.stringify({name, ...(opts||{})})}),
    remove: (id:string) => req(`/api/keys/${id}`, { method:'DELETE'}),
    setRpm: (id:string, rpm:number) => req(`/api/keys/${id}/rate-limit`, { method:'PUT', body: JSON.stringify({rpm})}),
    setLimits: (id:string, data: Record<string, unknown>) => req(`/api/keys/${id}/limits`, { method:'PUT', body: JSON.stringify(data)}),
    update: (id:string, data:{name:string}) => req(`/api/keys/${id}`, { method:'PUT', body: JSON.stringify(data)}),
    bulkRemove: (ids:string[]) => Promise.all(ids.map(id=> req(`/api/keys/${id}`, { method:'DELETE'}))),
  },
  stats: () => req('/api/stats'),
  logs: () => req('/api/logs'),
  /**
   * Paginated/filtered request logs. Response stays a JSON array; the total
   * matching row count rides on the X-Total-Count header.
   */
  logsQuery: async (params: LogsQuery = {}): Promise<{ rows: any[]; total: number | null }> => {
    const p = new URLSearchParams()
    if (params.limit != null) p.set('limit', String(params.limit))
    if (params.offset != null) p.set('offset', String(params.offset))
    if (params.model) p.set('model', params.model)
    if (params.key) p.set('key', params.key)
    if (params.endpoint) p.set('endpoint', params.endpoint)
    if (params.status) p.set('status', params.status)
    if (params.since) p.set('since', params.since)
    const res = await fetch(apiUrl(`/api/logs?${p.toString()}`), { credentials: 'same-origin' })
    if (!res.ok) {
      if (res.status === 401) { setBearerToken(null); window.dispatchEvent(new CustomEvent('gw:unauthorized')) }
      const text = await res.text()
      throw new Error(extractApiError(text, `log query failed (${res.status})`))
    }
    const rows = await res.json()
    const totalHdr = res.headers.get('X-Total-Count')
    const total = totalHdr != null && totalHdr !== '' && !Number.isNaN(Number(totalHdr)) ? Number(totalHdr) : null
    return { rows: Array.isArray(rows) ? rows : [], total }
  },
  health: () => fetch(apiUrl('/health')).then(r=>r.json()),
  catalog: {
    list: (q?: string, provider?: string, reasoning?: boolean, limit?: number) => {
      const p = new URLSearchParams()
      if(q) p.set('q', q)
      if(provider) p.set('provider', provider)
      if(reasoning) p.set('reasoning','true')
      if(limit) p.set('limit', String(limit))
      return req(`/api/models/catalog?${p.toString()}`)
    },
    get: (id: string) => req(`/api/models/catalog/by-id?id=${encodeURIComponent(id)}`),
    sync: () => req('/api/models/sync', { method:'POST'}),
    status: () => req('/api/models/status'),
    aliases: () => req('/api/models/aliases'),
    createAlias: (alias:string, target:string) => req('/api/models/aliases', { method:'POST', body: JSON.stringify({alias, target})}),
    deleteAlias: (alias:string) => req(`/api/models/aliases/${encodeURIComponent(alias)}`, { method:'DELETE'}),
    settings: () => req('/api/models/settings'),
    putSettings: (data:any) => req('/api/models/settings', { method:'PUT', body: JSON.stringify(data)}),
    /** DELETE /api/models/settings/{key} — 204 on success. */
    deleteSetting: (key: string) => req(`/api/models/settings/${encodeURIComponent(key)}`, { method:'DELETE'}),
  },
  providerModels: {
    list: (providerId?: string, q?: string) => {
      const p = new URLSearchParams()
      if(providerId) p.set('provider_id', providerId)
      if(q) p.set('q', q)
      return req(`/api/provider-models?${p.toString()}`)
    },
    discover: (providerId: string) => req(`/api/providers/${providerId}/discover`, { method:'POST'}),
    discoverAll: () => req('/api/discover-all', { method:'POST'}),
    add: (data: any) => req('/api/provider-models', { method:'POST', body: JSON.stringify(data)}),
    update: (id: string, data: any) => req(`/api/provider-models/${id}`, { method:'PUT', body: JSON.stringify(data)}),
    enrich: (id: string) => req(`/api/provider-models/${id}/enrich`, { method:'POST'}),
    remove: (id: string) => req(`/api/provider-models/${id}`, { method:'DELETE'}),
    bulkEnrich: (ids:string[]) => Promise.all(ids.map(id=> req(`/api/provider-models/${id}/enrich`, { method:'POST'}))),
    bulkRemove: (ids:string[]) => Promise.all(ids.map(id=> req(`/api/provider-models/${id}`, { method:'DELETE'}))),
  },
  orgs: {
    list: () => req('/api/orgs'),
    create: (name: string) => req('/api/orgs', { method:'POST', body: JSON.stringify({ name })}),
    remove: (id: string) => req(`/api/orgs/${id}`, { method:'DELETE'}),
    members: (orgId: string) => req(`/api/orgs/${orgId}/members`),
    addMember: (orgId: string, user_id: string, role: string) => req(`/api/orgs/${orgId}/members`, { method:'POST', body: JSON.stringify({ user_id, role })}),
  },
  profile: {
    get: () => req('/api/profile'),
    update: (data:any) => req('/api/profile', { method:'PUT', body: JSON.stringify(data)}),
    changePassword: (old_password:string, new_password:string) => req('/api/profile/password', { method:'POST', body: JSON.stringify({ old_password, new_password })}),
    activity: () => req('/api/profile/activity'),
    logins: () => req('/api/profile/logins'),
  },
  users: {
    list: () => req('/api/admin/users'),
    me: () => req('/api/admin/users/me'),
    create: (data: {username:string; password:string; role:string; display_name?:string}) => req('/api/admin/users', { method:'POST', body: JSON.stringify(data)}),
    update: (id:string, data:any) => req(`/api/admin/users/${id}`, { method:'PUT', body: JSON.stringify(data)}),
    remove: (id:string) => req(`/api/admin/users/${id}`, { method:'DELETE'}),
    resetPassword: (id:string, password:string) => req(`/api/admin/users/${id}/reset-password`, { method:'POST', body: JSON.stringify({password})}),
  },
  /** Admin-only audit trail: GET /api/audit?limit=&offset=&actor= */
  audit: {
    list: async (params: { limit?: number; offset?: number; actor?: string } = {}): Promise<{ rows: any[]; total: number | null }> => {
      const p = new URLSearchParams()
      if (params.limit != null) p.set('limit', String(params.limit))
      if (params.offset != null) p.set('offset', String(params.offset))
      if (params.actor) p.set('actor', params.actor)
      const res = await fetch(apiUrl(`/api/audit?${p.toString()}`), { credentials: 'same-origin' })
      if (!res.ok) {
        if (res.status === 401) { setBearerToken(null); window.dispatchEvent(new CustomEvent('gw:unauthorized')) }
        const text = await res.text()
        throw new Error(extractApiError(text, `audit query failed (${res.status})`))
      }
      const rows = await res.json()
      const totalHdr = res.headers.get('X-Total-Count')
      const total = totalHdr != null && totalHdr !== '' && !Number.isNaN(Number(totalHdr)) ? Number(totalHdr) : null
      return { rows: Array.isArray(rows) ? rows : [], total }
    },
  },
  passkey: {
    registerBegin: (userId?: string) => req(`/api/auth/passkey/register/begin${userId ? `?user_id=${userId}` : ''}`, { method:'POST' }),
    registerFinish: (session:string, credential:any) => req('/api/auth/passkey/register/finish', { method:'POST', headers: { 'X-WebAuthn-Session': session }, body: JSON.stringify(credential)}),
    registerFinishWithSession: (session:string, credential:any) => {
      // Handles both wrapped {session, credential} and raw
      return req('/api/auth/passkey/register/finish', { method:'POST', body: JSON.stringify({ session, credential })})
    },
    loginBegin: (username?: string) => req('/api/auth/passkey/login/begin', { method:'POST', body: JSON.stringify({ username: username||'' })}),
    loginFinish: (session:string, credential:any) => req('/api/auth/passkey/login/finish', { method:'POST', headers: { 'X-WebAuthn-Session': session }, body: JSON.stringify(credential)}),
    loginFinishWithSession: (session:string, credential:any) => req('/api/auth/passkey/login/finish', { method:'POST', body: JSON.stringify({ session, credential })}),
    list: (userId?:string) => req(`/api/auth/passkey/credentials${userId? `?user_id=${userId}`:''}`),
    disable: (userId?:string) => req(`/api/auth/passkey/disable${userId? `?user_id=${userId}`:''}`, { method:'POST' }),
    generateRecovery: (userId?:string) => req(`/api/auth/passkey/recovery/generate${userId? `?user_id=${userId}`:''}`, { method:'POST' }),
    verifyRecovery: (username:string, code:string) => req('/api/auth/recovery/verify', { method:'POST', body: JSON.stringify({ username, code })}),
  }
}

// Export helpers for direct fetch uses (Playground)
export function getApiBaseForFetch(): string { return getApiBase(); }
export function buildApiUrl(path: string): string { return apiUrl(path); }
