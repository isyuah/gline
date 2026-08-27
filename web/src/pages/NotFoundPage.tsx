import { ArrowLeft } from 'lucide-react'
import { Link } from 'react-router-dom'

export function NotFoundPage() {
  return <div className="state-panel"><strong>页面不存在</strong><p>该地址不属于 Gline 管理控制台。</p><Link className="button secondary" to="/"><ArrowLeft size={16} />返回概览</Link></div>
}
