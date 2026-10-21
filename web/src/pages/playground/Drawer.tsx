/**
 * Narrow-viewport side drawer with real dialog semantics.
 *
 * The previous markup was a scrim + panel with no `role="dialog"`, no
 * `aria-modal`, no accessible name, no focus move and no focus trap, so a
 * screen reader saw nothing and the keyboard stayed on `<body>`.
 *
 * Contract:
 * - `role="dialog"` + `aria-modal="true"` + `aria-label`
 * - focus moves to the first focusable control inside on open
 * - Tab/Shift+Tab cycle inside the panel
 * - Escape closes
 * - focus returns to the element that opened it on close
 * - the background is inert to pointer events (scrim covers it)
 */
import { useLayoutEffect, useRef } from 'react'

const FOCUSABLE = [
  'a[href]', 'button:not([disabled])', 'input:not([disabled])', 'select:not([disabled])',
  'textarea:not([disabled])', '[tabindex]:not([tabindex="-1"])',
].join(',')

export function Drawer({ open, onClose, label, side = 'left', returnFocus, children }: {
  open: boolean
  onClose: () => void
  label: string
  side?: 'left' | 'right'
  /**
   * The control that opened the drawer. Explicit is better than
   * `document.activeElement`, which is <body> when the panel is opened
   * programmatically (keyboard shortcut, autofocus, scripted click).
   */
  returnFocus?: React.RefObject<HTMLElement>
  children: React.ReactNode
}) {
  const panelRef = useRef<HTMLDivElement>(null)
  const restoreRef = useRef<HTMLElement | null>(null)
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  // useLayoutEffect: a passive cleanup runs after React has removed the panel,
  // which left the previously focused control detached and focus on <body>
  // instead of back on the trigger.
  useLayoutEffect(() => {
    if (!open) return
    restoreRef.current = returnFocus?.current
      || (document.activeElement && document.activeElement !== document.body ? document.activeElement as HTMLElement : null)
    const panel = panelRef.current
    const first = panel?.querySelector<HTMLElement>(FOCUSABLE)
    // Defer to the next frame so the panel exists and its animation started.
    const raf = requestAnimationFrame(() => {
      ;(first || panel)?.focus()
    })

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab' || !panel) return
      const items = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE))
        .filter(el => el.offsetParent !== null || el === document.activeElement)
      if (items.length === 0) { e.preventDefault(); panel.focus(); return }
      const firstEl = items[0]
      const lastEl = items[items.length - 1]
      const active = document.activeElement as HTMLElement | null
      if (!e.shiftKey && active === lastEl) { e.preventDefault(); firstEl.focus() }
      else if (e.shiftKey && (active === firstEl || !panel.contains(active))) { e.preventDefault(); lastEl.focus() }
    }
    document.addEventListener('keydown', onKey, true)
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      cancelAnimationFrame(raf)
      document.removeEventListener('keydown', onKey, true)
      document.body.style.overflow = previousOverflow
      // Return focus to the trigger so keyboard users keep their place.
      const restore = restoreRef.current
      restoreRef.current = null
      if (restore && document.contains(restore)) restore.focus()
    }
  }, [open, returnFocus])

  if (!open) return null

  return (
    <div className="fixed inset-x-0 bottom-0 z-40" style={{ top: '3.5rem' }}>
      <div className="absolute inset-0 bg-black/60 animate-fade" onClick={onClose} aria-hidden="true" />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        tabIndex={-1}
        className={`absolute inset-y-0 ${side === 'left' ? 'left-0 border-r' : 'right-0 border-l'} w-[min(360px,92vw)] bg-surface border-stone shadow-pop flex flex-col animate-sidebar outline-none`}
      >
        {children}
      </div>
    </div>
  )
}
