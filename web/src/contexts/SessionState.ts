import { createContext, useContext } from 'react'
import type { GlineApi } from '../lib/types'

export interface SessionConfig {
  token: string
  baseUrl: string
  mock: boolean
}

export interface SessionValue {
  config: SessionConfig | null
  api: GlineApi | null
  signIn(config: SessionConfig, remember: boolean): Promise<void>
  signOut(): void
}

export const SessionContext = createContext<SessionValue | null>(null)

export function useSession() {
  const value = useContext(SessionContext)
  if (!value) throw new Error('useSession must be used inside SessionProvider')
  return value
}

export function useApi() {
  const { api } = useSession()
  if (!api) throw new Error('API is unavailable before sign in')
  return api
}
