import { useMemo, useState, type ReactNode } from 'react'
import { HttpApi } from '../lib/api'
import { MockApi } from '../lib/mock-api'
import type { GlineApi } from '../lib/types'
import { SessionContext, type SessionConfig } from './SessionState'

const STORAGE_KEY = 'gline.console.session.v1'

function readStored(): SessionConfig | null {
  try {
    const value = localStorage.getItem(STORAGE_KEY)
    return value ? JSON.parse(value) as SessionConfig : null
  } catch {
    return null
  }
}

function buildApi(config: SessionConfig): GlineApi {
  return config.mock ? new MockApi() : new HttpApi({ token: config.token, baseUrl: config.baseUrl })
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<SessionConfig | null>(readStored)
  const api = useMemo(() => config ? buildApi(config) : null, [config])

  async function signIn(next: SessionConfig, remember: boolean) {
    const candidate = buildApi(next)
    await candidate.listProjects()
    setConfig(next)
    if (remember) localStorage.setItem(STORAGE_KEY, JSON.stringify(next))
    else localStorage.removeItem(STORAGE_KEY)
  }

  function signOut() {
    localStorage.removeItem(STORAGE_KEY)
    localStorage.removeItem('gline.console.project')
    setConfig(null)
  }

  return <SessionContext.Provider value={{ config, api, signIn, signOut }}>{children}</SessionContext.Provider>
}
