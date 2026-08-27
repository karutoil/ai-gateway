import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  Badge, Button, Card, Confirm, CopyButton, EmptyState, ErrorNote, Field,
  HealthDot, Icon, Input, PageHeader, Select, Skeleton, useToast,
} from '../components/ui'

function healthTextCls(status?: string | null): string {
  if (status === 'up') return 'text-teal'
  if (status === 'down') return 'text-red-400'
  return 'text-muted'
}

export default function Providers(){
  const [list, setList] = useState<any[]>([])
  const [name, setName] = useState('')
  const [type, setType] = useState('openai')
  const [base, setBase] = useState('')
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')

  // Presentation-only: skeleton grid, delete confirmation.
  const [loading, setLoading] = useState(true)
  const [confirmTarget, setConfirmTarget] = useState<any>(null)
  const [deleting, setDeleting] = useState(false)
  const toast = useToast()

  const load = () => api.providers.list()
    .then(setList)
    .catch(()=>{})
    .finally(()=> setLoading(false))
  useEffect(()=>{ load() },[])

  const create = async ()=>{
    try{
      setErr('')
      await api.providers.create({ name, type, base_url: base, api_key: key })
      setName(''); setKey(''); setBase('')
      toast.success('Provider added')
      load()
    } catch(e:any){
      setErr(e.message)
      toast.error(e.message || String(e))
    }
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

      {/* Create form */}
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
            </Select>
          </Field>
          <Field label="Base URL" hint="Optional. Leave blank to use the provider's official endpoint.">
            <Input placeholder="https://api.example.com/v1" value={base} onChange={e=>setBase(e.target.value)} />
          </Field>
          <Field label="API Key">
            <Input placeholder="sk-..." value={key} onChange={e=>setKey(e.target.value)} type="password" autoComplete="off" />
          </Field>
        </div>
        {err && <div className="mt-3"><ErrorNote message={err} /></div>}
        <div className="mt-4">
          <Button variant="primary" onClick={create}>
            <Icon name="plus" size={15} /> Add Provider
          </Button>
        </div>
      </Card>

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

            <div className="mt-auto pt-3 flex justify-end">
              <Button variant="ghost" size="sm"
                title={`Delete ${p.name}`}
                onClick={()=>setConfirmTarget(p)}>
                <Icon name="trash" size={14} /> Remove
              </Button>
            </div>
          </Card>
        ))}
        {!loading && list.length===0 && (
          <div className="col-span-full">
            <Card><EmptyState
              icon="server"
              title="No providers yet."
              hint="Add your first upstream provider above. Models are discovered from it on the Models page."
            /></Card>
          </div>
        )}
      </div>

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
    </div>
  )
}
