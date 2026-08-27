import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import {
  Card, Button, Badge, Icon, Input, Select, Field, PageHeader, SegmentedControl,
  TableShell, Th, Td, TableSkeleton, EmptyState, ErrorNote, Modal, Skeleton,
  CopyButton, useToast,
} from '../components/ui'

type Role = 'admin' | 'member' | 'readonly'
type Org = { id: string; name: string; created_at: string }
type Member = { id: string; org_id: string; user_id: string; role: Role; created_at: string }

const roleTone: Record<Role, 'brand' | 'good' | 'neutral'> = {
  admin: 'brand',
  member: 'good',
  readonly: 'neutral',
}

export default function Teams({ role = 'admin' }: { role?: string }) {
  // Org writes (create org, add member) are admin-only server-side.
  const isAdmin = role === 'admin'
  const toast = useToast()
  const [orgs, setOrgs] = useState<Org[]>([])
  const [activeId, setActiveId] = useState<string>('')
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [newOrgName, setNewOrgName] = useState('')
  const [creating, setCreating] = useState(false)
  const [members, setMembers] = useState<Member[]>([])
  const [membersLoading, setMembersLoading] = useState(false)
  const [membersErr, setMembersErr] = useState('')
  const [directory, setDirectory] = useState<{ id: string; username: string; role: string }[]>([])
  const [directoryLoading, setDirectoryLoading] = useState(false)
  const [inviteUserId, setInviteUserId] = useState('')
  const [inviteRole, setInviteRole] = useState<Role>('member')
  const [inviting, setInviting] = useState(false)
  const [filterRole, setFilterRole] = useState<Role | 'all'>('all')

  // Modals (presentation)
  const [createOpen, setCreateOpen] = useState(false)
  const [createErr, setCreateErr] = useState('')
  const [inviteOpen, setInviteOpen] = useState(false)
  const [inviteErr, setInviteErr] = useState('')

  const active = orgs.find(o => o.id === activeId) ?? orgs[0]
  const loadOrgs = async () => {
    setLoading(true); setErr('')
    try {
      const data = await api.orgs.list()
      const list: Org[] = Array.isArray(data) ? data : data?.data ?? []
      setOrgs(list)
      if (list.length > 0 && !activeId) setActiveId(list[0].id)
      else if (list.length === 0) setActiveId('')
      else if (activeId && !list.find(o => o.id === activeId)) setActiveId(list[0].id)
    } catch (e:any){ setErr(e.message||String(e)) } finally{ setLoading(false) }
  }
  const loadMembers = async (orgId: string) => {
    if (!orgId) { setMembers([]); return }
    setMembersLoading(true); setMembersErr('')
    try {
      const data = await api.orgs.members(orgId)
      setMembers(Array.isArray(data) ? data : data?.data ?? [])
    } catch(e:any){ setMembersErr(e.message||String(e)); setMembers([]) } finally{ setMembersLoading(false)}
  }
  useEffect(()=>{ loadOrgs()},[])
  useEffect(()=>{ if(activeId) loadMembers(activeId); else if(orgs.length===0) setMembers([])},[activeId])
  useEffect(()=>{ if(orgs.length>0 && !activeId) setActiveId(orgs[0].id)},[orgs])

  const createOrg = async () => {
    const name=newOrgName.trim()
    if(!name){ setCreateErr('Name required'); return }
    if(name.length>64){ setCreateErr('Too long — organization names are limited to 64 characters'); return }
    setCreating(true); setCreateErr('')
    try{
      const created:Org=await api.orgs.create(name)
      toast.success(`Organization "${created.name}" created`)
      setNewOrgName('')
      await loadOrgs()
      if(created?.id) setActiveId(created.id)
      setCreateOpen(false)
    }catch(e:any){ setCreateErr(e.message||String(e)) } finally{setCreating(false)}
  }
  const addMember = async () => {
    const uid=inviteUserId
    if(!uid){ setInviteErr('Select a user to invite'); return }
    if(!activeId){ setInviteErr('Select an organization first'); return }
    setInviting(true); setInviteErr('')
    try{
      // Send the existing user's id — the backend validates existence and
      // rejects duplicate membership.
      await api.orgs.addMember(activeId, uid, inviteRole)
      const label = directory.find(u=>u.id===uid)?.username || uid.slice(0,8)
      toast.success(`Invited ${label}`)
      setInviteUserId('')
      await loadMembers(activeId)
      setInviteOpen(false)
    }catch(e:any){ setInviteErr(e.message||String(e)) }finally{ setInviting(false) }
  }

  // Load the user directory for the invite dropdown (admin-only endpoint).
  const openInvite = () => {
    setInviteUserId(''); setInviteRole('member'); setInviteErr('')
    setInviteOpen(true)
    if (directory.length > 0 || directoryLoading) return
    setDirectoryLoading(true)
    api.users.list()
      .then((data:any)=>{
        const rows = Array.isArray(data) ? data : data?.data ?? []
        setDirectory(rows.map((u:any)=>({ id: u.id, username: u.username || u.id, role: u.role || '' })))
      })
      .catch((e:any)=> setInviteErr(e?.message || 'Could not load user list'))
      .finally(()=> setDirectoryLoading(false))
  }

  const filtered = filterRole==='all'? members : members.filter(m=>m.role===filterRole)

  const openCreate = ()=>{ setNewOrgName(''); setCreateErr(''); setCreateOpen(true) }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Teams"
        description="Organizations, members and their roles across the gateway."
        actions={isAdmin ? (
          <Button variant="primary" onClick={openCreate} disabled={loading}>
            <Icon name="plus" size={15}/>New organization
          </Button>
        ) : undefined}
      />

      {err && (
        <Card>
          <EmptyState
            icon="alert"
            title="Could not load organizations"
            hint={err}
            action={<Button variant="secondary" onClick={loadOrgs}><Icon name="refresh" size={15}/>Retry</Button>}
          />
        </Card>
      )}

      {!err && loading ? (
        <Card className="space-y-2">
          <Skeleton className="h-12 w-full"/>
          <Skeleton className="h-12 w-3/4"/>
          <Skeleton className="h-12 w-5/6"/>
        </Card>
      ) : orgs.length===0 ? (
        <EmptyState
          icon="users"
          title="No organizations yet"
          hint={isAdmin ? 'Create your first organization, then invite teammates from the user directory.' : 'No organizations exist yet — ask an admin to create one.'}
          action={isAdmin ? <Button variant="primary" onClick={openCreate}><Icon name="plus" size={15}/>New organization</Button> : undefined}
        />
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-[300px_minmax(0,1fr)] gap-4 items-start">
          {/* Organization selector cards */}
          <div className="flex lg:flex-col gap-2 overflow-x-auto pb-1">
            {orgs.map(o=>{
              const isActive = o.id === active?.id
              return (
                <button key={o.id} onClick={()=>setActiveId(o.id)}
                  className={`shrink-0 lg:w-full min-w-[200px] text-left rounded-xl border px-4 py-3 transition-all duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50 ${
                    isActive ? 'border-teal/60 bg-teal/10' : 'border-stone bg-raised/40 hover:border-paper/30 hover:bg-raised'
                  }`}>
                  <div className="flex items-center justify-between gap-3">
                    <div className="min-w-0">
                      <div className={`text-sm truncate ${isActive?'font-semibold':'font-medium'}`}>{o.name}</div>
                      <div className="text-xs text-muted mt-0.5 tabular-nums">
                        {isActive
                          ? `${members.length} member${members.length===1?'':'s'}`
                          : o.created_at ? `since ${new Date(o.created_at).toLocaleDateString()}` : 'organization'}
                      </div>
                    </div>
                    {isActive && <Icon name="check" size={15} className="text-teal shrink-0"/>}
                  </div>
                </button>
              )
            })}
          </div>

          {/* Members */}
          <div className="min-w-0 space-y-3">
            <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
              <div>
                <h2 className="text-sm font-semibold tracking-tight">Members</h2>
                <p className="text-xs text-muted mt-0.5">{active?.name}</p>
              </div>
              <div className="flex items-center gap-2 flex-wrap">
                <SegmentedControl<Role|'all'>
                  options={[{value:'all',label:'All'},{value:'admin',label:'Admin'},{value:'member',label:'Member'},{value:'readonly',label:'Readonly'}]}
                  value={filterRole}
                  onChange={setFilterRole}
                />
                {isAdmin && (
                  <Button variant="secondary" size="sm" disabled={!activeId}
                    title={activeId? 'Add a member to this organization':'Select an organization first'}
                    onClick={openInvite}>
                    <Icon name="userCog" size={14}/>Invite member
                  </Button>
                )}
              </div>
            </div>

            {membersErr && <ErrorNote message={membersErr}/>}

            <TableShell>
              <thead>
                <tr><Th>User</Th><Th>Role</Th><Th>Joined</Th></tr>
              </thead>
              {membersLoading ? (
                <TableSkeleton rows={5} cols={3}/>
              ) : !active ? (
                <tbody><tr><td colSpan={3}><EmptyState icon="users" title="No organization selected"/></td></tr></tbody>
              ) : (
                <tbody>
                  {filtered.map(m=>(
                    <tr key={m.id}>
                      <Td>
                        <span className="inline-flex items-center gap-1 max-w-[260px]">
                          <span className="font-mono text-xs truncate" title={m.user_id}>{m.user_id}</span>
                          <CopyButton value={m.user_id} label="Copy user id"/>
                        </span>
                      </Td>
                      <Td><Badge tone={roleTone[m.role]}>{m.role}</Badge></Td>
                      <Td className="text-xs text-muted tabular-nums whitespace-nowrap">{m.created_at? new Date(m.created_at).toLocaleDateString():'—'}</Td>
                    </tr>
                  ))}
                  {filtered.length===0 && (
                    <tr><td colSpan={3}>
                      <EmptyState icon="users"
                        title={filterRole==='all' ? 'No members yet' : `No ${filterRole}s`}
                        hint={filterRole==='all'
                          ? 'Invite teammates by user id or email to get started.'
                          : `Nobody has the "${filterRole}" role in this organization.`}/>
                    </td></tr>
                  )}
                </tbody>
              )}
            </TableShell>
          </div>
        </div>
      )}

      {/* Create organization */}
      <Modal open={createOpen} onClose={()=>{ if(!creating) setCreateOpen(false) }} title="New organization">
        <Field label="Organization name" hint="Up to 64 characters.">
          <Input autoFocus value={newOrgName} placeholder="e.g. acme-prod"
            onChange={e=>setNewOrgName(e.target.value)}
            onKeyDown={e=>{ if(e.key==='Enter') createOrg() }}/>
        </Field>
        {createErr && <div className="mt-3"><ErrorNote message={createErr}/></div>}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={()=>setCreateOpen(false)} disabled={creating}>Cancel</Button>
          <Button variant="primary" onClick={createOrg} disabled={creating || !newOrgName.trim()}>
            {creating ? 'Creating…' : 'Create organization'}
          </Button>
        </div>
      </Modal>

      {/* Invite member */}
      <Modal open={inviteOpen} onClose={()=>{ if(!inviting) setInviteOpen(false) }} title={`Invite member${active ? ` — ${active.name}` : ''}`}>
        <Field label="User" hint={directoryLoading ? 'Loading users…' : 'Only existing accounts can be invited — duplicates are rejected.'}>
          <Select autoFocus value={inviteUserId} onChange={e=>setInviteUserId(e.target.value)} disabled={directoryLoading}>
            <option value="">{directoryLoading ? 'Loading…' : 'Select a user…'}</option>
            {directory.map(u=> (
              <option key={u.id} value={u.id}>{u.username}{u.role ? ` (${u.role})` : ''}</option>
            ))}
          </Select>
        </Field>
        <Field label="Role" className="mt-4"
          hint="Admin manages providers & teams. Member can use the gateway. Readonly is view-only.">
          <Select value={inviteRole} onChange={e=>setInviteRole(e.target.value as Role)}>
            <option value="admin">admin</option>
            <option value="member">member</option>
            <option value="readonly">readonly</option>
          </Select>
        </Field>
        {inviteErr && <div className="mt-3"><ErrorNote message={inviteErr}/></div>}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={()=>setInviteOpen(false)} disabled={inviting}>Cancel</Button>
          <Button variant="primary" onClick={addMember} disabled={inviting || !inviteUserId}>
            {inviting ? 'Inviting…' : 'Send invite'}
          </Button>
        </div>
      </Modal>
    </div>
  )
}
