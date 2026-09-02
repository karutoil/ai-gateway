/**
 * Saved log views: named filter presets persisted in localStorage.
 *
 * Deliberately client-side (not the DB): views are personal UI preference,
 * not shared org state — storing them server-side would add a migration and
 * API surface for something each user tweaks privately.
 */

export type SavedView = {
  id: string
  name: string
  /** URLSearchParams-encoded filter params (same shape as the /logs URL) */
  params: string
  created_at: string
}

const KEY = 'gw_saved_log_views'

export function loadSavedViews(): SavedView[] {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

export function saveView(name: string, params: string): SavedView[] {
  const views = loadSavedViews()
  const view: SavedView = {
    id: crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(36).slice(2)}`,
    name: name.trim().slice(0, 60),
    params,
    created_at: new Date().toISOString(),
  }
  const next = [view, ...views].slice(0, 20) // cap so localStorage stays sane
  try { localStorage.setItem(KEY, JSON.stringify(next)) } catch {}
  return next
}

export function removeSavedView(id: string): SavedView[] {
  const next = loadSavedViews().filter(v => v.id !== id)
  try { localStorage.setItem(KEY, JSON.stringify(next)) } catch {}
  return next
}

/** Human-readable summary of a view's filter params for chip labels. */
export function describeViewParams(params: string): string {
  try {
    const p = new URLSearchParams(params)
    const parts: string[] = []
    const label: Record<string, string> = {
      q: 'search', key_id: 'key', provider_id: 'provider', model: 'model',
      status: 'status', stream: 'stream', has_error: 'errors',
      min_latency_ms: 'lat≥', max_latency_ms: 'lat≤', range: 'range', endpoint: 'endpoint',
    }
    for (const [k, v] of p.entries()) {
      if (label[k]) parts.push(`${label[k]}: ${v}`)
    }
    return parts.length ? parts.join(' · ') : 'all requests'
  } catch {
    return 'custom'
  }
}
