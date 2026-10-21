/**
 * Cross-page "focus the request search box" handshake.
 *
 * The header advertises ⌘K as a jump to request search, but navigating to
 * /logs left focus on <body>: the destination page mounts *after* the
 * shortcut fires, so a plain `ref.focus()` cannot reach it. A pending flag
 * survives the navigation and the Logs page consumes it on mount; the event
 * covers the case where Logs is already mounted.
 */
const EVENT = 'gw:focus-search'
let pending = false

export function requestSearchFocus() {
  pending = true
  window.dispatchEvent(new Event(EVENT))
}

export function consumeSearchFocus(): boolean {
  if (!pending) return false
  pending = false
  return true
}

export function onSearchFocusRequest(handler: () => void): () => void {
  window.addEventListener(EVENT, handler)
  return () => window.removeEventListener(EVENT, handler)
}
