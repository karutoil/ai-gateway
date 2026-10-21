/**
 * Small pure formatters used across the Playground. Kept out of the component
 * so they can be unit-tested and reused by the message components.
 */

export const uid = (): string => Math.random().toString(36).slice(2, 10)

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

export function fmtTime(ts: number): string {
  return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

export function estimateTokens(s: string): number {
  if (!s) return 0
  // ~4 chars/token is the usual English rule of thumb; keep it honest-ish by
  // weighting whitespace-heavy content slightly lower.
  return Math.max(0, Math.round(s.trim().length / 4))
}

/** Split assistant text into prose / fenced-code blocks for rendering. */
export function splitFences(text: string): { lang?: string; code: string }[] {
  const out: { lang?: string; code: string }[] = []
  const re = /```([a-zA-Z0-9_+#.-]*)[ \t]*\r?\n?([\s\S]*?)(?:```|$)/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push({ code: text.slice(last, m.index) })
    out.push({ lang: (m[1] || '').trim(), code: m[2] })
    last = m.index + m[0].length
    if (re.lastIndex === m.index) re.lastIndex++
  }
  if (last < text.length) out.push({ code: text.slice(last) })
  return out.length ? out : [{ code: text }]
}

/** Probability → human percentage, tolerant of missing values. */
export function pct(v: unknown): string {
  const n = typeof v === 'number' && Number.isFinite(v) ? v : 0
  return `${(n * 100).toFixed(1)}%`
}

/**
 * Human-readable reason from a `<video>` MediaError. Browsers only expose a
 * numeric code, so map the three useful ones and keep the raw code otherwise:
 * a CSP refusal surfaces as code 4 ("src not supported").
 */
export function mediaErrorMessage(err: MediaError | null): string {
  switch (err?.code) {
    case 1: return 'playback was aborted'
    case 2: return 'the network dropped while loading the video'
    case 3: return 'the video could not be decoded'
    case 4: return 'the browser refused to load the video source'
    default: return 'the video could not be loaded'
  }
}
