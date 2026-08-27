import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { registerPasskey, authenticatePasskey } from '../lib/webauthn'
import {
  PageHeader, Card, Button, Input, Select, Field, Badge, Icon, CopyButton,
  TableShell, Th, Td, TableSkeleton, EmptyState, ErrorNote, Modal, Confirm, useToast,
} from '../components/ui'

type DashboardUser = {
  id:string; username:string; role:string; display_name?:string; created_at:string
  passkey_enabled:boolean; has_recovery_code:boolean; passkey_count:number; disabled:boolean
  last_login_at?: string | null
  login_count?: number
}

function ago(iso?: string | null): string {
  if (!iso) return 'never'
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d ago`
  return new Date(iso).toLocaleDateString()
}

const ROLE_TONE: Record<string, 'info' | 'good' | 'warn' | 'neutral'> = {
  admin: 'info', member: 'good', support: 'warn', readonly: 'neutral',
}

const ROLE_OPTIONS = (
  <>
    <option value="admin">admin — full access</option>
    <option value="support">support — manage providers/keys</option>
    <option value="member">member — write</option>
    <option value="readonly">readonly — view only</option>
  </>
)

function Avatar({ user }: { user: DashboardUser }) {
  return (
    <span className="w-8 h-8 rounded-full bg-gradient-to-br from-amber to-teal flex items-center justify-center text-ink text-sm font-bold shrink-0 select-none">
      {(user.display_name || user.username)[0]?.toUpperCase()}
    </span>
  )
}

export default function Users(){
  const [list, setList] = useState<DashboardUser[]>([])
  const [me, setMe] = useState<DashboardUser|null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [newUser, setNewUser] = useState({username:'', password:'', role:'support', display_name:''})
  const [recoveryCodes, setRecoveryCodes] = useState<Record<string,string>>({})
  const [showPasskeyFor, setShowPasskeyFor] = useState<string|null>(null)
  const [passkeyBusy, setPasskeyBusy] = useState(false)

  // presentation-only state: modals replacing prompt()/confirm(), per-source sticky errors
  const [createError, setCreateError] = useState('')
  const [roleTarget, setRoleTarget] = useState<DashboardUser | null>(null)
  const [roleValue, setRoleValue] = useState('support')
  const [resetTarget, setResetTarget] = useState<DashboardUser | null>(null)
  const [resetPwValue, setResetPwValue] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<DashboardUser | null>(null)
  const [accountDisableTarget, setAccountDisableTarget] = useState<DashboardUser | null>(null)
  const [passkeyDisableTarget, setPasskeyDisableTarget] = useState<string | null>(null)

  const toast = useToast()

  const load = async()=>{
    setLoading(true); setErr('')
    try{
      const [users, self] = await Promise.all([api.users.list().catch(()=>[]), api.users.me().catch(()=>null)])
      setList(Array.isArray(users)? users: [])
      setMe(self as any)
    }catch(e:any){ setErr(e.message)}
    finally{ setLoading(false)}
  }
  useEffect(()=>{ load()},[])

  const isAdmin = me?.role === 'admin' || list.find(u=>u.username==='admin')?.role==='admin' // fallback
  // Actually check me role, if not loaded, check token? For now allow if me is admin
  void isAdmin
  const canManage = me?.role === 'admin'

  const create = async()=>{
    if(!newUser.username || !newUser.password){ setCreateError('Username and password are required.'); return}
    if(newUser.password.length < 8){ setCreateError('Password must be at least 8 characters.'); return}
    setCreateError('')
    try{
      await api.users.create(newUser)
      toast.success(`Created ${newUser.username} as ${newUser.role}`)
      setNewUser({username:'', password:'', role:'support', display_name:''})
      load()
    }catch(e:any){ setCreateError(e.message || String(e)); toast.error('Could not create user')}
  }

  const updateRole = async(id:string, role:string)=>{
    try{
      await api.users.update(id, {role})
      toast.success('Role updated')
      load()
    }catch(e:any){ toast.error(e.message || 'Role update failed') }
  }

  const del = async(id:string)=>{
    try{
      await api.users.remove(id)
      toast.success('User deleted')
      setDeleteTarget(null)
      load()
    }catch(e:any){ toast.error(e.message || 'Delete failed — server-side rules may block this user') }
  }

  const toggleDisabled = async(u: DashboardUser, disabled: boolean)=>{
    try{
      await api.users.update(u.id, {disabled})
      toast.success(disabled ? `${u.username} disabled` : `${u.username} enabled`)
      setAccountDisableTarget(null)
      load()
    }catch(e:any){ toast.error(e.message || 'Update failed') }
  }

  const resetPwSubmit = async()=>{
    if(!resetTarget || !resetPwValue){ return }
    if(resetPwValue.length < 8){ toast.error('Password must be at least 8 characters'); return }
    try{
      await api.users.resetPassword(resetTarget.id, resetPwValue)
      toast.success(`Password reset for ${resetTarget.username}`)
      setResetTarget(null); setResetPwValue('')
    }catch(e:any){ toast.error(e.message || 'Password reset failed') }
  }

  const handleRegisterPasskey = async(userId:string)=>{
    setPasskeyBusy(true); setErr('')
    try{
      const begin = await api.passkey.registerBegin(userId)
      const {session, credential} = await registerPasskey(begin)
      const res:any = await api.passkey.registerFinishWithSession(session, credential)
      if(res.recovery_code){
        setRecoveryCodes(m=>({...m, [userId]: res.recovery_code}))
        toast.info('Save the recovery code shown above')
      }
      toast.success('Passkey enabled')
      load()
    }catch(e:any){ toast.error('Passkey enrollment failed: '+(e.message||String(e)))}
    finally{ setPasskeyBusy(false)}
  }

  const handleDisablePasskey = async(userId:string)=>{
    try{
      await api.passkey.disable(userId); load()
      toast.success('Passkey disabled')
    }catch(e:any){ toast.error(e.message || 'Failed to disable passkey') }
    finally{ setPasskeyDisableTarget(null) }
  }

  const genRecovery = async(userId:string)=>{
    try{
      const res:any = await api.passkey.generateRecovery(userId)
      if(res.recovery_code){
        setRecoveryCodes(m=>({...m, [userId]: res.recovery_code}))
        toast.success('New recovery code generated')
        load()
      }
    }catch(e:any){ toast.error(e.message || 'Could not generate recovery code') }
  }

  const handleLoginPasskey = async(username?:string)=>{
    setPasskeyBusy(true)
    try{
      const begin = await api.passkey.loginBegin(username)
      const {session, credential} = await authenticatePasskey(begin)
      const res:any = await api.passkey.loginFinishWithSession(session, credential)
      if(res.token){
        // Session cookie was set by the server on login finish; the JWT is
        // never stored client-side.
        window.location.reload()
      }
    }catch(e:any){ toast.error('Passkey login failed: '+(e.message||String(e)))}
    finally{ setPasskeyBusy(false)}
  }

  const soleAdminBlocked = (u: DashboardUser) =>
    u.username==='admin' && list.filter(x=>x.role==='admin').length<=1

  const dismissRecovery = (uid:string)=>{
    setRecoveryCodes(prev => {
      const next = {...prev}; delete next[uid]; return next
    })
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Users"
        description={canManage ? 'Admins can create admins and support accounts, manage roles, reset passwords and manage passkeys.' : 'View users and roles.'}
        actions={<Badge tone="neutral"><span className="tabular-nums">{list.length}</span> users</Badge>}
      />

      {err && <ErrorNote message={err} />}

      {/* show-once recovery codes */}
      {Object.entries(recoveryCodes).map(([uid, code]) => {
        const u = list.find(x=>x.id===uid)
        return (
          <div key={uid} className="rounded-xl border border-teal/30 bg-teal/10 p-4">
            <div className="flex items-start gap-3">
              <div className="w-9 h-9 rounded-lg bg-teal/15 text-teal flex items-center justify-center shrink-0">
                <Icon name="lock" size={17} />
              </div>
              <div className="min-w-0 flex-1">
                <div className="text-sm font-semibold text-teal">Recovery code for {u?.username || 'user'}</div>
                <div className="text-xs text-muted mt-0.5">Store it now — shown only once. Required if the passkey is lost.</div>
                <div className="mt-2.5 flex items-center gap-2 rounded-lg border border-teal/30 bg-graphite/60 px-3 py-2">
                  <code className="font-mono text-sm text-paper break-all min-w-0 flex-1">{code}</code>
                  <CopyButton value={code} label="Copy recovery code" />
                </div>
              </div>
              <Button variant="ghost" size="sm" title="Dismiss" onClick={()=>dismissRecovery(uid)}>
                <Icon name="x" size={14} />
              </Button>
            </div>
          </div>
        )
      })}

      {canManage && (
        <Card>
          <div className="flex items-center gap-2 mb-4">
            <Icon name="plus" size={15} className="text-teal" />
            <h3 className="font-semibold text-sm">Create user</h3>
          </div>
          {createError && <div className="mb-3"><ErrorNote message={createError} /></div>}
          <div className="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-4 gap-3">
            <Field label="Username">
              <Input value={newUser.username} onChange={e=>setNewUser({...newUser, username:e.target.value})} placeholder="jane.doe" spellCheck={false} autoComplete="off" />
            </Field>
            <Field label="Password" hint="Minimum 8 characters. Share securely — the user should change it after first login.">
              <Input type="password" value={newUser.password} onChange={e=>setNewUser({...newUser, password:e.target.value})} placeholder="••••••••" autoComplete="new-password" />
            </Field>
            <Field label="Role">
              <Select value={newUser.role} onChange={e=>setNewUser({...newUser, role:e.target.value})}>
                {ROLE_OPTIONS}
              </Select>
            </Field>
            <Field label="Display name" hint="Optional.">
              <Input value={newUser.display_name} onChange={e=>setNewUser({...newUser, display_name:e.target.value})} placeholder="Jane Doe" spellCheck={false} />
            </Field>
          </div>
          <div className="flex items-center justify-between gap-3 mt-4 pt-4 border-t border-stone">
            <p className="text-xs text-muted">Support accounts can manage providers/keys but not users. Admins can create new admins.</p>
            <Button variant="primary" onClick={create}><Icon name="plus" size={15} /> Create user</Button>
          </div>
        </Card>
      )}

      <TableShell>
        <table className="w-full text-sm min-w-[760px]">
          <thead>
            <tr>
              <Th>User</Th>
              <Th>Role</Th>
              <Th>Passkey</Th>
              <Th>Recovery</Th>
              <Th>Last login</Th>
              <Th className="text-right">Actions</Th>
            </tr>
          </thead>
          {loading ? (
            <TableSkeleton rows={4} cols={6} />
          ) : (
            <tbody>
              {list.map(u=>(
                <tr key={u.id} className={`${u.disabled?'opacity-50':''} hover:bg-stone/20 transition-colors`}>
                  <Td>
                    <div className="flex items-center gap-3 min-w-0">
                      <Avatar user={u} />
                      <div className="min-w-0">
                        <div className="flex items-center gap-1.5">
                          <span className="font-semibold truncate">{u.username}</span>
                          {u.id===me?.id && (
                            <span className="text-[10px] font-semibold uppercase tracking-wide bg-teal/15 text-teal px-1.5 py-0.5 rounded-full shrink-0">you</span>
                          )}
                          {u.disabled && <Badge tone="bad">disabled</Badge>}
                        </div>
                        {u.display_name && <div className="text-xs text-muted truncate">{u.display_name}</div>}
                      </div>
                    </div>
                  </Td>
                  <Td>
                    <Badge tone={ROLE_TONE[u.role] ?? 'neutral'} dot>{u.role}</Badge>
                  </Td>
                  <Td>
                    <div className="flex flex-col items-start gap-1.5">
                      <span className="inline-flex items-center gap-1.5" title={u.passkey_enabled ? `${u.passkey_count} passkey(s) enrolled` : 'no passkey'}>
                        <Icon name="shield" size={15} className={u.passkey_enabled ? 'text-teal' : 'text-muted/40'} />
                        {u.passkey_enabled
                          ? <span className="text-xs text-muted tabular-nums">{u.passkey_count} enrolled</span>
                          : <span className="text-xs text-muted/60">none</span>}
                      </span>
                      {u.id === me?.id ? (
                        showPasskeyFor===u.id ? (
                          <div className="flex items-center gap-1">
                            <Button size="sm" variant="primary" onClick={()=>handleRegisterPasskey(u.id)} disabled={passkeyBusy}>Enroll</Button>
                            {u.passkey_enabled && (
                              <Button size="sm" variant="ghost" className="text-red-400 hover:text-red-300 hover:bg-red-500/10"
                                onClick={()=>setPasskeyDisableTarget(u.id)}>
                                Disable
                              </Button>
                            )}
                            <button onClick={()=>setShowPasskeyFor(null)} aria-label="Close"
                              className="w-6 h-6 rounded-md flex items-center justify-center text-muted hover:text-paper hover:bg-stone/50">
                              <Icon name="x" size={12} />
                            </button>
                          </div>
                        ) : (
                          <Button size="sm" variant="ghost" onClick={()=>setShowPasskeyFor(u.id)}>
                            {u.passkey_enabled ? 'Manage' : 'Enable'}
                          </Button>
                        )
                      ) : (
                        <span className="text-[11px] text-muted leading-snug">
                          Users enroll their own passkey from their Profile page.
                        </span>
                      )}
                    </div>
                  </Td>
                  <Td>
                    {u.has_recovery_code ? (
                      <Badge tone="warn">set</Badge>
                    ) : (
                      <span className="text-xs text-muted/60">—</span>
                    )}
                    {u.passkey_enabled && (
                      <Button size="sm" variant="ghost" className="mt-1 -ml-2" onClick={()=>genRecovery(u.id)}
                        title="Generate a new recovery code (invalidates the previous one)">
                        New code
                      </Button>
                    )}
                  </Td>
                  <Td>
                    <div className="text-xs text-muted whitespace-nowrap" title={u.last_login_at ? new Date(u.last_login_at).toLocaleString() : undefined}>
                      {ago(u.last_login_at)}
                    </div>
                    {typeof u.login_count === 'number' && (
                      <div className="text-[11px] text-muted/70 tabular-nums mt-0.5">{u.login_count} logins</div>
                    )}
                  </Td>
                  <Td>
                    <div className="flex items-center justify-end gap-1">
                      {canManage && (
                        <Button size="sm" variant="secondary" onClick={()=>{ setRoleTarget(u); setRoleValue(u.role) }} title="Edit role">
                          <Icon name="userCog" size={14} /> Role
                        </Button>
                      )}
                      <Button size="sm" variant="ghost" onClick={()=>{ setResetTarget(u); setResetPwValue('') }} title="Reset password">
                        <Icon name="key" size={14} />
                      </Button>
                      {canManage && (
                        u.disabled ? (
                          <Button size="sm" variant="ghost" onClick={()=>toggleDisabled(u, false)} title="Enable account">Enable</Button>
                        ) : (
                          <Button size="sm" variant="ghost" onClick={()=>setAccountDisableTarget(u)} title="Disable account">Disable</Button>
                        )
                      )}
                      <Button size="sm" variant="ghost" className="text-red-400 hover:text-red-300 hover:bg-red-500/10"
                        onClick={()=>setDeleteTarget(u)} disabled={soleAdminBlocked(u)}
                        title={soleAdminBlocked(u) ? 'Cannot delete the last remaining admin' : 'Delete user'}>
                        <Icon name="trash" size={14} />
                      </Button>
                    </div>
                  </Td>
                </tr>
              ))}
              {list.length===0 && (
                <tr><td colSpan={6}>
                  <EmptyState icon="users" title="No users yet" hint={canManage ? 'Create an admin or support account above.' : 'No accounts exist yet.'} />
                </td></tr>
              )}
            </tbody>
          )}
        </table>
        <div className="px-4 py-3 border-t border-stone bg-app/60 text-[11px] text-muted leading-relaxed">
          RBAC: <span className="text-amber font-medium">admin</span> = full access · <span className="text-teal font-medium">support / member</span> = providers &amp; keys · readonly = view only.
          Passkeys use WebAuthn — the private key stays on device; a recovery code is required if the passkey is lost.
        </div>
      </TableShell>

      <Card>
        <div className="flex items-center gap-2">
          <Icon name="zap" size={15} className="text-teal" />
          <h3 className="font-semibold text-sm">Passkey quick test</h3>
        </div>
        <p className="text-xs text-muted mt-1.5 max-w-2xl">Try passkey login for your own account — requires enrollment above and browser support. The public key stays on device; the recovery code bypasses it if lost.</p>
        <div className="mt-3">
          <Button variant="secondary" onClick={()=>handleLoginPasskey(me?.username)} disabled={passkeyBusy}>
            {passkeyBusy ? 'Waiting for authenticator…' : <> <Icon name="play" size={13} /> Login with passkey </>}
          </Button>
        </div>
      </Card>

      {/* edit-role modal */}
      <Modal open={roleTarget !== null} onClose={()=>setRoleTarget(null)} title={`Edit role — ${roleTarget?.username ?? ''}`} width="max-w-sm">
        <Field label="Role" hint="Applies to the user's next request. Admins can create admins.">
          <Select value={roleValue} onChange={e=>setRoleValue(e.target.value)}>
            {ROLE_OPTIONS}
          </Select>
        </Field>
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={()=>setRoleTarget(null)}>Cancel</Button>
          <Button variant="primary" onClick={()=>{ const t = roleTarget; setRoleTarget(null); if(t) updateRole(t.id, roleValue) }}>
            Save role
          </Button>
        </div>
      </Modal>

      {/* reset-password modal (replaces prompt()) */}
      <Modal open={resetTarget !== null}
        onClose={()=>{ setResetTarget(null); setResetPwValue('') }}
        title={`Reset password — ${resetTarget?.username ?? ''}`} width="max-w-sm">
        <Field label="New password" hint="At least 8 characters required. Share securely — the user should change it after logging in.">
          <Input type="password" value={resetPwValue} onChange={e=>setResetPwValue(e.target.value)} autoFocus autoComplete="new-password" placeholder="••••••••"
            onKeyDown={e=>{ if(e.key==='Enter' && resetPwValue.length >= 8) resetPwSubmit() }} />
        </Field>
        {resetPwValue.length > 0 && (
          <p className={`text-xs mt-2 ${resetPwValue.length >= 8 ? 'text-teal' : 'text-amber'}`}>
            {resetPwValue.length >= 8 ? 'Strong enough — 8+ characters.' : 'Too short — 8 characters minimum.'}
          </p>
        )}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={()=>{ setResetTarget(null); setResetPwValue('') }}>Cancel</Button>
          <Button variant="primary" disabled={!resetPwValue || resetPwValue.length < 8} onClick={resetPwSubmit}>Reset password</Button>
        </div>
      </Modal>

      {/* destructive: delete user */}
      <Confirm
        open={deleteTarget !== null}
        onClose={()=>setDeleteTarget(null)}
        onConfirm={()=>{ if(deleteTarget) del(deleteTarget.id) }}
        title={`Delete ${deleteTarget?.username ?? 'user'}?`}
        confirmLabel="Delete user"
        body={`This permanently removes ${deleteTarget?.username ?? 'this user'} and revokes their sessions. Requests authenticated as them will start failing immediately.`}
      />

      {/* destructive: disable account */}
      <Confirm
        open={accountDisableTarget !== null}
        onClose={()=>setAccountDisableTarget(null)}
        onConfirm={()=>{ if(accountDisableTarget) toggleDisabled(accountDisableTarget, true) }}
        title={accountDisableTarget?.id === me?.id ? 'Disable your own account?' : `Disable ${accountDisableTarget?.username ?? 'user'}?`}
        confirmLabel="Disable"
        body={
          accountDisableTarget?.id === me?.id
            ? 'You are disabling your OWN account — you will be locked out until another admin re-enables you.'
            : `${accountDisableTarget?.username ?? 'This user'} will be unable to sign in. Requests they authenticate will start failing until the account is re-enabled.`
        }
      />

      {/* destructive: disable passkey */}
      <Confirm
        open={passkeyDisableTarget !== null}
        onClose={()=>setPasskeyDisableTarget(null)}
        onConfirm={()=>{ if(passkeyDisableTarget) handleDisablePasskey(passkeyDisableTarget) }}
        title="Disable passkey?"
        confirmLabel="Disable passkey"
        body="This removes passkey sign-in for this user — they will need their password again, plus recovery-code handling if they lose access."
      />
    </div>
  )
}
