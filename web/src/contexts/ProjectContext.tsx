import { useMemo, useState, type ReactNode } from 'react'
import { useAsync } from '../hooks/useAsync'
import { ProjectContext } from './ProjectState'
import { useApi } from './SessionState'

export function ProjectProvider({ children }: { children: ReactNode }) {
  const api = useApi()
  const state = useAsync(() => api.listProjects(), [api])
  const [preferredId, setPreferredId] = useState(() => localStorage.getItem('gline.console.project') ?? '')
  const projects = useMemo(() => state.data ?? [], [state.data])
  const selectedId = projects.some((project) => project.id === preferredId) ? preferredId : projects[0]?.id ?? ''

  function setSelectedId(id: string) {
    setPreferredId(id)
    localStorage.setItem('gline.console.project', id)
  }

  const value = {
    projects,
    selectedId,
    selected: projects.find((project) => project.id === selectedId) ?? null,
    setSelectedId,
    loading: state.loading,
    error: state.error,
    reload: state.reload,
  }

  return <ProjectContext.Provider value={value}>{children}</ProjectContext.Provider>
}
