import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SessionProvider } from '../contexts/SessionContext'
import { LoginPage } from './LoginPage'

afterEach(() => vi.unstubAllGlobals())

describe('LoginPage', () => {
  it('先用 Project 列表验证 Token，成功后才进入控制台并记住配置', async () => {
    const fetcher = vi.fn(async (..._args: Parameters<typeof fetch>): Promise<Response> => new Response(JSON.stringify({ projects: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }))
    vi.stubGlobal('fetch', fetcher)
    const user = userEvent.setup()

    render(<MemoryRouter initialEntries={['/login']}><SessionProvider><Routes><Route path="/login" element={<LoginPage />} /><Route path="/" element={<div>控制台已连接</div>} /></Routes></SessionProvider></MemoryRouter>)

    await user.type(screen.getByLabelText('API Token'), 'glk_demo.secret')
    await user.click(screen.getByRole('button', { name: '进入控制台' }))

    expect(await screen.findByText('控制台已连接')).toBeInTheDocument()
    expect(fetcher).toHaveBeenCalledOnce()
    expect(fetcher.mock.calls[0][0]).toBe('/api/v1/projects')
    expect(fetcher.mock.calls[0][1]?.headers).toMatchObject({ Authorization: 'Bearer glk_demo.secret' })
    expect(JSON.parse(localStorage.getItem('gline.console.session.v1') ?? '{}')).toMatchObject({ token: 'glk_demo.secret', baseUrl: '/api/v1', mock: false })
  })

  it('认证失败时停留在登录页并展示服务端可操作错误', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: { code: 'invalid_credential', message: 'Token 已被吊销' } }), {
      status: 401,
      headers: { 'Content-Type': 'application/json' },
    })))
    const user = userEvent.setup()

    render(<MemoryRouter initialEntries={['/login']}><SessionProvider><LoginPage /></SessionProvider></MemoryRouter>)

    await user.type(screen.getByLabelText('API Token'), 'revoked-token')
    await user.click(screen.getByRole('button', { name: '进入控制台' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Token 已被吊销')
    expect(localStorage.getItem('gline.console.session.v1')).toBeNull()
  })
})
