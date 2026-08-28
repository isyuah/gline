import { ArrowRight, BookOpenText, CheckCircle2, Database, KeyRound, Server } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { Navigate, useLocation, useNavigate } from 'react-router-dom'
import { ApiError } from '../lib/api'
import { useSession } from '../contexts/SessionState'

export function LoginPage() {
  const { config, signIn } = useSession()
  const navigate = useNavigate()
  const location = useLocation()
  const [token, setToken] = useState('')
  const [baseUrl, setBaseUrl] = useState('/api/v1')
  const [remember, setRemember] = useState(true)
  const [mock, setMock] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  if (config) return <Navigate to="/" replace />

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(null)
    setSubmitting(true)
    try {
      await signIn({ token: mock ? 'mock-token' : token.trim(), baseUrl: baseUrl.trim() || '/api/v1', mock }, remember)
      const next = (location.state as { from?: string } | null)?.from ?? '/'
      navigate(next, { replace: true })
    } catch (cause) {
      setError(cause instanceof ApiError || cause instanceof Error ? cause.message : '无法验证连接。')
    } finally {
      setSubmitting(false)
    }
  }

  return <main className="login-page">
    <section className="login-intro">
      <div className="brand login-brand"><span className="brand-mark"><BookOpenText size={20} /></span><div><strong>Gline</strong><small>Operations Console</small></div></div>
      <div className="intro-copy"><span className="eyebrow">自托管日志管理</span><h1>从接入状态到故障日志，在一个工作台完成判断。</h1><p>面向需要运营小型服务集群的开发者。Token 只保存在当前浏览器，不会进入日志或 URL。</p></div>
      <ul className="feature-list"><li><CheckCircle2 /><span><strong>可靠接入</strong>追踪 Agent、Pipeline 与积压状态</span></li><li><Database /><span><strong>数据治理</strong>管理保留策略、隔离批次和用量</span></li><li><Server /><span><strong>受限查询</strong>通过时间范围和游标稳定检索日志</span></li></ul>
    </section>
    <section className="login-panel">
      <form onSubmit={submit}>
        <div className="form-heading"><KeyRound size={24} /><div><h2>连接 Gline Server</h2><p>使用具备管理或只读 Scope 的 API Token。</p></div></div>
        {error && <div className="inline-alert critical" role="alert">{error}</div>}
        <label className="field"><span>API 地址</span><input value={baseUrl} onChange={(event) => setBaseUrl(event.target.value)} placeholder="/api/v1" autoComplete="url" /><small>Compose 控制台请保留 `/api/v1`，由同源代理连接 Server。</small></label>
        <label className="field"><span>API Token</span><input type="password" value={token} onChange={(event) => setToken(event.target.value)} placeholder="glk_project_prefix.secret" autoComplete="current-password" required={!mock} disabled={mock} /></label>
        <label className="check-field"><input type="checkbox" checked={remember} onChange={(event) => setRemember(event.target.checked)} /><span>在这台设备上保持登录</span></label>
        {import.meta.env.DEV && <div className="mock-option"><label className="check-field"><input type="checkbox" checked={mock} onChange={(event) => setMock(event.target.checked)} /><span>使用本地演示数据</span></label><small>显式开发开关，不会请求后端。</small></div>}
        <button type="submit" className="button primary wide" disabled={submitting || (!mock && !token.trim())}>{submitting ? <><span className="spinner inverse" />正在验证</> : <>进入控制台<ArrowRight size={17} /></>}</button>
        <p className="form-note">连接时会读取 Project 列表验证 Token 和 Scope。Gline 不支持用户名密码登录。</p>
      </form>
    </section>
  </main>
}
