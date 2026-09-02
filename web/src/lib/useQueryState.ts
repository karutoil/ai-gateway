import { useCallback, useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'

/**
 * URL-synced state: the current value lives in the query string, so filtered
 * views are shareable/bookmarkable and survive refresh. Updates replace
 * (not push) history so changing filters doesn't spam the back stack.
 *
 * Setting a value equal to the default removes the param (clean URLs).
 */
export function useQueryState(key: string, defaultValue: string): [string, (v: string) => void] {
  const [searchParams, setSearchParams] = useSearchParams()
  const value = searchParams.get(key) ?? defaultValue

  const set = useCallback((v: string) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      if (v === defaultValue || v === '') next.delete(key)
      else next.set(key, v)
      return next
    }, { replace: true })
  }, [key, defaultValue, setSearchParams])

  return [value, set]
}

/**
 * Multi-param variant for the logs filter bar: read all filter params at
 * once and update any subset in one URL write.
 */
export function useFiltersState<T extends Record<string, string>>(defaults: T): [T, (patch: Partial<T>) => void] {
  const [searchParams, setSearchParams] = useSearchParams()

  const value = (() => {
    const out = { ...defaults }
    for (const key of Object.keys(defaults) as (keyof T)[]) {
      const v = searchParams.get(key as string)
      if (v != null) out[key] = v as T[keyof T]
    }
    return out
  })()

  // Re-read when the URL changes externally (saved-view apply, share links).
  const [force, setForce] = useState(0)
  useEffect(() => { setForce(f => f + 1) }, [searchParams])

  const set = useCallback((patch: Partial<T>) => {
    setSearchParams(prev => {
      const next = new URLSearchParams(prev)
      for (const [k, v] of Object.entries(patch)) {
        if (v === undefined || v === null || v === '' || v === defaults[k as keyof T]) next.delete(k)
        else next.set(k, String(v))
      }
      return next
    }, { replace: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [defaults, setSearchParams])

  // value recomputed from searchParams each render; force ticks the linter
  void force
  return [value, set]
}
