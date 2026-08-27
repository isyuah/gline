import { createContext, useContext } from 'react'
import type { Project } from '../lib/types'

export interface ProjectValue {
  projects: Project[]
  selectedId: string
  selected: Project | null
  setSelectedId(id: string): void
  loading: boolean
  error: unknown
  reload(): void
}

export const ProjectContext = createContext<ProjectValue | null>(null)

export function useProjects() {
  const value = useContext(ProjectContext)
  if (!value) throw new Error('useProjects must be used inside ProjectProvider')
  return value
}
