import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  PageHeader, Card, Button, Input, Textarea, Badge, Icon,
  TableShell, Th, Td, Skeleton, EmptyState, ErrorNote, Section, SegmentedControl, useToast,
} from '../components/ui'

/** Known setting keys with inline docs — surfaced next to matching rows. */
const KNOWN_KEYS: Record<string, string> = {
  price_fallback_input_usd_per_1m: 'Default input price ($ per 1M tokens) for models missing from the price catalog.',
  price_fallback_output_usd_per_1m: 'Default output price ($ per 1M tokens) for models missing from the price catalog.',
}

export default function Settings({ role = 'admin' }: { role?: string }) {
  // Settings writes (PUT + DELETE /api/models/settings) are admin-only.
  const isAdmin = role === 'admin'
  const [cfg, setCfg] = useState<Record<string, string>>({})
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [jsonText, setJsonText] = useState('')
  const [mode, setMode] = useState<'table' | 'json'>('table')
  const [newK, setNewK] = useState('')
  const [newV, setNewV] = useState('')
  const [status, setStatus] = useState<string>('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [saving, setSaving] = useState(false)
  const [removingKey, setRemovingKey] = useState<string | null>(null)

  const toast = useToast()

  const load = async () => {
    setLoading(true)
    setLoadError('')
    try {
      const raw = await api.catalog.settings()
      const data: Record<string, string> = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw as Record<string,string> : {}
      const strMap: Record<string, string> = {}
      for (const [k,v] of Object.entries(data)) strMap[k] = String(v ?? '')
      setCfg(strMap); setDraft(strMap); setJsonText(JSON.stringify(strMap, null, 2)); setStatus('')
    } catch (e: any) { setLoadError(e?.message || String(e)) } finally { setLoading(false) }
  }
  useEffect(() => { load() }, [])

  const save = async (data: Record<string, string>) => {
    if (Object.keys(data).length > 50) { setStatus('too many keys (max 50)'); return }
    for (const [k, v] of Object.entries(data)) { if (k.length > 128) { setStatus(`key too long: ${k.slice(0, 20)}`); return } if (v.length > 4096) { setStatus(`value too long for ${k}`); return } }
    setStatus(''); setSaving(true)
    try {
      // PUT responds {saved: n, skipped: [keys]} — skipped keys (internal or
      // over-limit) are never persisted, so surface them instead of failing
      // silently.
      const res: any = await api.catalog.putSettings(data)
      const saved: number = res && typeof res === 'object' && res.saved != null ? Number(res.saved) : Object.keys(data).length
      const skipped: string[] = res && typeof res === 'object' && Array.isArray(res.skipped) ? res.skipped : []
      if (skipped.length > 0) {
        toast.success(`Saved ${saved} entr${saved === 1 ? 'y' : 'ies'} — skipped ${skipped.length}: ${skipped.join(', ')}`)
      } else {
        toast.success(`Saved ${saved} setting${saved === 1 ? '' : 's'}`)
      }
      await load()
    } catch (e: any) { setStatus('save failed: ' + (e?.message || String(e))); toast.error('Save failed') }
    finally { setSaving(false) }
  }
  const saveTable = () => save(draft)
  const saveJson = () => {
    try {
      const parsed = JSON.parse(jsonText)
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) throw new Error('must be object')
      const strMap: Record<string, string> = {}; for (const [k,v] of Object.entries(parsed)) strMap[k]=String(v)
      save(strMap)
    } catch(e:any){ setStatus('invalid json: '+e.message)}
  }
  // Removals hit DELETE /api/models/settings/{key} immediately — the old
  // "Remove → Save" flow resurrected keys on reload because PUT only upserts.
  const removeKey = async (k: string) => {
    setRemovingKey(k)
    try {
      await api.catalog.deleteSetting(k)
      toast.success(`Removed ${k}`)
      await load()
    } catch (e:any) { toast.error(e?.message || `Could not remove ${k}`) }
    finally { setRemovingKey(null) }
  }
  const addEntry = () => {
    const key=newK.trim(); if(!key){setStatus('key required');return}; if(key.length>128){setStatus('key too long');return}; if(newV.length>4096){setStatus('value too long');return}; if(Object.keys(draft).length>=50){setStatus('max 50');return}
    setDraft({...draft,[key]:newV}); setNewK(''); setNewV(''); setStatus(''); toast.info('Added to draft — press Save to apply')
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Settings"
        description="Key–value configuration consumed by the model catalog and routing layer."
        actions={
          <>
            <Button variant="secondary" onClick={load} disabled={saving}><Icon name="refresh" size={14} /> Reload</Button>
            <SegmentedControl<'table' | 'json'>
              options={[{ value: 'table', label: 'Table' }, { value: 'json', label: 'JSON' }]}
              value={mode} onChange={setMode}
            />
          </>
        }
      />

      <Section
        title="Model catalog settings"
        description="Free-form settings map served from /api/models/settings. Removals apply immediately; adds/edits apply on Save. Limits: max 50 entries · keys ≤ 128 chars · values ≤ 4096 chars."
      >
        <div className="space-y-3">
          {/* load failure — real error state with retry, not a fake empty table */}
          {loadError && (
            <Card>
              <EmptyState
                icon="alert"
                title="Could not load settings"
                hint={loadError}
                action={<Button variant="secondary" onClick={load}><Icon name="refresh" size={15}/>Retry</Button>}
              />
            </Card>
          )}

          {/* sticky inline error near its source */}
          {status && !loading && !loadError && <ErrorNote message={status} />}

          {!isAdmin && !loading && !loadError && (
            <div className="border border-amber/30 bg-amber/10 rounded-xl p-3 text-sm flex items-center gap-2">
              <Icon name="alert" size={14} className="text-amber shrink-0"/> Settings are read-only for your role — changes require an admin.
            </div>
          )}

          {loading ? (
            <Card className="space-y-3">
              {[0,1,2,3].map(i => <Skeleton key={i} className="h-10 w-full" />)}
            </Card>
          ) : !loadError && mode==='table' ? (
            <Card pad={false}>
              {isAdmin && (
              <div className="p-5 pb-4">
                <div className="flex items-center gap-2 mb-3">
                  <Icon name="plus" size={15} className="text-teal" />
                  <h3 className="font-semibold text-sm">Add entry</h3>
                  <Badge tone="neutral"><span className="tabular-nums">{Object.keys(draft).length}</span> / 50</Badge>
                </div>
                <div className="flex flex-col sm:flex-row gap-2">
                  <Input value={newK} onChange={e=>setNewK(e.target.value)} placeholder="key — e.g. price_fallback_input_usd_per_1m" className="font-mono text-xs flex-1" spellCheck={false} autoComplete="off"
                    onKeyDown={e=>{ if(e.key==='Enter') addEntry() }} />
                  <Input value={newV} onChange={e=>setNewV(e.target.value)} placeholder="value" className="font-mono text-xs flex-1" spellCheck={false} autoComplete="off"
                    onKeyDown={e=>{ if(e.key==='Enter') addEntry() }} />
                  <Button variant="subtle" onClick={addEntry}><Icon name="plus" size={14} /> Add</Button>
                </div>
                <p className="text-[11px] text-muted mt-2 leading-relaxed">
                  Known keys: <code className="font-mono">price_fallback_input_usd_per_1m</code> /{' '}
                  <code className="font-mono">price_fallback_output_usd_per_1m</code> — default input/output pricing in $ per 1M
                  tokens, applied to models missing from the price catalog.
                </p>
              </div>
              )}

              <TableShell className="!shadow-none !rounded-none border-x-0 border-b-0">
                <table className="w-full text-sm min-w-[480px]">
                  <thead>
                    <tr><Th>Key</Th><Th>Value</Th><Th className="w-12"><span className="sr-only">Remove</span></Th></tr>
                  </thead>
                  <tbody>
                    {Object.entries(draft).map(([k,v])=>(
                      <tr key={k} className="hover:bg-stone/20 transition-colors">
                        <Td className="font-mono text-xs align-top pt-3.5">
                          {k}
                          {KNOWN_KEYS[k] && (
                            <span className="block text-[11px] text-muted font-sans mt-1 max-w-[280px] leading-relaxed">{KNOWN_KEYS[k]}</span>
                          )}
                        </Td>
                        <Td>
                          <Input value={v} onChange={e=>setDraft({...draft,[k]:e.target.value})} className="font-mono text-xs h-9" spellCheck={false} autoComplete="off" disabled={!isAdmin} />
                        </Td>
                        <Td className="text-center">
                          {isAdmin && (
                            <Button variant="ghost" size="sm" className="text-red-400 hover:text-red-300 hover:bg-red-500/10"
                              onClick={()=>removeKey(k)} disabled={removingKey === k} title={`Remove ${k} immediately`}>
                              <Icon name={removingKey === k ? 'refresh' : 'trash'} size={13} className={removingKey === k ? 'animate-spin' : ''} />
                            </Button>
                          )}
                        </Td>
                      </tr>
                    ))}
                    {Object.keys(draft).length===0 && (
                      <tr><td colSpan={3}><EmptyState icon="cog" title="No config." hint={isAdmin ? 'Add an entry above to create one.' : 'No catalog settings are configured.'} /></td></tr>
                    )}
                  </tbody>
                </table>
              </TableShell>

              {/* per-card pinned footer */}
              {isAdmin && (
                <div className="px-5 py-3 border-t border-stone bg-app/60 flex items-center justify-between gap-2">
                  <span className="text-xs text-muted">Edits and adds apply after saving; removals above apply immediately.</span>
                  <Button variant="primary" onClick={saveTable} disabled={saving}>{saving ? 'Saving…' : <> <Icon name="check" size={15} /> Save </>}</Button>
                </div>
              )}
            </Card>
          ) : !loadError ? (
            <Card pad={false}>
              <div className="p-5">
                <div className="flex items-center gap-2 mb-3">
                  <Icon name="logs" size={15} className="text-teal" />
                  <h3 className="font-semibold text-sm">Raw JSON</h3>
                </div>
                <Textarea value={jsonText} onChange={e=>setJsonText(e.target.value)} rows={16} className="min-h-[320px]" spellCheck={false} disabled={!isAdmin} />
              </div>
              {isAdmin && (
                <div className="px-5 py-3 border-t border-stone bg-app/60 flex items-center justify-between gap-2">
                  <span className="text-xs text-muted">Raw JSON object — same endpoint as Table mode.</span>
                  <Button variant="primary" onClick={saveJson} disabled={saving}>{saving ? 'Saving…' : <> <Icon name="check" size={15} /> Save JSON </>}</Button>
                </div>
              )}
            </Card>
          ) : null}
        </div>
      </Section>
    </div>
  )
}
