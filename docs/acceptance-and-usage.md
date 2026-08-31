# Gline 验收与使用手册

这份文档面向第一次运行 Gline 的开发者。目标是完成一条最小但完整的链路：

```text
本地日志文件 -> Go Agent -> 本地 WAL -> Go Server -> PostgreSQL -> Web 控制台查询
```

本文以 Windows PowerShell 和当前仓库 `E:\Proj\gline-full` 为例。若仓库路径不同，
请把命令中的路径替换为实际路径。

## 1. 先理解三个组件

| 组件 | 作用 | 第一次运行时是否需要单独启动 |
| --- | --- | --- |
| PostgreSQL | 保存项目、凭据、日志、用量、审计和运维数据 | 不需要，Compose 会启动 |
| Go Server | 提供认证、Agent 控制、批量接收、查询、维护和健康检查 API | 不需要，Compose 会启动 |
| React Web | 管理 Project、Agent、Pipeline、Key 和日志查询 | 不需要，Compose 会启动 |
| Go Agent | 读取本地文件、解析记录、写 WAL、重试发送和报告心跳 | 需要手动启动 |

Gline 中的几个名词关系如下：

```text
Project
  ├── Agent：一个采集进程的登记信息
  ├── Pipeline：一个 Agent 上的一条采集配置
  └── API Key：访问该 Project 的凭据
```

Agent 使用的是 Project 级 API Key。Bootstrap Token 是管理 Server 的初始管理员凭据，
只应该用于控制台和管理 API，不能写进 Agent 配置或提交到 Git。

## 2. 环境准备

最低要求：

- Windows 11 或兼容的 Windows 环境；
- Docker Desktop，且 Docker Engine 已启动；
- Docker Compose v2；
- 若要从源码启动 Agent，需要 Go 1.26.5 或兼容版本；
- 若要从源码启动 Web，需要 Node.js 和 pnpm。

进入仓库并确认 Docker 可用：

```powershell
Set-Location E:\Proj\gline-full
docker version
docker compose version
```

如果 `docker version` 的 Server 部分报错，先在 Docker Desktop 中启动 Engine，
再继续下面的步骤。

## 3. 首次启动 Server、PostgreSQL 和 Web

### 3.1 创建本地环境文件

不要直接修改 `.env.example`，先创建本地文件：

```powershell
Copy-Item .env.example .env
notepad .env
```

至少替换下面三个值，并且三者互相独立、长度不少于 24 个字符：

```text
GLINE_POSTGRES_PASSWORD=
GLINE_BOOTSTRAP_TOKEN=
GLINE_API_KEY_PEPPER=
```

可以使用 PowerShell 生成随机值，再手动写入 `.env`。这段命令只把结果显示在当前终端，
不要把终端输出复制到聊天记录、Issue 或 Git：

```powershell
function New-GlineSecret([int]$Length = 32) {
  $bytes = [byte[]]::new($Length)
  [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
  [Convert]::ToBase64String($bytes).Replace('+', '-').Replace('/', '_').TrimEnd('=')
}

New-GlineSecret
New-GlineSecret
New-GlineSecret
```

第一个结果用于数据库密码，第二个用于 Bootstrap Token，第三个用于 API Key Pepper。

### 3.2 处理端口

默认端口是：

| 服务 | 默认端口 | 用途 |
| --- | --- | --- |
| Server | `8080` | API、健康检查和 Prometheus 指标 |
| Web | `4173` | 浏览器控制台 |
| PostgreSQL | `5432` | 数据库 |

如果 `8080` 已被占用，可以在 `.env` 中改成：

```text
GLINE_HTTP_PORT=18080
```

下文用 `$serverPort` 表示 Server 的宿主机端口。若沿用默认值，设为 `8080`；
如果按当前工作树的本地环境运行，设为 `18080`：

```powershell
$serverPort = 18080
```

### 3.3 启动 Compose

```powershell
docker compose up --build -d
docker compose ps
```

预期是 `postgres`、`server`、`web` 都处于运行状态，且 `postgres`、`server` 显示
healthy。用下面的命令检查 Server：

```powershell
Invoke-WebRequest "http://127.0.0.1:$serverPort/livez" | Select-Object StatusCode
Invoke-WebRequest "http://127.0.0.1:$serverPort/readyz" | Select-Object StatusCode
```

两个请求都应返回 `200`。`livez` 表示进程还活着；`readyz` 还会检查应用是否已经准备
好接收请求。

打开 Web：

```text
http://127.0.0.1:4173
```

在登录页填入 `.env` 中的 `GLINE_BOOTSTRAP_TOKEN`。Web 的 API Base URL 保持默认的
同源 `/api/v1` 即可。

查看容器日志的命令：

```powershell
docker compose logs --tail=100 server
docker compose logs --tail=100 postgres
docker compose logs --tail=100 web
```

## 4. 在控制台创建第一条采集链路

下面的顺序很重要，因为 Agent 配置需要后面创建出来的 ID 和 API Key。

### 4.1 创建 Project

进入 `Projects` 页面，创建一个项目，例如：

```text
名称：Gline Demo
Slug：gline-demo
```

记住这个 Project。后续 Agent、Pipeline、API Key 都应该属于同一个 Project。

### 4.2 创建 Agent

进入该 Project 的 `Agents` 页面，创建：

```text
名称：local-agent
Hostname：local-dev
Version：dev
```

复制创建结果中的 Agent ID。它应该是 UUID 格式。

### 4.3 创建 Pipeline

进入 `Pipelines` 页面，创建一条绑定到刚才 Agent 的 Pipeline：

```text
名称：demo-api
Service：demo-api
```

复制 Pipeline ID，并记下 Server 返回的 `config_version`。第一次创建通常是 `1`，
但验收时以页面/API 中实际显示的值为准。

Pipeline 的 Server 状态和 Agent 的本地状态是两个概念：Server 保存期望状态，Agent
通过心跳报告观察状态。修改 Pipeline 配置版本后，必须同步本地配置并重启 Agent；旧的
Agent 不会悄悄使用未知配置继续采集。

### 4.4 创建 Agent API Key

进入该 Project 的 `Keys` 页面，创建一个专供 Agent 使用的 Key。至少选择：

```text
ingest
agent:write
```

创建成功后，完整 Secret 通常只展示一次，立即复制到临时安全位置。不要把它写入
`examples/glineconf.yaml`，也不要提交 `.glineconf`。

推荐把管理/查询凭据和 Agent 凭据分开：Agent 只需要采集和心跳权限，Bootstrap Token
不要下发到采集机器。

## 5. 启动本地 Agent

### 5.1 准备文件和配置

在仓库根目录执行：

```powershell
New-Item -ItemType Directory -Force data | Out-Null
New-Item -ItemType File -Force data\demo-api.log | Out-Null
Copy-Item examples\glineconf.yaml .glineconf
notepad .glineconf
```

修改 `.glineconf` 中这些字段：

| 字段 | 填什么 |
| --- | --- |
| `agent.id` | 控制台创建的 Agent ID |
| `pipelines[0].id` | 控制台创建的 Pipeline ID |
| `pipelines[0].config_version` | Server 当前显示的 Pipeline config version |
| `sender.destination.params.token` | 刚刚创建的 Agent API Key Secret |
| `sender.destination.params.url` | `http://127.0.0.1:8080/api/v1/batches`，若 Server 映射到 18080 则改为 18080 |

为了让暂停/恢复验收更快，可以把示例里的：

```yaml
heartbeat_interval: 30s
```

临时改为 `5s`。这只影响本地演示，不改变 Server 的数据模型。

### 5.2 启动并写入第一条日志

保持这个 PowerShell 窗口不要关闭，启动 Agent：

```powershell
go run ./cmd/agent -config .glineconf
```

另开一个 PowerShell 窗口写入日志：

```powershell
Add-Content -LiteralPath data\demo-api.log -Value 'INFO hello from gline'
Add-Content -LiteralPath data\demo-api.log -Value 'ERROR simulated checkout failure'
```

示例使用 `string_line` parser，因此每一行就是一条日志。Agent 会先把记录提交到本地
WAL，再由 Dispatcher 发送到 Server；Server 只有在 PostgreSQL 事务提交后才返回成功。

本地 Agent 的观测地址默认是：

```text
http://127.0.0.1:9109/metrics
http://127.0.0.1:9109/livez
```

如果没有启用 `metrics_addr`，这两个地址不会监听；这不影响采集本身。

## 6. 手工验收清单

### 6.1 最小成功链路

- [ ] `docker compose ps` 中三个服务正在运行；
- [ ] Server 的 `/livez` 和 `/readyz` 都返回 `200`；
- [ ] Web 可以使用 Bootstrap Token 登录；
- [ ] Project、Agent、Pipeline 和 Agent API Key 都已创建；
- [ ] Agent 进程没有因认证、配置版本或连接错误退出；
- [ ] `data/demo-api.log` 中新增的两行出现在 Web 的日志查询页面；
- [ ] `Agents` 页面能看到最近心跳和运行状态；
- [ ] `Usage` 页面能看到对应 Project 的计数；
- [ ] `Audit` 页面能看到管理操作。

### 6.2 重试和断点恢复

这是 Gline 最值得在简历中展示的验收场景：

1. 先确认一条普通日志已经在 Web 中可查询；
2. 停止 Server，但保留 Agent 和 PostgreSQL：

   ```powershell
   docker compose stop server
   Add-Content -LiteralPath data\demo-api.log -Value 'WARN written during server outage'
   ```

3. 观察 Agent 窗口，它应保持重试，不能把未确认的批次当作已发送；
4. 恢复 Server：

   ```powershell
   docker compose start server
   ```

5. 等待 Agent 重试完成，在 Web 中查询 `written during server outage`；
6. 确认日志最终出现一次，WAL 待发送量回落。

相同 `batch_id` 的重试会返回 `duplicate` 而不是再次插入日志。这个行为是持久化
WAL、批次幂等和数据库唯一约束共同保证的。

### 6.3 Pipeline 暂停、恢复和禁用

在 `Pipelines` 页面暂停 Pipeline，然后等待一个心跳周期，再追加日志：

```powershell
Add-Content -LiteralPath data\demo-api.log -Value 'INFO written while pipeline paused'
```

暂停后，Agent 应停止继续读取新的文件记录；已经进入 WAL 的记录仍允许排空。恢复后，
Agent 应继续读取和发送。禁用状态更严格：对应批次保留在 WAL 中，Dispatcher 可以继续
处理其他 Pipeline，重新启用后才恢复该 Pipeline。

### 6.4 配置版本保护

在控制台修改 Pipeline 配置，使 Server 的 `config_version` 增加。旧的 `.glineconf`
仍然使用旧版本时，Agent 应在心跳后把该 Pipeline 报为配置版本不匹配并停止读取，
而不是用不确定的配置继续运行。

随后把 `.glineconf` 的 `config_version` 改成 Server 的新值并重启 Agent；心跳恢复后，
Pipeline 应重新进入可运行状态。

## 7. 常见问题排查

### 页面打不开

先确认 Web 容器和端口：

```powershell
docker compose ps
Test-NetConnection 127.0.0.1 -Port 4173
```

如果 Web 端口被占用，在 `.env` 设置其他 `GLINE_WEB_PORT` 后重新执行：

```powershell
docker compose up -d --force-recreate web
```

### `/readyz` 不是 200

查看 Server 和 PostgreSQL 日志：

```powershell
docker compose logs --tail=200 server
docker compose logs --tail=200 postgres
```

常见原因是数据库尚未 healthy、`.env` 密码不一致，或 Server 端口仍使用了实际已被
占用的端口。

### Agent 返回 401 或 403

- 确认 Token 是控制台刚生成的完整 Secret，而不是 Key 名称或 ID；
- 确认 Key 属于当前 Project；
- 确认 Key 至少有 `ingest` 和 `agent:write` scope；
- 确认 `.glineconf` 中的 Agent ID、Pipeline ID 与控制台记录一致。

### Agent 报配置版本不匹配

比较控制台 Pipeline 的 `config_version` 和 `.glineconf` 中的值。同步后重启 Agent，
不要反复重启仍使用旧配置的进程。

### 日志没有出现在页面

按这个顺序检查：

1. `data/demo-api.log` 的修改时间和内容是否确实变化；
2. Agent 是否有 WAL 或 retry 错误；
3. `.glineconf` 的 URL 是否使用了正确宿主机端口；
4. Server 日志是否有 401、403、409 或 5xx；
5. Web 查询的 Project、时间范围、Service 是否选对。

查看 WAL 和本地隔离批次：

```powershell
go run ./cmd/quarantine -config .glineconf list
go run ./cmd/quarantine -config .glineconf inspect <batch-id>
```

不要直接删除 `data\agent.wal` 或 checkpoint 文件来“修复”问题；这样会丢失 Agent 的
持久化进度。先保存 Agent 日志和 WAL 状态，再定位错误。

## 8. 停止和清理

日常停止服务：

```powershell
docker compose stop server web postgres
```

或者停止并移除容器，但保留命名卷中的 PostgreSQL 数据：

```powershell
docker compose down
```

只有在明确接受数据库数据不可恢复、且这是一次性演示环境时，才使用：

```powershell
docker compose down -v
```

本地 Agent 的 `data` 目录和 `.glineconf` 需要单独保留或清理。它们包含 WAL、checkpoint、
日志和凭据，不应该提交到 Git。

## 9. 工程验证命令

只想验证源码而不手工启动 Web 时，可以在仓库根目录运行：

```powershell
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

验证前端：

```powershell
Set-Location web
pnpm lint
pnpm test
pnpm build
Set-Location ..
```

真实 PostgreSQL 集成测试是显式 opt-in，并且会创建临时 schema。不要指向有用数据：

```powershell
$env:GLINE_TEST_DATABASE_URL = 'postgres://gline:password@127.0.0.1:5432/gline_test?sslmode=disable'
go test -tags=integration ./... -count=1 -v
```

## 10. 代码量与推荐阅读方式

截至 2026-08-31，仓库共有 122 个 Go 文件、15,505 行物理文本行。这里的“行”包括
空行和注释，不等同于编译器意义上的有效代码行；统计排除了 `vendor`、`node_modules`、
`dist` 和 Git 目录。

| 区域 | 文件数 | 总行数 | 测试文件 | 测试行数 |
| --- | ---: | ---: | ---: | ---: |
| `cmd` | 3 | 224 | 1 | 68 |
| `internal/agent` | 56 | 5,826 | 15 | 2,429 |
| `internal/domain` | 6 | 599 | 1 | 76 |
| `internal/logentry` | 2 | 25 | 0 | 0 |
| `internal/protocol` | 2 | 489 | 1 | 71 |
| `internal/server` | 34 | 6,548 | 13 | 2,290 |
| `internal/storage` | 17 | 1,745 | 3 | 275 |
| `migrations` | 2 | 49 | 1 | 41 |
| **合计** | **122** | **15,505** | **35** | **5,250** |

生产文件是 87 个、10,255 行，测试文件约占全部物理行的 33.9%。这个规模已经不适合
“从第一个 Go 文件开始逐行通读”，但非常适合沿着一条业务链路逐层深入。

推荐采用“先运行、再读核心路径、最后补专题”的方式：

1. 先完成本文的最小成功链路，知道一次日志从文件到页面经过哪些组件；
2. 阅读 `docs/backend-tutorial/00-system-vision.md` 和
   `docs/backend-tutorial/01-backend-architecture.md`，建立边界和术语；
3. 阅读 Agent 主链路：
   `internal/agent/reliable/agent.go`、`control.go`、`dispatcher.go`、
   `internal/agent/spool/wal.go`、`internal/agent/build/reliable.go`；
4. 阅读 Server 主链路：
   `internal/server/bootstrap/application.go`、`internal/server/httpapi/router.go`、
   `internal/server/control/service.go`、`internal/server/ingest/service.go`、
   `internal/server/query/service.go`；
5. 最后读 `internal/storage/postgres` 和迁移文件，理解租户隔离、幂等、索引和用量；
6. 对可靠性、故障恢复和扩展性有兴趣时，再阅读
   `docs/backend-tutorial/04-reliability.md` 及后续章节。

我的建议不是二选一：不要让我替你生成覆盖每个函数的“代码百科”，也不建议你直接硬读
全部源码。最有效的组合是：你先按本手册跑通一条链路，再用上述阅读顺序自己看核心实现；
遇到具体模块时，让我补一份带调用链、状态转移、失败语义和关键测试的专题说明。这样文档
帮助你建立地图，源码阅读负责形成真正的实现理解。

## 11. 当前验收边界

本工作树已经通过 Go 单元测试、race、vet、构建、前端 lint/test/build、Compose 配置、
实际 PostgreSQL Agent 恢复链路和本地 Compose HTTP smoke。GitHub Actions 工作流也已配置
格式化、vet、单元、race、Linux 构建、PostgreSQL 集成和前端检查。

仍需项目使用者手工确认的内容是浏览器中的视觉与交互体验，以及你是否认可这套操作流程
作为最终产品体验。自动化检查证明“系统可运行”和“协议/数据链路正确”，不能替代用户对
页面、命名和操作成本的接受。
