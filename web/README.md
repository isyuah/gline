# Gline Operations Console

Gline 的 React + TypeScript 管理工作台。默认连接真实 `/api/v1`，开发模式下可以在登录页显式开启本地演示数据。

## 本地开发

```powershell
pnpm install
pnpm dev
```

Vite 会把 `/api`、`/healthz`、`/livez` 和 `/readyz` 代理到 `http://localhost:8080`。也可以在登录页输入完整 API 地址，例如 `http://localhost:8080/api/v1`。

```powershell
pnpm test
pnpm lint
pnpm build
```

## API 合同

所有请求类型、响应解包和错误映射集中在 `src/lib/api.ts`。页面不直接调用 `fetch`。

- 认证使用 `Authorization: Bearer <token>`。
- 错误支持 `{ "error": { "code", "message", "request_id", "details" } }`，同时兼容无 `error` 外层的形式。
- 列表响应兼容裸数组和具名字段，但后端应固定正式响应合同后移除不必要的兼容分支。
- 日志查询使用必填 `project_id`、`from`、`to` 和 `limit`，下一页传递不透明 `cursor`。
- API Key 管理权限使用权威 Scope `key:manage`；明文 Secret 只在创建响应展示一次。
- Readiness 使用 `/readyz`，界面也为后端同时提供 `/healthz` 留出独立入口。

当前前端采用的管理路由如下，最终联调时应与 Server 的路由测试逐项核对：

```text
GET/POST  /api/v1/projects
POST      /api/v1/projects/{id}/enable|disable
GET/POST  /api/v1/projects/{id}/keys
POST      /api/v1/projects/{id}/keys/{keyID}/revoke
GET       /api/v1/agents?project_id=...
GET       /api/v1/pipelines?project_id=...
POST      /api/v1/projects/{id}/pipelines/{pipelineID}/enable|pause|disable
GET       /api/v1/entries?...&cursor=...
GET/PUT   /api/v1/projects/{id}/retention
GET       /api/v1/projects/{id}/usage?from=...&to=...
GET       /api/v1/audit?project_id=...
GET       /api/v1/quarantine?project_id=...
POST      /api/v1/quarantine/{id}/replay|discard
GET       /readyz
```

## 安全边界

“保持登录”会把连接配置与 Token 保存在浏览器 `localStorage`；不勾选时只保留在当前页面生命周期。Token 不进入 URL、UI 日志或错误详情。生产部署应通过 TLS 提供控制台，并为管理员配置最小权限和有限有效期的 Key。
