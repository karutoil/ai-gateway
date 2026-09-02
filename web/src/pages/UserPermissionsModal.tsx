import { useEffect, useMemo, useState } from 'react'
import { api } from '../lib/api'
import { PERM_CATALOG, PERM_GROUPS, type Perm } from '../lib/permissions'
import { Modal, Button, Badge, Icon, useToast } from '../components/ui'

type PermState = 'inherit' | 'allow' | 'deny'

type PermissionsPayload = {
  user_id: string
  username: string
  role: string
  effective: Perm[]
  overrides: Record<string, boolean>
  catalog: Perm[]
}

/**
 * Per-user permissions editor. Three states per permission:
 *   inherit — role default applies (no override row)
 *   allow   — explicit grant (even if the role lacks it)
 *   deny    — explicit revoke (even if the role has it)
 *
 * Admins are fixed: the backend always allows admins regardless of overrides.
 */
export default function UserPermissionsModal({ userId, onClose, onSaved }: {
  userId: string | null
  onClose: () => void
  onSaved?: () => void
}) {
  const toast = useToast()
  const [data, setData] = useState<PermissionsPayload | null>(null)
  const [states, setStates] = useState<Record<string, PermState>>({})
  const [dirty, setDirty] = useState(false)
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!userId) { setData(null); setStates({}); setDirty(false); setError(''); return }
    setLoading(true)
    api.users.getPermissions(userId)
      .then((d: any) => {
        setData(d)
        const s: Record<string, PermState> = {}
        for (const p of PERM_CATALOG) {
          if (d.overrides && p in d.overrides) s[p] = d.overrides[p] ? 'allow' : 'deny'
        }
        setStates(s)
        setDirty(false)
      })
      .catch((e: any) => setError(e?.message || String(e)))
      .finally(() => setLoading(false))
  }, [userId])

  const effectiveSet = useMemo(() => new Set(data?.effective ?? []), [data])

  const cycle = (perm: string) => {
    setStates(prev => {
      const order: PermState[] = ['inherit', 'allow', 'deny']
      const cur = prev[perm] ?? 'inherit'
      const next = order[(order.indexOf(cur) + 1) % order.length]
      const s = { ...prev }
      if (next === 'inherit') delete s[perm]
      else s[perm] = next
      return s
    })
    setDirty(true)
  }

  const save = async () => {
    if (!userId || !data) return
    setSaving(true)
    try {
      const overrides: Record<string, boolean | null> = {}
      for (const [perm, st] of Object.entries(states)) {
        overrides[perm] = st === 'allow' ? true : st === 'deny' ? false : null
      }
      await api.users.setPermissions(userId, overrides)
      toast.success('Permissions updated — applies immediately')
      setDirty(false)
      onSaved?.()
      onClose()
    } catch (e: any) {
      toast.error(e?.message || 'Could not save permissions')
    } finally {
      setSaving(false)
    }
  }

  const adminFixed = data?.role === 'admin'
  const allowCount = Object.values(states).filter(s => s === 'allow').length
  const denyCount = Object.values(states).filter(s => s === 'deny').length

  return (
    <Modal open={userId !== null} onClose={onClose} title="User permissions" width="max-w-2xl">
      {error && <div className="text-sm text-red-400">{error}</div>}
      {loading && !data && <div className="py-8 text-center text-sm text-muted">Loading…</div>}
      {data && (
        <div className="space-y-4">
          <div className="flex items-center justify-between gap-2 flex-wrap">
            <div className="flex items-center gap-2 text-sm">
              <span className="font-medium">{data.username}</span>
              <Badge tone="neutral">{data.role}</Badge>
              {adminFixed && (
                <span className="text-xs text-muted">admin — always full access</span>
              )}
            </div>
            <div className="text-xs text-muted">
              {allowCount > 0 && <span className="text-teal">{allowCount} granted </span>}
              {denyCount > 0 && <span className="text-amber">{denyCount} denied </span>}
              {(allowCount === 0 && denyCount === 0) && 'following role defaults'}
            </div>
          </div>

          {adminFixed ? (
            <div className="space-y-3">
              <div className="rounded-lg border border-stone bg-app p-4 text-sm text-muted">
                Admins hold every permission by design — overrides don't apply to the admin
                role, so this account can never be locked out. Switch the user's role first
                if they should have limited access. Full set:
              </div>
              <div className="flex flex-wrap gap-1.5">
                {(data.effective ?? []).map(p => (
                  <Badge key={p} tone="good"><Icon name="check" size={10} /> {p}</Badge>
                ))}
              </div>
            </div>
          ) : (
            <div className="space-y-4 max-h-[55vh] overflow-y-auto pr-1">
              <p className="text-xs text-muted">
                Click a permission to cycle: <Badge tone="neutral">inherit</Badge> role default →
                <Badge tone="good">allow</Badge> force-grant → <Badge tone="bad">deny</Badge> force-revoke.
                Changes apply immediately, no re-login needed.
              </p>
              {PERM_GROUPS.map(g => (
                <div key={g.resource} className="rounded-lg border border-stone overflow-hidden">
                  <div className="px-3 py-2 bg-raised text-xs font-medium uppercase tracking-wide text-muted">{g.label}</div>
                  <div className="divide-y divide-stone">
                    {g.perms.map(p => {
                      const st = states[p.perm] ?? 'inherit'
                      const eff = effectiveSet.has(p.perm)
                      return (
                        <button key={p.perm} onClick={() => cycle(p.perm)}
                          className="w-full text-left px-3 py-2 flex items-center justify-between gap-3 hover:bg-raised/60 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-teal/50">
                          <span className="min-w-0">
                            <span className="text-sm block">{p.label}</span>
                            {p.hint && <span className="text-xs text-muted block">{p.hint}</span>}
                            <span className="font-mono text-[10px] text-muted/70">{p.perm}</span>
                          </span>
                          <span className="flex items-center gap-2 shrink-0">
                            <span className="text-[10px] text-muted">{eff ? 'effective' : '—'}</span>
                            <StateBadge st={st} />
                          </span>
                        </button>
                      )
                    })}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
      <div className="flex justify-end gap-2 mt-5 pt-4 border-t border-stone">
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        {!adminFixed && (
          <Button variant="primary" onClick={save} disabled={!dirty || saving || loading}>
            {saving ? 'Saving…' : 'Save permissions'}
          </Button>
        )}
      </div>
    </Modal>
  )
}

function isAdminUser(role?: string): boolean { return role === 'admin' }

function summarize(perms: string[]): string {
  const has = (p: string) => perms.includes(p)
  const parts: string[] = []
  if (has('keys:read')) parts.push('all keys')
  else if (has('keys:read_own')) parts.push('own keys only')
  if (has('keys:create')) parts.push('create keys')
  if (has('keys:rotate')) parts.push('rotate keys')
  if (has('logs:read')) parts.push('all request logs')
  else if (has('keys:read_own')) parts.push('own key activity')
  if (has('providers:read')) parts.push(has('providers:write') ? 'providers (manage)' : 'providers (view)')
  if (has('users:read')) parts.push('users')
  if (has('audit:read')) parts.push('audit')
  if (has('settings:write')) parts.push('settings')
  const rest = perms.length - parts.length
  return parts.length ? parts.join(' · ') + (rest > 0 ? ` +${rest} more` : '') : 'no access'
}

function StateBadge({ st }: { st: PermState }) {
  if (st === 'allow') return <Badge tone="good"><Icon name="check" size={10} /> allow</Badge>
  if (st === 'deny') return <Badge tone="bad"><Icon name="x" size={10} /> deny</Badge>
  return <Badge tone="neutral">inherit</Badge>
}
