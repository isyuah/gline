# 11. 接入、查询与 Retention 完整用例

> 本章把协议、鉴权和 PostgreSQL adapter 组合成用户可调用的纵向切片。它是实现路线，不代表当前仓库已完成持久化、查询或 retention。

## 1. 本章目标

完成后，Gline Server 应形成首个完整日志后端闭环：

```text
Agent batch
  -> HTTP limits/strict decode
  -> API Key authentication + ingest scope
  -> project context
  -> protocol/domain validation
  -> canonical payload hash
  -> PostgreSQL transaction
  -> commit
  -> accepted/duplicate ACK

Developer query
  -> API Key authentication + query scope
  -> bounded filters + cursor
  -> project-isolated keyset SQL
  -> response + next cursor

Retention worker
  -> bounded delete batches
  -> metrics/progress
  -> idempotency metadata kept for guarantee window
```

本章还要求你能解释三个不同时间语义：

- ingest commit 时间：Server 何时 ACK。
- query visibility：commit 后何时可查。
- retention cutoff：何时允许删除 entry 与幂等 metadata。

### 1.1 当前代码差距

当前上传路径只把 `entries` 交给打印型 Sink，成功响应不代表持久化；仓库尚无查询 endpoint、PostgreSQL Repository 或 retention worker。本章从已有原型出发描述目标纵向切片，所有数据库查询、后台任务和性能结论都必须在实现后用真实证据验收。

## 2. 模块边界

推荐布局：

```text
internal/server/ingest/
  service.go
  repository.go
  errors.go

internal/server/query/
  service.go
  repository.go
  cursor.go
  errors.go

internal/server/retention/
  worker.go
  repository.go
  policy.go

internal/server/httpapi/
  ingest_handler.go
  query_handler.go
  errors.go

internal/storage/postgres/
  batch_repository.go
  entry_repository.go
  retention_repository.go
```

职责：

- Handler：HTTP 解码、认证主体读取、response/error 映射。
- Service：用例编排和领域策略。
- Repository interface：用例所需的最小持久化能力。
- PostgreSQL adapter：事务、SQL、driver error 翻译。
- Worker：定时调度、取消、退避和指标，不写 SQL。

## 3. Ingest 用例

### 3.1 Service 接口

```go
type Service interface {
    Accept(ctx context.Context, projectID auth.ProjectID, request BatchInput) (AcceptResult, error)
}

type AcceptStatus uint8

const (
    AcceptStatusAccepted AcceptStatus = iota + 1
    AcceptStatusDuplicate
)

type AcceptResult struct {
    Status     AcceptStatus
    BatchID    BatchID
    EntryCount int
}
```

也可以让 HTTP mapper 先构造带 Project 的 domain `Batch`，然后 Service 只接收 Batch。只选一种方式，避免 `projectID` 参数和 Batch 内 Project 不一致。

### 3.2 Service 流程

```go
func (s *service) Accept(ctx context.Context, batch Batch) (AcceptResult, error) {
    if err := s.validator.Validate(batch, s.limits); err != nil {
        return AcceptResult{}, err
    }

    hash, err := PayloadSHA256(batch)
    if err != nil {
        return AcceptResult{}, fmt.Errorf("hash batch payload: %w", err)
    }

    result, err := s.repository.Accept(ctx, batch, hash)
    if err != nil {
        return AcceptResult{}, err
    }
    return result, nil
}
```

不要在 service 中启动 goroutine 异步写数据库。外部 ACK 的持久化边界就是当前 PostgreSQL transaction，直接等待它完成。

### 3.3 校验顺序

建议按成本从低到高：

1. HTTP Content-Type 与 body bytes。
2. JSON 语法、unknown fields、trailing content。
3. protocol version。
4. batch ID、agent ID、entries count。
5. 每个 entry 字段、sequence、attributes。
6. 规范化并计算 hash。
7. 进入数据库事务。

无效请求不能调用 repository。这样避免用数据库承担输入校验，也防止无意义事务耗尽 pool。

### 3.4 时间处理

- 所有协议时间必须带 offset；映射后统一 UTC。
- `observed_at` 必填。
- `event_time` 可空。
- `sent_at` 只用于诊断，不用于判断是否允许写入。
- 可以拒绝离当前时间极端遥远的值，但要谨慎：Agent 主机时钟可能错误。更安全的是接收、记录 clock skew 指标，并只对数据库支持范围外的值拒绝。

不要用 Server 当前时间覆盖 Agent observed time；另有 `received_at` 表示 Server 接收。

### 3.5 Attributes 策略

MVP 推荐允许：

- JSON null、boolean、number、string。
- 有界数组/对象（若确实需要）。
- 总字节、深度、键数、单 key 长度有界。

如果没有嵌套使用场景，可以只允许一层 scalar map，极大简化 hash、查询和存储。应在协议中明确，不能一边接受任意 JSON，一边声称资源有界。

敏感字段策略也要诚实：Gline 不能自动知道每条日志里的密码/token。可提供 Agent 侧字段删除/正则脱敏，但这不替代业务应用避免记录 secret。Server 至少不能把 body 再写到自己的日志。

## 4. Ingest HTTP Handler

```go
func (h *IngestHandler) Handle(c *gin.Context) {
    principal, ok := auth.PrincipalFromContext(c.Request.Context())
    if !ok {
        h.errors.Internal(c, errors.New("principal missing after auth middleware"))
        return
    }

    var req ingestv1.BatchRequest
    if err := httpdecode.JSON(c.Writer, c.Request, &req, h.limits.MaxBodyBytes); err != nil {
        h.errors.Decode(c, err)
        return
    }

    batch, err := h.mapper.ToDomain(principal.ProjectID, req)
    if err != nil {
        h.errors.Domain(c, err)
        return
    }

    result, err := h.service.Accept(c.Request.Context(), batch)
    if err != nil {
        h.errors.Domain(c, err)
        return
    }

    c.JSON(http.StatusOK, ingestv1.BatchResponse{
        Status:          mapStatus(result.Status),
        BatchID:         result.BatchID.String(),
        AcceptedEntries: result.EntryCount,
    })
}
```

`accepted_entries` 在 duplicate 时仍返回原 batch entry count，不是本次新增行数。字段名可讨论，若容易误解可叫 `entry_count`；一旦公开就保持兼容。

### 4.1 HTTP 状态映射

| 内部结果/错误 | HTTP | code/status |
| --- | --- | --- |
| 首次提交 | 200 | `accepted` |
| 已有相同 payload | 200 | `duplicate` |
| validation | 400 | `invalid_request` |
| protocol version | 400 | `unsupported_protocol` |
| ID 相同、payload 不同 | 409 | `idempotency_conflict` |
| body 过大 | 413 | `request_too_large` |
| auth/scope | 401/403 | 见鉴权章 |
| DB timeout/unavailable | 503 | `service_unavailable` |
| 未分类内部错误 | 500 | `internal_error` |

### 4.2 不使用 202 的原因

HTTP 202 通常表示请求已接受但尚未完成。如果返回 202 时 batch 只在内存，Server 崩溃仍会丢。当前设计直接等待 PostgreSQL commit，用 200 表示真正 ACK，语义更简单。

未来若引入持久化消息队列，可以在“队列 commit”后 ACK，但必须重新说明日志何时对 query 可见。

## 5. Ingest 并发与背压

### 5.1 并发来源

- 多 Agent 并发。
- 一个 Agent 多 dispatcher 并发。
- 网络超时导致同 batch 重叠重试。
- Server 多实例同时收到同 batch。

唯一约束处理正确性，连接池和限流处理资源上界。

### 5.2 Server 内是否增加 channel

MVP 不需要额外 ingest channel。HTTP request 本身就是自然并发单元，数据库 pool 限制并发事务。额外内存队列会引入：

- 队列满时如何响应；
- 进程崩溃丢失；
- 何时 ACK；
- shutdown 如何 drain；
- 多实例如何分配。

只有 profile 证明 HTTP goroutine/DB dispatch 需要独立控制时再引入，而且 ACK 仍不能早于持久化边界。

### 5.3 限流与 429

按 Project/key 的有限并发和速率可以保护系统。429 必须带可解析的 `Retry-After`，Agent 使用退避。不要在排队已经无限增长后才限流。

## 6. Query 用例

### 6.1 目标接口

```http
GET /api/v1/entries
    ?from=2026-08-23T08:00:00Z
    &to=2026-08-23T09:00:00Z
    &service=orders
    &level=ERROR
    &host=host-a
    &q=timeout
    &limit=100
    &cursor=...
```

MVP 是受控过滤 API，不是 Elasticsearch DSL。用户能完成最常见故障定位：在一个时间窗口内按 service/level/host 筛选并搜索 message。

### 6.2 时间窗口

使用半开区间：

```text
[from, to)
```

即 `from <= observed_at < to`。优点：相邻窗口 `[08:00,09:00)` 与 `[09:00,10:00)` 不重叠。

策略：

- `from/to` 建议必填，或提供很短且明确的默认窗口。
- `from < to`。
- 最大时间跨度可配置且有绝对上限。
- 非管理员 query key 不能绕过上限。
- 所有时间解析后转 UTC。

### 6.3 Query Service

```go
type Service interface {
    List(ctx context.Context, projectID auth.ProjectID, input ListInput) (Page, error)
}

func (s *service) List(ctx context.Context, projectID auth.ProjectID, in ListInput) (Page, error) {
    filter, err := s.normalize(projectID, in)
    if err != nil {
        return Page{}, err
    }
    return s.repository.List(ctx, filter)
}
```

normalize 负责：

- 解析/验证 level。
- trimming 与字符串长度。
- 默认/最大 limit。
- 时间范围。
- cursor 解码。
- cursor 的 filter hash 匹配。

ProjectID 是独立参数或被构造进 Filter，永远不来自 URL。

## 7. Keyset Pagination

### 7.1 首屏 SQL

```sql
SELECT
    id,
    batch_id,
    sequence,
    observed_at,
    event_time,
    agent_id,
    pipeline_id,
    service,
    host,
    level,
    message,
    attributes
FROM log_entries
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
ORDER BY observed_at DESC, id DESC
LIMIT $4;
```

Repository 实际传入 `clientLimit + 1`。

### 7.2 下一页 SQL

```sql
... AND (observed_at, id) < ($cursor_time, $cursor_id)
ORDER BY observed_at DESC, id DESC
LIMIT $n;
```

PostgreSQL row comparison 与排序列、方向一致。若改为 ASC，cursor 比较符也必须改。

### 7.3 为什么不用 offset

`OFFSET 100000` 通常仍需扫描/跳过前面大量行，且并发插入会导致页漂移。keyset 从上次位置继续，复杂度和稳定性更好。

### 7.4 一致性边界

普通 keyset pagination 不是数据库快照：

- 第一页之后新增、排序位置更靠前的数据不会突然挤进后续页，这是期望行为。
- 新增但带较早 `observed_at` 的 backfill 数据可能出现在后续页。
- retention 可能删除尚未读取的数据。

MVP 文档应说明“稳定位置分页，不提供跨多请求快照一致性”。若以后需要导出一致快照，要设计 snapshot/export job，不能把长数据库事务跨 HTTP 请求保持。

## 8. 安全的动态过滤 SQL

推荐 builder 只拼接代码内固定片段，所有值参数化：

```go
func buildListQuery(f query.Filter, fetch int) (string, []any) {
    var b strings.Builder
    args := []any{f.ProjectID, f.From, f.To}

    b.WriteString(`SELECT ... FROM log_entries
        WHERE project_id = $1
          AND observed_at >= $2
          AND observed_at < $3`)

    if f.Service != nil {
        args = append(args, *f.Service)
        fmt.Fprintf(&b, " AND service = $%d", len(args))
    }
    if f.Level != nil {
        args = append(args, f.Level.String())
        fmt.Fprintf(&b, " AND level = $%d", len(args))
    }
    // host、literal text、cursor 同理；列名与操作符不可来自用户输入。

    args = append(args, fetch)
    fmt.Fprintf(&b, " ORDER BY observed_at DESC, id DESC LIMIT $%d", len(args))
    return b.String(), args
}
```

这里 `fmt.Fprintf` 只写由代码决定的参数编号，不写用户值，因此不构成 SQL 注入。

### 8.1 `q` 的语义

建议定义为 message 的大小写不敏感“字面包含”，不是 wildcard。选择 `!` 作为明确的 escape 字符，并在参数中把 `!`、`%`、`_` 分别转换为 `!!`、`!%`、`!_`：

```sql
AND message ILIKE '%' || $n || '%' ESCAPE '!'
```

这样不依赖 PostgreSQL 字符串字面量对反斜杠的配置。为 `!`、`%`、`_`、反斜杠和 Unicode 写测试；反斜杠在这里是普通字符，不应被多转义。

如果直接允许 wildcard，应在 API 文档明确，并仍限制长度/窗口；不要让行为偶然取决于 SQL LIKE。

### 8.2 空过滤值

`service=` 是非法、等价未提供还是匹配空字符串，必须统一。由于领域 service 不允许空，建议空值返回 400，而不是悄悄忽略拼错的客户端请求。

## 9. Cursor Codec

### 9.1 规范化过滤器摘要

为了避免把一个查询 cursor 用在不同过滤器上，可 hash：

```text
cursor-filter-version
from UTC microseconds
to UTC microseconds
optional service marker + value
optional level marker + value
optional host marker + value
optional q marker + normalized value
```

不要包括 limit：用户可以在后续页调小 limit；是否允许调大应由上限决定。Project 可不放 cursor，因为由 Principal 强制，但放 Project 摘要也能更早给出 invalid cursor；绝不能让 cursor 的 Project 覆盖 Principal。

### 9.2 编码/解码

```go
type Position struct {
    ObservedAt time.Time
    EntryID    uuid.UUID
}

type Codec interface {
    Encode(position Position, filterHash [32]byte) (string, error)
    Decode(raw string, expectedFilterHash [32]byte) (Position, error)
}
```

decode 限制：

- 编码字符串长度。
- base64 decode 后长度。
- JSON unknown fields/trailing content。
- cursor version。
- 时间范围和 UUID。
- filter hash constant-time 比较不是安全必需，但可复用稳定 helper。

invalid cursor 返回 400 `invalid_request`，不要返回 500。

### 9.3 Cursor 不能授权

base64 只是不透明表示，不是签名。即使用户修改时间/ID，SQL 仍必须用 Principal Project 和查询限制。需要防篡改时加入 HMAC，但安全边界不能依赖“用户看不懂 cursor”。

## 10. Query Response 映射

领域 result 与 API DTO 分离：

```go
type Page struct {
    Entries    []Entry
    NextCursor string
}
```

response 规则：

- entries 稳定按 `(observed_at DESC, id DESC)`。
- `attributes` 输出 object。
- `event_time` 缺失时省略或 null，协议固定一种。
- 无下一页时省略 `next_cursor` 或返回 null，固定一种。
- 不返回内部 Project ID、hash、数据库 received row state。

如果需要 Project 信息，调用者本来已由 key 绑定，不需要每条重复。

## 11. Query Timeout、取消与资源上界

每个查询有：

- HTTP/request deadline。
- service 层最大窗口与 limit。
- repository query timeout。
- PostgreSQL statement timeout（可按连接/事务设置）。

这些值要协调：DB timeout 应略短于 HTTP write deadline，使 Server 有时间返回结构化 503，而不是连接直接断开。

取消必须传播到 pgx query。客户端断开后，不应继续扫描大量日志。

不要允许：

- 无时间范围全表搜索。
- 无上限 `limit`。
- 任意排序字段。
- attributes 任意 JSONPath DSL。
- 原始 SQL。

## 12. Retention 的产品语义

Retention 回答：“日志保留多久，以及系统如何在不阻塞接入的情况下删除旧数据？”

MVP 可以先全局配置：

```go
type Policy struct {
    EntryRetention         time.Duration
    IdempotencyRetention   time.Duration
    RunInterval            time.Duration
    DeleteBatchSize        int
    OperationTimeout       time.Duration
}
```

必须校验：

```text
IdempotencyRetention >= documented maximum Agent retry/offline window
```

`EntryRetention` 与 `IdempotencyRetention` 不必机械比较大小：entry cutoff 通常基于 `observed_at`，metadata cutoff 基于 `received_at`，二者不是同一个时间原点。只要 entries 仍存在，外键和清理条件就会阻止 metadata 删除；entries 已过期后，metadata 仍应覆盖承诺的合法重试窗口。MVP 使用保守固定窗口并明确保证范围。未来按 Project 配置时，Policy Repository 也必须带 Project。

## 13. 为什么不能一次 DELETE 全部旧数据

大事务会：

- 长时间持锁；
- 产生大量 WAL；
- 增加 vacuum 压力；
- 与 ingest/query 争用 I/O；
- shutdown 时难以取消；
- 失败后回滚成本大。

使用有界小批次，循环间允许取消和短暂停顿。

## 14. Entry 删除 SQL

一种 PostgreSQL 小批删除模式：

```sql
WITH doomed AS (
    SELECT project_id, id
    FROM log_entries
    WHERE observed_at < $1
    ORDER BY observed_at ASC, id ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
DELETE FROM log_entries AS e
USING doomed AS d
WHERE e.project_id = d.project_id
  AND e.id = d.id
RETURNING e.project_id, e.id;
```

说明：

- `$1` cutoff 在一次 worker run 开始时固定，避免循环中边界移动。
- `$2` 有配置上限。
- `SKIP LOCKED` 允许跳过被其他事务使用的行；单 worker 也可不需要，但为未来并发保留时要验证计划。
- `RETURNING` 可统计实际删除数，但返回大量 ID 也有成本；也可使用 command tag rows affected。

如果每个 Project retention 不同，应按 Project 逐个执行，且有公平调度，避免最大 Project 永远占满每轮。

## 15. Batch Metadata 清理

只有同时满足才允许删除：

1. `received_at < idempotency_cutoff`。
2. 已无关联 entry。

有界删除示意：

```sql
WITH doomed AS (
    SELECT b.project_id, b.batch_id
    FROM ingest_batches AS b
    WHERE b.received_at < $1
      AND NOT EXISTS (
          SELECT 1
          FROM log_entries AS e
          WHERE e.project_id = b.project_id
            AND e.batch_id = b.batch_id
      )
    ORDER BY b.received_at ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
DELETE FROM ingest_batches AS b
USING doomed AS d
WHERE b.project_id = d.project_id
  AND b.batch_id = d.batch_id;
```

需要为 `log_entries(project_id,batch_id,sequence)` 的 UNIQUE 索引和 batch received index 验证计划。不要在 entries 仍存在时依赖 `ON DELETE CASCADE` 快速删 batch，因为这会绕过 entry retention 语义并制造超大 cascade transaction。

## 16. Retention Worker 生命周期

```go
type Worker struct {
    repo   Repository
    policy Policy
    clock  Clock
    logger zerolog.Logger
}

func (w *Worker) Run(ctx context.Context) error {
    ticker := time.NewTicker(w.policy.RunInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            if err := w.runOnce(ctx); err != nil {
                w.recordFailure(err)
            }
        }
    }
}
```

`runOnce`：

1. 用 clock 计算固定 cutoffs。
2. 循环删 entry，每批一个短 operation context。
3. 本批返回 0 或少于 limit 时停止。
4. 在批次间检查 cancellation。
5. 再小批清理 orphan batch metadata。
6. 记录删除数、持续时间、最后成功时间。

### 16.1 防止重叠运行

一次 run 未结束时下一个 tick 到来，不要再开 goroutine。上面的同步循环天然防重叠。多 Server 实例会各自启动 worker，需选择：

- 部署只让一个实例启用 retention；或
- PostgreSQL advisory lock 选主；或
- `SKIP LOCKED` 允许合作，但仍要避免重复全局指标和不公平。

MVP 最简单是配置 `RETENTION_ENABLED`，Compose 只启一个；文档明确多实例限制。若使用 advisory lock，必须测试连接断开后的锁释放，并确保锁绑定 session/transaction 的方式正确。

### 16.2 失败策略

- 单轮失败不终止整个 Server。
- 记录有限字段的错误和失败计数。
- 下个 interval 重试，必要时 exponential backoff。
- 持续失败导致 readiness 是否失败要谨慎：日志仍可接入时，直接 not ready 可能扩大故障。更适合作为 degraded metric/告警。
- shutdown 取消 operation context，不无限等待 delete。

## 17. Retention、Vacuum 与磁盘

PostgreSQL DELETE 不会立即把所有磁盘归还操作系统。需要监控：

- dead tuples；
- autovacuum 是否跟上；
- table/index size；
- WAL；
- delete duration；
- ingest/query 延迟受影响程度。

不要通过每轮 `VACUUM FULL` 回收空间，它会强锁表，不适合在线常规任务。先调整批次、autovacuum 和索引；当大量时间范围删除成为确定瓶颈，再评估分区 drop。

## 18. Metrics 设计

### 18.1 Ingest

```text
gline_server_ingest_requests_total{result="accepted|duplicate|conflict|invalid|error"}
gline_server_ingest_entries_total{result="accepted|duplicate"}
gline_server_ingest_duration_seconds
gline_server_ingest_batch_entries
gline_server_db_transaction_duration_seconds{operation="ingest"}
```

### 18.2 Query

```text
gline_server_query_requests_total{result="success|invalid|timeout|error"}
gline_server_query_duration_seconds
gline_server_query_result_entries
```

### 18.3 Retention

```text
gline_server_retention_runs_total{result="success|error"}
gline_server_retention_deleted_entries_total
gline_server_retention_deleted_batches_total
gline_server_retention_duration_seconds
gline_server_retention_last_success_timestamp_seconds
```

禁止把 project ID、key ID、service、host、query text、batch ID 当默认 Prometheus label。它们基数高或敏感。逐 Project 诊断可使用受控日志或有限 admin API。

## 19. 结构化日志

Ingest 成功通常不逐 batch 写 info 日志，否则高吞吐下日志系统记录自己的接入会形成噪声。推荐：

- metrics 统计正常流量。
- conflict 记录 request ID、Project 的内部安全 ID、batch ID、agent ID，不记录 payload。
- database error 记录 operation、request ID、分类、安全 cause。
- query 慢请求记录 route、duration、结果数、时间跨度，不记录原始 q/message。
- retention 每轮输出汇总，不逐 entry 输出。

是否记录 Project ID 取决于日志访问边界；它不是 secret，但可属于内部元数据。公开错误响应不返回。

## 20. 测试策略：Ingest

### 20.1 Service 单元测试

- 无效 Batch 不调用 repository。
- hash 失败不调用 repository。
- accepted/duplicate 原样返回。
- conflict 不被吞掉。
- context cancellation 传播。

fake repository 只实现一个窄接口，不需要 mock SQL。

### 20.2 Handler 合同测试

- body 中无法指定 Project。
- principal Project 正确进入 domain。
- accepted response。
- duplicate response。
- 409 conflict。
- 413 body too large。
- 400 invalid/unknown/trailing JSON。
- 503 database unavailable。
- 500 不泄露内部错误。
- response 总带 request ID。

### 20.3 PostgreSQL 集成测试

沿用第 10 章：首次、重复、冲突、并发、事务回滚、双 Project。

### 20.4 HTTP + DB 纵向测试

使用真实 router 和真实测试数据库：

1. 插入 Project/key fixture。
2. POST batch。
3. 检查 200 accepted。
4. POST 完全相同 body，检查 200 duplicate。
5. POST 相同 batch ID、改变 message，检查 409。
6. 数据库 entry 数始终为第一次 batch 数量。

这个测试证明 transport、auth、mapper、hash、transaction 和 response 共同工作。

## 21. 测试策略：Query

### 21.1 Filter/codec 单元测试

- from/to 半开区间。
- from >= to。
- 窗口超限。
- limit 默认/上限。
- level 大小写规范策略。
- 空 service/host/q。
- cursor round-trip。
- cursor 未知版本/超长/损坏。
- A filter cursor 用于 B filter 被拒绝。

### 21.2 Repository 集成测试

构造特别数据：

- 两个 Project。
- 多个 service/level/host。
- 多条完全相同 `observed_at`。
- 边界恰好等于 from/to。
- message 含 `%`、`_`、反斜杠和 Unicode。

验证：

- Project A 永不返回 B。
- from 包含，to 不包含。
- `(time,id)` 排序稳定。
- 多页无重复、无遗漏。
- q 按字面包含，不把 `%` 当 wildcard。
- limit + 1 正确决定 next cursor。

```go
func TestListPaginatesEqualTimestampsWithoutDuplicates(t *testing.T) {
    // 插入多条相同 observed_at、不同 id 的 entries。
    // 用小 limit 遍历全部 cursor。
    // 将返回 ID 放入 set，断言数量和顺序。
}
```

不要断言内部 SQL 字符串完全相等。测试外部行为，并另用 query plan 检查性能。

## 22. 测试策略：Retention

使用 fake clock，不真实等待 interval。

### 22.1 Repository 集成测试

- cutoff 前 entry 被删，等于/晚于 cutoff 保留。
- 每次最多删除 batch size。
- 多轮最终清空到期数据。
- metadata 在 idempotency cutoff 前保留。
- metadata 仍有关联 entries 时不删除。
- Project/key 表不受影响。

### 22.2 Worker 单元测试

- runOnce 固定 cutoff。
- repository 返回 0 时停止。
- context cancel 立即结束。
- 单轮错误记录后下轮仍可执行。
- 不发生重叠 run。
- shutdown 不等待下一个 ticker。

### 22.3 与幂等窗口组合测试

1. 写入 batch。
2. entry retention 到期并删除 entries。
3. metadata 保留窗口内重试相同 batch。
4. 应返回 duplicate，不能重新插入 entries。
5. 超过明确 idempotency window 后 metadata 才可删除。

第 4 步暴露一个产品选择：entries 已按 retention 删除，但 duplicate 请求不会“复活”它们。这是正确的；retention 代表数据已经过期。文档需说明 Agent 不应在支持窗口内晚于 entry retention 才首次确认。

更稳妥的配置关系通常要求 entry retention 远大于 Agent 最大离线窗口。

## 23. 端到端演示

完成 Server 闭环后，设计一个可重复演示：

### 23.1 正常路径

1. Compose 启动 PostgreSQL，运行 migration，再启动 Server。
2. 创建 demo Project。
3. 创建 ingest/query 两个 key。
4. 用 Agent 或 curl 上传带固定 batch ID 的 5 条日志。
5. 查询 `service=orders&level=ERROR`。
6. 返回稳定排序结果。

### 23.2 幂等路径

1. 重复完全相同请求。
2. 响应 `duplicate`。
3. 查询和 SQL 计数证明没有重复 entry。
4. 修改 payload 但复用 ID。
5. 响应 409，数据库原数据不变。

### 23.3 权限路径

1. ingest key 查询得到 403。
2. query key 上传得到 403。
3. Project B key 查询不到 Project A 日志。

### 23.4 重启路径

1. 上传成功后停止并重新启动 Server。
2. 查询仍能返回数据。
3. 重试旧 batch 仍 duplicate。

这些证据比“接口返回 200”更能说明后端设计。

## 24. 故障注入矩阵

| 故障点 | 预期 Server 行为 | Agent 行为 | 最终数据库 |
| --- | --- | --- | --- |
| JSON 解码前断开 | 无事务 | 保留/重试 | 无新增 |
| batch metadata insert 后、entry insert 前失败 | rollback | 重试同 ID | 最终一份 |
| 部分 bulk insert 后失败 | rollback 全部 | 重试同 ID | 最终一份完整 batch |
| commit 前连接断开 | 可能 rollback | 重试同 ID | 最终一份 |
| commit 成功、200 丢失 | 已持久化 | 重试同 ID | duplicate，不增加 |
| duplicate response 丢失 | 已持久化 | 再次重试 | 仍 duplicate |
| 数据库 unavailable | 503/连接失败 | 指数退避 | 不产生部分数据 |
| payload conflict | 409 | quarantine/人工处理 | 原 batch 不变 |
| retention 删除运行中 | 小批事务 | 不相关 | ingest/query 延迟有界 |

故障测试使用带 run ID 和连续 sequence 的数据生成器，检查缺失集合与重复集合。只比较总行数无法发现“一条丢失 + 另一条重复”。

## 25. 性能实验

在正确性通过后再测性能：

### 25.1 Ingest

变量：

- batch entries；
- batch bytes；
- Agent 并发；
- Server 实例数；
- pool size；
- bulk insert 方式。

指标：

- entries/s；
- request p50/p95/p99；
- commit latency；
- pool wait；
- PostgreSQL CPU/I/O/WAL；
- duplicate 路径延迟。

### 25.2 Query

变量：

- 总数据量；
- 时间窗口；
- Project 数据倾斜；
- filter 组合；
- q 是否存在；
- page size；
- 同时 ingest 负载。

证据：

- p95/p99；
- `EXPLAIN (ANALYZE, BUFFERS)`；
- scanned/returned rows；
- index/table size；
- sort 与 temp file。

### 25.3 Retention

变量：batch size、interval、到期总量；观察 ingest/query 延迟、WAL、dead tuples、vacuum。

每个结果必须带 commit、硬件、PostgreSQL 版本、数据集和配置。不要提前写“数万条/秒”或“毫秒查询”。

## 26. OpenAPI 与客户端合同

当 endpoint 稳定后，为以下内容生成/维护 OpenAPI：

- Bearer auth scheme。
- `/api/v1/batches` request/accepted/duplicate/error。
- `/api/v1/entries` filters/page/error。
- 稳定 error code enum。
- limit、字段长度和时间格式。

OpenAPI 是合同证据，但生成文件必须由 CI lint，并用真实 Handler 合同测试补充。Schema 正确不证明事务和权限正确。

若生成客户端，必须实际编译/运行一个最小调用，不能只测试生成字符串。

## 27. ClickHouse 或消息队列何时评估

完成本章不引入它们。触发条件：

- PostgreSQL 在合理优化后仍无法达到已定义 SLO。
- 大规模 retention/vacuum 成为不可接受成本。
- 聚合/全文场景已经成为产品核心。
- ingest 与 query 确实需要独立扩缩容。
- 一个 batch 需要多个可靠消费者。

演进必须写新 ADR 和 benchmark 对照。尤其要重新定义：队列 commit 后 ACK 与 ClickHouse 可查询之间的延迟。不能牺牲已经建立的明确 ACK 语义来换取架构名词。

## 28. 实现提交顺序

建议按可解释纵向切片提交：

1. `feat(protocol): define versioned batch contract`
2. `feat(server): add bounded HTTP bootstrap and shutdown`
3. `feat(auth): add project-scoped API keys`
4. `feat(storage): migrate idempotent ingest schema`
5. `feat(ingest): commit batches before acknowledgement`
6. `feat(query): add project-isolated keyset pagination`
7. `feat(retention): delete expired logs in bounded batches`
8. `test(server): cover database-backed end-to-end contracts`

提交名只是示例。每个提交前检查 staged paths，只包含该切片，不带用户无关改动。是否创建 commit 由当时任务授权决定；本教程不会自动授权 push 或部署。

## 29. 验收证据总表

| 能力 | 最低证据 |
| --- | --- |
| 上传 | 真实 router + PostgreSQL accepted 测试 |
| 持久 ACK | 代码路径与 commit 后响应测试 |
| 幂等 | 顺序重复 + 并发重复 + ACK 丢失故障测试 |
| 冲突 | 同 ID 不同 payload 409，原数据不变 |
| 原子性 | entry 写失败后 batch/entry 均无残留 |
| 权限 | ingest/query scope matrix |
| 隔离 | 双 Project 真实 SQL 测试 |
| 查询 | 过滤、半开区间、keyset 多页无重无漏 |
| 资源上界 | body/entry/window/limit/query timeout 测试 |
| Retention | 小批删除、取消、幂等 metadata 窗口 |
| 重启 | Server restart 后查询和 duplicate 仍成立 |
| 性能 | 固定环境 benchmark + query plan，不写猜测数字 |

建议命令按实际包调整：

```text
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

再执行 Compose/真实 PostgreSQL E2E。单元测试全部通过不等于应用可运行，旧构建产物运行成功也不等于当前工作树已集成。

## 30. 常见坑

### 30.1 Service 又变成薄 Handler

如果所有校验、hash、事务结果解释仍在 Handler，未来 CLI/gRPC 无法复用。Handler 只做 transport。

### 30.2 accepted 在 commit 前返回

这是最严重的语义错误。只有 durable boundary 后才 ACK。

### 30.3 duplicate 只比较 batch ID

同 ID 不同内容会静默丢一批数据。必须比较 Server 规范化 payload hash/version/count。

### 30.4 查询没有强制时间范围

一个普通 key 就能触发全表扫描，造成资源风险。时间窗口和 limit 是安全边界。

### 30.5 Cursor 使用 offset 或只有时间

offset 大页慢且漂移；只有时间会在同 timestamp 跳过/重复。使用 `(observed_at,id)`。

### 30.6 Cursor 携带 Project 并覆盖 Principal

这是越权漏洞。Principal 永远是唯一 Project 来源。

### 30.7 `q` 直接带入 LIKE wildcard

用户输入 `%` 可能匹配全部。若产品定义为 literal contains，必须 escape。

### 30.8 Retention 一次删除全部

造成长事务、WAL 和锁竞争。用小批、有界 timeout、可取消循环。

### 30.9 entries 到期就同步删除 batch metadata

缩短去重保证，离线 Agent 重试可能复活数据。metadata 有独立、更长窗口。

### 30.10 Retention 失败让整个 Server crash

后台维护失败应可见、可重试，但通常不应立即中断仍可用的 ingest/query。

### 30.11 指标带 Project/Batch/Service label

高基数会使可观测系统自身失控。使用有限结果类别，具体 ID 放受控日志。

### 30.12 用测试小数据断言索引有效

优化器对小表选择顺序扫描是合理的。性能结论需要代表性数据规模。

## 31. 复盘题

1. Ingest Handler、Service、Repository 各自负责什么？
2. 为什么 validation 应在事务前完成？
3. accepted 和 duplicate 为什么都使用 HTTP 200？
4. 为什么当前不在 Server 内增加异步内存 channel？
5. `[from,to)` 比双闭区间更适合连续时间查询的原因是什么？
6. keyset pagination 在并发插入下提供什么保证，又不提供什么保证？
7. 如何让 `q` 表示 literal contains 而不意外支持 wildcard？
8. retention 为什么要分 entry window 与 idempotency metadata window？
9. PostgreSQL DELETE 后为什么磁盘不一定立即下降？
10. 哪种真实证据足以启动 ClickHouse 评估？
11. 如何证明“无丢失无重复”，为什么只检查总行数不够？
12. 如果 commit 成功但 ACK 丢失，完整数据流会怎样恢复？

## 32. 完成门

- [ ] `POST /api/v1/batches` 经过 strict decode、auth、scope、domain、hash、transaction。
- [ ] Project 只来自 Principal，客户端无法覆盖。
- [ ] accepted/duplicate 都在 commit/已提交事实确认后返回 200。
- [ ] conflict 返回 409，Agent 不自动换 ID。
- [ ] DB 不可用返回可重试 503，不泄露内部错误。
- [ ] `GET /api/v1/entries` 强制 Project、时间窗口和 limit。
- [ ] SQL 参数化，固定 `(observed_at DESC,id DESC)` keyset。
- [ ] 多页查询在相同 timestamp 数据中无重复、无遗漏。
- [ ] q 的 literal/wildcard 语义已明确并测试。
- [ ] retention 小批、可取消、不重叠，失败可观测。
- [ ] 幂等 metadata 保留窗口不短于声明的合法重试窗口。
- [ ] 双 Project、scope matrix、重启和并发重试由真实 E2E 覆盖。
- [ ] 故障测试使用 run ID + 连续 sequence 检查集合。
- [ ] benchmark 数字只有实际运行并记录环境后才进入 README/简历。
- [ ] ClickHouse/消息队列仍是证据触发的后续演进。

完成本章后，Gline 才达到第一个可称为“自托管日志后端”的 Server 版本。接下来应回到[教程入口](README.md)，继续 Agent spool、checkpoint、故障恢复、可观测性、性能与交付章节，而不是立即拆微服务。
