/**
 * Shared types for the Playground surface.
 *
 * Everything here is data-only: no React, no DOM. The protocol helpers
 * (`protocol.ts`), the attachment pipeline (`attachments.ts`) and the
 * persistence layer (`persist.ts`) all speak these shapes, which keeps the
 * 2k-line page component from being the only place the transcript schema
 * exists.
 */

export type Mode = 'openai' | 'anthropic' | 'responses' | 'systemone' | 'images' | 'video'

export type AttachKind = 'image' | 'text' | 'other'

export type Attachment = {
  id: string
  name: string
  size: number
  mime: string
  kind: AttachKind
  /** Downscaled data URL for images. */
  dataUrl?: string
  /** Extracted text for text-like files. */
  text?: string
  /** Set when the file was skipped or truncated, shown on the chip. */
  note?: string
}

export type Usage = { input?: number; output?: number; total?: number; cached?: number }

export type Call = {
  id: string
  endpoint: Mode
  path: string
  model: string
  at: number
  status: string
  request: any
  response: any
  headers?: Record<string, string>
  latencyMs?: number
  ttftMs?: number
  cache?: string
  fallback?: string
}

/**
 * What the assistant turn actually produced. Persisted so a restored message
 * renders the right branch even though its payload (base64 images, decision
 * body, video bytes) is not persisted.
 */
export type RenderKind = 'text' | 'images' | 'video' | 'decision'

/**
 * Client-side video job state. `job` is the gateway's job resource; the rest is
 * what the browser learned about previewing it.
 *
 * - `url` is a blob: object URL minted when a gateway-key override forces an
 *   authenticated fetch (the plain content URL is used otherwise, so the
 *   element's own request carries the session cookie).
 * - `error` is a client-side failure (poll or content fetch), rendered
 *   unconditionally by VideoCard.
 * - `fatal` means "stop polling": either the job can never progress (404/410,
 *   other 4xx) or the attempt budget was exhausted.
 * - `direct` marks a same-origin content URL (no blob) so the media element's
 *   own request carries the session cookie and `media-src 'self'` applies.
 * - `attempts` counts consecutive poll failures; it drives the backoff.
 */
export type VideoState = {
  job: any
  url?: string
  direct?: boolean
  error?: string
  fatal?: boolean
  attempts?: number
}

export type Msg = {
  id: string
  role: 'user' | 'assistant' | 'error'
  text: string
  at: number
  attachments?: Attachment[]
  model?: string
  endpoint?: Mode
  streaming?: boolean
  usage?: Usage
  latencyMs?: number
  ttftMs?: number
  /**
   * In-band failure that arrived after text was already streamed (the gateway
   * sends `{"error":{...}}` post-commit). Kept separate from `text` so a long
   * partial answer stays readable prose and the failure still gets its own
   * announced strip.
   */
  error?: string
  /** Render discriminator — see RenderKind. */
  render?: RenderKind
  /**
   * False when the generated payload did not survive persistence (base64
   * images, decision body) or never existed. MessageRow renders an explicit
   * placeholder instead of a loading indicator in that case.
   */
  retained?: boolean
  /** System One typed answers. */
  decision?: any
  /** Generated images (b64_json or url). */
  images?: any[]
  /** Video job snapshot + preview URL (or the reason the preview failed). */
  video?: VideoState
}

export type Session = {
  id: string
  title: string
  at: number
  messages: Msg[]
  calls: Call[]
}

export type CatalogModel = {
  provider_name?: string
  model_id: string
  display_name?: string
  context_window?: number
  max_output?: number
  reasoning?: boolean
  reasoning_type?: string
  reasoning_levels?: string
  tool_call?: boolean
  attachment?: boolean
  decision?: boolean
  image?: boolean
  video?: boolean
  image_only?: boolean
  video_only?: boolean
  input_cost?: number
  output_cost?: number
  image_cost?: number
  video_cost?: number
  kind?: string
}

export type QType = 'noul' | 'choice' | 'score'
export type QuestionDraft = { id: string; type: QType; instructions: string; criteria: string }
