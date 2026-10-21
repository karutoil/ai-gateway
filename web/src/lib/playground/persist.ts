/**
 * Transcript persistence.
 *
 * Only the text of a conversation survives a reload. Generated image bytes
 * (base64) would blow the localStorage quota, video bytes are blob URLs that
 * die with the page, and attachment bytes are re-derivable by the user — so
 * each message additionally stores a `render` discriminator and a `retained`
 * flag. A restored message therefore never has to guess: it renders either its
 * payload or an explicit "not retained" placeholder, but never a loading
 * indicator (see MessageRow).
 */
import { uid } from './format'
import type { Msg, RenderKind, Session, VideoState } from './types'

const STORE_KEY = 'gw_playground_sessions'

/** Max serialized size of a decision body worth keeping (probabilities only). */
const MAX_DECISION_BYTES = 8 * 1024

export function renderKindOf(m: Msg): RenderKind {
  if (m.render) return m.render
  if (m.decision) return 'decision'
  if (m.images && m.images.length > 0) return 'images'
  if (m.video) return 'video'
  return 'text'
}

/** Image metadata that is safe to persist (no base64, no URL). */
function slimImages(images: any[] | undefined) {
  if (!images || images.length === 0) return undefined
  return images.map(i => ({
    revised_prompt: typeof i?.revised_prompt === 'string' ? i.revised_prompt : undefined,
    mime_type: typeof i?.mime_type === 'string' ? i.mime_type : undefined,
  }))
}

/** Video job state minus the blob URL. */
function slimVideo(video: VideoState | undefined) {
  if (!video) return undefined
  return {
    job: video.job,
    error: video.error,
    fatal: video.fatal,
    attempts: video.attempts,
  }
}

function slimMessage(m: Msg): Msg {
  const render = renderKindOf(m)
  // Base64 image bytes are never persisted; the metadata above is kept so the
  // placeholder can still name what was generated.
  const images = render === 'images' ? slimImages(m.images) : undefined
  let decision: any = undefined
  let decisionRetained = false
  if (render === 'decision' && m.decision) {
    try {
      const json = JSON.stringify(m.decision)
      if (json.length <= MAX_DECISION_BYTES) { decision = JSON.parse(json); decisionRetained = true }
    } catch { /* unserializable — drop it */ }
  }
  // A completed video can be re-fetched from /content after a reload, so its
  // payload is considered recoverable; its blob URL is not.
  const video = render === 'video' ? slimVideo(m.video) : undefined
  const retained = render === 'images' ? false
    : render === 'decision' ? decisionRetained
    : render === 'video' ? !!video?.job?.id
    : true

  return {
    ...m,
    streaming: false,
    attachments: m.attachments?.map(a => ({ ...a, dataUrl: undefined, text: undefined })),
    render,
    retained,
    images,
    decision,
    video,
  }
}

export function persist(sessions: Session[], activeId: string) {
  try {
    const slim = sessions.slice(-12).map(s => ({
      ...s,
      messages: s.messages.slice(-120).map(slimMessage),
      // Calls survive as metadata only; payloads are dropped to stay inside
      // the storage quota, and the inspector labels them as such.
      calls: s.calls.slice(0, 20).map(c => ({ ...c, request: undefined, response: undefined, headers: undefined })),
    }))
    localStorage.setItem(STORE_KEY, JSON.stringify({ sessions: slim, activeId }))
  } catch { /* quota or private mode — persistence is a nicety, not a requirement */ }
}

/**
 * Rehydrate the stored transcript. Terminal messages must never come back as
 * "streaming": `streaming` is cleared and `retained` is recomputed from the
 * payload that actually exists in storage.
 */
export function restore(): { sessions: Session[]; activeId: string } | null {
  try {
    const raw = localStorage.getItem(STORE_KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed?.sessions) || parsed.sessions.length === 0) return null
    const sessions: Session[] = parsed.sessions.map((s: any) => ({
      id: String(s.id || uid()),
      title: String(s.title || 'New conversation'),
      at: Number(s.at) || Date.now(),
      messages: Array.isArray(s.messages) ? s.messages.map((m: any) => {
        const msg: Msg = { ...m, streaming: false }
        const render = renderKindOf(msg)
        const hasPayload = render === 'text' ? true
          : render === 'images' ? false // bytes never persisted
          : render === 'decision' ? !!msg.decision
          : render === 'video' ? !!msg.video?.job?.id
          : true
        return {
          ...msg,
          render,
          retained: typeof msg.retained === 'boolean' ? (msg.retained && hasPayload) : hasPayload,
          // A restored video must not resume a poll loop that can never finish.
          video: msg.video ? { ...msg.video, url: undefined } : undefined,
        }
      }) : [],
      calls: Array.isArray(s.calls) ? s.calls.filter((c: any) => c && c.id) : [],
    }))
    const activeId = sessions.some(s => s.id === parsed.activeId) ? parsed.activeId : sessions[0].id
    return { sessions, activeId }
  } catch { return null }
}

export { STORE_KEY }
