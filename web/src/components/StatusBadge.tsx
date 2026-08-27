import type { ReactNode } from 'react'

const labels: Record<string, string> = {
  active: '正常', disabled: '已停用', stale: '失联', enabled: '已启用', paused: '已暂停', error: '异常',
  running: '运行中', stopped: '已停止', revoked: '已吊销', expired: '已过期', pending: '待处理',
  replaying: '重放中', resolved: '已解决', discarded: '已丢弃', success: '成功', rejected: '已拒绝', failed: '失败',
  healthy: '健康', degraded: '降级', unavailable: '不可用',
}

const tones: Record<string, string> = {
  active: 'positive', enabled: 'positive', running: 'positive', success: 'positive', healthy: 'positive', resolved: 'positive',
  stale: 'warning', paused: 'warning', pending: 'warning', degraded: 'warning', replaying: 'info',
  disabled: 'neutral', stopped: 'neutral', revoked: 'neutral', expired: 'neutral', discarded: 'neutral',
  error: 'critical', failed: 'critical', rejected: 'critical', unavailable: 'critical',
}

export function StatusBadge({ status, children }: { status: string; children?: ReactNode }) {
  return <span className={`status-badge status-${tones[status] ?? 'neutral'}`}><span aria-hidden="true" />{children ?? labels[status] ?? status}</span>
}
