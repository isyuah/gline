# 09. 可观测性、限流配额与后台任务

一个后端系统“能跑”不代表它能运维。日志平台尤其容易出现一种危险状态：接入接口返回 2xx，但数据库连接池已经耗尽；Agent 仍显示发送成功，但查询延迟持续升高；后台清理删除了过多数据，却没有审计记录。

本章把可观测性、资源治理和后台任务放在一起，因为三者处理的是同一个问题：系统在资源不足、依赖异常和长时间运行时，如何让操作者知道发生了什么、限制损害范围并恢复业务。

## 9.1 当前差距与目标

在开始实现前，用代码和运行证据确认当前状态，不把占位 handler、日志字段或测试中的 fake 当成完整能力。典型差距包括：

- 没有统一 request ID、batch ID、project ID 的诊断上下文；
- metrics 名称、标签和单位未固定；
- `/healthz`、`/readyz` 的语义混用；
- 只有连接数限制，没有 Project 配额或查询成本上限；
- 后台任务直接在 handler 中同步执行；
- retention/replay 没有租约、幂等和审计；
- 健康检查把 PostgreSQL 短暂抖动误报成整个进程死亡；
- 诊断日志可能泄露 Authorization、原始日志正文或搜索词。

目标是让每一个运维判断都有证据：

```text
请求 -> 结构化日志 + metrics + 可选 trace
       -> 资源预算检查
       -> 业务处理/后台任务
       -> 状态变化和审计事件
```

## 9.2 前置知识和定义

### 四类观测信号

| 信号 | 适合回答 | Gline 示例 |
| --- | --- | --- |
| Log | 某一次具体事件是什么 | batch 永久失败、migration 失败 |
| Metric | 现象是否持续和趋势如何 | ingest error rate、spool bytes |
| Trace | 一次请求跨哪些边界 | HTTP -> repository -> PostgreSQL |
| Profile | CPU、内存和锁花在哪里 | pprof CPU/heap/mutex |

### 健康状态

- **live**：进程仍能响应，适合判断是否需要重启；不检查所有依赖。
- **ready**：实例是否应该接收流量；可以检查必需数据库连接、migration 状态和关键配置。
- **degraded**：进程可运行但某个可选能力不可用，例如 quarantine replay 暂停。
- **draining**：收到 shutdown 信号后不接收新工作，等待已有请求在 deadline 内结束。

健康检查是给调度器的协议，不是给人看的 debug 端点。不要把所有错误都返回 500，也不要在响应体中回显数据库 DSN。

## 9.3 诊断上下文与日志合同

在 HTTP 边界建立请求上下文：

```go
type RequestContext struct {
    RequestID string
    ProjectID string
    AgentID   string
    BatchID   string
}
```

字段原则：

- `request_id` 每个请求唯一；若客户端传入，先验证长度和字符集再接受，否则重新生成；
- `project_id`、`agent_id`、`batch_id` 是低基数或有限基数标识，应避免把它们全部塞进高频 metrics 标签；
- 只记录错误分类、耗时、大小、数量和稳定的资源 ID；
- 不记录 `Authorization`、API Key 原文、原始日志 content、任意搜索词；
- 错误日志包含 `error_code` 和安全的 `cause`，敏感细节只进入受控 debug 日志；
- 采样策略必须在代码和部署配置中可见，不能靠临时删日志。

统一日志事件例子：

```json
{
  "level": "warn",
  "event": "ingest_batch_duplicate",
  "request_id": "req_01...",
  "project_id": "proj_01...",
  "batch_id": "bat_01...",
  "payload_hash_match": true,
  "duration_ms": 14
}
```

示例中的 ID 只是格式示意，生产中不要复制固定 ID。日志中用布尔值 `payload_hash_match` 代替 hash 原文，避免无必要的高基数字符串。

## 9.4 Metrics 设计

先固定命名和单位，再写埋点。建议以 Prometheus 风格表达：

```text
gline_http_requests_total{route,method,status_class}
gline_http_request_duration_seconds{route,method}
gline_ingest_batches_total{result}
gline_ingest_entries_total{result}
gline_ingest_payload_bytes_total{result}
gline_ingest_inflight
gline_ingest_rejected_total{reason}
gline_db_pool_in_use
gline_db_pool_wait_duration_seconds
gline_query_rows_returned_total
gline_query_rejected_total{reason}
gline_background_job_runs_total{job,result}
gline_background_job_duration_seconds{job}
gline_background_job_lag_seconds{job}
gline_audit_events_total{event}
```

Agent 侧的关键指标在 Server ACK 语义下解释：

```text
gline_agent_spool_bytes
gline_agent_spool_batches
gline_agent_oldest_pending_seconds
gline_agent_send_attempts_total{result}
gline_agent_batches_reclaimed_total
gline_agent_quarantine_total{reason}
gline_agent_source_lag_seconds{pipeline}
```

标签限制：

- `route` 使用模板路径，不把原始 URL 放进去；
- `status_class` 使用 `2xx/4xx/5xx`，不要每个状态码都造成大量组合；
- `result`、`reason`、`job` 使用代码中固定的枚举；
- 不使用 `batch_id`、`request_id`、原始 IP、日志 service 名作为高频标签；
- 记录 bytes 和 seconds 时统一单位，命名体现 `_bytes`、`_seconds`。

告警应围绕用户结果，而不是某个 goroutine：

| 现象 | 证据组合 | 操作含义 |
| --- | --- | --- |
| 接入持续失败 | 5xx rate + DB pool wait + ingest lag | 检查数据库、迁移和容量 |
| Agent 积压 | spool bytes、oldest pending、send result | 暂停扩张输入，恢复 Server 或增加接入容量 |
| 查询退化 | p95/p99 + rows returned + query rejected | 缩小时间窗口、检查索引和连接池 |
| Retention 卡住 | job lag + deleted rows + DB locks | 先暂停批量删除，观察锁和 vacuum |
| Key 被滥用 | rejected by key + audit + request rate | 吊销或轮换凭证 |

## 9.5 Health、Readiness 和依赖检查

建议端点：

```text
GET /health/live
GET /health/ready
GET /health/details   # 仅管理凭证或本地调试可见
```

`live` 不访问数据库，避免数据库异常导致进程被调度器反复重启；`ready` 检查：

1. 配置已经解析且必需密钥存在；
2. 数据库 ping 在超时内成功；
3. schema 版本满足应用要求；
4. 进程不在 draining；
5. 关键资源没有超过不可接受的上限。

`ready` 失败时，接入请求应返回明确的临时错误，Agent 保留 batch 并重试。不要让 readiness 失败自动删除数据。

健康检查的代码骨架：

```go
type HealthChecker interface {
    Live(ctx context.Context) error
    Ready(ctx context.Context) (ReadyReport, error)
}

type ReadyReport struct {
    Status string            `json:"status"`
    Checks map[string]string `json:"checks"`
}
```

`/health/details` 不应该直接暴露 `err.Error()`，而应把内部错误转换为 `database_unreachable`、`migration_pending` 等稳定错误码，详细错误只写受控日志。

## 9.6 限流、配额和资源预算

Gline 至少有三层预算：

1. 请求预算：请求体大小、header 大小、单请求超时；
2. Project 预算：每分钟 entries/bytes、并发 ingest、查询时间窗口和返回行数；
3. 实例预算：HTTP 并发、数据库连接、后台任务并发和内存。

先做可解释的固定窗口或令牌桶，不要一开始引入跨节点精确限流。单节点实现可以使用内存 token bucket；水平扩展后再决定是否需要 Redis 或网关限流，并在文档中说明“单节点限流”与“集群全局限流”的语义差异。

```go
type Quota struct {
    MaxEntriesPerMinute int64
    MaxBytesPerMinute   int64
    MaxInflight         int
    MaxQueryWindow      time.Duration
    MaxQueryRows        int
}

type Admission interface {
    AllowIngest(ctx context.Context, keyID, projectID string, entries int, bytes int64) (Reservation, error)
    AllowQuery(ctx context.Context, projectID string, window time.Duration) (Reservation, error)
}

type Reservation interface {
    Commit()  // 新批次事务成功后确认 entries/bytes 消耗
    Release() // duplicate 或失败时退还，且始终释放并发槽
}
```

预留要有释放动作：

```go
reservation, err := admission.AllowIngest(ctx, projectID, entryCount, payloadBytes)
if err != nil {
    return writeProblem(w, ErrQuotaExceeded)
}
defer reservation.Release()

if result.Status == StatusAccepted {
    reservation.Commit()
}
```

不要在数据库事务提交之前永久扣除配额，也不要在请求失败后忘记释放并发槽。配额计数的精确一致性要根据目标说明：MVP 可以是实例级近似；如果产品声称跨节点严格额度，就必须引入共享计数存储和新的故障语义。

响应分类：

- `413 Payload Too Large`：单批次超过协议上限，Agent 不应无限重试；
- `429 Too Many Requests`：资源暂时不足，返回可选 `Retry-After`；
- `503 Service Unavailable`：实例不 ready 或依赖不可用，Agent 保留 batch；
- `403 Forbidden`：权限/项目状态问题，通常进入配置暂停或 quarantine。

### 9.6.1 当前源码：单实例 Ingest Admission

当前实现位于 `internal/server/admission`，由 bootstrap 注入
`ingest.Service`。完整顺序是：

```text
Bearer 鉴权成功
  -> 协议 Decode / Normalize / domain Validate
  -> API Key 请求令牌
  -> Project entries / bytes 令牌和 inflight 槽预留
  -> PostgreSQL ingest transaction
  -> accepted: Reservation.Commit
  -> duplicate / error: Reservation.Release 退还 entries / bytes
  -> 所有分支释放 inflight
```

配置入口：

| 环境变量 | 默认值 | 作用域 |
| --- | ---: | --- |
| `GLINE_INGEST_REQUESTS_PER_MINUTE` | 600 | 单实例、每 API Key |
| `GLINE_INGEST_ENTRIES_PER_MINUTE` | 120000 | 单实例、每 Project |
| `GLINE_INGEST_BYTES_PER_MINUTE` | 268435456 | 单实例、每 Project |
| `GLINE_INGEST_MAX_INFLIGHT` | 16 | 单实例、每 Project |

请求预算保护 Server 工作量，因此一个已鉴权但被 Project 预算拒绝的请求也会消耗
Key 请求令牌。entries/bytes 更接近业务用量：只有新 batch 成功提交后才最终扣除；
幂等 `duplicate`、事务回滚和其他失败都会退还。若单个 batch 已经大于整分钟容量，
返回 413，因为等待不会让它变得可接收；暂时耗尽返回 429 和向上取整的
`Retry-After` 秒数，Agent 按同一个 batch ID 与 payload 重试，不推进 checkpoint。
正常配置在 Server 启动时还会验证 entries/bytes 分钟容量至少放得下一个协议最大
batch，避免“协议允许但准入永远拒绝”的不可达状态；运行时的 413 是额外防御。

对应指标使用有限枚举：

```text
gline_server_admission_requests_total{result="accepted|rejected",reason="none|key_rate|project_entries|project_bytes|project_inflight"}
gline_server_admission_inflight
```

这里故意不使用 API Key ID 或 Project ID 作为 label。`admission accepted` 只表示
请求获得了本地预算，不等于数据库事务成功；最终结果看
`gline_server_ingest_batches_total`。限流器会清理长期不活跃且没有 inflight 的状态，
避免 Key 轮换无限增长内存。

水平扩展时，每个 Server 副本拥有独立 token bucket。若三台副本配置相同，集群
有效容量近似三倍且受负载均衡分布影响。这是当前明确的单实例近似语义；只有产品
确实需要全局配额时，才引入网关或共享存储，并重新设计可用性、延迟和故障降级。

## 9.7 后台任务模型

后台任务必须拥有自己的生命周期、租约、批量上限、取消和审计，而不是在 HTTP handler 里 `go func()`。

建议抽象：

```go
type Job interface {
    Name() string
    Run(ctx context.Context, now time.Time) (JobResult, error)
}

type JobResult struct {
    Processed int64
    Failed    int64
    NextRun   time.Time
}
```

第一批任务：

### Retention Worker

- 读取 Project 的 retention policy；
- 按时间范围和固定行数分批删除；
- 每批提交独立事务，避免长事务锁表；
- 记录删除行数、耗时、最大观察时间和错误；
- 删除后关注 PostgreSQL vacuum/膨胀，不承诺立即释放磁盘。

### Quarantine Replay Worker

- 只处理 `quarantine_batches.status = 'pending'` 的隔离记录；`replaying` 由租约持有者处理，不能重复 claim；
- 获取租约，防止多实例重复 replay；
- 使用原始 batch ID 和 payload replay；
- 成功得到 ACK/duplicate 后标记已处理；
- schema conflict 或 hash conflict 不自动无限重试；
- 每次人工或自动 replay 写 audit event。

### Agent State Worker

- 根据心跳时间判断 `active`、`stale`、`disabled`；
- 不因一次网络错误立即将 Agent 标为离线；
- 只生成状态事件，不修改 Agent 的本地 spool；
- 事件去重，避免每个轮询周期写一条相同审计。

### Usage Aggregator

- 按 Project 和时间桶汇总 entries、bytes、rejects；
- 输入来自已提交的 ingest 结果，而非请求开始计数；
- 可以异步近似，不影响日志主写入事务；
- 明确迟到数据如何修正已生成的桶。

租约表骨架：

```sql
CREATE TABLE job_leases (
    job_name TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    heartbeat_at TIMESTAMPTZ NOT NULL
);
```

更新租约必须带 owner 和未过期条件。进程崩溃后由其他实例在租约过期后接管。Job 处理必须幂等，即使租约在网络分区中短暂重叠，也不能产生不可接受的重复业务结果。

## 9.8 审计和安全诊断

审计和运行日志不同：运行日志回答“程序发生了什么”，审计回答“谁在什么时候对哪个资源做了什么”。至少审计：

- Project 创建、禁用、配置改变；
- API Key 创建、轮换、吊销；
- Agent 注册、禁用、配置版本变化；
- quarantine、replay、retention 手工触发；
- 管理员改变 quota 或 retention。

```sql
CREATE TABLE audit_events (
    id          bigserial PRIMARY KEY,
    project_id  uuid REFERENCES projects(id),
    actor_type  text NOT NULL,
    actor_id    text NOT NULL,
    action      text NOT NULL,
    resource    text NOT NULL,
    resource_id text NOT NULL,
    outcome     text NOT NULL CHECK (outcome IN ('success', 'rejected', 'failed')),
    metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
```

这里沿用第 02、06 章的 `audit_events` 合同；请求 ID 如果需要持久化，放入允许列表中的 `metadata.request_id`，不再另造一套 `occurred_at/resource_type` 字段。

`metadata` 只能放经过允许列表筛选的键。不能把 API Key、Authorization、原始日志正文、任意请求头或完整 SQL 放进去。

安全诊断清单：

- 401、403、429、413、503 的 metrics 分开；
- API Key 只存 hash/pepper 后的验证材料，不能存明文；
- 启动时检查配置文件权限和必需 secret；
- pprof 只监听本地或管理网络；
- details health 需要管理授权；
- 日志采样不能因为异常升高而输出敏感正文；
- 审计写入失败要有明确策略：关键管理操作失败则拒绝成功，普通 ingest 不因审计附加写入失败而改变数据合同，除非产品明确要求强审计。

## 9.9 实施顺序、测试与完成门

实施顺序：

1. 定义错误码、request context 和结构化日志字段；
2. 添加 live/ready 并用 fake dependency 验证状态；
3. 埋点 HTTP、ingest、query、DB pool 和 Agent spool 指标；
4. 加请求/Project/实例级限制；
5. 实现 Job runner 和 lease；
6. 落地 retention、quarantine replay、agent state、usage；
7. 加 audit event 与脱敏验证；
8. 最后写告警规则和 runbook。

测试与故障注入：

| 场景 | 应证明 |
| --- | --- |
| DB ping 超时 | ready=false，live 仍可响应 |
| Project quota 超限 | 429，Agent 保留 batch，指标可见 |
| Job 处理一半崩溃 | 已提交批次可重入，未提交批次可重试 |
| lease owner 崩溃 | 过期后由另一实例接管 |
| audit DB 暂时失败 | 按定义的关键/非关键策略处理，不静默吞错 |
| pprof/health 未授权访问 | 返回拒绝，不泄露内部细节 |

验收证据：

- dashboard 或文本输出能展示 ingest、query、spool、DB pool 和 job lag；
- readiness 在数据库不可用和 draining 两种场景下行为不同；
- quota 测试证明拒绝不会推进 Agent checkpoint；
- retention/replay 具备批次上限和取消；
- 同一管理操作有可检索 audit event；
- 所有 metrics 标签经过基数审查；
- 没有未经验证的吞吐、延迟或容量数字。

复盘题：

1. 为什么 live 不能直接等同于 ready？
2. 如果 Server 有三台实例，内存限流的准确含义是什么？
3. retention 与查询并发如何共享数据库预算？
4. Job lease 过期和数据库事务提交之间有什么重复处理窗口？
5. 哪些诊断信息即使对开发者有帮助也不能进入默认日志？

完成门：

- [ ] live/ready/draining 语义和状态码固定；
- [ ] 接入、查询、DB、spool、后台任务都有低基数指标；
- [ ] quota/限流拒绝不会误推进 checkpoint；
- [ ] 后台任务有 bounded batch、取消、租约和幂等；
- [ ] 管理操作有审计，敏感字段经过脱敏；
- [ ] 有最小可用告警和故障 runbook。
