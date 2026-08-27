import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  PageHeader, Card, Button, Input, Textarea, Badge, Icon,
  TableShell, Th, Td, Skeleton, EmptyState, ErrorNote, Section, SegmentedControl, useToast,
} from '../components/ui'

export default function Settings() {
  const [cfg, setCfg] = useState<Record<string, string>>({})
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [jsonText, setJsonText] = useState('')
  const [mode, setMode] = useState<'table' | 'json'>('table')
  const [newK, setNewK] = useState('')
  const [newV, setNewV] = useState('')
  const [status, setStatus] = useState<string>('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)

  const toast = useToast()

  const load = async () => {
    setLoading(true)
    try {
      const raw = await api.catalog.settings().catch(() => null)
      const data: Record<string, string> = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw as Record<string,string> : {}
      const strMap: Record<string, string> = {}
      for (const [k,v] of Object.entries(data)) strMap[k] = String(v ?? '')
      setCfg(strMap); setDraft(strMap); setJsonText(JSON.stringify(strMap, null, 2)); setStatus('')
    } catch (e: any) { setStatus('load failed: ' + (e?.message || String(e))) } finally { setLoading(false) }
  }
  useEffect(() => { load() }, [])

  const save = async (data: Record<string, string>) => {
    if (Object.keys(data).length > 50) { setStatus('too many keys (max 50)'); return }
    for (const [k, v] of Object.entries(data)) { if (k.length > 128) { setStatus(`key too long: ${k.slice(0, 20)}`); return } if (v.length > 4096) { setStatus(`value too long for ${k}`); return } }
    setStatus(''); setSaving(true)
    try {
      const saved = await api.catalog.putSettings(data)
      const resolved: Record<string,string> = saved && typeof saved === 'object' && !Array.isArray(saved) ? saved as Record<string,string> : data
      const strMap: Record<string,string> = {}; for (const [k,v] of Object.entries(resolved)) strMap[k]=String(v)
      const finalMap = Object.keys(strMap).length ? strMap : data
      setCfg(finalMap); setDraft(finalMap); setJsonText(JSON.stringify(finalMap, null, 2)); setStatus('')
      toast.success('Settings saved')
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
  const removeKey = (k: string) => { const next={...draft}; delete next[k]; setDraft(next); toast.info('Removed — press Save to apply') }
  const addEntry = () => {
    const key=newK.trim(); if(!key){setStatus('key required');return}; if(key.length>128){setStatus('key too long');return}; if(newV.length>4096){setStatus('value too long');return}; if(Object.keys(draft).length>=50){setStatus('max 50');return}
    setDraft({...draft,[key]:newV}); setNewK(''); setNewV(''); toast.info('Added — press Save to apply')
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
        description="Free-form settings map served from /api/models/settings. Limits: max 50 entries · keys ≤ 128 chars · values ≤ 4096 chars."
      >
        <div className="space-y-3">
          {/* sticky inline error near its source */}
          {status && !loading && <ErrorNote message={status} />}

          {loading ? (
            <Card className="space-y-3">
              {[0,1,2,3].map(i => <Skeleton key={i} className="h-10 w-full" />)}
            </Card>
          ) : mode==='table' ? (
            <Card pad={false}>
              <div className="p-5 pb-4">
                <div className="flex items-center gap-2 mb-3">
                  <Icon name="plus" size={15} className="text-teal" />
                  <h3 className="font-semibold text-sm">Add entry</h3>
                  <Badge tone="neutral"><span className="tabular-nums">{Object.keys(draft).length}</span> / 50</Badge>
                </div>
                <div className="flex flex-col sm:flex-row gap-2">
                  <Input value={newK} onChange={e=>setNewK(e.target.value)} placeholder="key" className="font-mono text-xs flex-1" spellCheck={false} autoComplete="off"
                    onKeyDown={e=>{ if(e.key==='Enter') addEntry() }} />
                  <Input value={newV} onChange={e=>setNewV(e.target.value)} placeholder="value" className="font-mono text-xs flex-1" spellCheck={false} autoComplete="off"
                    onKeyDown={e=>{ if(e.key==='Enter') addEntry() }} />
                  <Button variant="subtle" onClick={addEntry}><Icon name="plus" size={14} /> Add</Button>
                </div>
              </div>

              <TableShell className="!shadow-none !rounded-none border-x-0 border-b-0">
                <table className="w-full text-sm min-w-[480px]">
                  <thead>
                    <tr><Th>Key</Th><Th>Value</Th><Th className="w-12"><span className="sr-only">Remove</span></Th></tr>
                  </thead>
                  <tbody>
                    {Object.entries(draft).map(([k,v])=>(
                      <tr key={k} className="hover:bg-stone/20 transition-colors">
                        <Td className="font-mono text-xs align-top pt-3.5">{k}</Td>
                        <Td><Input value={v} onChange={e=>setDraft({...draft,[k]:e.target.value})} className="font-mono text-xs h-9" spellCheck={false} autoComplete="off" /></Td>
                        <Td className="text-center">
                          <Button variant="ghost" size="sm" className="text-red-400 hover:text-red-300 hover:bg-red-500/10"
                            onClick={()=>removeKey(k)} title={`Remove ${k}`}>
                            <Icon name="trash" size={13} />
                          </Button>
                        </Td>
                      </tr>
                    ))}
                    {Object.keys(draft).length===0 && (
                      <tr><td colSpan={3}><EmptyState icon="cog" title="No config." hint="Add an entry above to create one." /></td></tr>
                    )}
                  </tbody>
                </table>
              </TableShell>

              {/* per-card pinned footer */}
              <div className="px-5 py-3 border-t border-stone bg-app/60 flex items-center justify-between gap-2">
                <span className="text-xs text-muted">Changes apply only after saving.</span>
                <Button variant="primary" onClick={saveTable} disabled={saving}>{saving ? 'Saving…' : <> <Icon name="check" size={15} /> Save </>}</Button>
              </div>
            </Card>
          ) : (
            <Card pad={false}>
              <div className="p-5">
                <div className="flex items-center gap-2 mb-3">
                  <Icon name="logs" size={15} className="text-teal" />
                  <h3 className="font-semibold text-sm">Raw JSON</h3>
                </div>
                <Textarea value={jsonText} onChange={e=>setJsonText(e.target.value)} rows={16} className="min-h-[320px]" spellCheck={false} />
              </div>
              <div className="px-5 py-3 border-t border-stone bg-app/60 flex items-center justify-between gap-2">
                <span className="text-xs text-muted">Raw JSON object — same endpoint as Table mode.</span>
                <Button variant="primary" onClick={saveJson} disabled={saving}>{saving ? 'Saving…' : <> <Icon name="check" size={15} /> Save JSON </>}</Button>
              </div>
            </Card>
          )}
        </div>
      </Section>
    </div>
  )
}
