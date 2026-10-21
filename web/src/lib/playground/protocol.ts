/**
 * Pure protocol helpers for the Playground: request-body shaping, streaming
 * frame parsing, text/usage extraction and error-envelope unpacking.
 *
 * Nothing here touches React or the DOM, which is the point — these are the
 * functions that decide what the user sees for a given byte stream, so they
 * are the ones that need tests (see `protocol.test-d.ts` notes in the review).
 */
import { fmtBytes, uid } from './format'
import type { Attachment, QuestionDraft, Usage } from './types'

/* ------------------------------------------------------------------ */
/* Attachments → provider content parts                                */
/* ------------------------------------------------------------------ */

export function splitDataUrl(url: string): { mime: string; b64: string } {
  const m = /^data:([^;]+);base64,(.*)$/.exec(url)
  if (!m) return { mime: 'image/png', b64: '' }
  return { mime: m[1], b64: m[2] }
}

/** Text-like attachments are inlined as fenced context after the prompt. */
export function inlinedText(atts: Attachment[]): string {
  const files = atts.filter(a => a.kind === 'text' && a.text)
  if (!files.length) return ''
  return files.map(a => `\n\n--- Attached file: ${a.name} ---\n${a.text}`).join('')
}

/**
 * Binary attachments we cannot inline are still named, so the model knows a
 * file was referenced rather than silently seeing an unexplained prompt.
 */
export function attachmentManifest(atts: Attachment[]): string {
  const other = atts.filter(a => a.kind === 'other')
  if (!other.length) return ''
  return `\n\n--- Attached files (not inlined, ${other.length}) ---\n` +
    other.map(a => `- ${a.name} (${a.mime}, ${fmtBytes(a.size)})`).join('\n')
}

export function promptText(text: string, atts: Attachment[]): string {
  return text + inlinedText(atts) + attachmentManifest(atts)
}

/** Only emit a text part when there is text — empty parts trip strict validators. */
function withTextPart(full: string, parts: any[]) {
  return full ? [{ type: 'text', text: full }, ...parts] : parts
}

export function openAIContent(text: string, atts: Attachment[]) {
  const imgs = atts.filter(a => a.kind === 'image' && a.dataUrl)
  const full = promptText(text, atts)
  if (!imgs.length) return full
  return withTextPart(full, imgs.map(a => ({ type: 'image_url', image_url: { url: a.dataUrl } })))
}

export function anthropicContent(text: string, atts: Attachment[]) {
  const imgs = atts.filter(a => a.kind === 'image' && a.dataUrl)
  const full = promptText(text, atts)
  if (!imgs.length) return full
  return withTextPart(full, imgs.map(a => {
    const { mime, b64 } = splitDataUrl(a.dataUrl!)
    return { type: 'image', source: { type: 'base64', media_type: mime, data: b64 } }
  }))
}

export function responsesContent(text: string, atts: Attachment[], role: 'user' | 'assistant' = 'user') {
  const imgs = atts.filter(a => a.kind === 'image' && a.dataUrl)
  const full = promptText(text, atts)
  if (!imgs.length) return full
  // Assistant turns replay as output_text; input_text on an assistant message is
  // rejected by strict Responses-native upstreams.
  const textPart = { type: role === 'assistant' ? 'output_text' : 'input_text', text: full }
  const imageParts = imgs.map(a => ({ type: 'input_image', image_url: a.dataUrl }))
  return full ? [textPart, ...imageParts] : imageParts
}

/** Reference images attached to an image-generation request, as data URLs. */
export function referenceImages(atts: Attachment[]): string[] {
  return atts.filter(a => a.kind === 'image' && a.dataUrl).map(a => a.dataUrl!)
}

/* ------------------------------------------------------------------ */
/* Decision (System One) request shaping                               */
/* ------------------------------------------------------------------ */

export function parseLevels(s?: string): string[] {
  if (!s) return []
  try { const v = JSON.parse(s); return Array.isArray(v) ? v : [] } catch { return s.split(',').map(x => x.trim()).filter(Boolean) }
}

export function parseChoiceCriteria(s: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of s.split('\n')) {
    const t = line.trim()
    if (!t) continue
    const i = t.indexOf(':')
    if (i === -1) out[t] = t
    else {
      const k = t.slice(0, i).trim()
      const v = t.slice(i + 1).trim()
      if (k) out[k] = v || k
    }
  }
  return out
}

export function parseScoreCriteria(s: string): string[] {
  return s.split(/[\n,]/).map(x => x.trim()).filter(Boolean)
}

export function parseState(s: string): any {
  const t = s.trim()
  if (!t) return ''
  try { return JSON.parse(t) } catch { return s }
}

export function buildDecisionBody(model: string, stateInput: string, drafts: QuestionDraft[]) {
  const questions: Record<string, any> = {}
  for (const q of drafts) {
    const id = q.id.trim()
    if (!id) throw new Error('Every question needs an id.')
    if (questions[id]) throw new Error(`Duplicate question id "${id}".`)
    const instructions = q.instructions.trim()
    if (!instructions) throw new Error(`Question "${id}" needs instructions.`)
    const entry: any = { type: q.type, instructions }
    if (q.type === 'choice') {
      const criteria = parseChoiceCriteria(q.criteria)
      if (Object.keys(criteria).length < 2) throw new Error(`Choice "${id}" needs at least 2 options (one "key: description" per line).`)
      entry.criteria = criteria
    } else if (q.type === 'score') {
      const criteria = parseScoreCriteria(q.criteria)
      if (criteria.length < 2) throw new Error(`Score "${id}" needs at least 2 ordered levels (comma or newline separated).`)
      entry.criteria = criteria
    }
    questions[id] = entry
  }
  if (Object.keys(questions).length === 0) throw new Error('Add at least one question.')
  return { model, state: parseState(stateInput), questions }
}

/* ------------------------------------------------------------------ */
/* Streaming frame parsing                                             */
/* ------------------------------------------------------------------ */

/** Payload of one SSE line, or '' when the line carries no JSON. */
function framePayload(line: string): string {
  const trimmed = line.trim()
  if (!trimmed) return ''
  const payload = trimmed.startsWith('data:') ? trimmed.slice(5).trim() : trimmed
  if (!payload || payload === '[DONE]') return ''
  return payload.startsWith('{') ? payload : ''
}

/**
 * Pull a text delta out of one streamed event line.
 *
 * Only true *delta* frames are accepted. Several gateways (ours included) send
 * a terminal `response.output_text.done` frame whose payload repeats the FULL
 * text, and some send a final `{content:[{text}]}` snapshot; treating those as
 * deltas duplicates the whole answer in the bubble.
 */
export function deltaFrom(line: string): string {
  const payload = framePayload(line)
  if (!payload) return ''
  try {
    const j = JSON.parse(payload)
    if (!j || typeof j !== 'object') return ''
    if (j.type === 'response.output_text.delta') {
      return typeof j.delta === 'string' ? j.delta : ''
    }
    if (j.type === 'content_block_delta') {
      // Anthropic sends thinking deltas too; only text_delta is answer text.
      if (j.delta && typeof j.delta.type === 'string' && j.delta.type !== 'text_delta') return ''
      if (typeof j.delta?.text === 'string') return j.delta.text
      return ''
    }
    // Chat Completions (OpenAI + every OpenAI-compatible provider).
    const openai = j.choices?.[0]?.delta?.content
    if (typeof openai === 'string') return openai
    // Anthropic message deltas that omit the content_block_delta wrapper.
    const anth = j.delta?.text
    if (typeof anth === 'string' && !j.delta?.type) return anth
  } catch { /* partial JSON across reads — the caller keeps the buffer */ }
  return ''
}

/**
 * Extract an in-band error message from one streamed frame.
 *
 * The gateway signals a failure that happens AFTER bytes were already sent by
 * emitting `data: {"error":{"message":"…"}}` followed by `[DONE]`
 * (internal/proxy/upstream.go). Anthropic-native upstreams use
 * `{"type":"error","error":{"message":"…"}}`. Without this the partial answer
 * looks complete — the worst failure mode for a chat client.
 */
export function streamErrorFrom(line: string): string {
  const payload = framePayload(line)
  if (!payload) return ''
  let j: any
  try { j = JSON.parse(payload) } catch { return '' }
  if (!j || typeof j !== 'object') return ''
  const err = j.error
  if (typeof err === 'string' && err) return err
  if (err && typeof err === 'object') {
    if (typeof err.message === 'string' && err.message) return err.message
    if (typeof err.type === 'string' && err.type) return err.type
  }
  // Anthropic also sends {"type":"error","message":"…"} in some paths.
  if (j.type === 'error' && typeof j.message === 'string' && j.message) return j.message
  return ''
}

/* ------------------------------------------------------------------ */
/* Response text / usage extraction                                    */
/* ------------------------------------------------------------------ */

export function safeParse(s: string): any {
  try { return JSON.parse(s) } catch { return null }
}

/**
 * Pull the human-readable message out of an error envelope. Providers disagree
 * on the shape ({"error":"..."}, {"error":{"message"}}, {"message"}), so probe
 * them in order and fall back to the trimmed body for non-JSON errors.
 */
export function errorMessage(body: string): string {
  const raw = (body || '').trim()
  if (!raw) return ''
  const j = safeParse(raw)
  if (j && typeof j === 'object') {
    if (typeof j.error === 'string') return j.error
    if (j.error && typeof j.error.message === 'string') return j.error.message
    if (typeof j.message === 'string') return j.message
    if (typeof j.detail === 'string') return j.detail
  }
  return raw.length > 400 ? `${raw.slice(0, 400)}…` : raw
}

/** Last-ditch extraction when no streaming deltas matched. */
export function tryParseRich(buffer: string): { text: string } {
  const trimmed = buffer.trim()
  const whole = safeParse(trimmed)
  if (whole) return { text: extractText(whole) || JSON.stringify(whole, null, 2) }
  const texts: string[] = []
  for (const line of buffer.split('\n')) {
    const payload = line.trim().startsWith('data:') ? line.trim().slice(5).trim() : line.trim()
    if (!payload.startsWith('{')) continue
    const j = safeParse(payload)
    if (!j) continue
    const t = extractText(j)
    if (t) texts.push(t)
  }
  if (texts.length) return { text: texts.join('') }
  return { text: buffer.slice(0, 3000) }
}

export function extractText(j: any): string {
  if (!j || typeof j !== 'object') return ''
  if (typeof j.output_text === 'string') return j.output_text
  if (Array.isArray(j.content)) {
    const t = j.content.map((c: any) => c?.text || '').join('')
    if (t) return t
  }
  if (typeof j.text === 'string') return j.text
  const delta = j.choices?.[0]?.delta?.content ?? j.choices?.[0]?.message?.content
  if (typeof delta === 'string') return delta
  if (Array.isArray(delta)) return delta.map((c: any) => c?.text || '').join('')
  if (Array.isArray(j.output)) {
    const t = j.output.flatMap((o: any) => Array.isArray(o?.content) ? o.content.map((c: any) => c?.text || '') : []).join('')
    if (t) return t
  }
  return ''
}

export function extractUsageFromObject(j: any): Usage | undefined {
  const u = j?.usage
  if (!u) return undefined
  const input = u.input_tokens ?? u.prompt_tokens
  const output = u.output_tokens ?? u.completion_tokens
  const total = u.total_tokens ?? (input !== undefined && output !== undefined ? input + output : undefined)
  if (input === undefined && output === undefined && total === undefined) return undefined
  return { input, output, total, cached: u.cache_read_input_tokens ?? u.cached_tokens }
}

/** Scan a raw SSE buffer for the last usage block any provider emitted. */
export function extractUsage(buffer: string): Usage | undefined {
  let found: Usage | undefined
  for (const line of buffer.split('\n')) {
    const payload = line.trim().startsWith('data:') ? line.trim().slice(5).trim() : line.trim()
    if (!payload.startsWith('{')) continue
    const j = safeParse(payload)
    if (!j) continue
    const u = extractUsageFromObject(j) || extractUsageFromObject(j.response) || extractUsageFromObject(j.message)
    if (u) found = u
  }
  return found
}

/* ------------------------------------------------------------------ */
/* Misc exports used by the page                                       */
/* ------------------------------------------------------------------ */

/** Title for a new conversation, from the prompt or the first attachment. */
export function titleFrom(text: string, attachments: Attachment[]): string {
  const t = text.trim().replace(/\s+/g, ' ')
  if (t) return t.length > 42 ? `${t.slice(0, 42)}…` : t
  if (attachments.length) return `📎 ${attachments[0].name}`
  return 'New conversation'
}

export { uid }
