import {
  Activity,
  BookOpenText,
  ChevronDown,
  CircleGauge,
  Database,
  FileSearch,
  FolderKanban,
  LogOut,
  Menu,
  Settings2,
  ShieldCheck,
  X,
} from 'lucide-react'
import { useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { useProjects } from '../contexts/ProjectState'
import { useSession } from '../contexts/SessionState'

const navigation = [
  { to: '/', label: '概览', icon: CircleGauge, end: true },
  { to: '/projects', label: '项目', icon: FolderKanban },
  { to: '/agents', label: 'Agent 与管道', icon: Activity },
  { to: '/logs', label: '日志搜索', icon: FileSearch },
  { to: '/operations', label: '数据治理', icon: Database },
  { to: '/health', label: '系统健康', icon: ShieldCheck },
]

export function AppShell() {
  const [mobileOpen, setMobileOpen] = useState(false)
  const location = useLocation()
  const { config, signOut } = useSession()
  const { projects, selectedId, setSelectedId, loading } = useProjects()

  const active = navigation.find((item) => item.end ? location.pathname === item.to : location.pathname.startsWith(item.to))

  return <div className="app-shell">
    {mobileOpen && <button type="button" aria-label="关闭导航" className="mobile-overlay" onClick={() => setMobileOpen(false)} />}
    <aside className={`sidebar ${mobileOpen ? 'open' : ''}`}>
      <div className="brand"><span className="brand-mark"><BookOpenText size={20} /></span><div><strong>Gline</strong><small>Operations Console</small></div><button type="button" className="icon-button mobile-close" title="关闭" onClick={() => setMobileOpen(false)}><X /></button></div>
      <nav aria-label="主导航">
        {navigation.map(({ to, label, icon: Icon, end }) => <NavLink key={to} to={to} end={end} onClick={() => setMobileOpen(false)}><Icon size={18} /><span>{label}</span></NavLink>)}
      </nav>
      <div className="sidebar-footer">
        <div className="connection-chip"><span className={`connection-dot ${config?.mock ? 'mock' : ''}`} /><div><strong>{config?.mock ? '演示数据' : '实时 API'}</strong><small>{config?.baseUrl}</small></div></div>
        <button type="button" className="sidebar-action" onClick={signOut}><LogOut size={17} />退出控制台</button>
      </div>
    </aside>
    <div className="workspace">
      <header className="topbar">
        <button type="button" className="icon-button menu-button" title="打开导航" onClick={() => setMobileOpen(true)}><Menu /></button>
        <div className="mobile-page-title">{active?.label ?? 'Gline'}</div>
        <label className="project-switcher"><span>当前项目</span><div><Settings2 size={15} /><select aria-label="当前项目" value={selectedId} disabled={loading || projects.length === 0} onChange={(event) => setSelectedId(event.target.value)}><option value="">{loading ? '正在加载…' : '选择项目'}</option>{projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select><ChevronDown size={14} /></div></label>
      </header>
      <main className="main-content"><Outlet /></main>
    </div>
  </div>
}
