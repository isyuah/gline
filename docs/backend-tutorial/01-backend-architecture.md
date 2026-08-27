# 01. 后端架构：模块化单体里的四个平面

本章把上一章的定位落成可写代码的结构。重点不是画一张漂亮的架构图，而是回答：一个请求从哪里进入、经过哪些边界、在哪个组件拥有事务、如何关闭，以及未来怎样在不重写领域层的情况下扩展。

## 1. 当前代码差距

当前 `cmd/server` 仍承担较多启动和路由职责，`internal/server/modules` 处于原型阶段。可以保留现有上传模块作为迁移起点，但不要继续把以下职责堆到 Handler：

* 解析配置、创建数据库连接和业务 service；
* API Key 查找、Project 推导和 Scope 判断；
* Batch 校验、hash 计算和事务写入；
* SQL 查询、分页 cursor 和错误映射；
* 后台 worker 的生命周期。

第一步是画出依赖方向，再逐个迁移 use case。迁移期间允许旧路由和新路由并存，但一个请求只能有一个最终业务入口，避免双写。

## 2. 前置知识

* Go package 的 import 方向和 interface 隔离；
* HTTP server 的 handler/middleware 生命周期；
* `context.Context` 取消、deadline 和 request-scoped values；
* `database/sql` 的 `BeginTx`、commit/rollback 和连接池；
* 结构化日志、Prometheus 风格指标和健康检查。

## 3. 目标拓扑

```text
                  +-----------------------+
                  |      HTTP Server       |
                  | request-id/recovery    |
                  | body-limit/auth/metric |
                  +-----------+-----------+
                              |
       +----------------------+----------------------+
       |                      |                      |
  Control API            Ingest API              Query API
       |                      |                      |
  control service       ingest service          query service
       |                      |                      |
       +----------------------+----------------------+
                              |
                   Repository interfaces
                              |
                       PostgreSQL adapter
                              |
                          PostgreSQL

 Operations workers (retention/usage/replay/agent-state)
     use the same service and repository contracts
```

Server 进程无本地业务状态。可以有进程内 metrics、短期 limiter 和 worker channel，但不能把尚未落库的 Batch 作为 ACK 语义的唯一承载。

## 4. 包边界和依赖规则

建议目录：

```text
internal/
  protocol/ingestv1/       # HTTP/JSON DTO、协议错误码、wire 校验
  domain/                   # ID、值对象、状态机、领域错误
  server/
    auth/                   #认证上下文和 API key service
    control/                # Project、Key、Agent、Pipeline use case
    ingest/                 # AcceptBatch use case
    query/                  # SearchEntries use case
    operations/             # retention、usage、quarantine、audit worker
    httpapi/                # router、handler、中间件、错误映射
    bootstrap/              # 配置、依赖组装、启动/关闭
  storage/
    postgres/               # db pool、migration、repository 实现
  platform/
    logging/
    metrics/
```

依赖方向应该是：

```text
cmd/server -> server/bootstrap -> server/{httpapi,control,ingest,query,operations}
server/* -> domain + 自己需要的窄 repository interface
storage/postgres -> domain + server 定义的 repository interface
protocol -> domain 的可转换类型（不反向依赖 HTTP）
```

具体规则：

1. `cmd` 不写业务规则，只读取配置并调用 bootstrap。
2. `httpapi` 不 import `pgx`/`sql`，也不直接拼 SQL。
3. service 不接收 `http.Request`，只接收 context 和领域输入。
4. repository interface 放在使用者一侧，避免一个巨型 `Store`。
5. row struct、DTO、domain struct 分离；映射代码可以显式但必须集中。
6. Operations worker 复用 service 的权限和状态规则，但不伪造 HTTP 请求。
7. 日志和指标 adapter 向外依赖，领域层不得依赖具体 logger。

## 5. 请求生命周期

### 5.1 通用中间件顺序

顺序本身是合同，建议从外到内固定为：

```text
panic recovery
  -> request id
  -> body/read timeout
  -> access log (不记录 secret/body)
  -> authentication (除公开 health)
  -> scope guard (按 route)
  -> handler
```

body limit 必须早于 JSON decode；认证必须早于任何从请求取 Project 的业务逻辑；query timeout 必须覆盖数据库调用而不是只覆盖 handler 的外壳。

### 5.2 Ingest 时序

```text
HTTP request
  -> 限制 body
  -> API Key hash lookup
  -> AuthContext{projectID, scopes, keyID}
  -> Decode BatchDTO
  -> DTO -> Domain Batch
  -> AcceptBatch(ctx, auth, batch)
       -> validate size/count/time
       -> payload hash
       -> BEGIN
       -> insert ingest_batch (unique project+batch)
       -> if new: insert entries
       -> COMMIT
  -> 200 accepted/duplicate
```

如果事务失败或上下文取消，响应必须是错误，不得伪造 ACK。相同批次并发到达时，依赖唯一约束和冲突处理，而不是依赖进程内 mutex；多个 Server 实例也必须得到相同结果。

### 5.3 Query 时序

```text
HTTP request
  -> AuthContext{projectID, query scope}
  -> ParseQueryInput
  -> normalize filter + decode cursor
  -> QueryService.Search
       -> repository SQL includes project_id predicate
       -> ORDER BY observed_at DESC, id DESC
       -> LIMIT requested+1 (capped)
  -> encode next cursor
  -> response DTO
```

cursor 必须包含排序键和必要的查询版本信息，不能让客户端修改成另一个 Project 的游标。cursor 解码失败返回可操作的 400，而不是回退到第一页。

## 6. 领域 service 的窄接口

接口应该表达业务所需的最小能力。下面是骨架，实际字段以 [02-domain-model-and-data-model.md](./02-domain-model-and-data-model.md) 为准：

```go
type BatchRepository interface {
    Insert(ctx context.Context, tx Tx, batch BatchRow) (InsertResult, error)
    InsertEntries(ctx context.Context, tx Tx, rows []EntryRow) error
    FindByID(ctx context.Context, projectID ProjectID, batchID BatchID) (BatchRow, error)
}

type TxManager interface {
    WithinTx(ctx context.Context, fn func(context.Context, Tx) error) error
}

type AcceptBatchService struct {
    tx       TxManager
    batches  BatchWriter
    entries  EntryWriter
    hasher   PayloadHasher
}

func (s *AcceptBatchService) Accept(
    ctx context.Context,
    auth AuthContext,
    batch ingestv1.Batch,
) (AcceptResult, error) {
    if err := auth.Require("ingest"); err != nil {
        return AcceptResult{}, err
    }
    domainBatch, err := NewBatchFromProtocol(auth.ProjectID, batch, s.hasher)
    if err != nil {
        return AcceptResult{}, err
    }
    // The ACK boundary is the successful return from WithinTx.
    return s.acceptInTransaction(ctx, domainBatch)
}
```

这里的 `Tx` 是 storage adapter 暴露的最小事务抽象；业务层不应到处调用具体 driver 的 `*sql.Tx`。如果当前阶段暂时直接使用 `*sql.Tx`，也要把这种耦合限定在 `storage/postgres` 内，并在迁移记录中说明。

## 7. HTTP 适配器骨架

Handler 只做四件事：解码、调用、映射、写响应。

```go
func (h *Handler) postBatch(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    var in ingestv1.BatchRequest
    if err := decodeJSON(w, r, &in); err != nil {
        writeProblem(w, err)
        return
    }
    auth, ok := authctx.FromContext(ctx)
    if !ok {
        writeProblem(w, ErrUnauthenticated)
        return
    }
    result, err := h.ingest.Accept(ctx, auth, in.Batch)
    if err != nil {
        writeProblem(w, err)
        return
    }
    writeJSON(w, http.StatusOK, result)
}
```

不要把 `project_id` 从 `in` 取出来覆盖 `auth.ProjectID`。DTO 中即使为了兼容旧客户端保留该字段，也必须忽略或拒绝它。

## 8. Bootstrap 和关闭顺序

`cmd/server` 只负责：

```go
func main() {
    cfg, err := config.Load(os.Environ())
    if err != nil { log.Fatal(err) }
    app, err := bootstrap.New(cfg)
    if err != nil { log.Fatal(err) }
    if err := app.Run(context.Background()); err != nil {
        log.Fatal(err)
    }
}
```

`bootstrap.New` 按依赖顺序创建：logger/metrics -> PostgreSQL pool -> repositories -> services -> router -> workers。启动顺序建议：

1. 解析并校验配置；
2. 连接数据库并执行可控 migration 检查；
3. 创建 repositories 和 service；
4. 启动 HTTP server；
5. 启动 operations workers；
6. readiness 在 1-4 成功后才返回 ready。

关闭顺序相反：先停止接受新请求，再停止/等待后台 worker，等待正在执行的业务请求，在 deadline 内关闭数据库 pool。`Close` 必须幂等，超时后保留未处理数据并记录原因。

```go
func (a *App) Shutdown(ctx context.Context) error {
    a.ready.Store(false)
    httpErr := a.http.Shutdown(ctx)
    workerErr := a.workers.Stop(ctx)
    dbErr := a.db.Close()
    return errors.Join(httpErr, workerErr, dbErr)
}
```

不要在 `defer db.Close()` 之后再启动需要数据库的 goroutine；这类启动顺序错误在本地很难出现，却会在进程停止时丢掉最后一批工作。

## 9. 分步实现

### Step 1：提取配置和错误合同

定义 `Config`、`Problem`、`ErrorCode` 和 `AuthContext`。先把超时、body limit、query limit、数据库 DSN 和 key pepper 作为显式配置，避免散落在 Handler 常量中。

### Step 2：定义领域接口

按业务用例定义 `ProjectRepository`、`APIKeyRepository`、`AgentRepository`、`BatchRepository`、`EntryRepository`、`AuditRepository`。每个接口只暴露一个模块需要的操作。

### Step 3：迁移路由

把现有 `/api/v1/batches` 适配到新 `ingest` service；保留原路由路径，先以行为测试保护当前调用者，再逐步删除旧模块里的业务逻辑。

### Step 4：组装完整 Server

创建 `App` 结构体持有 HTTP server、worker group、db pool 和 readiness 状态。将组装逻辑放在 bootstrap 测试中，使用 fake repository 验证没有跨层 import。

### Step 5：加入控制与运维路由

只有 Control/Ingest/Query 的 service 都有测试后，才增加 Admin 路由和后台 worker。每个 worker 使用 `context`、有限批次和可观察的错误处理。

## 10. 测试策略

测试稳定合同：

* Handler：请求解码、错误映射、body 限制和认证上下文存在性；
* Service：Project/Scope 检查、事务调用和领域状态机；
* Repository：唯一约束、查询排序、过滤和 cursor；
* Bootstrap：服务可构造、readiness 依赖数据库、关闭不泄漏 goroutine；
* Integration：真实 PostgreSQL 中的 ingest/query/retention；
* Race：并发重复批次和 worker stop；
* Fault injection：commit 失败、连接断开、worker cancellation、replay 失败。

不测试“某个 Handler 恰好调用某个私有 helper 三次”这种实现细节。应该测试外部可观察的结果：是否 ACK、是否落库、是否隔离、是否能再次启动恢复。

## 11. 验收证据

本章完成时至少应有：

```text
go test ./internal/server/... -count=1
go test ./internal/storage/... -count=1
go test -race ./... -count=1
go vet ./...
```

此外保存一份依赖图或静态检查结果，证明 `httpapi` 没有 import PostgreSQL adapter；保存一次启动/关闭日志，证明 readiness 在 DB 连接成功后才变为 ready。

## 12. 常见坑

* 用 `internal/server/store.Store` 包含所有表方法，导致任何模块都能读写任何数据；
* Handler 直接使用数据库 row 作为 JSON，数据库列改名即破坏 API；
* 把 worker 启动放进全局 `init()`，测试无法控制生命周期；
* health 只返回 200，不检查数据库或 migration 状态；
* server shutdown 立即 `os.Exit`，没有给 request/worker 留 deadline；
* 用请求 context 驱动需要跨请求继续的异步任务，客户端断开后任务被误取消；
* 在 access log 中记录 `Authorization`、payload 或完整 query；
* 为了“微服务结构”把每个平面先拆成独立进程，却没有给出数据一致性合同。

## 13. 复盘题

1. 为什么 repository interface 应由使用者定义，而不是由 PostgreSQL adapter 定义？
2. 如果 Query worker 和 HTTP Query 同时运行，怎样保证它们共享同一个查询边界和超时规则？
3. readiness 与 liveness 的失败条件分别是什么？数据库暂时不可用时两者应如何表现？
4. 哪些状态应放在 PostgreSQL，哪些短状态可以放在内存？为什么 ACK 相关状态不能只放内存？
5. 未来拆 Ingest Service 时，哪一个接口边界最可能保持稳定，哪一个需要版本化？

## 14. 本章完成门

* 能画出从 HTTP request 到 repository 到 PostgreSQL 的依赖方向；
* 能说明 Handler、service、repository、worker 各自不负责什么；
* 能解释 commit 后 ACK 在哪一层实现；
* 能启动一个 `App`，在 readiness 前拒绝业务流量，关闭时不直接杀进程；
* 能用一次 integration test 证明两个 Project 不能互读日志；
* 能指出未来拆分服务的证据条件，而不是把“微服务”作为默认终点。

