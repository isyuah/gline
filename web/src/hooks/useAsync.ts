import { useCallback, useEffect, useRef, useState } from 'react'

export interface AsyncState<T> {
  data: T | null
  error: unknown
  loading: boolean
}

export function useAsync<T>(loader: () => Promise<T>, dependencies: readonly unknown[] = []) {
  const mounted = useRef(true)
  const [nonce, setNonce] = useState(0)
  const [state, setState] = useState<AsyncState<T>>({ data: null, error: null, loading: true })

  useEffect(() => () => { mounted.current = false }, [])

  useEffect(() => {
    let active = true
    setState((current) => ({ ...current, loading: true, error: null }))
    loader().then(
      (data) => active && mounted.current && setState({ data, error: null, loading: false }),
      (error) => active && mounted.current && setState((current) => ({ data: current.data, error, loading: false })),
    )
    return () => { active = false }
    // Dependencies are controlled by callers; loader is intentionally excluded.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...dependencies, nonce])

  const reload = useCallback(() => setNonce((value) => value + 1), [])
  return { ...state, reload }
}
