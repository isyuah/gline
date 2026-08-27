import { ChevronDown, ChevronLeft, ChevronRight, ChevronsUpDown, Filter, Search, X } from 'lucide-react'
import { useMemo, useState, type FormEvent } from 'react'
import { EmptyState, ErrorState, LoadingState } from '../components/AsyncState'
import { PageHeader } from '../components/PageHeader'
import { useProjects } from '../contexts/ProjectState'
import { useApi } from '../contexts/SessionState'
import { useAsync } from '../hooks/useAsync'
import { formatDateTime, toIso, toLocalInput } from '../lib/format'
import type { EntryFilters, LogEntry, Project } from '../lib/types'

interface SearchForm {
  from: string
  to: string
  service: string
  host: string
  level: string
  q: string
}

function initialForm(): SearchForm {
  return { from: toLocalInput(new Date(Date.now() - 60 * 60_000)), to: toLocalInput(new Date()), service: '', host: '', level: '', q: '' }
}

export function LogsPage() {
  const { selected } = useProjects()
  return <LogsForProject key={selected?.id ?? 'no-project'} selected={selected} />
}

function LogsForProject({ selected }: { selected: Project | null }) {
  const api = useApi()
  const [form, setForm] = useState<SearchForm>(initialForm)
  const [filters, setFilters] = useState<SearchForm>(initialForm)
  const [cursorStack, setCursorStack] = useState<(string | undefined)[]>([undefined])
  const [page, setPage] = useState(0)
  const [expanded, setExpanded] = useState<number | null>(null)
  const cursor = cursorStack[page]

  const query: EntryFilters | null = useMemo(() => selected ? { projectId: selected.id, from: toIso(filters.from), to: toIso(filters.to), service: filters.service || undefined, host: filters.host || undefined, level: filters.level || undefined, q: filters.q || undefined, cursor, limit: 100 } : null, [selected, filters, cursor])
  const state = useAsync(() => query ? api.searchEntries(query) : Promise.resolve({ entries: [], next_cursor: null }), [api, query?.projectId, query?.from, query?.to, query?.service, query?.host, query?.level, query?.q, query?.cursor])

  function update<K extends keyof SearchForm>(key: K, value: SearchForm[K]) { setForm((current) => ({ ...current, [key]: value })) }
  function submit(event: FormEvent) { event.preventDefault(); setFilters(form); setCursorStack([undefined]); setPage(0); setExpanded(null) }
  function next() { if (!state.data?.next_cursor) return; setCursorStack((current) => [...current.slice(0, page + 1), state.data!.next_cursor!]); setPage((value) => value + 1); setExpanded(null) }
  function previous() { setPage((value) => Math.max(0, value - 1)); setExpanded(null) }

  return <div className="page-stack logs-page">
    <PageHeader title="日志搜索" description="所有查询都必须有明确时间范围，并使用 keyset cursor 稳定翻页。" />
    <form className="search-panel" onSubmit={submit}>
      <div className="search-primary"><label className="field search-keyword"><span>关键词</span><div><Search size={17} /><input value={form.q} onChange={(event) => update('q', event.target.value)} placeholder="搜索 message 与可索引字段" />{form.q && <button type="button" className="clear-input" title="清空关键词" onClick={() => update('q', '')}><X size={14} /></button>}</div></label><button type="submit" className="button primary"><Search size={16} />执行查询</button></div>
      <div className="filter-grid"><label className="field"><span>开始时间</span><input type="datetime-local" value={form.from} max={form.to} onChange={(event) => update('from', event.target.value)} required /></label><label className="field"><span>结束时间</span><input type="datetime-local" value={form.to} min={form.from} onChange={(event) => update('to', event.target.value)} required /></label><label className="field"><span>Service</span><input value={form.service} onChange={(event) => update('service', event.target.value)} placeholder="checkout-api" /></label><label className="field"><span>Host</span><input value={form.host} onChange={(event) => update('host', event.target.value)} placeholder="node-a" /></label><label className="field"><span>Level</span><div className="select-wrap"><select value={form.level} onChange={(event) => update('level', event.target.value)}><option value="">全部级别</option><option value="error">error</option><option value="warn">warn</option><option value="info">info</option><option value="debug">debug</option></select><ChevronDown size={14} /></div></label></div>
      <div className="active-filter-bar"><Filter size={15} /><span>{selected?.name ?? '未选择 Project'}</span><span>{formatDateTime(toIso(filters.from))} → {formatDateTime(toIso(filters.to))}</span>{filters.level && <span>level:{filters.level}</span>}{filters.service && <span>service:{filters.service}</span>}{filters.host && <span>host:{filters.host}</span>}</div>
    </form>
    <section className="content-section log-results">
      <header className="section-header"><div><h2>查询结果</h2><p>{state.data ? `第 ${page + 1} 页 · ${state.data.entries.length} 条` : '等待查询'}</p></div><div className="pagination"><button type="button" className="icon-button" title="上一页" disabled={page === 0 || state.loading} onClick={previous}><ChevronLeft /></button><span>{page + 1}</span><button type="button" className="icon-button" title="下一页" disabled={!state.data?.next_cursor || state.loading} onClick={next}><ChevronRight /></button></div></header>
      {state.loading ? <LoadingState label="正在查询日志" /> : state.error ? <ErrorState error={state.error} retry={state.reload} /> : !state.data?.entries.length ? <EmptyState title="没有匹配的日志" description="调整时间范围或移除部分过滤条件后重试。" /> : <div className="log-list" role="table" aria-label="日志结果">
        <div className="log-list-head" role="row"><span>时间</span><span>级别</span><span>Service / Host</span><span>Message</span><span><ChevronsUpDown size={15} /></span></div>
        {state.data.entries.map((entry) => <LogRow key={entry.id} entry={entry} expanded={expanded === entry.id} onToggle={() => setExpanded((current) => current === entry.id ? null : entry.id)} />)}
      </div>}
    </section>
  </div>
}

function LogRow({ entry, expanded, onToggle }: { entry: LogEntry; expanded: boolean; onToggle(): void }) {
  return <div className={`log-row-wrap ${expanded ? 'expanded' : ''}`}><button type="button" className="log-row" role="row" onClick={onToggle} aria-expanded={expanded}><time>{formatDateTime(entry.observed_at)}</time><span className={`level level-${entry.level}`}>{entry.level}</span><span className="log-source"><strong>{entry.service}</strong><small>{entry.host}</small></span><code className="log-message">{entry.message}</code><ChevronDown className="row-chevron" size={16} /></button>{expanded && <div className="log-detail"><dl><div><dt>Entry ID</dt><dd>{entry.id}</dd></div><div><dt>Batch</dt><dd><code>{entry.batch_id}</code></dd></div><div><dt>Agent</dt><dd><code>{entry.agent_id}</code></dd></div><div><dt>Pipeline</dt><dd><code>{entry.pipeline_id}</code></dd></div><div><dt>接入时间</dt><dd>{formatDateTime(entry.ingested_at)}</dd></div></dl><div><strong>Attributes</strong><pre>{JSON.stringify(entry.attributes, null, 2)}</pre></div></div>}</div>
}
