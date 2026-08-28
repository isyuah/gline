import { describe, expect, it, vi } from 'vitest'
import { ApiError, HttpApi } from './api'

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('HttpApi', () => {
  it('默认 fetch 绑定 globalThis，避免浏览器 Illegal invocation', async () => {
    const fetcher = function (this: typeof globalThis) {
      if (this !== globalThis) throw new TypeError('Illegal invocation')
      return Promise.resolve(jsonResponse({ projects: [] }))
    }
    vi.stubGlobal('fetch', fetcher)

    const api = new HttpApi({ token: 'glk_test.secret' })

    await expect(api.listProjects()).resolves.toEqual([])
  })

  it('保留后端稳定错误码和 request id，供页面给出可操作错误', async () => {
    const fetcher = vi.fn(async () => jsonResponse({
      error: {
        code: 'missing_scope',
        message: '需要 project:read scope',
        request_id: 'req-4f8a',
      },
    }, 403)) as unknown as typeof fetch
    const api = new HttpApi({ token: 'glk_test.secret', fetcher })

    const error = await api.listProjects().catch((cause: unknown) => cause)

    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({ status: 403, code: 'missing_scope', requestId: 'req-4f8a' })
    expect((error as Error).message).toBe('需要 project:read scope')
  })

  it('日志搜索把有界时间、过滤器和 keyset cursor 编码到请求', async () => {
    const fetcher = vi.fn(async () => jsonResponse({ entries: [], next_cursor: null })) as unknown as typeof fetch
    const api = new HttpApi({ baseUrl: 'http://gline.test/api/v1', token: 'token', fetcher })

    await api.searchEntries({
      projectId: 'prj-demo',
      from: '2026-08-27T00:00:00.000Z',
      to: '2026-08-27T01:00:00.000Z',
      service: 'checkout api',
      host: 'node-a',
      level: 'error',
      q: 'payment timeout',
      cursor: 'eyJpZCI6NDJ9',
      limit: 75,
    })

    const url = new URL(String(vi.mocked(fetcher).mock.calls[0][0]))
    expect(url.pathname).toBe('/api/v1/entries')
    expect(Object.fromEntries(url.searchParams)).toMatchObject({
      project_id: 'prj-demo',
      service: 'checkout api',
      host: 'node-a',
      level: 'error',
      q: 'payment timeout',
      cursor: 'eyJpZCI6NDJ9',
      limit: '75',
    })
  })
})
