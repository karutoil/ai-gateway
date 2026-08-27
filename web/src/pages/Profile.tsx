import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import { registerPasskey } from '../lib/webauthn'
import {
  PageHeader, Card, Button, Input, Field, Badge, Icon, CopyButton,
  Skeleton, EmptyState, ErrorNote, Confirm, useToast,
} from '../components/ui'

type Me = { id:string; username:string; role:string; display_name?:string; created_at:string; last_login_at?:string; login_count:number; passkey_enabled:boolean; has_recovery_code:boolean; passkey_count:number }

const ROLE_TONE: Record<string, 'info' | 'good' | 'warn' | 'neutral'> = {
  admin: 'info', member: 'good', support: 'warn', readonly: 'neutral',
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

export default function Profile(){
  const [me, setMe] = useState<Me|null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')
  const [confirmPw, setConfirmPw] = useState('')
  const [creds, setCreds] = useState<any[]>([])
  const [activity, setActivity] = useState<any[]>([])
  const [logins, setLogins] = useState<any>(null)
  const [recovery, setRecovery] = useState<string|null>(null)
  const [busy, setBusy] = useState(false)

  // presentation-only state: sticky per-card errors replacing the old global status line
  const [accountError, setAccountError] = useState('')
  const [pwError, setPwError] = useState('')
  const [confirmDisable, setConfirmDisable] = useState(false)

  const toast = useToast()

  const load = async()=>{
    setLoading(true); setErr('')
    try{
      const [u, c, a, l] = await Promise.all([
        api.profile.get().catch(()=> api.users.me()),
        api.passkey.list().catch(()=>[]),
        api.profile.activity().catch(()=>[]),
        api.profile.logins().catch(()=>null),
      ])
      setMe(u as Me)
      setDisplayName((u as any).display_name || '')
      setCreds(Array.isArray(c)? c: [])
      setActivity(Array.isArray(a)? a: [])
      setLogins(l)
    }catch(e:any){ setErr(e.message)}
    finally{ setLoading(false)}
  }
  useEffect(()=>{ load()},[])

  const saveDisplayName = async()=>{
    setAccountError('')
    try{
      await api.profile.update({ display_name: displayName })
      toast.success('Display name saved')
      load()
    }catch(e:any){ setAccountError(e.message || String(e)); toast.error('Could not save display name')}
  }

  const changePassword = async()=>{
    if(newPw !== confirmPw){ setPwError('Passwords do not match.'); return}
    if(newPw.length < 4){ setPwError('New password must be at least 4 characters.'); return}
    setPwError('')
    try{
      await api.profile.changePassword(oldPw, newPw)
      toast.success('Password changed')
      setOldPw(''); setNewPw(''); setConfirmPw('')
    }catch(e:any){ setPwError(e.message || String(e)); toast.error('Could not change password')}
  }

  const enroll = async()=>{
    setBusy(true); setErr('')
    try{
      const begin:any = await api.passkey.registerBegin()
      const {session, credential} = await registerPasskey(begin)
      const res:any = await api.passkey.registerFinishWithSession(session, credential)
      if(res.recovery_code){
        setRecovery(res.recovery_code)
        toast.success('Passkey enabled')
      } else toast.success('Passkey enabled')
      load()
    }catch(e:any){ toast.error('Enroll failed: '+(e.message||String(e)))}
    finally{ setBusy(false)}
  }

  const disable = async()=>{
    try{
      await api.passkey.disable(); load(); toast.success('Passkey disabled')
    }catch(e:any){ toast.error(e.message || 'Failed to disable passkey') }
    finally{ setConfirmDisable(false) }
  }

  const genRecovery = async()=>{
    try{
      const res:any = await api.passkey.generateRecovery()
      if(res.recovery_code){ setRecovery(res.recovery_code); toast.success('New recovery code generated'); load()}
    }catch(e:any){ toast.error(e.message || 'Could not generate recovery code') }
  }

  if(loading) return (
    <div className="space-y-6 max-w-4xl">
      <PageHeader title="Profile" description="Your account, security and recent gateway activity." />
      <div className="grid md:grid-cols-3 gap-4">
        <Card className="md:col-span-1 space-y-3">
          <Skeleton className="h-16 w-16 rounded-full mx-auto" />
          <Skeleton className="h-4 w-28 mx-auto" />
          <Skeleton className="h-3 w-36 mx-auto" />
          <Skeleton className="h-10 w-full mt-4" />
        </Card>
        <Card className="md:col-span-2 space-y-3">
          {[0,1,2].map(i => <Skeleton key={i} className="h-10 w-full" />)}
          <Skeleton className="h-10 w-32" />
        </Card>
      </div>
      <Card className="space-y-3">{[0,1,2].map(i => <Skeleton key={i} className="h-4 w-full" />)}</Card>
    </div>
  )
  if(!me) return (
    <div className="space-y-6 max-w-4xl">
      <PageHeader title="Profile" description="Your account, security and recent gateway activity." />
      <ErrorNote message={err || 'Profile not found'} />
    </div>
  )

  return (
    <div className="space-y-6 max-w-4xl">
      <PageHeader title="Profile" description="Your account, security and recent gateway activity." />

      {/* show-once recovery code */}
      {recovery && (
        <div className="rounded-xl border border-teal/30 bg-teal/10 p-4">
          <div className="flex items-start gap-3">
            <div className="w-9 h-9 rounded-lg bg-teal/15 text-teal flex items-center justify-center shrink-0">
              <Icon name="lock" size={17} />
            </div>
            <div className="min-w-0 flex-1">
              <div className="text-sm font-semibold text-teal">Recovery code</div>
              <div className="text-xs text-muted mt-0.5">Store it now — shown only once. Required if your passkey is lost.</div>
              <div className="mt-2.5 flex items-center gap-2 rounded-lg border border-teal/30 bg-graphite/60 px-3 py-2">
                <code className="font-mono text-sm text-paper break-all min-w-0 flex-1">{recovery}</code>
                <CopyButton value={recovery} label="Copy recovery code" />
              </div>
            </div>
            <Button variant="ghost" size="sm" onClick={()=>setRecovery(null)} title="Dismiss"><Icon name="x" size={14} /></Button>
          </div>
        </div>
      )}

      <div className="grid md:grid-cols-3 gap-4">
        {/* identity card */}
        <Card className="md:col-span-1">
          <div className="flex flex-col items-center text-center pb-4 border-b border-stone">
            <span className="w-16 h-16 rounded-full bg-gradient-to-br from-amber to-teal flex items-center justify-center text-ink font-bold text-2xl select-none">
              {me.username[0]?.toUpperCase()}
            </span>
            <div className="font-semibold mt-3">{me.username}</div>
            {me.display_name && <div className="text-sm text-muted mt-0.5">{me.display_name}</div>}
            <div className="mt-2"><Badge tone={ROLE_TONE[me.role] ?? 'neutral'} dot>{me.role}</Badge></div>
            <div className="text-xs text-muted mt-2">member since {new Date(me.created_at).toLocaleDateString()}</div>
          </div>
          <div className="pt-4">
            <Field label="Display name">
              <Input value={displayName} onChange={e=>setDisplayName(e.target.value)} placeholder="Display name" spellCheck={false}
                onKeyDown={e=>{ if(e.key==='Enter') saveDisplayName() }} />
            </Field>
            {accountError && <div className="mt-2"><ErrorNote message={accountError} /></div>}
            <Button variant="secondary" size="sm" className="mt-3 w-full" onClick={saveDisplayName}>
              <Icon name="check" size={14} /> Save display name
            </Button>
          </div>
        </Card>

        {/* change password card */}
        <Card className="md:col-span-2">
          <div className="flex items-center gap-2 mb-4">
            <Icon name="lock" size={15} className="text-teal" />
            <h3 className="font-semibold text-sm">Change password</h3>
          </div>
          {pwError && <div className="mb-3"><ErrorNote message={pwError} /></div>}
          <div className="grid sm:grid-cols-3 gap-3">
            <Field label="Current password">
              <Input type="password" value={oldPw} onChange={e=>setOldPw(e.target.value)} placeholder="••••••••" autoComplete="current-password" />
            </Field>
            <Field label="New password">
              <Input type="password" value={newPw} onChange={e=>setNewPw(e.target.value)} placeholder="••••••••" autoComplete="new-password" />
            </Field>
            <Field label="Confirm new" hint="Minimum 4 characters.">
              <Input type="password" value={confirmPw} onChange={e=>setConfirmPw(e.target.value)} placeholder="••••••••" autoComplete="new-password"
                onKeyDown={e=>{ if(e.key==='Enter') changePassword() }} />
            </Field>
          </div>
          <div className="flex justify-end pt-4 border-t border-stone mt-4">
            <Button variant="primary" onClick={changePassword}><Icon name="lock" size={15} /> Change password</Button>
          </div>
        </Card>
      </div>

      <div className="grid md:grid-cols-3 gap-4">
        {/* passkeys card */}
        <Card>
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2">
              <Icon name="shield" size={15} className={me.passkey_enabled ? 'text-teal' : 'text-muted'} />
              <h3 className="font-semibold text-sm">Passkeys</h3>
            </div>
            <Badge tone={me.passkey_enabled ? 'good' : 'neutral'} dot={me.passkey_enabled}>
              {me.passkey_enabled ? `${me.passkey_count} enabled` : 'disabled'}
            </Badge>
          </div>

          {me.passkey_enabled && !me.has_recovery_code && (
            <div className="mb-3 flex items-start gap-2 rounded-lg border border-amber/30 bg-amber/10 px-3 py-2 text-xs text-amber">
              <Icon name="alert" size={14} className="mt-0.5 shrink-0" />
              <span>Generate a recovery code — required if your passkey is forgotten.</span>
            </div>
          )}

          <div className="flex flex-wrap gap-2">
            <Button variant="primary" size="sm" onClick={enroll} disabled={busy}>
              {busy ? 'Waiting…' : <> <Icon name="shield" size={14} /> Enable passkey </>}
            </Button>
            {me.passkey_enabled && (
              <Button variant="ghost" size="sm" className="text-red-400 hover:text-red-300 hover:bg-red-500/10" onClick={()=>setConfirmDisable(true)}>
                Disable
              </Button>
            )}
            <Button variant="secondary" size="sm" onClick={genRecovery}>New recovery code</Button>
          </div>

          {creds.length>0 && (
            <div className="mt-4 rounded-lg border border-stone overflow-hidden">
              <div className="px-3 py-2 border-b border-stone bg-app/60 text-[11px] font-medium uppercase tracking-wide text-muted tabular-nums">
                {creds.length} credential(s)
              </div>
              {creds.map((c:any,i:number)=>(
                <div key={i} className="px-3 py-2 flex justify-between gap-2 border-b border-stone/40 last:border-b-0 font-mono text-xs">
                  <span className="truncate">{String(c.id).slice(0,16)}…</span>
                  <span className="text-muted shrink-0">{(c.transports||[]).join(',')}</span>
                </div>
              ))}
            </div>
          )}

          <p className="text-xs text-muted mt-3 leading-relaxed">Passkeys use WebAuthn — the private key never leaves this device. A recovery code bypasses the passkey if it&apos;s lost.</p>
        </Card>

        {/* activity timeline card */}
        <Card className="md:col-span-2">
          <div className="flex items-center justify-between mb-4">
            <div className="flex items-center gap-2">
              <Icon name="pulse" size={15} className="text-teal" />
              <h3 className="font-semibold text-sm">Recent activity</h3>
            </div>
            <Badge tone="neutral"><span className="tabular-nums">{activity.length}</span> events</Badge>
          </div>

          {activity.length===0 ? (
            <EmptyState icon="pulse" title="No recent activity." hint="Actions you take across the gateway appear here." />
          ) : (
            <ul className="relative">
              {/* left rail */}
              <div className="absolute left-[5px] top-2 bottom-2 w-px bg-stone" aria-hidden />
              {activity.map((a:any)=>(
                <li key={a.id} className="relative pl-6 pb-4 last:pb-0">
                  <span className="absolute left-0 top-1 w-[11px] h-[11px] rounded-full bg-teal ring-4 ring-surface" aria-hidden />
                  <div className="flex flex-wrap items-baseline justify-between gap-x-3">
                    <span className="text-sm font-medium">{a.action}</span>
                    <span className="text-xs text-muted whitespace-nowrap">{ago(a.created_at)}</span>
                  </div>
                  <div className="font-mono text-xs text-muted truncate mt-0.5" title={`${a.target_type}:${a.target_id}`}>
                    {a.target_type}:{a.target_id}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>

      {/* logins card */}
      {logins && (
        <Card>
          <div className="flex items-center gap-2 mb-4">
            <Icon name="logout" size={15} className="text-teal" />
            <h3 className="font-semibold text-sm">Logins</h3>
          </div>
          <div className="grid grid-cols-3 gap-3">
            <div className="rounded-lg bg-app border border-stone p-3">
              <div className="text-[11px] font-medium uppercase tracking-wide text-muted">Total logins</div>
              <div className="text-xl font-semibold tabular-nums mt-1">{logins.login_count ?? me.login_count}</div>
            </div>
            <div className="rounded-lg bg-app border border-stone p-3">
              <div className="text-[11px] font-medium uppercase tracking-wide text-muted">Last login</div>
              <div className="text-sm font-medium mt-1.5 whitespace-nowrap" title={logins.last_login_at ? new Date(logins.last_login_at).toLocaleString() : undefined}>
                {ago(logins.last_login_at)}
              </div>
              {logins.last_login_at && <div className="text-[11px] text-muted mt-0.5">{new Date(logins.last_login_at).toLocaleString()}</div>}
            </div>
            <div className="rounded-lg bg-app border border-stone p-3">
              <div className="text-[11px] font-medium uppercase tracking-wide text-muted">Joined</div>
              <div className="text-sm font-medium mt-1.5">{new Date(logins.created_at ?? me.created_at).toLocaleDateString()}</div>
            </div>
          </div>
        </Card>
      )}

      {/* destructive confirmation */}
      <Confirm
        open={confirmDisable}
        onClose={()=>setConfirmDisable(false)}
        onConfirm={disable}
        title="Disable passkey?"
        confirmLabel="Disable"
        body="Disable passkey sign-in? Your existing recovery code remains valid until regenerated — after this you will need your password to sign in."
      />
    </div>
  )
}
