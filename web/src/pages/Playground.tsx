/**
 * Playground — a conversation-first surface for exercising the gateway.
 *
 * The page is a real chat client (sessions, history, streaming, attachments)
 * wrapped around the gateway's test surface (six API modes, per-request
 * sampling, a request inspector and a call history). Peripheral panels —
 * request configuration on the left, the inspector on the right — are
 * optional and slide in only when asked for, so the default view is a
 * full-height conversation.
 *
 * Attachments are client-side only: images are downscaled and sent as vision
 * content parts (`image_url` / Anthropic `image` / Responses `input_image`),
 * text-like files are inlined into the prompt as fenced context. Nothing is
 * uploaded to the gateway; the bytes ride the normal /v1 request.
 *
 * Layout of the code: pure protocol/formatting helpers live in
 * `src/lib/playground/`, the video job lifecycle in
 * `src/lib/playground/useVideoJobs.ts`, and the panel/message components in
 * `src/pages/playground/`. This file owns state and the request lifecycle.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, buildApiUrl } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import {
  Badge, Button, Field, Icon, Input, Modal, Select, useToast,
} from '../components/ui'
import ModelCombobox from '../components/ModelCombobox'
import { Drawer } from './playground/Drawer'
import { Inspector } from './playground/Inspector'
import { SettingsPanel, type SettingsProps } from './playground/SettingsPanel'
import { Welcome } from './playground/Welcome'
import { AttachmentChip, MessageRow } from './playground/messages'
import { MODES, modeOf } from './playground/constants'
import { estimateTokens, uid } from '../lib/playground/format'
import {
  MAX_FILES, prepareFile,
} from '../lib/playground/attachments'
import {
  anthropicContent, buildDecisionBody, deltaFrom, errorMessage, extractText, extractUsage,
  extractUsageFromObject, openAIContent, parseLevels, referenceImages, responsesContent,
  safeParse, streamErrorFrom, titleFrom, tryParseRich,
} from '../lib/playground/protocol'
import { persist, restore } from '../lib/playground/persist'
import { useVideoJobs } from '../lib/playground/useVideoJobs'
import type { Attachment, CatalogModel, Msg, QuestionDraft, Session, Mode, Call } from '../lib/playground/types'

/* ------------------------------------------------------------------ */
/* Types re-exported for the extracted modules                         */
/* ------------------------------------------------------------------ */

export type { Msg, Session, Attachment, CatalogModel }

const DEFAULT_QUESTIONS: QuestionDraft[] = [
  { id: 'urgent', type: 'noul', instructions: 'Is this support request urgent?', criteria: '' },
  { id: 'team', type: 'choice', instructions: 'Which team should handle this request?',
    criteria: 'billing: Payments, invoices, and refunds\ntechnical: Outages, errors, and configuration\nsales: Plans and upgrades' },
  { id: 'severity', type: 'score', instructions: 'How severe is the customer impact?',
    criteria: 'No impact, Minor, Major, Critical' },
]

/**
 * One column width for every band of the chat (header, tabs, thread,
 * composer). Each band is full-width with a hairline, so the rules run edge to
 * edge while the content stays aligned — the layout a real chat client uses.
 */
const CHAT_COL = 'mx-auto w-full max-w-[880px] px-3 sm:px-6 2xl:max-w-[960px] 3xl:max-w-[1040px]'

/* ------------------------------------------------------------------ */
/* Small helpers                                                       */
/* ------------------------------------------------------------------ */

function newSession(): Session {
  return { id: uid(), title: 'New conversation', at: Date.now(), messages: [], calls: [] }
}

/** Tailwind's xl / 2xl breaks, mirrored in JS so panels are rendered once. */
function useMedia(query: string): boolean {
  const [match, setMatch] = useState(() => typeof window !== 'undefined' && window.matchMedia(query).matches)
  useEffect(() => {
    const mql = window.matchMedia(query)
    const onChange = () => setMatch(mql.matches)
    onChange()
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [query])
  return match
}

/* ================================================================== */
/* Page                                                                */
/* ================================================================== */

export default function Playground() {
  const toast = useToast()
  const boot = useMemo(restore, [])
  const [sessions, setSessions] = useState<Session[]>(() => boot?.sessions ?? [newSession()])
  const [activeId, setActiveId] = useState<string>(() => boot?.activeId ?? '')
  const active = sessions.find(s => s.id === activeId) || sessions[0]

  /* ---- request configuration ---- */
  const [mode, setMode] = useState<Mode>('openai')
  const [model, setModel] = useState('')
  const [models, setModels] = useState<CatalogModel[]>([])
  const [gatewayKey, setGatewayKey] = useState('')
  const [availableKeys, setAvailableKeys] = useState<any[]>([])

  const [system, setSystem] = useState('')
  const [stream, setStream] = useState(true)
  const [temperature, setTemperature] = useState('')
  const [maxTokens, setMaxTokens] = useState('1024')
  const [reasoningEffort, setReasoningEffort] = useState('')

  /* ---- mode-specific ---- */
  const [imageN, setImageN] = useState(1)
  const [imageSize, setImageSize] = useState('1024x1024')
  const [videoSeconds, setVideoSeconds] = useState('8')
  const [videoSize, setVideoSize] = useState('1280x720')
  const [stateInput, setStateInput] = useState('Checkout has been failing for every customer for the last hour.')
  const [questions, setQuestions] = useState<QuestionDraft[]>(DEFAULT_QUESTIONS)

  /* ---- composer ---- */
  const [draft, setDraft] = useState('')
  const [attachments, setAttachments] = useState<Attachment[]>([])

  /* ---- run state ---- */
  const [running, setRunning] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const [inspector, setInspector] = useState<{ request: any; response: any; headers?: Record<string, string>; at?: number } | null>(null)
  const [tab, setTab] = useState<'metrics' | 'request' | 'response' | 'history'>('metrics')

  /* ---- chrome ---- */
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [inspectorOpen, setInspectorOpen] = useState(false)
  const [chatMenu, setChatMenu] = useState(false)
  const [renameOpen, setRenameOpen] = useState(false)
  const [renameValue, setRenameValue] = useState('')
  const [dragging, setDragging] = useState(false)
  const [advanced, setAdvanced] = useState(false)

  const threadRef = useRef<HTMLDivElement>(null)
  const taRef = useRef<HTMLTextAreaElement>(null)
  const systemRef = useRef<HTMLTextAreaElement>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const settingsTriggerRef = useRef<HTMLButtonElement>(null)
  const inspectorTriggerRef = useRef<HTMLButtonElement>(null)
  const stickRef = useRef(true)
  const dragDepth = useRef(0)

  // Panels are rendered exactly once: inline where the breakpoint allows it,
  // as an overlay drawer below it. Rendering both would duplicate every
  // control (and the system-prompt ref) in the DOM.
  const wideEnough = useMedia('(min-width: 1280px)')
  const extraWide = useMedia('(min-width: 1536px)')

  /* ---------------------------------------------------------------- */
  /* Bootstrap                                                         */
  /* ---------------------------------------------------------------- */

  useEffect(() => {
    if (!sessions.some(s => s.id === activeId)) setActiveId(sessions[0].id)
  }, [sessions, activeId])

  // Debounced: streaming patches state every animation frame, and a
  // localStorage write per frame would visibly stutter the stream.
  useEffect(() => {
    const t = setTimeout(() => persist(sessions, activeId), 900)
    return () => clearTimeout(t)
  }, [sessions, activeId])

  useEffect(() => {
    api.providerModels.list().then((d: any) => setModels(d?.data || [])).catch(() => {})
    api.keys.list().then((keys: any) => setAvailableKeys(keys || [])).catch(() => {})
  }, [])

  // Abort any in-flight stream when the page unmounts so a long generation
  // cannot keep running (and patching state) after navigation.
  useEffect(() => () => { abortRef.current?.abort() }, [])

  /* ---------------------------------------------------------------- */
  /* Model eligibility                                                 */
  /* ---------------------------------------------------------------- */

  const isImageModel = (m: CatalogModel) => m.image === true || m.kind === 'image'
  const isImageOnly = (m: CatalogModel) => m.image_only === true
  const isVideoModel = (m: CatalogModel) => m.video === true || m.kind === 'video'
  const isVideoOnly = (m: CatalogModel) => m.video_only === true

  const eligible = useCallback((m: CatalogModel, api: Mode) => {
    if (api === 'video') return isVideoModel(m)
    if (isVideoOnly(m)) return false
    if (api === 'images') return isImageModel(m)
    if (isImageOnly(m)) return false
    return api === 'systemone' ? !!m.decision : !m.decision
  }, [])

  const visibleModels = useMemo(() => models.filter(m => eligible(m, mode)), [models, mode, eligible])

  // Keep the selection consistent with the endpoint: drop a model that does not
  // belong to the current pool (mode switch) and clear it entirely when the new
  // endpoint has no candidates, so Send is disabled instead of firing the old
  // chat model at /v1/systemone.
  useEffect(() => {
    const pool = models.filter(m => eligible(m, mode))
    if (pool.length === 0) {
      if (model) setModel('')
      return
    }
    if (!pool.some(m => `${m.provider_name}/${m.model_id}` === model)) {
      setModel(`${pool[0].provider_name}/${pool[0].model_id}`)
    }
  }, [mode, models, eligible]) // eslint-disable-line react-hooks/exhaustive-deps

  const catalogInfo = visibleModels.find(m => `${m.provider_name}/${m.model_id}` === model) || null
  const reasoningLevels = parseLevels(catalogInfo?.reasoning_levels)
  const supportsReasoning = !!catalogInfo?.reasoning && reasoningLevels.length > 0

  const comboboxOptions = useMemo(() => {
    const seen = new Set<string>()
    return visibleModels
      .filter(m => { const k = `${m.provider_name}/${m.model_id}`; if (seen.has(k)) return false; seen.add(k); return true })
      .map(m => ({ value: `${m.provider_name}/${m.model_id}`, label: m.model_id, group: m.provider_name }))
  }, [visibleModels])

  const isDecision = mode === 'systemone'
  const isImageMode = mode === 'images'
  const isVideoMode = mode === 'video'
  const isChatMode = !isDecision && !isImageMode && !isVideoMode

  /* ---------------------------------------------------------------- */
  /* Session helpers                                                   */
  /* ---------------------------------------------------------------- */

  const patchSession = useCallback((sid: string, fn: (s: Session) => Session) => {
    setSessions(prev => prev.map(s => (s.id === sid ? fn(s) : s)))
  }, [])

  const patchMessage = useCallback((sid: string, mid: string, fn: (m: Msg) => Msg) => {
    setSessions(prev => prev.map(s => s.id !== sid ? s : {
      ...s, messages: s.messages.map(m => (m.id === mid ? fn(m) : m)),
    }))
  }, [])

  const newChat = useCallback(() => {
    const s = newSession()
    setSessions(prev => [...prev, s])
    setActiveId(s.id)
    setDraft(''); setAttachments([]); setInspector(null)
    requestAnimationFrame(() => taRef.current?.focus())
  }, [])

  const closeSession = useCallback((sid: string) => {
    const idx = sessions.findIndex(s => s.id === sid)
    const next = sessions.filter(s => s.id !== sid)
    if (next.length === 0) {
      const fresh = newSession()
      setSessions([fresh]); setActiveId(fresh.id)
      return
    }
    setSessions(next)
    if (sid === activeId) setActiveId(next[Math.min(idx, next.length - 1)].id)
  }, [sessions, activeId])

  // Below their inline breakpoints the panels are overlays; keep only one open
  // at a time so a narrow viewport never stacks two drawers.
  const toggleSettings = useCallback(() => {
    const next = !settingsOpen
    setSettingsOpen(next)
    if (next && !extraWide) setInspectorOpen(false)
  }, [settingsOpen, extraWide])
  const toggleInspector = useCallback(() => {
    const next = !inspectorOpen
    setInspectorOpen(next)
    if (next && !wideEnough) setSettingsOpen(false)
  }, [inspectorOpen, wideEnough])

  const clearChat = useCallback(() => {
    patchSession(active.id, s => ({ ...s, messages: [], calls: [] }))
    setInspector(null)
  }, [active?.id, patchSession])

  /* ---------------------------------------------------------------- */
  /* Auto-scroll + composer sizing                                     */
  /* ---------------------------------------------------------------- */

  const messageCount = active?.messages.length ?? 0
  const streamedText = active?.messages[active.messages.length - 1]?.text
  // `attachments` is a dependency too: sending clears it, and that clear must
  // re-pin the scroll even when the message count did not change.
  //
  // Never scroll on an empty thread: the welcome pane is taller than the
  // viewport on a phone, so pinning to the bottom cut its top off (the icon and
  // heading were clipped above the fold on first paint).
  useEffect(() => {
    const el = threadRef.current
    if (!el || !stickRef.current || messageCount === 0) return
    el.scrollTop = el.scrollHeight
  }, [messageCount, streamedText, running, activeId, attachments.length])

  const onThreadScroll = () => {
    const el = threadRef.current
    if (!el) return
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 120
  }

  useEffect(() => {
    const el = taRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 240)}px`
  }, [draft])

  // Chat-menu dismissal and the drawers share Escape. Drawers own Escape via
  // the Drawer component (which also restores focus); this only closes the
  // inline panels and the menu.
  useEffect(() => {
    if (!settingsOpen && !inspectorOpen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { setSettingsOpen(false); setInspectorOpen(false); setChatMenu(false) }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [settingsOpen, inspectorOpen])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.ctrlKey || e.metaKey)) return
      if (e.key === '/') { e.preventDefault(); setSettingsOpen(v => !v) }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])

  /* ---------------------------------------------------------------- */
  /* Attachments                                                       */
  /* ---------------------------------------------------------------- */

  const ingestFiles = useCallback(async (files: File[]) => {
    if (!files.length) return
    const room = MAX_FILES - attachments.length
    if (room <= 0) { toast.error(`Up to ${MAX_FILES} attachments per message.`); return }
    const accepted = files.slice(0, room)
    if (files.length > room) toast.info(`Only the first ${room} files were added.`)
    try {
      const prepared = await Promise.all(accepted.map(f => prepareFile(f).catch(e => ({
        id: uid(), name: f.name, size: f.size, mime: f.type, kind: 'other' as const, note: String(e?.message || e),
      }))))
      setAttachments(prev => [...prev, ...prepared])
      const skipped = prepared.filter(p => p.kind === 'other').length
      if (skipped) toast.info(`${skipped} binary file${skipped > 1 ? 's' : ''} attached by name only.`)
    } catch (e: any) {
      toast.error(String(e?.message || e))
    }
  }, [attachments.length, toast])

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault()
    dragDepth.current = 0
    setDragging(false)
    const files = Array.from(e.dataTransfer?.files || [])
    if (files.length) void ingestFiles(files)
  }
  const hasFiles = (e: React.DragEvent) => !!e.dataTransfer?.types?.includes('Files')
  // dragenter/dragleave fire in balanced pairs as the pointer crosses child
  // elements, so a depth counter is the correct overlay lifetime. dragover
  // fires continuously and must only suppress the browser's default handling.
  const onDragEnter = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    dragDepth.current += 1
    setDragging(true)
  }
  const onDragOver = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
  }
  const onDragLeave = (e: React.DragEvent) => {
    // Not gated on hasFiles(): some browsers expose no usable dataTransfer on
    // dragleave, and an unmatched decrement only ever hides the overlay.
    if (e.dataTransfer?.types?.includes('Files')) e.preventDefault()
    dragDepth.current = Math.max(0, dragDepth.current - 1)
    if (dragDepth.current === 0) setDragging(false)
  }

  // A drag that ends outside the window (or is cancelled with Esc) fires no
  // dragleave on the drop zone, which would strand the overlay on screen.
  useEffect(() => {
    const clear = () => { dragDepth.current = 0; setDragging(false) }
    window.addEventListener('dragend', clear)
    window.addEventListener('drop', clear)
    return () => { window.removeEventListener('dragend', clear); window.removeEventListener('drop', clear) }
  }, [])

  /* ---------------------------------------------------------------- */
  /* Request building                                                  */
  /* ---------------------------------------------------------------- */

  const temp = temperature === '' ? undefined : Number(temperature)
  const maxTok = maxTokens === '' ? undefined : parseInt(maxTokens, 10)
  const effort = reasoningEffort || undefined

  /** Replay prior turns, attaching images only from the last few user turns. */
  const historyFor = (s: Session, currentMsgId: string) => {
    const turns = s.messages.filter(m => (m.role === 'user' || m.role === 'assistant') && m.id !== currentMsgId && m.text.trim())
    const imageBudget = 3
    let seen = 0
    const kept = turns.map(m => ({ m, withImages: false }))
    for (let i = kept.length - 1; i >= 0; i--) {
      if (kept[i].m.role === 'user' && kept[i].m.attachments?.some(a => a.kind === 'image')) {
        if (seen < imageBudget) { kept[i].withImages = true; seen++ }
      }
    }
    return kept
  }

  function buildBody(s: Session, currentMsgId: string, text: string, atts: Attachment[]) {
    if (isImageMode) {
      // The Images API accepts reference images for image-to-image; the UI
      // promises them, so they must be in the body. `image` takes one data URL,
      // `images` up to MaxReferenceImages.
      const refs = referenceImages(atts)
      return {
        model, prompt: text, n: imageN, size: imageSize, response_format: 'b64_json',
        ...(refs.length === 1 ? { image: refs[0] } : {}),
        ...(refs.length > 1 ? { images: refs } : {}),
      }
    }
    if (isVideoMode) {
      const refs = referenceImages(atts)
      return {
        model, prompt: text, seconds: videoSeconds, size: videoSize,
        ...(refs.length ? { input_reference: refs[0] } : {}),
      }
    }
    if (isDecision) return buildDecisionBody(model, stateInput, questions)

    const hist = historyFor(s, currentMsgId)
    if (mode === 'anthropic') {
      const messages: any[] = []
      for (const { m, withImages } of hist) {
        messages.push({ role: m.role, content: anthropicContent(m.text, withImages ? (m.attachments || []) : []) })
      }
      messages.push({ role: 'user', content: anthropicContent(text, atts) })
      const body: any = { model, max_tokens: maxTok || 1024, messages }
      if (stream) body.stream = true
      if (system.trim()) body.system = system.trim()
      if (temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
      if (effort) body.thinking = { type: 'enabled', effort }
      return body
    }
    if (mode === 'responses') {
      const input: any[] = hist.map(({ m, withImages }) => ({
        role: m.role, content: withImages ? responsesContent(m.text, m.attachments || [], m.role === 'assistant' ? 'assistant' : 'user') : m.text,
      }))
      input.push({ role: 'user', content: responsesContent(text, atts) })
      const body: any = { model, input, stream }
      if (system.trim()) body.instructions = system.trim()
      if (temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
      if (maxTok && !Number.isNaN(maxTok)) body.max_output_tokens = maxTok
      if (effort) body.reasoning = { effort }
      return body
    }
    const messages: any[] = []
    if (system.trim()) messages.push({ role: 'system', content: system.trim() })
    for (const { m, withImages } of hist) {
      messages.push({ role: m.role, content: openAIContent(m.text, withImages ? (m.attachments || []) : []) })
    }
    messages.push({ role: 'user', content: openAIContent(text, atts) })
    const body: any = { model, messages, stream }
    if (temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
    if (maxTok && !Number.isNaN(maxTok)) body.max_tokens = maxTok
    if (effort) body.reasoning_effort = effort
    return body
  }

  const headers = useCallback((json = true): Record<string, string> => {
    const h: Record<string, string> = json ? { 'Content-Type': 'application/json' } : {}
    const key = gatewayKey.trim()
    if (key) h['Authorization'] = `Bearer ${key}`
    return h
  }, [gatewayKey])

  /* ---------------------------------------------------------------- */
  /* Send                                                              */
  /* ---------------------------------------------------------------- */

  const send = useCallback(async () => {
    if (running) return
    const text = draft.trim()
    const atts = attachments
    if (!text && atts.length === 0 && !isDecision) return
    if (!model) { toast.error('Select a model first.'); return }

    const sid = active.id
    const session = sessions.find(s => s.id === sid)!
    const userMsg: Msg = {
      id: uid(), role: 'user', text, at: Date.now(),
      attachments: atts.length ? atts : undefined,
    }
    const assistantMsg: Msg = {
      id: uid(), role: 'assistant', text: '', at: Date.now(),
      model, endpoint: mode, streaming: isChatMode,
      render: isImageMode ? 'images' : isVideoMode ? 'video' : isDecision ? 'decision' : 'text',
      retained: true,
    }

    const titled = session.title === 'New conversation'
    setSessions(prev => prev.map(s => s.id !== sid ? s : {
      ...s,
      title: titled ? titleFrom(text, atts) : s.title,
      messages: [...s.messages, userMsg, assistantMsg],
    }))
    setDraft('')
    setAttachments([])
    setRunning(true)
    stickRef.current = true
    setTab('metrics')

    const ctrl = new AbortController()
    abortRef.current = ctrl
    const started = performance.now()
    let ttft = 0

    const path = isImageMode ? '/v1/images/generations'
      : isVideoMode ? '/v1/videos'
      : isDecision ? '/v1/systemone'
      : mode === 'anthropic' ? '/v1/messages'
      : mode === 'responses' ? '/v1/responses'
      : '/v1/chat/completions'

    let body: any
    try { body = buildBody(session, userMsg.id, text, atts) }
    catch (e: any) {
      patchMessage(sid, assistantMsg.id, m => ({ ...m, role: 'error', streaming: false, text: String(e?.message || e) }))
      setRunning(false); abortRef.current = null
      return
    }

    const record = (status: string, response: any, extra: Partial<Call> = {}) => {
      const latencyMs = Math.round(performance.now() - started)
      patchMessage(sid, assistantMsg.id, m => ({ ...m, streaming: false, latencyMs, ttftMs: ttft || undefined }))
      const call: Call = {
        id: uid(), endpoint: mode, path, model, at: Date.now(), status,
        request: body, response, latencyMs, ttftMs: ttft || undefined, ...extra,
      }
      patchSession(sid, s => ({ ...s, calls: [call, ...s.calls].slice(0, 40) }))
      setInspector({ request: body, response, headers: extra.headers, at: Date.now() })
    }

    try {
      const res = await fetch(buildApiUrl(path), {
        method: 'POST', headers: headers(), credentials: 'same-origin',
        body: JSON.stringify(body), signal: ctrl.signal,
      })
      const respHeaders = Object.fromEntries(res.headers.entries())
      const cache = res.headers.get('x-cache') || undefined
      const fallback = res.headers.get('x-fallback-used') || undefined

      if (!res.ok) {
        const errText = await res.text()
        // Show the provider's message, not a wall of JSON; the exact envelope
        // stays available in the inspector's Response tab.
        const friendly = errorMessage(errText) || res.statusText || 'request failed'
        patchMessage(sid, assistantMsg.id, m => ({
          ...m, role: 'error', streaming: false,
          text: `HTTP ${res.status} — ${friendly}`,
        }))
        record(String(res.status), safeParse(errText) ?? { raw: errText, status: res.status }, { headers: respHeaders, cache, fallback })
        setRunning(false); abortRef.current = null
        return
      }

      /* --- images ------------------------------------------------- */
      if (isImageMode) {
        const j = await res.json()
        const list = Array.isArray(j?.data) ? j.data : []
        patchMessage(sid, assistantMsg.id, m => ({
          ...m, streaming: false, render: 'images', retained: list.length > 0, images: list,
          usage: j?.usage ? { input: j.usage.input_tokens, output: j.usage.output_tokens, total: j.usage.total_tokens } : undefined,
          text: list.length ? '' : JSON.stringify(j, null, 2),
        }))
        record('ok', j, { headers: respHeaders, cache, fallback })
        setRunning(false); abortRef.current = null
        return
      }

      /* --- video -------------------------------------------------- */
      if (isVideoMode) {
        const j = await res.json()
        patchMessage(sid, assistantMsg.id, m => ({
          ...m, streaming: false, render: 'video', retained: !!j?.id,
          video: { job: j, attempts: 0 },
        }))
        record(String(j?.status || 'queued'), j, { headers: respHeaders, cache, fallback })
        setRunning(false); abortRef.current = null
        return
      }

      /* --- decisions ---------------------------------------------- */
      if (isDecision) {
        const j = await res.json()
        patchMessage(sid, assistantMsg.id, m => ({
          ...m, streaming: false, render: 'decision', retained: !!j?.answers, decision: j,
          usage: j?.usage ? { input: j.usage.input_tokens, output: j.usage.output_tokens, total: j.usage.total_tokens } : undefined,
        }))
        record('ok', j, { headers: respHeaders, cache, fallback })
        setRunning(false); abortRef.current = null
        return
      }

      /* --- chat (streaming or buffered) ---------------------------- */
      const contentType = res.headers.get('content-type') || ''
      const isSSE = contentType.includes('text/event-stream') || stream

      if (isSSE && res.body) {
        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let pending = ''
        let buffer = ''
        let acc = ''
        let streamErr = ''
        let frame = 0
        let cancelled = false
        const flush = () => {
          frame = 0
          const snapshot = acc
          patchMessage(sid, assistantMsg.id, m => ({ ...m, text: snapshot }))
        }
        const schedule = () => {
          if (cancelled || frame) return
          frame = requestAnimationFrame(flush)
        }
        const handleLine = (line: string) => {
          // An in-band error frame can arrive AFTER text was already streamed
          // (upstream died post-commit). Record it: the partial answer must not
          // look complete.
          if (!streamErr) {
            const err = streamErrorFrom(line)
            if (err) { streamErr = err; return }
          }
          const delta = deltaFrom(line)
          if (!delta) return
          if (!ttft) ttft = Math.round(performance.now() - started)
          acc += delta
          schedule()
        }
        while (true) {
          const { done, value } = await reader.read()
          if (done) break
          const chunk = decoder.decode(value, { stream: true })
          buffer += chunk
          pending += chunk
          const nl = pending.lastIndexOf('\n')
          if (nl === -1) continue
          const complete = pending.slice(0, nl + 1)
          pending = pending.slice(nl + 1)
          complete.split('\n').forEach(handleLine)
        }
        if (pending.trim()) handleLine(pending)
        cancelled = true
        if (frame) cancelAnimationFrame(frame)

        // Fall back to whatever the payload actually was if no deltas matched.
        if (!acc.trim() && !streamErr) {
          const parsed = tryParseRich(buffer)
          acc = parsed.text
        }
        // Usage is extracted from the same buffer, independent of the error path.
        const usage = extractUsage(buffer)
        patchMessage(sid, assistantMsg.id, m => ({
          ...m,
          streaming: false,
          text: acc,
          // A mid-stream error always demotes the turn to an error message, so
          // a truncated answer can never be mistaken for a complete one.
          role: streamErr ? 'error' : m.role,
          error: streamErr || undefined,
          usage,
          retained: true,
          latencyMs: Math.round(performance.now() - started),
          ttftMs: ttft || undefined,
        }))
        record(streamErr ? 'error' : 'ok', safeParse(buffer) ?? { raw: buffer.slice(0, 4000) }, { headers: respHeaders, cache, fallback })
        if (streamErr) toast.error(`Stream failed mid-answer — ${streamErr}`)
        setRunning(false); abortRef.current = null
        return
      }

      const j = await res.json()
      const textOut = extractText(j)
      patchMessage(sid, assistantMsg.id, m => ({
        ...m, streaming: false, render: 'text', retained: true,
        text: textOut || JSON.stringify(j, null, 2),
        usage: extractUsageFromObject(j),
        latencyMs: Math.round(performance.now() - started),
      }))
      record('ok', j, { headers: respHeaders, cache, fallback })
      setRunning(false); abortRef.current = null
    } catch (e: any) {
      const aborted = e?.name === 'AbortError'
      patchMessage(sid, assistantMsg.id, m => ({
        ...m, role: aborted ? 'assistant' : 'error', streaming: false,
        text: m.text || (aborted ? 'Stopped.' : String(e?.message || e)),
      }))
      record(aborted ? 'aborted' : 'error', { error: String(e?.message || e) })
      setRunning(false); abortRef.current = null
    }
  }, [
    running, draft, attachments, model, active?.id, sessions, mode, isImageMode, isVideoMode, isDecision, isChatMode,
    imageN, imageSize, videoSeconds, videoSize, stateInput, questions, system, stream, temp, maxTok, effort, headers, patchMessage, patchSession, toast,
  ])

  const stop = useCallback(() => { abortRef.current?.abort() }, [])

  /* ---------------------------------------------------------------- */
  /* Video jobs                                                        */
  /* ---------------------------------------------------------------- */

  const videoJobs = useMemo(() => {
    const out: { sid: string; mid: string; id: string }[] = []
    for (const s of sessions) {
      for (const m of s.messages) {
        const id = m.video?.job?.id
        const status = m.video?.job?.status
        if (!id) continue
        // Terminal status, an explicit stop marker, or a preview already
        // resolved all end the poll lifecycle.
        if (status === 'completed' || status === 'failed') continue
        if (m.video?.fatal || m.video?.url) continue
        out.push({ sid: s.id, mid: m.id, id })
      }
    }
    return out
  }, [sessions])

  const completedVideos = useMemo(() => {
    const out: { sid: string; mid: string; id: string }[] = []
    for (const s of sessions) {
      for (const m of s.messages) {
        if (m.video?.job?.id && m.video.job.status === 'completed' && !m.video.url && !m.video.error) {
          out.push({ sid: s.id, mid: m.id, id: m.video.job.id })
        }
      }
    }
    return out
  }, [sessions])

  const liveMessageIds = useMemo(
    () => sessions.flatMap(s => s.messages.map(m => m.id)),
    [sessions],
  )

  // A gateway-key override mints authenticated blob previews; when the key
  // changes those URLs are stale (the hook revokes them) and the messages must
  // forget them so the content fetch runs again with the new credential.
  const keyRef = useRef(gatewayKey)
  useEffect(() => {
    if (keyRef.current === gatewayKey) return
    keyRef.current = gatewayKey
    setSessions(prev => prev.map(s => ({
      ...s,
      messages: s.messages.map(m => (m.video && m.video.direct === false && m.video.url)
        ? { ...m, video: { ...m.video, url: undefined, direct: undefined } }
        : m),
    })))
  }, [gatewayKey])

  const { retryVideo, fallbackToBlob } = useVideoJobs({
    jobs: videoJobs,
    completed: completedVideos,
    patchMessage,
    buildHeaders: () => headers(false),
    authOverride: !!gatewayKey.trim(),
    authVersion: gatewayKey.trim(),
    liveMessageIds,
  })

  /* ---------------------------------------------------------------- */
  /* Render                                                            */
  /* ---------------------------------------------------------------- */

  const sendLabel = isImageMode ? 'Generate' : isVideoMode ? 'Generate' : 'Send'
  const activeMode = modeOf(mode)
  const draftTokens = estimateTokens(draft) + attachments.reduce((n, a) => n + (a.kind === 'text' ? estimateTokens(a.text || '') : 0), 0)
  const lastAssistant = [...(active?.messages || [])].reverse().find(m => m.role === 'assistant' && m.text)
  const modelRequired = visibleModels.length > 0 || !!model
  // Decision mode takes its input from the State editor, so an empty composer
  // is a valid request there; every other mode needs a prompt or an attachment.
  const hasInput = isDecision || !!draft.trim() || attachments.length > 0
  const canSend = hasInput && !!model

  const resetSettings = () => {
    setSystem(''); setStream(true); setTemperature(''); setMaxTokens('1024')
    setReasoningEffort(''); setAdvanced(false)
    toast.success('Request configuration reset')
  }

  // Shared by the inline column and the narrow-viewport drawer so the two can
  // never drift apart.
  const settingsProps: SettingsProps = {
    mode, setMode, isChatMode, isDecision, isImageMode, isVideoMode,
    model, catalogInfo, visibleModels, comboboxOptions, setModel,
    system, setSystem, systemRef, stream, setStream,
    temperature, setTemperature, maxTokens, setMaxTokens,
    reasoningEffort, setReasoningEffort, supportsReasoning, reasoningLevels,
    imageN, setImageN, imageSize, setImageSize,
    videoSeconds, setVideoSeconds, videoSize, setVideoSize,
    stateInput, setStateInput, questions, setQuestions,
    gatewayKey, setGatewayKey, availableKeys, advanced, setAdvanced,
    onClose: () => setSettingsOpen(false), onReset: resetSettings,
  }

  const inspectorProps = {
    tab, setTab, data: inspector, running, session: active,
    onClose: () => setInspectorOpen(false),
    onPick: (call: Call) => setInspector({ request: call.request, response: call.response, headers: call.headers, at: call.at }),
    toast,
    // null = nothing selected yet (prompt the user); a call with no payload =
    // restored from storage and honestly labelled as such.
    payloadRetained: inspector === null || !!inspector.request || !!inspector.response,
  }

  return (
    <div
      className="flex flex-col bg-app overflow-hidden"
      style={{ height: 'calc(100dvh - 3.5rem)' }}
      onDrop={onDrop} onDragEnter={onDragEnter} onDragOver={onDragOver} onDragLeave={onDragLeave}
    >
      {/* The shell renders one h1 per page; the playground is a full-height
          surface with no visible page title, so give assistive tech the
          landmark heading it expects. */}
      <h1 className="sr-only">Playground</h1>
      {/* ---------------------------------------------------------- */}
      {/* Quickbar                                                    */}
      {/* ---------------------------------------------------------- */}
      <div className="shrink-0 border-b border-stone bg-surface">
      <div className={`${CHAT_COL} py-2 flex flex-wrap items-center gap-2`}>
        <div className="flex items-center gap-1.5 min-w-0">
          <span className="hidden sm:inline text-[10px] font-semibold uppercase tracking-[0.14em] text-muted shrink-0">API</span>
          <Select
            value={mode}
            onChange={e => setMode(e.target.value as Mode)}
            className="!h-8 !w-auto !text-xs !py-0 pr-7 min-w-[112px] max-w-[190px]"
            aria-label="API endpoint"
          >
            {MODES.map(m => <option key={m.id} value={m.id}>{m.label}</option>)}
          </Select>
          <span className="hidden md:block w-px h-5 bg-stone shrink-0" />
        </div>
        {/* On phones the picker drops to its own full-width row so a long
            provider/model chip can never squash the action buttons. */}
        <div className="order-last w-full xl:order-none xl:w-auto xl:flex-1 min-w-0 max-w-full xl:max-w-[420px]">
          <ModelCombobox
            mode="single"
            value={model ? [model] : []}
            onChange={next => setModel(next.length ? next[next.length - 1] : '')}
            options={comboboxOptions}
            chipLabel={v => v.split('/').slice(1).join('/') || v}
            placeholder={model ? '' : (visibleModels.length ? 'Select a model' : 'No models discovered')}
            emptyHint="No models match this endpoint. Run Discover on the Models page."
            ariaLabel="Model"
          />
        </div>
        {/* Capability badges live in the settings panel; repeating them here
            squeezed the model picker into a two-row wrap at 1280–1600px. */}
        <span className="hidden 3xl:flex items-center gap-1.5">
          {catalogInfo?.attachment && <Badge tone="neutral">vision</Badge>}
          {catalogInfo?.reasoning && <Badge tone="neutral">reasoning</Badge>}
        </span>
        <div className="flex items-center gap-1.5 ml-auto">
          <Button size="sm" variant="ghost" onClick={toggleSettings}
            className={`!px-2 ${settingsOpen ? '!text-paper !bg-raised' : ''}`} title="Request configuration (Ctrl/Cmd + /)"
            aria-expanded={settingsOpen} buttonRef={settingsTriggerRef}>
            <Icon name="sliders" size={14} /><span className="hidden sm:inline">Parameters</span>
          </Button>
          <Button size="sm" variant="ghost" onClick={toggleInspector}
            className={`!px-2 ${inspectorOpen ? '!text-paper !bg-raised' : ''}`} title="Request inspector"
            aria-expanded={inspectorOpen} buttonRef={inspectorTriggerRef}>
            <Icon name="panelRight" size={14} /><span className="hidden sm:inline">Inspector</span>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => exportSession(active, { mode, model, system, temperature, maxTokens, reasoningEffort, stream })} className="!px-2" title="Export this conversation">
            <Icon name="download" size={14} /><span className="hidden lg:inline">Export</span>
          </Button>
          <Button size="sm" variant="primary" onClick={newChat} className="!px-2.5" title="New conversation">
            <Icon name="plus" size={14} /><span className="hidden sm:inline">New chat</span>
          </Button>
        </div>
      </div>
      </div>

      {/* ---------------------------------------------------------- */}
      {/* Body                                                        */}
      {/* ---------------------------------------------------------- */}
      <div className="flex-1 min-h-0 flex relative">
        {settingsOpen && wideEnough && (
          <aside className="hidden xl:flex w-[320px] shrink-0 border-r border-stone bg-surface flex-col min-h-0" aria-label="Request configuration">
            <SettingsPanel {...settingsProps} />
          </aside>
        )}

        {/* ---------------- chat ---------------- */}
        <section className="flex-1 min-w-0 flex flex-col bg-app" aria-label="Conversation">
          {/* chat header */}
          <div className="shrink-0 border-b border-stone bg-surface/60">
          <div className={`${CHAT_COL} h-11 flex items-center gap-2`}>
            <span className="w-1.5 h-1.5 rounded-full shrink-0 bg-accent" aria-hidden="true" />
            <button onClick={() => { setRenameValue(active?.title || ''); setRenameOpen(true) }}
              className="text-[13px] font-semibold -tracking-tight truncate hover:text-accent transition-colors text-left min-w-0">
              {active?.title || 'New conversation'}
            </button>
            <Badge tone="info">{activeMode.short}</Badge>
            <span className="hidden sm:block font-mono text-[10px] text-muted truncate max-w-[240px]">{model || '—'}</span>
            <div className="ml-auto flex items-center gap-1">
              {isChatMode && <Badge tone={stream ? 'good' : 'neutral'} dot={stream}>{stream ? 'stream' : 'buffered'}</Badge>}
              <Button size="sm" variant="ghost" className="!px-1.5" title="Copy last answer"
                onClick={async () => {
                  if (!lastAssistant) { toast.info('No answer to copy yet'); return }
                  const ok = await copyToClipboard(lastAssistant.text)
                  if (ok) toast.success('Copied'); else toast.error('Copy failed')
                }}>
                <Icon name="copy" size={14} /><span className="sr-only">Copy last answer</span>
              </Button>
              <div className="relative">
                <Button size="sm" variant="ghost" className="!px-1.5" title="Conversation options" onClick={() => setChatMenu(v => !v)}
                  aria-haspopup="menu" aria-expanded={chatMenu}>
                  <Icon name="chevronDown" size={14} /><span className="sr-only">Conversation options</span>
                </Button>
                {chatMenu && (
                  <>
                    <div className="fixed inset-0 z-30" onClick={() => setChatMenu(false)} aria-hidden="true" />
                    <div role="menu" aria-label="Conversation options" className="absolute right-0 top-9 z-40 w-52 rounded-lg border border-stone bg-surface shadow-pop p-1.5">
                      <MenuItem icon="pencil" onClick={() => { setChatMenu(false); setRenameValue(active?.title || ''); setRenameOpen(true) }}>Rename</MenuItem>
                      <MenuItem icon="copy" onClick={() => { setChatMenu(false); duplicateSession(active, setSessions, setActiveId) }}>Duplicate</MenuItem>
                      <MenuItem icon="download" onClick={() => { setChatMenu(false); exportSession(active, { mode, model }) }}>Export JSON</MenuItem>
                      <div className="my-1 h-px bg-stone" role="separator" />
                      <MenuItem icon="trash" danger onClick={() => { setChatMenu(false); clearChat() }}>Clear messages</MenuItem>
                      <MenuItem icon="x" danger disabled={sessions.length <= 1} onClick={() => { setChatMenu(false); closeSession(active.id) }}>Delete conversation</MenuItem>
                    </div>
                  </>
                )}
              </div>
            </div>
          </div>
          </div>

          {/* session tabs */}
          <div className="shrink-0 border-b border-stone bg-surface/30">
          <div className={`${CHAT_COL} flex items-center gap-1 py-1.5 overflow-x-auto hide-scrollbar`} role="tablist" aria-label="Conversations">
            {sessions.map(s => (
              // A real button, not a div with onClick: reachable and operable
              // by keyboard, announced as a tab.
              <div key={s.id} role="presentation"
                className={`group shrink-0 flex items-center gap-1 rounded-lg pl-2.5 pr-1 h-7 text-xs border transition-colors max-w-[220px] ${
                  s.id === activeId ? 'border-accent/40 bg-accent/10 text-paper' : 'border-stone bg-app text-muted hover:text-paper'}`}>
                <button
                  role="tab"
                  aria-selected={s.id === activeId}
                  onClick={() => setActiveId(s.id)}
                  className="flex items-center gap-1 min-w-0 h-full text-left"
                  title={s.title}>
                  <Icon name="messageSquare" size={11} className="shrink-0 opacity-70" />
                  <span className="truncate max-w-[150px]">{s.title}</span>
                  <span className="font-mono text-[10px] opacity-60">{s.messages.length || ''}</span>
                </button>
                <button
                  onClick={e => { e.stopPropagation(); closeSession(s.id) }}
                  className="w-5 h-5 rounded flex items-center justify-center opacity-0 group-hover:opacity-100 focus-visible:opacity-100 focus:opacity-100 hover:bg-stone/60 transition-opacity"
                  aria-label={`Close ${s.title}`}>
                  <Icon name="x" size={11} />
                </button>
              </div>
            ))}
            <button onClick={newChat} className="shrink-0 w-7 h-7 rounded-lg border border-dashed border-stone text-muted hover:text-paper hover:border-accent/50 flex items-center justify-center transition-colors" title="New conversation" aria-label="New conversation">
              <Icon name="plus" size={13} />
            </button>
          </div>
          </div>

          {/* thread */}
          <div ref={threadRef} onScroll={onThreadScroll} className="flex-1 min-h-0 overflow-y-auto overscroll-contain">
            {/* Transcript is top-anchored (the convention for model chat) and
                the empty state is centred in the viewport instead. */}
            <div className={`${CHAT_COL} flex min-h-full flex-col py-5`}>
              <div className={(!active || active.messages.length === 0) ? 'my-auto' : 'space-y-6'}>
              {(!active || active.messages.length === 0) ? (
                <Welcome
                  mode={mode}
                  setMode={setMode}
                  model={model}
                  hasModels={visibleModels.length > 0}
                  onPick={t => { setDraft(t); requestAnimationFrame(() => taRef.current?.focus()) }}
                  onAttach={() => fileRef.current?.click()}
                  onOpenSettings={() => setSettingsOpen(true)}
                />
              ) : (
                active.messages.map((m, i) => (
                  <MessageRow
                    key={m.id}
                    msg={m}
                    isLast={i === active.messages.length - 1}
                    onRetry={() => {
                      const prev = [...active.messages.slice(0, i)].reverse().find(x => x.role === 'user')
                      if (!prev) return
                      setDraft(prev.text)
                      setAttachments((prev.attachments || []).filter(a => a.kind !== 'image' || a.dataUrl))
                      requestAnimationFrame(() => taRef.current?.focus())
                    }}
                    onUse={() => { setDraft(m.text); requestAnimationFrame(() => taRef.current?.focus()) }}
                    onCopy={() => { void copyToClipboard(m.text).then(ok => ok ? toast.success('Copied') : toast.error('Copy failed')) }}
                    onRetryVideo={m.video?.job?.id ? () => {
                      // A direct-URL preview that the media element refused gets
                      // one authenticated blob attempt; anything else re-polls.
                      if (m.video?.direct && m.video?.url) void fallbackToBlob(active.id, m.id, m.video.job.id)
                      else retryVideo(active.id, m.id)
                    } : undefined}
                  />
                ))
              )}
              </div>
            </div>
          </div>

          {/* composer */}
          <div className="shrink-0 border-t border-stone bg-surface pt-2.5 pb-3">
            <div className={CHAT_COL}>
              {attachments.length > 0 && (
                <div className="flex flex-wrap gap-1.5 mb-2">
                  {attachments.map(a => (
                    <AttachmentChip key={a.id} a={a} onRemove={() => setAttachments(prev => prev.filter(x => x.id !== a.id))} />
                  ))}
                </div>
              )}
              <div className="rounded-xl border border-stone bg-app focus-within:border-accent focus-within:ring-2 focus-within:ring-accent/15 transition-colors">
                <textarea
                  ref={taRef}
                  value={draft}
                  onChange={e => setDraft(e.target.value)}
                  onKeyDown={e => {
                    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void send() }
                  }}
                  onPaste={e => {
                    const files = Array.from(e.clipboardData?.files || [])
                    if (files.length) { e.preventDefault(); void ingestFiles(files) }
                  }}
                  rows={1}
                  aria-label="Message"
                  placeholder={composerPlaceholder(mode)}
                  className="w-full resize-none bg-transparent outline-none text-sm leading-relaxed px-3.5 pt-3 pb-1 placeholder:text-muted max-h-[240px]"
                />
                <div className="flex items-center gap-1.5 px-2 pb-2">
                  <button onClick={() => fileRef.current?.click()} title="Attach files"
                    className="h-7 px-2 rounded-lg text-xs font-medium text-muted hover:text-paper hover:bg-raised inline-flex items-center gap-1.5 transition-colors">
                    <Icon name="paperclip" size={14} /><span className="hidden sm:inline">Attach</span>
                  </button>
                  <button onClick={() => { setSettingsOpen(true); requestAnimationFrame(() => systemRef.current?.focus()) }} title="System instructions"
                    className={`h-7 px-2 rounded-lg text-xs font-medium inline-flex items-center gap-1.5 transition-colors ${system.trim() ? 'text-accent bg-accent/10' : 'text-muted hover:text-paper hover:bg-raised'}`}>
                    <Icon name="cog" size={14} /><span className="hidden sm:inline">System</span>
                  </button>
                  <button onClick={() => setSettingsOpen(true)} title="Request parameters"
                    className="h-7 px-2 rounded-lg text-xs font-medium text-muted hover:text-paper hover:bg-raised inline-flex items-center gap-1.5 transition-colors">
                    <Icon name="sliders" size={14} />
                    <span className="hidden md:inline">{paramSummary(mode, { stream, temperature, maxTokens, reasoningEffort })}</span>
                  </button>

                  <div className="ml-auto flex items-center gap-2">
                    <span className="hidden sm:inline font-mono text-[10px] text-muted tabular-nums">~{draftTokens} tok</span>
                    {running ? (
                      <Button variant="secondary" size="sm" onClick={stop} title="Stop generating">
                        <Icon name="stop" size={13} /> Stop
                      </Button>
                    ) : (
                      <Button variant="primary" size="sm" onClick={() => void send()} disabled={!canSend}
                        title={!model ? 'Select a model first' : `${sendLabel} (Enter)`}>
                        <Icon name="send" size={13} /> {sendLabel}
                      </Button>
                    )}
                  </div>
                </div>
              </div>
              <div className="mt-1.5 flex items-center gap-3 text-[11px] text-muted">
                <span className="truncate">
                  {activeMode.path} · Enter to send · Shift+Enter for a newline
                </span>
                <span className="ml-auto shrink-0 hidden sm:flex items-center gap-1.5">
                  <Icon name="paperclip" size={11} /> drop or paste files
                </span>
              </div>
              {!model && !modelRequired && (
                <div className="mt-1 text-[11px] text-amber" role="status">
                  No model is eligible for this endpoint — nothing can be sent. Run Discover on the Models page.
                </div>
              )}
            </div>
          </div>
        </section>

        {inspectorOpen && extraWide && (
          <aside className="hidden 2xl:flex w-[340px] shrink-0 border-l border-stone bg-surface flex-col min-h-0" aria-label="Request inspector">
            <Inspector {...inspectorProps} />
          </aside>
        )}
      </div>

      {/* ---------------------------------------------------------- */}
      {/* Narrow-viewport drawers (overlays below the inline breaks)  */}
      {/* ---------------------------------------------------------- */}
      {settingsOpen && !wideEnough && (
        <Drawer open onClose={() => setSettingsOpen(false)} label="Request configuration" side="left"
          returnFocus={settingsTriggerRef}>
          <SettingsPanel {...settingsProps} />
        </Drawer>
      )}
      {inspectorOpen && !extraWide && (
        <Drawer open onClose={() => setInspectorOpen(false)} label="Request inspector" side="right"
          returnFocus={inspectorTriggerRef}>
          <Inspector {...inspectorProps} />
        </Drawer>
      )}

      {/* Drop overlay */}
      {dragging && (
        <div className="fixed inset-0 z-50 pointer-events-none flex items-center justify-center bg-app/80 backdrop-blur-sm animate-fade" role="presentation">
          <div className="rounded-xl border-2 border-dashed border-accent/60 bg-surface px-8 py-6 text-center shadow-pop">
            <Icon name="paperclip" size={22} className="mx-auto text-accent" />
            <div className="mt-2 font-semibold text-sm">Drop files to attach</div>
            <div className="text-xs text-muted mt-1">
              {isDecision ? 'Attachments are not used by decisions'
                : isImageMode || isVideoMode ? 'The first image becomes the reference image · text files are inlined as context'
                : 'Images become vision input · text files are inlined as context'}
            </div>
          </div>
        </div>
      )}

      <input
        ref={fileRef} type="file" multiple hidden
        accept={`image/*,.txt,.md,.json,.jsonl,.yaml,.yml,.toml,.ini,.csv,.tsv,.log,.xml,.html,.css,.js,.jsx,.ts,.tsx,.py,.rb,.go,.rs,.java,.kt,.swift,.c,.cc,.cpp,.h,.hpp,.cs,.php,.sh,.bash,.sql,.graphql,.proto`}
        onChange={e => { const files = Array.from(e.target.files || []); e.target.value = ''; void ingestFiles(files) }}
      />

      <Modal open={renameOpen} onClose={() => setRenameOpen(false)} title="Rename conversation" width="max-w-md">
        <Field label="Title">
          <Input value={renameValue} onChange={e => setRenameValue(e.target.value)} autoFocus
            onKeyDown={e => { if (e.key === 'Enter') { patchSession(active.id, s => ({ ...s, title: renameValue.trim() || 'New conversation' })); setRenameOpen(false) } }} />
        </Field>
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={() => setRenameOpen(false)}>Cancel</Button>
          <Button variant="primary" onClick={() => { patchSession(active.id, s => ({ ...s, title: renameValue.trim() || 'New conversation' })); setRenameOpen(false) }}>Save</Button>
        </div>
      </Modal>
    </div>
  )
}

/* ================================================================== */
/* Chrome pieces                                                       */
/* ================================================================== */

function MenuItem({ children, icon, onClick, danger, disabled }: {
  children: React.ReactNode; icon: any; onClick: () => void; danger?: boolean; disabled?: boolean
}) {
  return (
    <button onClick={onClick} disabled={disabled} role="menuitem"
      className={`w-full flex items-center gap-2 px-2.5 h-8 rounded-md text-[13px] text-left transition-colors disabled:opacity-40 disabled:pointer-events-none ${
        danger ? 'text-danger hover:bg-danger/10' : 'text-paper hover:bg-raised'}`}>
      <Icon name={icon} size={13} /> {children}
    </button>
  )
}

function paramSummary(mode: Mode, p: { stream: boolean; temperature: string; maxTokens: string; reasoningEffort: string }): string {
  if (mode === 'images' || mode === 'video' || mode === 'systemone') return 'Parameters'
  const bits: string[] = []
  if (p.temperature !== '') bits.push(`t=${p.temperature}`)
  if (p.maxTokens) bits.push(`max=${p.maxTokens}`)
  if (p.reasoningEffort) bits.push(p.reasoningEffort)
  if (!p.stream) bits.push('buffered')
  return bits.length ? bits.join(' · ') : 'Parameters'
}

function composerPlaceholder(mode: Mode): string {
  switch (mode) {
    case 'images': return 'Describe the image you want to generate…'
    case 'video': return 'Describe a scene, motion, camera movement and style…'
    // Decisions are not conversational: the evaluated state lives in the
    // request panel, so the composer is only used for attachments.
    case 'systemone': return 'Optional note — the decision input is the State field in Parameters…'
    default: return 'Ask anything, or drop a file to attach it…'
  }
}

function exportSession(session: Session, config: any) {
  const payload = {
    exported_at: new Date().toISOString(),
    conversation: session.title,
    configuration: config,
    messages: session.messages.map(m => ({
      role: m.role, at: new Date(m.at).toISOString(), model: m.model, endpoint: m.endpoint,
      text: m.text, usage: m.usage, latency_ms: m.latencyMs, ttft_ms: m.ttftMs,
      attachments: m.attachments?.map(a => ({ name: a.name, size: a.size, mime: a.mime, kind: a.kind })),
      decision: m.decision,
    })),
    calls: session.calls,
  }
  const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `playground-${session.title.replace(/[^a-z0-9]+/gi, '-').toLowerCase().slice(0, 40) || 'session'}.json`
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 2000)
}

function duplicateSession(session: Session, setSessions: React.Dispatch<React.SetStateAction<Session[]>>, setActiveId: (id: string) => void) {
  const copy: Session = {
    id: uid(), title: `${session.title} copy`, at: Date.now(),
    messages: session.messages.map(m => ({ ...m, id: uid(), streaming: false })),
    calls: [],
  }
  setSessions(prev => [...prev, copy])
  setActiveId(copy.id)
}
