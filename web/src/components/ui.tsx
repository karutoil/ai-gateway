/**
 * Gateway Design System — shared UI primitives.
 *
 * Conventions for every page:
 *  - Import { Card, Button, Input, ... } from '../components/ui' (or './ui').
 *  - Never hand-roll raw <input>/<button> styling; compose primitives instead
 *    so spacing/focus/motion stay consistent app-wide.
 *  - Feedback goes through useToast(); destructive actions go through Confirm.
 *  - Loading lists/tables render <Skeleton/> rows; empty results render
 *    <EmptyState/>.
 */
import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { create } from 'zustand'

/* ------------------------------------------------------------------ */
/* Icons — inline stroke SVGs (feather-style); no runtime dependency. */
/* ------------------------------------------------------------------ */

export type IconName =
  | 'pulse' | 'chart' | 'server' | 'box' | 'route' | 'key' | 'users' | 'userCog'
  | 'play' | 'logs' | 'cog' | 'sun' | 'moon' | 'x' | 'chevronDown' | 'chevronLeft'
  | 'chevronRight' | 'plus' | 'trash' | 'pencil' | 'copy' | 'check' | 'alert'
  | 'search' | 'refresh' | 'menu' | 'logout' | 'shield' | 'zap' | 'lock' | 'external'

const paths: Record<IconName, ReactNode> = {
  pulse: <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />,
  chart: (
    <>
      <line x1="18" y1="20" x2="18" y2="10" /><line x1="12" y1="20" x2="12" y2="4" />
      <line x1="6" y1="20" x2="6" y2="14" />
    </>
  ),
  server: (
    <>
      <rect x="2" y="2" width="20" height="8" rx="2" /><rect x="2" y="14" width="20" height="8" rx="2" />
      <line x1="6" y1="6" x2="6.01" y2="6" /><line x1="6" y1="18" x2="6.01" y2="18" />
    </>
  ),
  box: (
    <>
      <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
      <polyline points="3.27 6.96 12 12.01 20.73 6.96" /><line x1="12" y1="22.08" x2="12" y2="12" />
    </>
  ),
  route: (
    <>
      <circle cx="6" cy="19" r="3" /><circle cx="18" cy="5" r="3" />
      <path d="M12 19h4.5a3.5 3.5 0 0 0 0-7h-9a3.5 3.5 0 0 1 0-7H12" />
    </>
  ),
  key: (
    <>
      <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
    </>
  ),
  users: (
    <>
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" /><path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </>
  ),
  userCog: (
    <>
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" />
      <circle cx="19" cy="16" r="2" /><path d="M19 10.5v1.2M19 20.3v1.2M22.9 14l-1 .6M16.1 17.4l-1 .6M22.9 18l-1-.6M16.1 14.6l-1-.6" />
    </>
  ),
  play: <polygon points="5 3 19 12 5 21 5 3" />,
  logs: (
    <>
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <polyline points="14 2 14 8 20 8" /><line x1="16" y1="13" x2="8" y2="13" /><line x1="16" y1="17" x2="8" y2="17" />
    </>
  ),
  cog: (
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </>
  ),
  sun: (
    <>
      <circle cx="12" cy="12" r="5" />
      <line x1="12" y1="1" x2="12" y2="3" /><line x1="12" y1="21" x2="12" y2="23" />
      <line x1="4.22" y1="4.22" x2="5.64" y2="5.64" /><line x1="18.36" y1="18.36" x2="19.78" y2="19.78" />
      <line x1="1" y1="12" x2="3" y2="12" /><line x1="21" y1="12" x2="23" y2="12" />
      <line x1="4.22" y1="19.78" x2="5.64" y2="18.36" /><line x1="18.36" y1="5.64" x2="19.78" y2="4.22" />
    </>
  ),
  moon: <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />,
  x: <><line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" /></>,
  chevronDown: <polyline points="6 9 12 15 18 9" />,
  chevronLeft: <polyline points="15 18 9 12 15 6" />,
  chevronRight: <polyline points="9 18 15 12 9 6" />,
  plus: <><line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" /></>,
  trash: (
    <>
      <polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
    </>
  ),
  pencil: <path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z" />,
  copy: (
    <>
      <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
    </>
  ),
  check: <polyline points="20 6 9 17 4 12" />,
  alert: (
    <>
      <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
      <line x1="12" y1="9" x2="12" y2="13" /><line x1="12" y1="17" x2="12.01" y2="17" />
    </>
  ),
  search: (
    <>
      <circle cx="11" cy="11" r="8" /><line x1="21" y1="21" x2="16.65" y2="16.65" />
    </>
  ),
  refresh: (
    <>
      <polyline points="23 4 23 10 17 10" /><polyline points="1 20 1 14 7 14" />
      <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" />
    </>
  ),
  menu: <><line x1="3" y1="12" x2="21" y2="12" /><line x1="3" y1="6" x2="21" y2="6" /><line x1="3" y1="18" x2="21" y2="18" /></>,
  logout: (
    <>
      <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" /><polyline points="16 17 21 12 16 7" />
      <line x1="21" y1="12" x2="9" y2="12" />
    </>
  ),
  shield: <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />,
  zap: <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />,
  lock: (
    <>
      <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </>
  ),
  external: (
    <>
      <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
      <polyline points="15 3 21 3 21 9" /><line x1="10" y1="14" x2="21" y2="3" />
    </>
  ),
}

export function Icon({ name, size = 16, className = '' }: { name: IconName; size?: number; className?: string }) {
  return (
    <svg
      width={size} height={size} viewBox="0 0 24 24" fill="none"
      stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
      className={`shrink-0 ${className}`} aria-hidden="true"
    >
      {paths[name]}
    </svg>
  )
}

/* ------------------------------------------------------------------ */
/* Buttons                                                             */
/* ------------------------------------------------------------------ */

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'subtle'

export function Button({
  children, onClick, disabled, variant = 'secondary', size = 'md',
  className = '', type = 'button', title,
}: {
  children: ReactNode; onClick?: () => void; disabled?: boolean
  variant?: ButtonVariant; size?: 'sm' | 'md'; className?: string
  type?: 'button' | 'submit'; title?: string
}) {
  const base =
    'inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-all duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50 disabled:opacity-40 disabled:pointer-events-none select-none active:scale-[0.98]'
  const sizes = size === 'sm' ? 'text-xs px-2.5 h-8' : 'text-sm px-4 h-10'
  const variants: Record<ButtonVariant, string> = {
    primary: 'bg-teal text-graphite font-semibold hover:brightness-110 shadow-sm',
    secondary: 'border border-stone bg-surface text-paper hover:border-paper/30 hover:bg-stone/30',
    ghost: 'text-muted hover:text-paper hover:bg-stone/40',
    danger: 'bg-red-500/90 text-white hover:bg-red-500',
    subtle: 'bg-stone/50 text-paper hover:bg-stone/80',
  }
  return (
    <button type={type} onClick={onClick} disabled={disabled} title={title}
      className={`${base} ${sizes} ${variants[variant]} ${className}`}>
      {children}
    </button>
  )
}

/* ------------------------------------------------------------------ */
/* Surfaces                                                            */
/* ------------------------------------------------------------------ */

export function Card({ children, className = '', pad = true }: { children: ReactNode; className?: string; pad?: boolean }) {
  return (
    <div className={`rounded-xl border border-stone bg-surface shadow-card ${pad ? 'p-5' : ''} ${className}`}>
      {children}
    </div>
  )
}

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-3 mb-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="text-muted text-sm mt-1 max-w-2xl leading-relaxed">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
    </div>
  )
}

export function Badge({ children, tone = 'neutral', dot = false }: {
  children: ReactNode; tone?: 'neutral' | 'good' | 'warn' | 'bad' | 'info' | 'brand'; dot?: boolean
}) {
  const tones = {
    neutral: 'bg-stone/50 text-muted',
    good: 'bg-teal/10 text-teal',
    warn: 'bg-amber/10 text-amber',
    bad: 'bg-red-500/10 text-red-400',
    info: 'bg-paper/10 text-paper',
    brand: 'bg-teal text-graphite',
  }
  const dotColor = { neutral: '', good: 'bg-teal', warn: 'bg-amber', bad: 'bg-red-400', info: 'bg-paper/70', brand: '' }
  return (
    <span className={`inline-flex items-center gap-1.5 text-xs font-medium px-2 py-0.5 rounded-full ${tones[tone]}`}>
      {dot && <span className={`w-1.5 h-1.5 rounded-full ${dotColor[tone]} ${tone === 'good' ? 'animate-pulse-soft' : ''}`} />}
      {children}
    </span>
  )
}

/** Canonical traffic-light for provider/model health values. */
export function HealthDot({ health }: { health?: string | null }) {
  const up = health === 'up'
  const down = health === 'down'
  const color = up ? 'bg-teal' : down ? 'bg-red-400' : 'bg-muted/50'
  const label = up ? 'healthy' : down ? 'down' : 'unknown'
  return <span title={`health: ${label}`} className={`inline-block w-2 h-2 rounded-full ${color} ${up ? 'shadow-glow' : ''}`} />
}

/* ------------------------------------------------------------------ */
/* Form controls                                                       */
/* ------------------------------------------------------------------ */

const fieldCls =
  'w-full bg-app border border-stone rounded-lg px-3 h-10 text-sm placeholder:text-muted/60 transition-colors focus:outline-none focus:border-teal/60 focus:ring-2 focus:ring-teal/20 disabled:opacity-50'

export function Field({ label, hint, children, className = '' }: { label: string; hint?: string; children: ReactNode; className?: string }) {
  return (
    <label className={`block ${className}`}>
      <span className="block text-xs font-medium text-muted mb-1.5 uppercase tracking-wide">{label}</span>
      {children}
      {hint && <span className="block text-xs text-muted mt-1">{hint}</span>}
    </label>
  )
}

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={`${fieldCls} ${props.className || ''}`} />
}

export function Select(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={`${fieldCls} ${props.className || ''}`} />
}

export function Textarea(props: React.TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={`${fieldCls} h-auto min-h-[100px] py-2 font-mono text-xs ${props.className || ''}`} />
}

/* ------------------------------------------------------------------ */
/* Copyable value                                                      */
/* ------------------------------------------------------------------ */

/**
 * Best-effort clipboard write with a plain-HTTP fallback (navigator.clipboard
 * is only available on secure origins, so LAN installs need the textarea +
 * execCommand path). Returns 'copied' | 'failed'.
 */
function copyText(value: string): Promise<'copied' | 'failed'> {
  if (navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(value).then(
      () => 'copied' as const,
      () => legacyCopy(value),
    )
  }
  return Promise.resolve(legacyCopy(value))
}

function legacyCopy(value: string): 'copied' | 'failed' {
  try {
    const ta = document.createElement('textarea')
    ta.value = value
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.left = '-9999px'
    document.body.appendChild(ta)
    ta.select()
    ta.setSelectionRange(0, value.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok ? 'copied' : 'failed'
  } catch {
    return 'failed'
  }
}

export function CopyButton({ value, label, size = 'sm' }: { value: string; label?: string; size?: 'sm' | 'md' }) {
  const [done, setDone] = useState(false)
  const [failed, setFailed] = useState(false)
  return (
    <Button
      size={size} variant="ghost"
      title={label || 'Copy'}
      onClick={() => {
        copyText(value).then((result) => {
          if (result === 'copied') {
            setFailed(false)
            setDone(true)
            setTimeout(() => setDone(false), 1200)
          } else {
            setFailed(true)
            setTimeout(() => setFailed(false), 3000)
          }
        })
      }}
    >
      <Icon name={failed ? 'alert' : done ? 'check' : 'copy'} size={size === 'sm' ? 13 : 15} className={done ? 'text-teal' : failed ? 'text-red-400' : ''} />
      {done ? 'Copied' : failed ? 'Copy failed — select manually' : ''}
    </Button>
  )
}

/* ------------------------------------------------------------------ */
/* Tables                                                              */
/* ------------------------------------------------------------------ */

export function TableShell({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <Card pad={false} className={`overflow-hidden ${className}`}>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">{children}</table>
      </div>
    </Card>
  )
}

export function Th({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <th className={`text-left text-[11px] font-semibold uppercase tracking-wider text-muted px-4 py-3 border-b border-stone bg-app/60 whitespace-nowrap ${className}`}>
      {children}
    </th>
  )
}

export function Td({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <td className={`px-4 py-3 border-b border-stone/50 align-middle ${className}`}>{children}</td>
}

/* ------------------------------------------------------------------ */
/* States                                                              */
/* ------------------------------------------------------------------ */

export function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`rounded-md bg-stone/60 animate-shimmer ${className}`} />
}

export function TableSkeleton({ rows = 4, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <tbody>
      {Array.from({ length: rows }).map((_, r) => (
        <tr key={r}>
          {Array.from({ length: cols }).map((_, c) => (
            <Td key={c}><Skeleton className="h-4 w-full max-w-[140px]" /></Td>
          ))}
        </tr>
      ))}
    </tbody>
  )
}

export function EmptyState({ icon = 'box', title, hint, action }: { icon?: IconName; title: string; hint?: string; action?: ReactNode }) {
  return (
    <div className="py-14 flex flex-col items-center text-center">
      <div className="w-12 h-12 rounded-xl bg-stone/40 text-muted flex items-center justify-center mb-3">
        <Icon name={icon} size={22} />
      </div>
      <div className="font-medium">{title}</div>
      {hint && <div className="text-muted text-sm mt-1 max-w-sm">{hint}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  )
}

export function ErrorNote({ message }: { message: string }) {
  if (!message) return null
  return (
    <div className="flex items-start gap-2 text-sm text-red-400 bg-red-500/10 border border-red-500/20 rounded-lg px-3 py-2.5">
      <Icon name="alert" size={15} className="mt-0.5 shrink-0" />
      <span>{message}</span>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Modal                                                               */
/* ------------------------------------------------------------------ */

export function Modal({ open, onClose, title, children, width = 'max-w-lg' }: {
  open: boolean; onClose: () => void; title: string; children: ReactNode; width?: string
}) {
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose])
  if (!open) return null
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm animate-fade" onClick={onClose} />
      <div role="dialog" aria-modal="true" aria-label={title} className={`relative w-full ${width} card-modal rounded-xl border border-stone bg-surface shadow-2xl`}>
        <div className="flex items-center justify-between px-5 py-4 border-b border-stone">
          <h2 className="font-semibold tracking-tight">{title}</h2>
          <button onClick={onClose} aria-label="Close"
            className="w-8 h-8 -mr-1 rounded-lg flex items-center justify-center text-muted hover:text-paper hover:bg-stone/50 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50">
            <Icon name="x" size={16} />
          </button>
        </div>
        <div className="p-5">{children}</div>
      </div>
    </div>
  )
}

export function Confirm({ open, onClose, onConfirm, title, body, confirmLabel = 'Delete', busy = false }: {
  open: boolean; onClose: () => void; onConfirm: () => void
  title: string; body: string; confirmLabel?: string; busy?: boolean
}) {
  return (
    <Modal open={open} onClose={onClose} title={title} width="max-w-md">
      <p className="text-sm text-muted leading-relaxed">{body}</p>
      <div className="flex justify-end gap-2 mt-5">
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
        <Button variant="danger" onClick={onConfirm} disabled={busy}>
          {busy ? 'Working…' : confirmLabel}
        </Button>
      </div>
    </Modal>
  )
}

/* ------------------------------------------------------------------ */
/* Toasts (global)                                                     */
/* ------------------------------------------------------------------ */

export type ToastKind = 'success' | 'error' | 'info'
interface ToastState {
  toasts: { id: number; kind: ToastKind; text: string }[]
  push: (kind: ToastKind, text: string) => void
  dismiss: (id: number) => void
}

let nextToastId = 1
export const useToastStore = create<ToastState>((set) => ({
  toasts: [],
  push: (kind, text) => {
    const id = nextToastId++
    set((s) => ({ toasts: [...s.toasts.slice(-4), { id, kind, text }] }))
    setTimeout(() => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })), 4200)
  },
  dismiss: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}))

/** Hook for pages: const toast = useToast(); toast.success('Saved'). */
export function useToast() {
  const push = useToastStore((s) => s.push)
  return {
    success: (t: string) => push('success', t),
    error: (t: string) => push('error', t),
    info: (t: string) => push('info', t),
  }
}

export function Toaster() {
  const { toasts, dismiss } = useToastStore()
  const tone: Record<ToastKind, string> = {
    success: 'border-teal/40 text-teal',
    error: 'border-red-500/40 text-red-400',
    info: 'border-stone text-paper',
  }
  const iconName: Record<ToastKind, IconName> = { success: 'check', error: 'alert', info: 'alert' }
  return (
    <div className="fixed bottom-4 right-4 z-[60] space-y-2 w-[320px] max-w-[calc(100vw-2rem)]">
      {toasts.map((t) => (
        <div key={t.id}
          className={`toast-in flex items-start gap-2.5 rounded-lg border bg-surface px-3.5 py-3 shadow-pop text-sm ${tone[t.kind]}`}
          role="status">
          <Icon name={iconName[t.kind]} size={15} className="mt-0.5 shrink-0" />
          <span className="text-paper leading-snug flex-1">{t.text}</span>
          <button onClick={() => dismiss(t.id)} className="text-muted hover:text-paper shrink-0" aria-label="Dismiss">
            <Icon name="x" size={13} />
          </button>
        </div>
      ))}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Misc                                                                */
/* ------------------------------------------------------------------ */

/** Tabs — simple controlled tabs rendered as segmented control. */
export function SegmentedControl<T extends string>({ options, value, onChange }: {
  options: { value: T; label: string }[]; value: T; onChange: (v: T) => void
}) {
  return (
    <div className="inline-flex rounded-lg border border-stone bg-app p-0.5 gap-0.5" role="tablist">
      {options.map((o) => (
        <button key={o.value} role="tab" aria-selected={value === o.value} onClick={() => onChange(o.value)}
          className={`px-3 h-8 rounded-md text-xs font-medium transition-all duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal/50 ${
            value === o.value ? 'bg-surface text-paper shadow-sm border border-stone/60' : 'text-muted hover:text-paper'
          }`}>
          {o.label}
        </button>
      ))}
    </div>
  )
}

/** Click-outside helper for menus/dropdowns. */
export function useClickOutside(onOutside: () => void) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const handler = (e: MouseEvent | TouchEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onOutside()
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [onOutside])
  return ref
}

/** Collapsible section row used by Settings-style long forms. */
const SectionCtx = createContext<{ title: string }>({ title: '' })
export function Section({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <SectionCtx.Provider value={{ title }}>
      <section className="mb-8">
        <div className="mb-3">
          <h2 className="text-sm font-semibold tracking-wide uppercase text-muted">{title}</h2>
          {description && <p className="text-muted text-sm mt-0.5">{description}</p>}
        </div>
        {children}
      </section>
    </SectionCtx.Provider>
  )
}
