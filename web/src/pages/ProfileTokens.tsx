import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { PageHeader, Card, Button, Input, Field, Badge, Icon, CopyButton, Modal, Confirm, EmptyState, useToast } from '../components/ui'

type Pat = {
  id: string
  name: string
  prefix: string
  scopes?: string
  last_used_at?: string | null
  expires_at?: string | null
  created_at: string
  revoked_at?: string | null
}

/**
 * Personal access tokens for the dashboard API — long-lived bearer tokens
 * (gwp_...) for automation/CI. Scopes narrow the user's own permissions;
 * a PAT can never exceed the rights of the user who created it.
 */
export default function ProfileTokens() {
  const toast = useToast()
  const [tokens, setTokens] = useState<Pat[]>([])
  const [loading, setLoading] = useState(true)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [days, setDays] = useState('')
  const [scopes, setScopes] = useState('')
  const [created, setCreated] = useState<{ secret: string; name: string } | null>(null)
  const [revokeTarget, setRevokeTarget] = useState<Pat | null>(null)

  const load = () => api.profile.tokens().then((t: any) => setTokens(Array.isArray(t) ? t : [])).catch(() => setTokens([])).finally(() => setLoading(false))

  useEffect(() => { load() }, [])

  const create = async () => {
    if (!name.trim()) return
    setCreating(true)
    try {
      const res = await api.profile.createToken({
        name: name.trim(),
        expires_days: days ? Number(days) : undefined,
        scopes: scopes.trim() || undefined,
      })
      setCreated({ secret: res.secret, name: name.trim() })
      setName(''); setDays(''); setScopes('')
      load()
    } catch (e: any) {
      toast.error(e?.message || 'Could not create token')
    } finally {
      setCreating(false)
    }
  }

  const revoke = async () => {
    if (!revokeTarget) return
    try {
      await api.profile.revokeToken(revokeTarget.id)
      toast.success(`Token "${revokeTarget.name}" revoked`)
      setRevokeTarget(null)
      load()
    } catch (e: any) {
      toast.error(e?.message || 'Revoke failed')
    }
  }

  return (
    <Card className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold flex items-center gap-2">
            <Icon name="key" size={15} className="text-teal" /> API tokens
          </h3>
          <p className="text-xs text-muted mt-0.5">
            Bearer tokens for the dashboard API (<code className="font-mono">Authorization: Bearer gwp_...</code>).
            Scopes narrow what a token may do — a token never exceeds your own permissions.
          </p>
        </div>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-[1fr_120px_1fr_auto] gap-2 items-end">
        <Field label="Name"><Input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. ci-reporting" /></Field>
        <Field label="Expires (days)"><Input value={days} inputMode="numeric" onChange={e => setDays(e.target.value.replace(/\D/g, ''))} placeholder="never" /></Field>
        <Field label="Scopes (optional)"><Input value={scopes} onChange={e => setScopes(e.target.value)} placeholder="logs:read,analytics:read" spellCheck={false} /></Field>
        <Button variant="primary" onClick={create} disabled={!name.trim() || creating}>
          <Icon name="plus" size={14} />{creating ? 'Creating…' : 'Create'}
        </Button>
      </div>

      {loading ? (
        <div className="text-sm text-muted py-3">Loading…</div>
      ) : tokens.length === 0 ? (
        <EmptyState icon="key" title="No API tokens yet" hint="Create one to let scripts or CI query the dashboard API as you." />
      ) : (
        <div className="space-y-1.5">
          {tokens.map(t => {
            const revoked = !!t.revoked_at
            const expired = t.expires_at && new Date(t.expires_at) < new Date()
            return (
              <div key={t.id} className={`flex items-center justify-between gap-3 rounded-lg border px-3 py-2 ${revoked || expired ? 'border-stone/50 opacity-50' : 'border-stone'}`}>
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-sm truncate">{t.name}</span>
                    {revoked ? <Badge tone="bad">revoked</Badge> : expired ? <Badge tone="bad">expired</Badge> : <Badge tone="good">active</Badge>}
                    {t.scopes ? <Badge tone="info">{t.scopes}</Badge> : <span className="text-[10px] text-muted">full user access</span>}
                  </div>
                  <div className="text-xs text-muted font-mono mt-0.5">
                    {t.prefix}… · created {new Date(t.created_at).toLocaleDateString()}
                    {t.last_used_at ? ` · used ${new Date(t.last_used_at).toLocaleDateString()}` : ' · never used'}
                    {t.expires_at ? ` · expires ${new Date(t.expires_at).toLocaleDateString()}` : ''}
                  </div>
                </div>
                {!revoked && !expired && (
                  <Button variant="ghost" size="sm" className="text-red-400 hover:bg-red-500/10 shrink-0" onClick={() => setRevokeTarget(t)}>
                    <Icon name="trash" size={13} /> Revoke
                  </Button>
                )}
              </div>
            )
          })}
        </div>
      )}

      {/* One-time secret display */}
      <Modal open={!!created} onClose={() => setCreated(null)} title="Token created" width="max-w-md">
        {created && (
          <>
            <div className="rounded-lg border border-teal/40 bg-teal/5 p-3">
              <div className="text-xs text-muted mb-1.5 uppercase tracking-wide">Token — shown only once</div>
              <code className="block font-mono text-sm break-all select-all text-teal">{created.secret}</code>
            </div>
            <p className="text-xs text-muted mt-2">
              Use it with <code className="font-mono">Authorization: Bearer …</code> on dashboard API calls
              ({created.name} will act with your permissions{created.name ? '' : ''}).
            </p>
            <div className="flex justify-end mt-4">
              <CopyButton value={created.secret} label="Copy token" size="md" />
            </div>
          </>
        )}
      </Modal>

      <Confirm
        open={!!revokeTarget}
        onClose={() => setRevokeTarget(null)}
        onConfirm={revoke}
        title={`Revoke token "${revokeTarget?.name}"?`}
        body="Anything using this token will immediately lose access. This cannot be undone."
        confirmLabel="Revoke token"
      />
    </Card>
  )
}
