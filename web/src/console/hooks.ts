import { useCallback, useEffect, useState } from 'react'

export type Loadable<T> = { data: T | undefined; error: Error | undefined; loading: boolean; reload: () => void }

/** Loads data on mount and whenever `deps` change; `reload()` refetches on demand. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[] = []): Loadable<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<Error>()
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    load()
      .then((result) => {
        if (!cancelled) {
          setData(result)
          setError(undefined)
        }
      })
      .catch((e: Error) => !cancelled && setError(e))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { data, error, loading, reload }
}

/** Wraps an async action with busy / error / result state, for buttons and forms. */
export function useAction<A extends unknown[], R>(action: (...args: A) => Promise<R>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [result, setResult] = useState<R>()

  const run = async (...args: A): Promise<R | undefined> => {
    setBusy(true)
    setError(undefined)
    try {
      const value = await action(...args)
      setResult(value)
      return value
    } catch (e) {
      setError(e as Error)
      return undefined
    } finally {
      setBusy(false)
    }
  }

  return { run, busy, error, result, setResult, clearError: () => setError(undefined) }
}
