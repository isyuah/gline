import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import './App.css'
import { AppShell } from './components/AppShell'
import { ProjectProvider } from './contexts/ProjectContext'
import { useSession } from './contexts/SessionState'
import { AgentsPage } from './pages/AgentsPage'
import { DashboardPage } from './pages/DashboardPage'
import { HealthPage } from './pages/HealthPage'
import { LoginPage } from './pages/LoginPage'
import { LogsPage } from './pages/LogsPage'
import { NotFoundPage } from './pages/NotFoundPage'
import { OperationsPage } from './pages/OperationsPage'
import { ProjectsPage } from './pages/ProjectsPage'

function ProtectedShell() {
  const { config } = useSession()
  const location = useLocation()
  if (!config) return <Navigate to="/login" replace state={{ from: location.pathname }} />
  return <ProjectProvider><AppShell /></ProjectProvider>
}

export default function App() {
  return <Routes>
    <Route path="/login" element={<LoginPage />} />
    <Route element={<ProtectedShell />}>
      <Route index element={<DashboardPage />} />
      <Route path="projects" element={<ProjectsPage />} />
      <Route path="agents" element={<AgentsPage />} />
      <Route path="logs" element={<LogsPage />} />
      <Route path="operations" element={<OperationsPage />} />
      <Route path="health" element={<HealthPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Route>
  </Routes>
}
