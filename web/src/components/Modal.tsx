import { X } from 'lucide-react'
import { useEffect, type ReactNode } from 'react'

export function Modal({ open, title, description, children, onClose }: { open: boolean; title: string; description?: string; children: ReactNode; onClose(): void }) {
  useEffect(() => {
    if (!open) return
    const handle = (event: KeyboardEvent) => event.key === 'Escape' && onClose()
    window.addEventListener('keydown', handle)
    return () => window.removeEventListener('keydown', handle)
  }, [open, onClose])

  if (!open) return null
  return <div className="modal-backdrop" role="presentation" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
    <section className="modal" role="dialog" aria-modal="true" aria-labelledby="modal-title">
      <header className="modal-header"><div><h2 id="modal-title">{title}</h2>{description && <p>{description}</p>}</div><button type="button" className="icon-button" title="关闭" aria-label="关闭" onClick={onClose}><X /></button></header>
      {children}
    </section>
  </div>
}
