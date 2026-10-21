/**
 * Clipboard write with a legacy fallback.
 *
 * `navigator.clipboard` only exists in a secure context (https, or
 * localhost). A self-hosted gateway reached over plain `http://` on a LAN
 * address has none, and the old call sites wrote
 *
 *   navigator.clipboard?.writeText(v).then(...)
 *
 * which evaluates to `undefined.then(...)` there and threw a TypeError, so
 * every "Copy" affordance silently did nothing. One helper, one fallback.
 */
export type CopyResult = 'copied' | 'failed'

function legacyCopy(value: string): CopyResult {
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
  } catch { return 'failed' }
}

export function copyText(value: string): Promise<CopyResult> {
  if (navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(value).then(() => 'copied' as const, () => legacyCopy(value))
  }
  return Promise.resolve(legacyCopy(value))
}

/** Convenience boolean form for call sites that only need success/failure. */
export function copyToClipboard(value: string): Promise<boolean> {
  return copyText(value).then(r => r === 'copied')
}
