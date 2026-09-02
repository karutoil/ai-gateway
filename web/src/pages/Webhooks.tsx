import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  PageHeader, Card, Button, Input, Field, Badge, Icon, Modal, Confirm, EmptyState, useToast,
} from '../components/ui'

type Hook = {
  id: string
  name: string
  url: string
  events: string
  format?: 'json' | 'discord' | 'slack'
  enabled: boolean
  created_at: string
  updated_at: string
  last_status?: string
  last_delivery?: string | null
}

/** Event types the gateway emits (mirror of internal emitters). */
const EVENT_TYPES = [
  'key.created', 'key.revoked', 'key.rotated',
  'user.created', 'user.updated', 'user.disabled',
  'logs.export', 'org.created', 'test.ping',
  'billing.over_quota', 'billing.export',
]

/**
 * Admin page for outbound webhooks: every enabled hook receives matching
 * gateway events (HMAC-signed) at its URL. Gated by settings:write.
 */
export default function Webhooks() {
  const toast = useToast()
  const [hooks, setHooks] = useState<Hook[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [editing, setEditing] = useState<Partial<Hook> | null>(null)
  const [events, setEvents] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<Hook | null>(null)
  const [testing, setTesting] = useState<string | null>(null)

  const load = () => api.webhooks.list()
    .then((h: any) => setHooks(Array.isArray(h) ? h : []))
    .catch(e => setErr(e?.message || String(e)))
    .finally(() => setLoading(false))

  useEffect(() => { load() }, [])

  const openCreate = () => { setEditing({ name: '', url: '', enabled: true, format: 'json' } as Partial<Hook>); setEvents([]) }
  const openEdit = (h: Hook) => {
    setEditing({ ...h })
    setEvents(h.events ? h.events.split(',').map(s => s.trim()).filter(Boolean) : [])
  }

  const toggleEvent = (e: string) => {
    setEvents(prev => prev.includes(e) ? prev.filter(x => x !== e) : [...prev, e])
  }

  const save = async () => {
    if (!editing?.name?.trim() || !editing.url?.trim()) return
    setSaving(true)
    const body = {
      name: editing.name.trim(),
      url: editing.url.trim(),
      events: events.join(','),
      enabled: editing.enabled !== false,
    }
    try {
      if (editing.id) {
        await api.webhooks.update(editing.id, { ...body, secret: (editing as any).secret || '' })
      } else {
        await api.webhooks.create({ ...body, secret: (editing as any).secret || '' })
      }
      toast.success(editing.id ? 'Webhook updated' : 'Webhook created')
      setEditing(null)
      load()
    } catch (e: any) {
      toast.error(e?.message || 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const test = async (h: Hook) => {
    setTesting(h.id)
    try {
      const res = await api.webhooks.test(h.id)
      toast.success(`Test sent — ${res.status}`)
      load()
    } catch (e: any) {
      toast.error(e?.message || 'Test failed')
    } finally {
      setTesting(null)
    }
  }

  const remove = async () => {
    if (!deleteTarget) return
    try {
      await api.webhooks.remove(deleteTarget.id)
      toast.success('Webhook deleted')
      setDeleteTarget(null)
      load()
    } catch (e: any) { toast.error(e?.message || 'Delete failed') }
  }

  return (
    <div className="space-y-4">
      <PageHeader
        title="Webhooks"
        description="Outbound event delivery to your systems. Payloads are JSON, POST, signed with HMAC-SHA256 (X-Webhook-Signature)."
        actions={
          <Button variant="primary" onClick={openCreate}>
            <Icon name="plus" size={15} /> Add webhook
          </Button>
        }
      />

      {err && <Card><EmptyState icon="alert" title="Could not load webhooks" hint={err} /></Card>}

      {!err && loading && <Card><div className="text-sm text-muted py-4">Loading…</div></Card>}

      {!err && !loading && hooks.length === 0 && (
        <Card>
          <EmptyState
            icon="zap"
            title="No webhooks configured"
            hint="Add an endpoint to receive gateway events — key rotations, user changes, exports, and more."
            action={<Button variant="primary" onClick={openCreate}>Add your first webhook</Button>}
          />
        </Card>
      )}

      {hooks.length > 0 && (
        <div className="space-y-2">
          {hooks.map(h => (
            <Card key={h.id} className="!p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="font-medium">{h.name}</span>
                    {h.enabled ? <Badge tone="good">enabled</Badge> : <Badge tone="neutral">disabled</Badge>}
                    {h.events
                      ? <Badge tone="info">{h.events.split(',').length} events</Badge>
                      : <span className="text-xs text-muted">all events</span>}
                  </div>
                  <div className="font-mono text-xs text-muted mt-1 truncate" title={h.url}>{h.url}</div>
                  <div className="text-xs text-muted mt-1">
                    {h.last_status
                      ? <>last delivery: <span className={h.last_status.startsWith('2') ? 'text-teal' : 'text-amber'}>{h.last_status}</span></>
                      : 'no deliveries yet'}
                  </div>
                </div>
                <div className="flex items-center gap-1 shrink-0">
                  <Button variant="secondary" size="sm" disabled={testing === h.id} onClick={() => test(h)}>
                    {testing === h.id ? '…' : 'Test'}
                  </Button>
                  <Button variant="ghost" size="sm" onClick={() => openEdit(h)} title="Edit"><Icon name="pencil" size={14} /></Button>
                  <Button variant="ghost" size="sm" className="text-red-400 hover:bg-red-500/10" onClick={() => setDeleteTarget(h)} title="Delete">
                    <Icon name="trash" size={14} />
                  </Button>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

      {/* Create / edit modal */}
      <Modal open={!!editing} onClose={() => setEditing(null)} title={editing?.id ? 'Edit webhook' : 'Add webhook'} width="max-w-lg">
        {editing && (
          <div className="space-y-3">
            <Field label="Name">
              <Input value={editing.name || ''} onChange={e => setEditing({ ...editing, name: e.target.value })} placeholder="e.g. Slack relay" />
            </Field>
            <Field label="Endpoint URL">
              <Input value={editing.url || ''} onChange={e => setEditing({ ...editing, url: e.target.value })} placeholder="https://example.com/hooks/gateway" spellCheck={false} />
            </Field>
            <Field label="Secret (optional)">
              <Input
                value={(editing as any).secret || ''}
                onChange={e => setEditing({ ...editing, secret: e.target.value } as any)}
                placeholder="HMAC key for X-Webhook-Signature"
                spellCheck={false}
              />
            </Field>

				<Field label="Payload format">
					<div className="flex gap-1.5">
						{(['json', 'discord', 'slack'] as const).map((f) => (
							<button
								key={f}
								type="button"
								onClick={() => setEditing({ ...editing, format: f } as typeof editing)}
								className={`px-3 py-1.5 rounded-lg text-xs font-medium border transition-colors focus:outline-none ${
									(editing?.format || 'json') === f
										? 'border-teal/60 bg-teal/10 text-teal'
										: 'border-stone text-muted hover:text-paper'
								}`}
							>
								{f === 'json' ? 'Generic JSON' : f === 'discord' ? 'Discord' : 'Slack'}
							</button>
						))}
					</div>
					<p className="text-xs text-muted mt-1">
						{editing?.format === 'discord'
							? 'Formats the payload for Discord incoming webhooks (message + embed).'
							: editing?.format === 'slack'
								? 'Formats the payload for Slack incoming webhooks ({text: ...}).'
								: 'Raw gateway event envelope ({event, payload, ts}).'}
					</p>
				</Field>
            <Field label="Events" hint="Leave all unchecked to receive every event.">
              <div>
                <div className="text-xs font-medium text-muted uppercase tracking-wide mb-1.5">Events</div>
                <div className="flex flex-wrap gap-1.5">
                  {EVENT_TYPES.map(e => (
                    <button
                      key={e}
                      onClick={() => toggleEvent(e)}
                      className={`px-2 py-1 rounded-md text-xs font-mono border transition-colors focus:outline-none ${
                        events.includes(e)
                          ? 'border-teal/60 bg-teal/10 text-teal'
                          : 'border-stone text-muted hover:text-paper'
                      }`}
                    >
                      {e}
                    </button>
                  ))}
                </div>
              </div>
            </Field>
            <label className="flex items-center gap-2 text-sm cursor-pointer select-none">
              <input
                type="checkbox"
                checked={editing.enabled !== false}
                onChange={e => setEditing({ ...editing, enabled: e.target.checked })}
                className="w-4 h-4 accent-teal"
              />
              Enabled
            </label>
          </div>
        )}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={() => setEditing(null)}>Cancel</Button>
          <Button variant="primary" onClick={save} disabled={saving || !editing?.name?.trim() || !editing?.url?.trim()}>
            {saving ? 'Saving…' : editing?.id ? 'Save changes' : 'Create webhook'}
          </Button>
        </div>
      </Modal>

      <Confirm
        open={!!deleteTarget}
        onClose={() => setDeleteTarget(null)}
        onConfirm={remove}
        title={`Delete webhook "${deleteTarget?.name}"?`}
        body="Events will no longer be delivered to this endpoint."
        confirmLabel="Delete"
      />
    </div>
  )
}
