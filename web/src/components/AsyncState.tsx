import { AlertTriangle, Inbox, LockKeyhole, RefreshCw } from 'lucide-react'
import { ApiError } from '../lib/api'

export function LoadingState({ label = '正在加载数据' }: { label?: string }) {
  return <div className="state-panel compact" role="status"><span className="spinner" /> <span>{label}</span></div>
}

export function EmptyState({ title, description, action }: { title: string; description: string; action?: React.ReactNode }) {
  return <div className="state-panel"><Inbox size={30} /><strong>{title}</strong><p>{description}</p>{action}</div>
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const forbidden = error instanceof ApiError && error.isForbidden
  const message = error instanceof Error ? error.message : '发生了未知错误。'
  return <div className="state-panel state-error" role="alert">
    {forbidden ? <LockKeyhole size={30} /> : <AlertTriangle size={30} />}
    <strong>{forbidden ? '没有访问权限' : '数据加载失败'}</strong>
    <p>{message}</p>
    {error instanceof ApiError && error.requestId && <code>请求 ID：{error.requestId}</code>}
    {retry && <button type="button" className="button secondary" onClick={retry}><RefreshCw size={16} />重试</button>}
  </div>
}
