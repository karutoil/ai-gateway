/**
 * Transcript message components.
 *
 * The central rule here: a *terminal* message never renders as loading. Every
 * branch picks between the real payload and an explicit placeholder, and the
 * loading indicator is gated on `msg.streaming` alone. That matters because
 * persistence cannot keep base64 images, decision bodies are size-capped and
 * video bytes are blob URLs — a restored message has to say "not retained"
 * rather than pretend it is still generating.
 */
import { useMemo, useState } from 'react'
import { Badge, Button, Icon, Progress } from '../../../components/ui'
import { copyToClipboard } from '../../../lib/clipboard'
import { fmtBytes, fmtTime, mediaErrorMessage, pct, splitFences } from '../../../lib/playground/format'
import type { Attachment, Msg, RenderKind, VideoState } from '../../../lib/playground/types'
import { modeOf } from '../constants'

/** Discriminator for the rendering branch — mirrors what the message produced. */
export function renderKind(msg: Msg): RenderKind {
  if (msg.render) return msg.render
  if (msg.decision) return 'decision'
  if (msg.images && msg.images.length > 0) return 'images'
  if (msg.video) return 'video'
  return 'text'
}

export function MessageRow({ msg, isLast, onRetry, onUse, onCopy, onRetryVideo }: {
  msg: Msg; isLast: boolean; onRetry: () => void; onUse: () => void; onCopy: () => void
  onRetryVideo?: () => void
}) {
  if (msg.role === 'user') {
    return (
      <div className="flex justify-end animate-page">
        <div className="max-w-[min(85%,640px)] min-w-0">
          {msg.attachments && msg.attachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5 justify-end mb-1.5">
              {msg.attachments.map(a => <AttachmentChip key={a.id} a={a} compact />)}
            </div>
          )}
          {msg.text && (
            <div className="rounded-xl rounded-br-md bg-accent text-onaccent px-3.5 py-2.5 text-sm leading-relaxed whitespace-pre-wrap break-words">
              {msg.text}
            </div>
          )}
          <div className="mt-1 text-right font-mono text-[10px] text-muted">{fmtTime(msg.at)}</div>
        </div>
      </div>
    )
  }

  const errored = msg.role === 'error'
  const kind = renderKind(msg)
  const streaming = msg.streaming === true
  const imageCount = kind === 'images' ? (msg.images?.length || 0) : 0

  return (
    <div className="group/msg flex gap-3 animate-page">
      <div className={`w-7 h-7 rounded-lg shrink-0 flex items-center justify-center border ${
        errored ? 'bg-danger/10 border-danger/30 text-danger' : 'bg-accent/10 border-accent/25 text-accent'}`}>
        <Icon name={errored ? 'alert' : 'sparkles'} size={14} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap mb-1">
          <span className="font-mono text-[11px] text-muted truncate max-w-[280px]">{msg.model || 'gateway'}</span>
          {msg.endpoint && <span className="font-mono text-[10px] text-muted border border-stone rounded-full px-1.5 py-px">{modeOf(msg.endpoint).short}</span>}
          {streaming && (
            <span className="font-mono text-[10px] text-accent animate-pulse-soft" aria-live="polite">streaming…</span>
          )}
        </div>

        {kind === 'decision' ? (
          msg.decision
            ? <DecisionAnswers data={msg.decision} />
            : <NotRetained what="Decision answers" hint="They were not kept with this transcript — re-run the request to see them again." />
        ) : kind === 'images' ? (
          imageCount > 0 && (msg.images || []).every(d => d?.b64_json || d?.url)
            ? <ImageGrid images={msg.images!} />
            : <NotRetained
                what={imageCount > 0 ? `${imageCount} generated image${imageCount > 1 ? 's' : ''}` : 'The generated image'}
                hint="Image bytes are not stored in the transcript, so they cannot be shown after a reload. Re-run the prompt to generate again."
              />
        ) : kind === 'video' ? (
          <VideoCard video={msg.video || { job: {} }} onRetry={onRetryVideo} />
        ) : errored ? (
          // Two shapes share the error role: a request-level failure (only text)
          // and a mid-stream failure after a partial answer (text + error). The
          // partial answer stays normal prose; the failure gets its own
          // announced strip rather than turning 40 lines of output red.
          msg.error && msg.text ? (
            <div className="space-y-2">
              <div className="py-0.5"><RichText text={msg.text} /></div>
              <div className="rounded-lg border border-danger/30 bg-surface px-3 py-2 text-sm text-danger" role="alert">
                ⚠ {msg.error}
                <span className="block text-xs text-muted mt-1">The answer above is incomplete — the stream failed part-way through.</span>
              </div>
            </div>
          ) : (
            <div className="rounded-xl border border-danger/30 bg-surface px-3.5 py-3" role="alert">
              <RichText text={msg.error ? `⚠ ${msg.error}` : msg.text} error />
            </div>
          )
        ) : (
          // Assistant prose is not boxed: a bordered slab around every reply
          // reads like a card grid, not a conversation.
          <div className="py-0.5">
            {msg.text
              ? <RichText text={msg.text} />
              : streaming
                ? <TypingDots />
                : <NotRetained what="This answer" hint="No text was returned, and nothing was stored for it." />}
          </div>
        )}

        {!streaming && (msg.usage || msg.latencyMs) && (
          <div className="mt-1.5 flex items-center gap-3 flex-wrap font-mono text-[10px] text-muted">
            {msg.usage?.input !== undefined && <span>in {msg.usage.input}</span>}
            {msg.usage?.output !== undefined && <span>out {msg.usage.output}</span>}
            {msg.usage?.total !== undefined && <span>total {msg.usage.total}</span>}
            {msg.ttftMs !== undefined && <span>ttft {msg.ttftMs}ms</span>}
            {msg.latencyMs !== undefined && <span>{msg.latencyMs}ms</span>}
          </div>
        )}

        {!streaming && msg.text && (
          <div className="mt-1 flex items-center gap-0.5 -ml-1.5 opacity-100 transition-opacity sm:opacity-0 sm:group-hover/msg:opacity-100 sm:focus-within:opacity-100">
            <Button size="sm" variant="ghost" className="!h-7 !px-1.5 !text-[11px]" onClick={onCopy}><Icon name="copy" size={12} /> Copy</Button>
            {isLast && <Button size="sm" variant="ghost" className="!h-7 !px-1.5 !text-[11px]" onClick={onRetry}><Icon name="refresh" size={12} /> Retry</Button>}
            <Button size="sm" variant="ghost" className="!h-7 !px-1.5 !text-[11px]" onClick={onUse}><Icon name="arrowRight" size={12} /> Use as prompt</Button>
          </div>
        )}
      </div>
    </div>
  )
}

/** Explicit stand-in for a payload that is gone. Never a loading indicator. */
function NotRetained({ what, hint }: { what: string; hint: string }) {
  return (
    <div className="rounded-xl border border-dashed border-stone bg-surface/60 px-3.5 py-3">
      <div className="flex items-center gap-2 text-[13px] font-medium text-muted">
        <Icon name="box" size={13} /> {what} — not retained
      </div>
      <div className="mt-1 text-xs text-muted leading-relaxed">{hint}</div>
    </div>
  )
}

function TypingDots() {
  return (
    <div className="flex items-center gap-1 h-5" role="status" aria-label="Generating a response">
      {[0, 1, 2].map(i => (
        <span key={i} className="w-1.5 h-1.5 rounded-full bg-muted animate-pulse-soft" style={{ animationDelay: `${i * 160}ms` }} />
      ))}
    </div>
  )
}

function ImageGrid({ images }: { images: any[] }) {
  return (
    <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5">
      {images.map((d: any, i: number) => {
        const src = d.b64_json ? `data:${d.mime_type || 'image/png'};base64,${d.b64_json}` : d.url
        return (
          <a key={i} href={src} target="_blank" rel="noreferrer"
            className="group block rounded-lg border border-stone bg-surface overflow-hidden">
            <img src={src} alt={d.revised_prompt || `generated image ${i + 1}`} className="w-full block" />
            {d.revised_prompt && <div className="px-2.5 py-2 text-[11px] text-muted leading-snug border-t border-stone">{d.revised_prompt}</div>}
          </a>
        )
      })}
    </div>
  )
}

export function RichText({ text, error }: { text: string; error?: boolean }) {
  const blocks = useMemo(() => splitFences(text), [text])
  return (
    // Cap the measure so prose stays readable on a wide column; code blocks
    // and media are unaffected because they live outside this wrapper.
    <div className={`max-w-[92ch] text-sm leading-relaxed space-y-2.5 break-words ${error ? 'text-danger' : 'text-paper'}`}>
      {blocks.map((b, i) => b.lang !== undefined ? (
        <CodeBlock key={i} code={b.code} lang={b.lang} />
      ) : (
        <p key={i} className="whitespace-pre-wrap">{b.code}</p>
      ))}
    </div>
  )
}

function CodeBlock({ code, lang }: { code: string; lang?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="rounded-lg border border-stone overflow-hidden bg-ink">
      <div className="flex items-center justify-between px-2.5 py-1 border-b border-cream/10">
        <span className="font-mono text-[10px] text-cream/50">{lang || 'code'}</span>
        <button
          // copyToClipboard carries the execCommand fallback for non-secure
          // contexts; the raw `navigator.clipboard?.writeText(...).then(...)`
          // pattern throws a TypeError on plain-HTTP self-hosted gateways.
          onClick={() => { void copyToClipboard(code).then(ok => { if (ok) { setCopied(true); setTimeout(() => setCopied(false), 1200) } }) }}
          className="font-mono text-[10px] text-cream/50 hover:text-cream/90 transition-colors inline-flex items-center gap-1">
          <Icon name={copied ? 'check' : 'copy'} size={10} /> {copied ? 'copied' : 'copy'}
        </button>
      </div>
      <pre className="px-3 py-2.5 overflow-x-auto text-[12px] leading-relaxed font-mono text-cream/90 whitespace-pre">{code.replace(/\n$/, '')}</pre>
    </div>
  )
}

export function AttachmentChip({ a, onRemove, compact }: { a: Attachment; onRemove?: () => void; compact?: boolean }) {
  const [open, setOpen] = useState(false)
  const icon = a.kind === 'image' ? 'image' : a.kind === 'text' ? 'file' : 'box'
  const canPreview = !!a.dataUrl
  return (
    <span className="relative inline-flex">
      <span className="inline-flex items-center gap-1.5 rounded-lg border border-stone bg-surface pl-1.5 pr-2 py-1 text-[11px] max-w-[240px]">
        {canPreview ? (
          // A real button: the preview toggle is reachable and operable by keyboard.
          <button type="button" onClick={() => setOpen(v => !v)} aria-expanded={open}
            aria-label={`Preview ${a.name}`} title={`Preview ${a.name}`}
            className="shrink-0 rounded focus-visible:ring-2 focus-visible:ring-accent/60">
            <img src={a.dataUrl} alt="" className="w-6 h-6 rounded object-cover border border-stone" />
          </button>
        ) : (
          <span className="w-6 h-6 rounded bg-raised flex items-center justify-center text-muted shrink-0"><Icon name={icon} size={12} /></span>
        )}
        <span className="min-w-0">
          <span className="block truncate font-medium text-paper">{a.name}</span>
          {!compact && (
            <span className="block truncate font-mono text-[10px] text-muted">
              {fmtBytes(a.size)}{a.note ? ` · ${a.note}` : ''}
            </span>
          )}
        </span>
        {onRemove && (
          <button onClick={e => { e.stopPropagation(); onRemove() }} className="w-4 h-4 rounded flex items-center justify-center text-muted hover:text-danger shrink-0" aria-label={`Remove ${a.name}`}>
            <Icon name="x" size={11} />
          </button>
        )}
      </span>
      {open && a.dataUrl && (
        <>
          <span className="fixed inset-0 z-40" onClick={() => setOpen(false)} />
          <img src={a.dataUrl} alt={a.name}
            className="absolute bottom-full left-0 mb-2 z-50 max-w-[320px] max-h-[320px] rounded-lg border border-stone shadow-pop bg-surface" />
        </>
      )}
    </span>
  )
}

/**
 * Video job card.
 *
 * Status is announced politely (aria-live) and the progress bar carries
 * progressbar semantics. `video.error` is rendered unconditionally, whatever
 * the job's own status says — a content fetch or poll failure is exactly the
 * case where the job resource is still "queued" and the card used to freeze.
 */
export function VideoCard({ video, onRetry }: { video: VideoState; onRetry?: () => void }) {
  const job = video.job || {}
  const done = job.status === 'completed'
  const failed = job.status === 'failed'
  const playable = !!video.url && !video.error
  const progress = typeof job.progress === 'number' ? job.progress : 0
  const jobError = job?.error?.message || job?.error?.code
  const [mediaError, setMediaError] = useState('')

  const label = video.error ? 'Video unavailable'
    : done ? 'Video ready'
    : failed ? 'Video failed'
    : job.status === 'in_progress' ? 'Generating…' : 'Queued'

  return (
    <div className="rounded-xl border border-stone bg-surface p-3 space-y-2.5">
      <div className="flex items-center justify-between gap-2 flex-wrap" aria-live="polite" aria-busy={!done && !failed && !video.error}>
        <span className="text-sm font-medium text-paper">{label}</span>
        <Badge tone={done ? 'good' : (failed || video.error) ? 'bad' : 'info'} dot={!done && !failed && !video.error}>
          {job.status || 'queued'}
        </Badge>
      </div>
      {!done && !failed && !video.error && (
        <div className="space-y-1">
          <Progress value={progress} label="Video generation progress" />
          <div className="text-right font-mono text-[10px] text-muted tabular-nums">{Math.max(0, Math.min(100, Math.round(progress)))}%</div>
        </div>
      )}
      <div className="flex flex-wrap gap-x-3 gap-y-1 font-mono text-[11px] text-muted">
        {job.model && <span>{job.model}</span>}
        {job.seconds && <span>{job.seconds}s</span>}
        {job.size && <span>{job.size}</span>}
        {job.id && <span className="truncate max-w-[180px]">{job.id}</span>}
      </div>

      {playable && (
        <video
          controls src={video.url} className="rounded-lg w-full max-w-[560px] bg-black"
          onError={e => setMediaError(mediaErrorMessage((e.currentTarget as HTMLVideoElement).error))}
        >
          <track kind="captions" />
        </video>
      )}

      {playable && mediaError && (
        <div className="rounded-lg border border-amber/40 bg-surface px-2.5 py-2 text-xs text-amber space-y-1.5" role="alert">
          <div>
            The preview could not start: {mediaError}.
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {onRetry && (
              <Button size="sm" variant="ghost" className="!h-7 !px-2 !text-[11px]" onClick={() => { setMediaError(''); onRetry() }}>
                <Icon name="refresh" size={12} /> Try an authenticated download
              </Button>
            )}
            <a href={video.url} download={`${job.id || 'video'}.mp4`} className="underline">Download the video</a>
          </div>
        </div>
      )}

      {/* Client-side failure: poll error, content-fetch error, media refusal. */}
      {video.error && (
        <div className="rounded-lg border border-danger/30 bg-surface px-2.5 py-2 text-xs text-danger space-y-1.5" role="alert">
          <div>
            {done ? 'The job finished, but its file could not be fetched — ' : 'The job status could not be read — '}
            {video.error}
          </div>
          {onRetry && (
            <Button size="sm" variant="ghost" className="!h-7 !px-2 !text-[11px]" onClick={onRetry}>
              <Icon name="refresh" size={12} /> Retry
            </Button>
          )}
        </div>
      )}

      {failed && (
        <div className="text-sm text-danger border-l-2 border-danger/70 pl-2.5">{jobError || 'Generation failed.'}</div>
      )}
      {done && !video.url && !video.error && (
        <div className="text-xs text-muted" role="status">Fetching the rendered file…</div>
      )}
      {!done && !failed && !video.error && job.status !== 'in_progress' && (
        <div className="text-[11px] text-muted">Waiting for the provider to pick the job up…</div>
      )}
    </div>
  )
}

/* ================================================================== */
/* System One / decision answers                                       */
/* ================================================================== */

export function DecisionAnswers({ data }: { data: any }) {
  const answers = data?.answers || {}
  const ids = Object.keys(answers)
  if (ids.length === 0) {
    const serverError = typeof data?.error === 'string' ? data.error : data?.error?.message
    return (
      <div className="text-sm text-muted">
        {serverError || 'No answers returned.'}
      </div>
    )
  }
  return (
    <div className="space-y-3">
      {ids.map(id => {
        const a = answers[id] || {}
        return (
          <div key={id} className="rounded-lg border border-stone bg-surface p-3">
            <div className="flex items-center justify-between gap-2 mb-2">
              <span className="font-mono text-xs text-paper truncate">{id}</span>
              <Badge tone="info">{a.type || 'answer'}</Badge>
            </div>
            {a.type === 'noul' && <NoulAnswer a={a} />}
            {a.type === 'choice' && <ChoiceAnswer a={a} />}
            {a.type === 'score' && <ScoreAnswer a={a} />}
          </div>
        )
      })}
    </div>
  )
}

function NoulAnswer({ a }: { a: any }) {
  const p = typeof a.noul === 'number' && Number.isFinite(a.noul) ? a.noul : 0
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between">
        <span className="text-sm text-muted">probability yes</span>
        <span className="font-mono text-lg tabular-nums text-paper">{pct(p)}</span>
      </div>
      <Progress value={p * 100} label="Probability yes" />
      <div className="flex justify-between text-[11px] text-muted font-mono"><span>no {pct(1 - p)}</span><span>yes {pct(p)}</span></div>
    </div>
  )
}

function ChoiceAnswer({ a }: { a: any }) {
  const probs = (a.probabilities || {}) as Record<string, number>
  const entries = Object.entries(probs).sort((x, y) => Number(y[1]) - Number(x[1]))
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-sm text-muted">choice</span>
        <span className="font-mono text-sm text-accent">{a.choice}</span>
        {typeof a.confidence === 'number' && <Badge tone="neutral">confidence {pct(a.confidence)}</Badge>}
      </div>
      {entries.map(([k, v]) => (
        <div key={k} className="space-y-1">
          <div className="flex justify-between text-xs font-mono">
            <span className={k === a.choice ? 'text-paper' : 'text-muted'}>{k}</span>
            <span className="tabular-nums text-muted">{pct(v)}</span>
          </div>
          <Progress value={Number(v) * 100} tone={k === a.choice ? 'accent' : 'amber'} label={`${k} probability`} />
        </div>
      ))}
    </div>
  )
}

function ScoreAnswer({ a }: { a: any }) {
  const probs = (a.probabilities || {}) as Record<string, number>
  const legend = (a.legend || {}) as Record<string, string>
  const entries = Object.entries(probs).sort((x, y) => Number(x[0]) - Number(y[0]))
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-sm text-muted">score</span>
        <span className="font-mono text-sm text-accent">{typeof a.score === 'number' ? a.score.toFixed(2) : a.score}</span>
        {typeof a.confidence === 'number' && <Badge tone="neutral">confidence {pct(a.confidence)}</Badge>}
      </div>
      {entries.map(([k, v]) => (
        <div key={k} className="space-y-1">
          <div className="flex justify-between text-xs font-mono">
            <span className="text-muted">{legend[k] ?? `level ${k}`}</span>
            <span className="tabular-nums text-muted">{pct(v)}</span>
          </div>
          <Progress value={Number(v) * 100} tone="amber" label={`Level ${k} probability`} />
        </div>
      ))}
    </div>
  )
}
