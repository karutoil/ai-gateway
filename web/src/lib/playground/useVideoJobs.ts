/**
 * Video job lifecycle for the Playground: polling with backoff, abortable
 * fetches, visibility awareness, and content prefetch that never re-downloads
 * the same job twice.
 *
 * Design notes (these are the defects this hook exists to prevent):
 *
 * 1. The previous poll loop only stopped on a terminal *status*. A job that
 *    returned 404/410/401 forever (purged job, wrong key, provider 5xx that
 *    never resolves) polled every 4s until the tab closed, with no error shown.
 *    Every failure path here ends in either a retry with exponential backoff or
 *    a `fatal` marker that removes the job from the poll set.
 * 2. `document.hidden` is honoured: a background tab does no work, and the tab
 *    polls once immediately when it becomes visible again.
 * 3. In-flight fetches are aborted on unmount/effect teardown.
 * 4. Completed videos are fetched at most once per message id (a ref set), and
 *    every object URL is revoked on delete, clear, retry and unmount.
 * 5. Preview URLs degrade correctly under the gateway's CSP: the plain
 *    same-origin content URL is preferred (`media-src 'self'`), and a blob URL
 *    is only minted when a gateway-key override forces an authenticated fetch
 *    (or when the direct URL is refused, in which case the caller retries).
 */
import { useCallback, useEffect, useRef } from 'react'
import { buildApiUrl } from '../api'
import { errorMessage } from './protocol'
import type { Msg, VideoState } from './types'

export type VideoJobRef = { sid: string; mid: string; id: string }

/** 4s → 8s → 16s → 30s (cap). */
export function backoffMs(attempts: number): number {
  return Math.min(30_000, 4_000 * 2 ** Math.min(Math.max(attempts, 0), 3))
}

type PatchMessage = (sid: string, mid: string, fn: (m: Msg) => Msg) => void

export function useVideoJobs(opts: {
  /** Non-terminal jobs that may still progress. */
  jobs: VideoJobRef[]
  /** Completed jobs whose bytes are not in the message yet. */
  completed: { sid: string; mid: string; id: string }[]
  patchMessage: PatchMessage
  /** Headers for gateway calls; `authOverride` is true when it carries a key. */
  buildHeaders: () => Record<string, string>
  authOverride: boolean
  /** Bumps when the auth override changes, forcing a content re-fetch. */
  authVersion: string
  /** Message ids currently in the transcript, used to revoke dead URLs. */
  liveMessageIds: string[]
}) {
  const { jobs, completed, patchMessage, buildHeaders, authOverride, authVersion, liveMessageIds } = opts

  /** Message ids whose content bytes were already requested (success or hard failure). */
  const fetched = useRef<Set<string>>(new Set())
  /** mid → object URL, so it can be revoked exactly once. */
  const objectUrls = useRef<Map<string, string>>(new Map())
  /** mid → consecutive poll failures. Kept in a ref so a state patch cannot
   *  reset the backoff (the poll effect is keyed on job ids, not attempts). */
  const attemptsRef = useRef<Map<string, number>>(new Map())
  const contentAbort = useRef<AbortController | null>(null)
  const headersRef = useRef(buildHeaders)
  headersRef.current = buildHeaders

  const revoke = useCallback((mid: string) => {
    const url = objectUrls.current.get(mid)
    if (url) {
      URL.revokeObjectURL(url)
      objectUrls.current.delete(mid)
    }
  }, [])

  /* ---------------- polling ---------------- */

  const jobKey = jobs.map(j => `${j.mid}:${j.id}`).join('|')

  useEffect(() => {
    if (!jobs.length) return
    let stopped = false
    let timer: number | undefined
    const ctrl = new AbortController()

    const patchVideo = (sid: string, mid: string, fn: (v: VideoState) => VideoState) =>
      patchMessage(sid, mid, m => ({ ...m, video: fn(m.video || { job: {} }) }))

    const tick = async () => {
      if (stopped) return
      // A hidden tab does not poll: the browser throttles timers anyway and a
      // watcher tab must not generate traffic on its own.
      if (document.hidden) return
      let nextDelay = 30_000
      for (const job of jobs) {
        if (stopped) return
        if (document.hidden) return
        try {
          const res = await fetch(buildApiUrl(`/v1/videos/${job.id}`), {
            headers: headersRef.current(), credentials: 'same-origin', signal: ctrl.signal,
          })
          if (!res.ok) {
            const body = await res.text().catch(() => '')
            const reason = errorMessage(body) || res.statusText || 'request failed'
            // 4xx (except rate-limit / timeout) cannot succeed on retry: the
            // job is gone, expired, or belongs to another key.
            const fatal = res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429
            const attempts = (attemptsRef.current.get(job.mid) || 0) + 1
            attemptsRef.current.set(job.mid, attempts)
            patchVideo(job.sid, job.mid, v => ({
              ...v,
              error: `HTTP ${res.status} — ${reason}`,
              fatal,
              attempts,
            }))
            if (!fatal) nextDelay = Math.min(nextDelay, backoffMs(attempts))
            continue
          }
          const j = await res.json()
          attemptsRef.current.delete(job.mid)
          patchVideo(job.sid, job.mid, v => ({ ...v, job: j, error: undefined, fatal: false, attempts: 0 }))
          if (j?.status !== 'completed' && j?.status !== 'failed') nextDelay = Math.min(nextDelay, 4_000)
        } catch (e: any) {
          if (stopped || e?.name === 'AbortError') return
          const attempts = (attemptsRef.current.get(job.mid) || 0) + 1
          attemptsRef.current.set(job.mid, attempts)
          patchVideo(job.sid, job.mid, v => ({
            ...v,
            error: `poll failed — ${String(e?.message || e)}`,
            attempts,
          }))
          nextDelay = Math.min(nextDelay, backoffMs(attempts))
        }
      }
      if (stopped) return
      timer = window.setTimeout(tick, nextDelay)
    }

    void tick()

    // Resume immediately when the tab comes back; the scheduled timer may have
    // been dropped while hidden.
    const onVisible = () => {
      if (document.hidden || stopped) return
      window.clearTimeout(timer)
      void tick()
    }
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      stopped = true
      window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
      ctrl.abort()
    }
  }, [jobKey]) // eslint-disable-line react-hooks/exhaustive-deps

  /* ---------------- content prefetch ---------------- */

  const fetchContent = useCallback(async (sid: string, mid: string, id: string) => {
    const res = await fetch(buildApiUrl(`/v1/videos/${id}/content`), {
      headers: headersRef.current(), credentials: 'same-origin', signal: contentAbort.current?.signal,
    })
    if (!res.ok) {
      const body = await res.text().catch(() => '')
      const reason = errorMessage(body) || res.statusText || 'request failed'
      throw new Error(`HTTP ${res.status} — ${reason}`)
    }
    const blob = await res.blob()
    revoke(mid)
    const url = URL.createObjectURL(blob)
    objectUrls.current.set(mid, url)
    patchMessage(sid, mid, m => ({ ...m, render: 'video', retained: true, video: { ...(m.video || { job: {} }), url, direct: false, error: undefined } }))
  }, [patchMessage, revoke])

  const completedKey = completed.map(j => `${j.mid}:${j.id}`).join('|')

  useEffect(() => {
    if (!completed.length) return
    let stopped = false
    const ctrl = new AbortController()
    contentAbort.current = ctrl

    for (const job of completed) {
      if (fetched.current.has(job.mid)) continue
      fetched.current.add(job.mid)

      if (!authOverride) {
        // Default path: point <video> at the same-origin content route. The
        // element's own request carries the session cookie, so no bytes are
        // copied into memory and `media-src 'self'` covers it.
        patchMessage(job.sid, job.mid, m => ({
          ...m, render: 'video', retained: true,
          video: { ...(m.video || { job: {} }), url: buildApiUrl(`/v1/videos/${job.id}/content`), direct: true, error: undefined },
        }))
        continue
      }

      void (async () => {
        try {
          await fetchContent(job.sid, job.mid, job.id)
        } catch (e: any) {
          if (stopped || e?.name === 'AbortError') return
          // Allow a manual retry to re-attempt the download.
          fetched.current.delete(job.mid)
          patchMessage(job.sid, job.mid, m => ({
            ...m, render: 'video', retained: true,
            video: { ...(m.video || { job: {} }), error: String(e?.message || e) },
          }))
        }
      })()
    }

    return () => { stopped = true; ctrl.abort(); if (contentAbort.current === ctrl) contentAbort.current = null }
  }, [completedKey, authOverride, authVersion]) // eslint-disable-line react-hooks/exhaustive-deps

  /* ---------------- revocation on delete / auth change ---------------- */

  const lastAuthRef = useRef(authVersion)

  useEffect(() => {
    const live = new Set(liveMessageIds)
    for (const mid of Array.from(objectUrls.current.keys())) {
      // Deleted/cleared message: release its blob immediately.
      if (!live.has(mid)) { revoke(mid); fetched.current.delete(mid) }
    }
  }, [liveMessageIds, revoke])

  // A new gateway key invalidates every blob fetched with the old one. Only run
  // when the key actually changed; the page clears `video.url` in the same
  // commit so the content effect re-runs and re-fetches with the new key.
  useEffect(() => {
    if (lastAuthRef.current === authVersion) return
    lastAuthRef.current = authVersion
    const mids = Array.from(objectUrls.current.keys())
    for (const mid of mids) revoke(mid)
    fetched.current.clear()
  }, [authVersion, revoke])

  useEffect(() => () => {
    objectUrls.current.forEach(u => URL.revokeObjectURL(u))
    objectUrls.current.clear()
    fetched.current.clear()
    attemptsRef.current.clear()
  }, [])

  /**
   * Retry a video that failed to poll or to download. Clears the failure
   * markers so the job re-enters the poll set, and drops the fetched guard so
   * the bytes are requested again.
   */
  const retryVideo = useCallback((sid: string, mid: string) => {
    revoke(mid)
    fetched.current.delete(mid)
    attemptsRef.current.delete(mid)
    patchMessage(sid, mid, m => ({
      ...m,
      video: m.video
        // Clearing `url` lets the content effect pick the job up again.
        ? { ...m.video, url: undefined, direct: undefined, error: undefined, fatal: false, attempts: 0 }
        : m.video,
    }))
  }, [patchMessage, revoke])

  /**
   * The direct (cookie-authenticated) content URL was refused by the media
   * element. Fall back to an authenticated fetch + blob URL, which works when
   * media-src allows blob: — and, when CSSP still refuses it, surfaces the
   * real error instead of a black box.
   */
  const fallbackToBlob = useCallback(async (sid: string, mid: string, id: string) => {
    fetched.current.delete(mid)
    try {
      await fetchContent(sid, mid, id)
    } catch (e: any) {
      patchMessage(sid, mid, m => ({
        ...m,
        video: { ...(m.video || { job: {} }), error: String(e?.message || e), direct: false },
      }))
    }
  }, [fetchContent, patchMessage])

  return { retryVideo, fallbackToBlob }
}
