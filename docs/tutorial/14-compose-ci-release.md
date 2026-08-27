# 14. Docker Compose、CI 与可复现发布

> 本章是目标交付教程。当前仓库是否已有 Dockerfile、Compose、CI 或 Release，必须以工作树为准；没有通过本章验证前，不要声称“一键部署”“跨平台发布”或“镜像已验证”。

相关设计：[目标部署拓扑](../03-target-architecture.md)、[开发路线图](../06-development-roadmap.md)、[简历与演示手册](../07-resume-demo-interview.md)。入口：[教程目录](./README.md)、[如何使用本教程](./00-how-to-use-this-tutorial.md)。

## 1. 本章目标

完成本章后，你应能：

1. 用一个 Compose project 启动 Gline Server 与 PostgreSQL，并保留命名卷中的开发数据。
2. 把 migration、readiness、启动顺序和版本兼容性变成明确流程。
3. 从没有任何本机路径 `replace` 的仓库构建、测试和打包。
4. 在 CI 中分层运行静态检查、单元/race、PostgreSQL 集成、端到端和镜像构建。
5. 为 Windows/Linux 生成可追溯的 Agent/Server artifact，并附校验和。
6. 区分“镜像构建成功”“Compose 可运行”“发布 artifact 可安装”“用户已验收”。
7. 在升级失败时回退应用版本，同时避免默认破坏 PostgreSQL volume。

## 2. 前置条件

- `go.mod` 不包含指向本机磁盘的 `replace`。
- `go mod tidy` 后 manifest 与 lock 数据一致。
- Agent 与 Server 的配置可以由明确文件/环境变量提供，secret 不写入默认配置。
- Server 有 `/livez` 与 `/readyz`；readiness 检查 PostgreSQL 与 migration。
- Server 使用显式 timeout 和 graceful shutdown。
- migration 文件已版本控制，升级顺序已定义。
- 至少有一条 Agent -> Server -> PostgreSQL -> Query API 的端到端测试。
- Docker Desktop/Engine 与 Compose 插件可用；CI runner 的 Docker 能力已确认。

如果本机依赖尚未清理，先完成路线图 GL-002。容器不能用来掩盖不可复现的模块依赖。

## 3. 交付边界与产物

目标产物：

```text
Source commit
  -> Windows/Linux Agent binary
  -> Windows/Linux Server binary
  -> Server container image
  -> checksums
  -> migration bundle
  -> example config / OpenAPI / release notes
```

每个产物都必须能回答：

- 来自哪个 commit/tag？
- 使用哪个 Go 版本和构建参数？
- 支持哪个配置/schema 版本？
- 通过了哪些验证？
- 如何检查完整性？
- 如何回退？

Compose 是开发、演示和集成环境，不自动等于生产编排方案。首版目标是可重复和可观察，而不是模拟 Kubernetes。

## 4. 推荐目录边界

随着实现推进，可以形成：

```text
deployments/
  compose/
    compose.yaml
    .env.example
    config/
      server.example.yaml
      agent.example.yaml
    secrets/
      .gitkeep          # 仅在确有需要时；secret 文件本身被忽略
Dockerfile.server
Dockerfile.agent        # 只有容器化 Agent 确有演示价值时添加
migrations/
.github/workflows/
  ci.yml
  release.yml
```

不要机械复制布局。已有项目约定优先；关键是配置、secret、持久数据和构建上下文边界清晰。

## 5. Compose 服务设计

### 5.1 最小拓扑

```text
postgres (命名 volume)
   ^
   | migration / transaction / query
gline-server

host gline-agent -> gline-server published port
```

Agent 首版更适合在宿主机运行，以真实读取宿主文件。若后来提供 Agent 容器示例，必须明确只读日志 volume、spool 持久 volume 和文件 identity 差异，不能把它作为唯一使用方式。

### 5.2 Compose 骨架

以下是配置骨架，不是可直接复制上线的最终文件。镜像版本、命令和路径必须在实现时替换为已验证值并固定：

```yaml
name: gline

services:
  postgres:
    image: ${GLINE_POSTGRES_IMAGE:?set GLINE_POSTGRES_IMAGE to a tested pinned image}
    environment:
      POSTGRES_DB: gline
      POSTGRES_USER: gline
      POSTGRES_PASSWORD_FILE: /run/secrets/postgres_password
    secrets:
      - postgres_password
    volumes:
      - gline_postgres_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gline -d gline"]
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s
    restart: unless-stopped

  migrate:
    image: ${GLINE_SERVER_IMAGE:?set GLINE_SERVER_IMAGE to the current build}
    command: ["<migration-command>", "up"]
    environment:
      GLINE_DATABASE_URL_FILE: /run/secrets/database_url
    secrets:
      - database_url
    depends_on:
      postgres:
        condition: service_healthy
    restart: "no"

  server:
    image: ${GLINE_SERVER_IMAGE:?set GLINE_SERVER_IMAGE to the current build}
    environment:
      GLINE_DATABASE_URL_FILE: /run/secrets/database_url
      GLINE_AUTH_PEPPER_FILE: /run/secrets/auth_pepper
    secrets:
      - database_url
      - auth_pepper
    ports:
      - "127.0.0.1:${GLINE_SERVER_PORT:-8080}:8080"
      - "127.0.0.1:${GLINE_OPS_PORT:-9091}:9091"
    depends_on:
      postgres:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully
    healthcheck:
      test: ["CMD", "/gline-server", "healthcheck", "--url", "http://127.0.0.1:9091/readyz"]
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s
    restart: unless-stopped

volumes:
  gline_postgres_data:

secrets:
  postgres_password:
    file: ./secrets/postgres_password.txt
  database_url:
    file: ./secrets/database_url.txt
  auth_pepper:
    file: ./secrets/auth_pepper.txt
```

注意：

- `<migration-command>` 只有在真实 CLI 实现后才能替换。
- healthcheck 使用二进制子命令只是一个目标方案，也可以用镜像中已验证的 HTTP 工具；不要为了 healthcheck 塞入不受控工具。
- 镜像 tag 应固定到发布版本或 commit，正式环境可进一步固定 digest；不要只用 `latest`。
- 端口示例默认绑定 loopback，是否开放由部署网络策略决定。
- Compose `secrets` 在本地并不等价于云 secret manager，但至少避免把 secret 写进镜像和 YAML。

## 6. 配置与 secret

### 6.1 优先级

明确并测试配置优先级，例如：

```text
命令行显式参数 > 环境变量 > 配置文件 > 安全默认值
```

敏感项优先支持 `_FILE` 形式，以便从挂载文件读取。若同时提供直接环境变量与 `_FILE`，冲突必须报错或有明文规则，不能静默选一个。

### 6.2 `.env.example`

示例文件只包含非 secret 配置和占位符：

```dotenv
GLINE_POSTGRES_IMAGE=<tested-postgres-image-or-digest>
GLINE_SERVER_IMAGE=<current-gline-server-image>
GLINE_SERVER_PORT=8080
GLINE_OPS_PORT=9091
```

真实 `.env`、`secrets/*.txt`、Agent Key、database URL 都必须被忽略。CI 使用平台 secret 注入，日志中不执行打印所有环境变量的命令。

### 6.3 配置验证

进程启动前一次性验证：

- 必填值存在；
- 地址、duration、size 合法且有上限；
- secret 非空但不回显；
- pprof 默认关闭；
- schema 版本策略明确；
- 生产模式不允许已知不安全默认凭证。

错误应指出配置键和修复方式，不打印 secret 值。

## 7. 命名卷与非破坏性操作

`gline_postgres_data` 是可复用开发状态。日常命令：

PowerShell / CI/Linux 均可：

```text
docker compose -f deployments/compose/compose.yaml up -d postgres
docker compose -f deployments/compose/compose.yaml run --rm migrate
docker compose -f deployments/compose/compose.yaml up -d server
docker compose -f deployments/compose/compose.yaml ps
docker compose -f deployments/compose/compose.yaml logs --no-log-prefix server
docker compose -f deployments/compose/compose.yaml stop server
docker compose -f deployments/compose/compose.yaml down
```

`docker compose down` 默认不会删除命名卷，但执行前仍应确认正在操作正确 project/file。禁止把以下命令放进默认开发、CI 或升级脚本：

```text
docker compose down -v
docker volume rm ...
docker system prune --volumes
```

需要全新测试数据库时，CI 应创建唯一 Compose project 或临时数据库，而不是删除开发卷。任何真实数据重置都应是单独、显式、经过目标确认的操作，本教程不提供“一条命令清空所有状态”。

### 7.1 重启前检查

```powershell
docker compose -f deployments/compose/compose.yaml ps
docker volume ls --filter 'name=gline'
docker compose -f deployments/compose/compose.yaml config
```

Linux 命令相同。`config` 输出可能展开环境变量；如果可能包含 secret，不要把完整输出上传到公共 CI artifact。

## 8. Migration 运行模型

### 8.1 推荐策略

本地 Compose：显式 one-shot `migrate` service，成功后 Server 启动。

生产式部署：发布系统在滚动 Server 前执行一次 migration job；Server 本身只检查兼容性，不由每个副本并发执行破坏性迁移。

### 8.2 Expand / Migrate / Contract

对可能影响回退的变更拆成：

1. Expand：新增 nullable 列/新表/兼容索引，旧二进制仍可运行。
2. Migrate：后台回填或双读兼容，记录进度与失败。
3. Contract：只有所有实例升级且回退窗口结束后，删除旧列/约束。

这比“发布时直接 rename/drop 列”更容易回退。

### 8.3 readiness 与 migration

Server 记录自己支持的 schema 范围。readyz 失败情况：

- DB 不可达；
- migration dirty；
- schema 低于最低版本；
- schema 高于二进制最大兼容版本；
- shutdown draining。

不要让“表存在”成为唯一判断。

### 8.4 备份不是一句话

升级涉及不可逆数据变更前，需要验证恢复流程。至少记录：

- 备份时间和目标数据库标识；
- 使用的 PostgreSQL 工具/版本；
- 备份 artifact 的访问控制；
- 在隔离数据库中的恢复验证；
- RPO/RTO 是否满足当前项目目标。

不要把未恢复验证过的 dump 称为可用备份。

## 9. Server Dockerfile 骨架

使用多阶段构建，具体基础镜像由项目固定并记录 digest：

```dockerfile
# syntax=docker/dockerfile:1
ARG BUILD_IMAGE
ARG RUNTIME_IMAGE

FROM ${BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X '<version-package>.Version=${VERSION}' -X '<version-package>.Commit=${COMMIT}'" \
    -o /out/gline-server ./cmd/server

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/gline-server /gline-server
USER nonroot:nonroot
EXPOSE 8080 9091
ENTRYPOINT ["/gline-server"]
```

实现时需要验证：

- `<version-package>` 替换为真实变量路径；没有该变量就先实现版本包。
- runtime image 是否包含 CA certificate、时区需求和预期用户。
- 骨架假定 runtime image 已定义 `nonroot` 用户；若没有，应改为经过文件权限验证的固定数字 UID/GID。
- `CGO_ENABLED=0` 与数据库 driver/平台需求兼容。
- binary 在空白容器内可启动并响应 livez。
- 镜像中没有源码、Go cache、secret 和测试数据。
- 以非 root 用户运行，挂载路径权限满足要求。

如果基础镜像没有 shell，healthcheck 不应假设 `/bin/sh` 存在。

## 10. 本地 Compose 实施步骤

### 步骤 1：固定构建输入

记录 Go 版本、基础镜像、PostgreSQL 镜像、migration 工具版本。不要使用浮动 `latest`。

### 步骤 2：构建 Server 镜像

PowerShell：

```powershell
$commit = git rev-parse --short=12 HEAD
docker build --file Dockerfile.server `
  --build-arg VERSION=dev `
  --build-arg COMMIT=$commit `
  --build-arg BUILD_IMAGE='<tested-build-image>' `
  --build-arg RUNTIME_IMAGE='<tested-runtime-image>' `
  --tag "gline-server:$commit" .
```

CI/Linux：

```bash
commit="$(git rev-parse --short=12 HEAD)"
docker build --file Dockerfile.server \
  --build-arg VERSION=dev \
  --build-arg COMMIT="$commit" \
  --build-arg BUILD_IMAGE='<tested-build-image>' \
  --build-arg RUNTIME_IMAGE='<tested-runtime-image>' \
  --tag "gline-server:$commit" .
```

### 步骤 3：解析 Compose 配置

设置非 secret image/port 变量，准备被忽略的本地 secret 文件，然后运行 `docker compose ... config --quiet`。不要把展开后的 secret 输出到日志。

### 步骤 4：启动数据库与 migration

先启动 PostgreSQL并等待 healthy，再运行 one-shot migration。migration 非零退出时停止，不启动 Server。

### 步骤 5：启动并验证 Server

检查容器状态、livez、readyz、version endpoint/command。创建测试 Project/Key，执行上传与查询 smoke。

### 步骤 6：验证重启持久性

记录一条测试 run ID，停止并重新启动 Server/PostgreSQL容器，不删除 volume。重启后查询仍应得到数据。

### 步骤 7：验证关闭

向 Server 发起在途请求并执行正常 stop，确认 readiness 先失败、请求有界完成、容器退出码与日志合理。

## 11. CI 的第一条门：禁止本机依赖

CI 必须从无本机 `replace` 的状态开始。Go module JSON 能比正则更稳健地区分本地 replacement。

PowerShell 检查：

```powershell
$module = go mod edit -json | ConvertFrom-Json
$local = @($module.Replace | Where-Object { -not $_.New.Version })
if ($local.Count -gt 0) {
  $local | ForEach-Object { Write-Error "local replace: $($_.Old.Path) => $($_.New.Path)" }
  exit 1
}
```

CI/Linux（runner 需有 `jq`）：

```bash
if ! go mod edit -json | jq -e '[.Replace[]? | select(.New.Version == null)] | length == 0' >/dev/null; then
  echo 'local module replace is forbidden in CI' >&2
  exit 1
fi
```

之后执行：

```text
go mod download
go mod verify
go mod tidy
git diff --exit-code -- go.mod go.sum
```

如果 `tidy` 产生 diff，应在本地通过 Go 工具修正并评审，而不是让 CI 自动提交。

## 12. CI 分层设计

建议依赖图：

```text
module/reproducibility
       |
       +--> format + vet + build + unit
       |             |
       |             +--> race
       |
       +--> PostgreSQL integration
                     |
                     +--> e2e/fault smoke
       |
       +--> image build + container smoke
```

### 12.1 快速 job

- `gofmt` 检查；
- `go vet ./...`；
- `go build ./cmd/...`；
- `go test ./... -count=1`；
- OpenAPI/migration lint（实现后）；
- module tidy diff。

### 12.2 race job

在受支持的 Linux runner 独立执行 `go test -race ./... -count=1`。它成本较高，但应覆盖 Agent 生命周期和共享状态。

### 12.3 integration job

启动固定版本 PostgreSQL service，使用唯一数据库：

- 运行 migration；
- 运行 `-tags=integration`；
- 检查幂等、隔离、分页、readiness；
- 失败时保存脱敏日志，不保存 secret。

### 12.4 E2E/fault job

PR 可以跑正常闭环和一个关键 ACK 丢失场景；完整四窗口故障矩阵可在主分支或定时运行。不能让昂贵测试永远不执行。

### 12.5 image job

- 构建当前 commit 镜像；
- 以非 root 运行；
- 空数据库 migration + ready；
- 上传/查询 smoke；
- 检查镜像版本与 commit；
- 不推送，除非是经过授权的 release workflow。

## 13. CI 配置骨架

以下只展示结构，Action 版本和权限必须查阅并固定到项目评估版本：

```yaml
name: ci

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  verify:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@<pinned-version-or-sha>
      - uses: actions/setup-go@<pinned-version-or-sha>
        with:
          go-version-file: go.mod
          cache: true
      - name: Reject local replace
        run: |
          if ! go mod edit -json | jq -e '[.Replace[]? | select(.New.Version == null)] | length == 0' >/dev/null; then
            exit 1
          fi
      - run: go mod download
      - run: go mod verify
      - run: go mod tidy
      - run: git diff --exit-code -- go.mod go.sum
      - run: test -z "$(gofmt -l .)"
      - run: go vet ./...
      - run: go build ./cmd/...
      - run: go test ./... -count=1
```

不要直接照抄占位符。第三方 Action 应固定版本/commit，权限按 job 最小化。发布 job 与普通 PR job 分开，PR 不获得 registry 写权限。

## 14. 跨平台构建

Go 产物目标取决于实际支持范围。第一版可明确：

```text
windows/amd64
linux/amd64
linux/arm64（只有运行验证后才列为支持）
```

构建骨架：

PowerShell：

```powershell
$commit = git rev-parse HEAD
$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags "-X '<version-package>.Commit=$commit'" `
  -o 'dist/gline-agent_windows_amd64.exe' ./cmd/agent
```

CI/Linux：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-X '<version-package>.Commit=$commit'" \
  -o dist/gline-agent_linux_amd64 ./cmd/agent
```

交叉编译成功不代表该平台可用。Windows Agent 必须在 Windows runner 上至少运行 `--version`、配置错误、启动/关闭和文件轮转 smoke；Linux 同理。

构建完成后清除当前 shell 中任务专用的 `GOOS/GOARCH/CGO_ENABLED`，避免影响后续本机构建。

## 15. 版本信息与可追溯性

Agent 和 Server 应支持：

```text
gline-agent version
gline-server version
```

输出：

- semantic version 或 `dev`；
- commit SHA；
- build time（是否加入取决于可复现构建策略）；
- Go version；
- dirty 标记（本地构建）。

不要把主机用户名、绝对工作目录或 secret 编入产物。

镜像至少包含 OCI labels：source repository、revision、version。release notes 指向对应 commit 与 migration 范围。

## 16. Release 工作流

只有显式 tag/release 授权后才推送或发布。自动化步骤：

1. 校验 tag 格式与目标 commit。
2. 复用已经通过的源 commit，不在 release job 修改代码。
3. 运行完整 module/build/test/integration 门。
4. 构建 Windows/Linux 产物。
5. 构建并 smoke Server image。
6. 生成 SHA-256 checksums。
7. 可选生成 SBOM 与漏洞扫描报告。
8. 附 migration、配置示例、OpenAPI 与 changelog。
9. 发布前验证 artifact 中版本等于 tag/commit。
10. 推送 registry/release 是独立、最小权限步骤。

校验和：

PowerShell：

```powershell
Get-FileHash -Algorithm SHA256 -LiteralPath 'dist/gline-agent_windows_amd64.exe'
```

CI/Linux：

```bash
sha256sum dist/gline-agent_linux_amd64
```

checksums 文件应由 CI 从最终 artifact 生成，不能在 artifact 变化后复用旧文件。

## 17. Release 验证矩阵

| 产物 | 最低验证 |
| --- | --- |
| Windows Agent | `version`、错误配置非零退出、真实文件采集、关闭 |
| Linux Agent | 同上，加权限/轮转场景 |
| Windows Server | `version`、配置校验、连接测试 DB、shutdown |
| Linux Server | 同上，容器内非 root smoke |
| Server image | migration、livez/readyz、上传、查询、重启 |
| migration bundle | 空库升级、上一支持版本升级、dirty 检测 |
| example config | 能被当前二进制解析，不含真实 secret |
| checksums | 下载后重新计算一致 |

测试通过只能称为 verified；陌生用户按文档成功运行后，才有更强的 runnable/accepted 证据。

## 18. 升级与回退

### 18.1 升级前

- 读取 release notes 和 schema 范围；
- 记录当前 image/tag/commit 和 migration 版本；
- 验证备份可恢复；
- 检查 Agent/Server 协议兼容矩阵；
- 确认 Compose project 和命名 volume；
- 保存脱敏配置摘要。

### 18.2 升级

1. 拉取固定版本镜像。
2. 执行兼容的 expand migration。
3. 只重建变化的 service。
4. 等待 ready。
5. 执行上传、查询、duplicate smoke。
6. 观察错误率、延迟、DB 与 Agent spool。

### 18.3 回退

- 若 schema 仍向后兼容，恢复上一个固定 image/tag。
- 不删除 PostgreSQL volume。
- 若 migration 不兼容，按已验证恢复/forward-fix 方案处理，不能盲目运行 destructive down。
- Agent 未确认 batch 保留在 spool，协议兼容矩阵决定能否继续发送。
- 保存失败证据，避免回退后丢失根因。

回退演练必须在隔离环境完成一次，不能只写步骤。

## 19. 逐步验收流程

PowerShell：

```powershell
go version
go mod verify
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
docker compose -f deployments/compose/compose.yaml config --quiet
docker compose -f deployments/compose/compose.yaml up -d postgres
docker compose -f deployments/compose/compose.yaml run --rm migrate
docker compose -f deployments/compose/compose.yaml up -d server
docker compose -f deployments/compose/compose.yaml ps
```

CI/Linux 使用同样的 Go/Compose 子命令；语法差异只出现在变量与续行。随后执行项目提供的 health、upload、query 和 sequence validator，而不是只看容器 `running`。

结束本次测试时可以 `docker compose ... down`；命名 volume 保留。如果服务用于用户继续体验，可以保持运行并记录端口、image 和停止方法。

## 20. 失败处理

### Compose 显示 running，但 readyz 失败

依次检查 migration service 退出码、数据库 health、schema 兼容、secret 文件挂载和 Server 日志。`running` 只表示进程未退出，不代表可接流量。

### migration 失败

停止后续启动。保留数据库与 migration 日志，读取 dirty/version 状态。不要删除 volume 重来；先区分脚本错误、权限、锁等待和已有不兼容 schema。

### CI 找到 local replace

通过正式 module 依赖、工作区结构或标准测试库解决。不要在 CI 创建相同绝对目录，也不要仅对 CI 隐藏 replace。

### 镜像能构建但无法启动

检查 runtime CA、用户权限、配置路径、entrypoint、CGO 和目标架构。进入构建产物验证 `version`，不要不断添加 shell/包扩大镜像而不定位原因。

### 重启后数据消失

检查 Compose project 名、volume mount、PostgreSQL data path 和是否误用了匿名 volume。立即停止覆盖性写入，保留 `docker inspect`/volume metadata 证据。

### 发布 artifact 与 tag 不一致

发布失败。检查 checkout ref、缓存和 ldflags；删除/替换远程发布属于外部副作用，需要显式授权。

## 21. 常见错误

- 使用 `latest` 作为唯一部署版本。
- 在 YAML、镜像层或日志中写 secret。
- Server 启动时多个副本并发做破坏性 migration。
- 认为 `depends_on` 等于业务 ready，却没有 health/readiness。
- 日常脚本包含 `down -v` 或 broad prune。
- CI 在本机绝对路径存在时通过，clone 后失败。
- 只交叉编译，不在目标 OS 运行 smoke。
- 只测试镜像启动，不测试上传、查询和重启持久性。
- 数据库 schema 已不可逆变化，却声称可直接回退二进制。
- 在 PR job 注入 registry/release 写权限。
- 构建成功后自动推送，未经用户授权修改远程状态。

## 22. 验收证据

至少保存：

- commit/tag、Go/Docker/PostgreSQL/基础镜像版本；
- `go mod verify`、tidy diff、build/test/race/integration 结果；
- local replace gate 输出；
- Compose 解析、service health、migration version；
- 上传、查询、duplicate 和重启持久性 smoke；
- Windows/Linux artifact 的运行结果；
- image digest/labels 与 SHA-256 checksums；
- 升级与回退演练记录；
- 未通过项和适用平台限制。

公开 artifact 前清理 secret、DSN、真实日志、绝对本机路径和测试数据库内容。

## 23. 复盘题

1. 为什么 Compose `running` 不能证明 Server 可用？
2. 为什么命名卷是默认开发状态，日常命令不能删除？
3. local `replace` 为什么必须在 CI 第一阶段失败？
4. migration 为什么更适合作为显式 job？
5. expand/migrate/contract 如何改善回退能力？
6. 交叉编译成功与平台支持有什么区别？
7. 为什么 release job 要最小权限，并与 PR job 分离？
8. 镜像 tag、commit、digest 分别解决什么问题？
9. 哪些证据能证明当前 release 对应当前源码？
10. 数据库升级失败时，为什么不应先删除 volume 重建？

## 24. 完成门

- [ ] Go module 无本机路径 `replace`，全新环境可下载、构建和测试。
- [ ] Compose 使用一个明确 project 和 PostgreSQL 命名卷，日常流程不删除数据。
- [ ] 配置/secret 不进入仓库、镜像和日志，冲突优先级受测试。
- [ ] migration 是显式、可观察步骤，readyz 校验 DB/schema。
- [ ] 镜像以非 root 运行，并通过 livez、readyz、上传、查询、重启 smoke。
- [ ] CI 包含 module、format/vet/build/unit、race、PostgreSQL integration 和 image smoke。
- [ ] Windows/Linux artifact 在目标 OS 上至少完成运行验证。
- [ ] release artifact 带 version/commit/checksum，并与 tag 一致。
- [ ] 升级与回退不默认破坏命名卷，至少完成一次隔离演练。
- [ ] 所有远程 push/publish/release 都只在显式授权后发生。

通过本章后，项目具备“可复现交付”的证据。生产可用仍取决于备份恢复、部署平台、安全运维和用户验收，不能由 Compose/CI 自动推导。
