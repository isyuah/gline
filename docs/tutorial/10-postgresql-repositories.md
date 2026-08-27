# 10. PostgreSQL、迁移、事务与 Repository

> 本章实现目标存储边界，不表示仓库当前已经连接 PostgreSQL。架构选择依据见[PostgreSQL-first ADR](../adr/0002-postgresql-first.md)和[至少一次与幂等 ADR](../adr/0003-at-least-once-idempotency.md)。

## 1. 本章目标

完成后，Server 的持久化层应具备：

- 可重复执行、版本受控的 PostgreSQL migration。
- Project、API Key、ingest batch 和 log entry 的约束与索引。
- 有界、可观测、可关闭的数据库连接池。
- `auth`、`ingest`、`query` 各自定义的窄 Repository 接口。
- `(project_id, batch_id)` 唯一约束裁决并发重试。
- batch metadata 与全部 entries 在同一个事务提交。
- 相同 ID + 相同 payload hash 返回 duplicate。
- 相同 ID + 不同 hash 返回 idempotency conflict。
- 只有 commit 成功后，调用层才可能返回 HTTP 200。
- 使用真实 PostgreSQL 验证事务、约束、隔离和查询计划。

PostgreSQL-first 不是“永远只用 PostgreSQL”。它意味着先用一个事务能力强、部署简单的存储建立正确闭环，再由真实 benchmark 决定是否分区、增加扩展或演进到 ClickHouse。

## 2. 当前差距

当前 Server 的 `EntrySink` 只有：

```go
Accept(ctx context.Context, entries []logentry.LogEntry) error
```

`TestSink` 只打印 entries。它没有：

- Project 边界；
- batch metadata；
- 幂等 ID；
- 事务；
- 持久化 ACK；
- 查询；
- migration；
- 重启恢复；
- 数据保留策略。

因此不要把打印成功的 HTTP 200 解释为“Server 已接收并可靠保存”。目标 ACK 必须移动到数据库 commit 之后。

## 3. 依赖引入规则

本教程不写死未来可能不兼容的依赖版本。实现时：

1. 检查当前 Go 版本和 `go.mod`。
2. 从候选驱动、migration 工具的官方文档确认兼容性。
3. 使用包管理器安装：

```text
go get <postgres-driver-module>
go get <migration-library-or-tool-module>
go mod tidy
```

4. 让 Go 工具维护 `go.mod`/`go.sum`，禁止手工猜版本或编辑校验和。
5. 使用 `go list -m all` 核对解析版本。
6. 在无本机绝对 `replace` 的环境运行测试。

本文代码骨架使用 `pgx`/`pgxpool` 的概念，因为它适合 PostgreSQL 原生能力和批量写入。最终 API 以安装时官方文档为准。如果项目选择 `database/sql`，领域边界和事务语义不变，只调整 adapter。

## 4. 迁移策略

### 4.1 推荐目录

```text
migrations/
  000001_create_projects_and_api_keys.up.sql
  000001_create_projects_and_api_keys.down.sql
  000002_create_ingest_batches.up.sql
  000002_create_ingest_batches.down.sql
  000003_create_log_entries.up.sql
  000003_create_log_entries.down.sql
```

是否使用单文件 `-- +goose Up/Down` 取决于选定工具。只选一种格式。

### 4.2 谁执行 migration

推荐：

- 本地 Compose：独立 migrate step/service 先运行，成功后启动 Server。
- CI：明确执行 up，验证 schema，再运行集成测试。
- 生产式部署：发布流程单独执行 migration。
- Server：启动时检查 schema 版本兼容性，但不由每个实例自动争抢迁移。

多实例自动迁移需要 advisory lock、失败恢复和版本兼容设计。对当前简历项目，显式迁移更容易解释。

### 4.3 Up 与 Down

- Up migration 是主要合同，必须从空数据库完整执行。
- Down migration 主要用于本地开发验证，删除表/数据是破坏性操作。
- 不要在正常部署回滚时自动执行 destructive down。
- 已发布 migration 不要原地修改；新增 migration 修正。

### 4.4 迁移事务

PostgreSQL 大部分 DDL 可事务化，但某些操作例如特定 `CREATE INDEX CONCURRENTLY` 不能放普通事务。每个 migration 要知道工具是否自动包事务，不要假设。

## 5. 核心 Schema

Project/API Key 表见[鉴权章节](09-auth-project-scopes.md)。这里重点定义 batch 和 entries。

### 5.1 `ingest_batches`

```sql
CREATE TABLE ingest_batches (
    project_id          uuid NOT NULL REFERENCES projects(id),
    batch_id            uuid NOT NULL,
    agent_id            text NOT NULL,
    sent_at             timestamptz NOT NULL,
    received_at         timestamptz NOT NULL DEFAULT now(),
    entry_count         integer NOT NULL,
    payload_hash        bytea NOT NULL,
    payload_hash_version smallint NOT NULL,

    PRIMARY KEY (project_id, batch_id),
    CHECK (agent_id <> ''),
    CHECK (entry_count > 0),
    CHECK (octet_length(payload_hash) = 32),
    CHECK (payload_hash_version > 0)
);

CREATE INDEX ingest_batches_received_at_idx
    ON ingest_batches (received_at);
```

关键点：

- 主键本身就是 `(project_id, batch_id)`，不是只额外建一个普通索引。
- 同一个 batch ID 在不同 Project 可合法存在，隔离边界清晰。
- `entry_count` 参与 duplicate 响应和一致性检查。
- `received_at` 由 Server/数据库产生；`sent_at` 来自 Agent，仅用于诊断。
- payload hash 固定 32 字节，并保存 hash 格式版本。

### 5.2 `log_entries`

```sql
CREATE TABLE log_entries (
    project_id  uuid NOT NULL,
    id          uuid NOT NULL,
    batch_id    uuid NOT NULL,
    sequence    integer NOT NULL,
    observed_at timestamptz NOT NULL,
    event_time  timestamptz NULL,
    pipeline_id text NOT NULL,
    agent_id    text NOT NULL,
    service     text NOT NULL,
    host        text NOT NULL,
    level       text NOT NULL,
    message     text NOT NULL,
    attributes  jsonb NOT NULL DEFAULT '{}'::jsonb,

    PRIMARY KEY (project_id, id),
    UNIQUE (project_id, batch_id, sequence),
    FOREIGN KEY (project_id, batch_id)
        REFERENCES ingest_batches(project_id, batch_id)
        ON DELETE CASCADE,

    CHECK (sequence >= 0),
    CHECK (pipeline_id <> ''),
    CHECK (agent_id <> ''),
    CHECK (service <> ''),
    CHECK (host <> ''),
    CHECK (level IN ('TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR', 'FATAL', 'UNKNOWN')),
    CHECK (jsonb_typeof(attributes) = 'object')
);
```

为什么 entries 再存一份 `agent_id`：批次已有它，但查询响应和过滤经常需要 entry 直接得到来源。初期可通过 batch join 避免重复；是否反规范化应以查询计划和模型稳定性决定。若按上面存储，领域校验必须保证 entry 与 batch 的 agent 一致，数据库无法仅靠普通 CHECK 跨表验证。

`id` 可由 Server 在事务前生成稳定 UUID。它是查询分页 tie-breaker，不是网络幂等键。网络幂等由 batch ID + sequence 负责。

### 5.3 初始索引

```sql
CREATE INDEX log_entries_project_time_idx
    ON log_entries (project_id, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_service_time_idx
    ON log_entries (project_id, service, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_level_time_idx
    ON log_entries (project_id, level, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_host_time_idx
    ON log_entries (project_id, host, observed_at DESC, id DESC);
```

这些索引对应 MVP 核心过滤器。不要一次给每种组合都建索引：

- 每个索引都会增加写放大和磁盘占用。
- PostgreSQL 不一定能把多个单列索引组合成最佳有序分页计划。
- 真实查询分布决定复合索引顺序。

`message ILIKE '%text%'` 无法有效使用普通 B-tree。先用有界时间窗口控制扫描；只有计划和延迟证明需要，再评估 `pg_trgm` GIN/GiST。不要在没有 benchmark 前把全文扩展当必需组件。

### 5.4 BRIN 与分区何时出现

时间近似追加的大表可评估 BRIN；大量 retention 删除可评估按时间分区。但它们不是第一版默认：

- BRIN 很小，但对 Project + 多维过滤不一定替代 B-tree。
- 分区引入分区键、唯一约束、迁移和清理复杂度。
- 先用真实数据量和 `EXPLAIN (ANALYZE, BUFFERS)` 判断。

## 6. 类型与 row mapping

### 6.1 领域对象不带 SQL 细节

```go
// internal/server/ingest
type Batch struct { /* domain fields */ }
```

### 6.2 PostgreSQL row 位于 adapter

```go
// internal/storage/postgres
type batchRow struct {
    ProjectID         uuid.UUID
    BatchID           uuid.UUID
    AgentID           string
    SentAt            time.Time
    ReceivedAt        time.Time
    EntryCount        int32
    PayloadHash       []byte
    PayloadHashVersion int16
}

type entryRow struct {
    ProjectID  uuid.UUID
    ID         uuid.UUID
    BatchID    uuid.UUID
    Sequence   int32
    ObservedAt time.Time
    EventTime  *time.Time
    PipelineID string
    AgentID    string
    Service    string
    Host       string
    Level      string
    Message    string
    Attributes []byte
}
```

mapper 要检查数值缩窄是否安全。例如协议 sequence 是 `int64` 而数据库列是 integer 时，超过 `int32` 必须在事务前拒绝，不能直接 cast 溢出。

从数据库扫描 `payload_hash` 后验证恰好 32 字节。数据库 CHECK 是防线，不代表 adapter 可假设损坏永远不发生。

## 7. 连接池

### 7.1 Open 骨架

```go
type PoolOptions struct {
    MaxConnections int32
    MinConnections int32
    MaxConnLifetime time.Duration
    MaxConnIdleTime time.Duration
    HealthCheckPeriod time.Duration
    ConnectTimeout time.Duration
    ApplicationName string
}

func Open(ctx context.Context, databaseURL string, opts PoolOptions) (*pgxpool.Pool, error) {
    cfg, err := pgxpool.ParseConfig(databaseURL)
    if err != nil {
        return nil, fmt.Errorf("parse database configuration: %w", ErrInvalidDatabaseConfig)
    }

    // 按所安装 pgx 版本的公开 API 设置 pool 字段。
    // 不要把 databaseURL 放入日志或返回错误。

    connectCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
    defer cancel()

    pool, err := pgxpool.NewWithConfig(connectCtx, cfg)
    if err != nil {
        return nil, fmt.Errorf("create database pool: %w", err)
    }
    if err := pool.Ping(connectCtx); err != nil {
        pool.Close()
        return nil, fmt.Errorf("ping database: %w", err)
    }
    return pool, nil
}
```

不要把底层 parse error 原样发给 HTTP 客户端。启动日志可以保留安全 cause，但要确认 driver 不把含密码 DSN 放进 error。

### 7.2 Pool 大小推导

不要复制一个“常见值”。估算：

```text
所有 Server 实例 MaxConns 总和
+ migration/admin/retention 预留
< PostgreSQL max_connections 安全预算
```

再通过指标观察：

- acquire duration；
- acquired/idle/total connections；
- canceled acquire；
- query duration；
- PostgreSQL active/idle/lock wait。

池等待高不一定要加连接，也可能是 SQL 慢、事务过长或数据库 I/O 已饱和。

## 8. Repository 接口应由使用者定义

### 8.1 Auth 模块

```go
type CredentialRepository interface {
    FindByPublicID(ctx context.Context, publicID string) (CredentialRecord, error)
}
```

### 8.2 Ingest 模块

```go
type BatchRepository interface {
    Accept(ctx context.Context, batch Batch, payloadHash PayloadHash) (AcceptResult, error)
}
```

### 8.3 Query 模块

```go
type EntryRepository interface {
    List(ctx context.Context, filter Filter) (Page, error)
}
```

### 8.4 Retention 模块

```go
type RetentionRepository interface {
    DeleteEntriesBefore(ctx context.Context, cutoff time.Time, limit int) (DeleteResult, error)
    DeleteOrphanedBatchesBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}
```

不要建立：

```go
type Store interface {
    CreateProject(...)
    FindKey(...)
    AcceptBatch(...)
    ListEntries(...)
    DeleteOldEntries(...)
    Health(...)
}
```

大接口会迫使每个测试 fake 实现无关方法，也让模块依赖整个数据库能力。

## 9. 幂等事务算法

### 9.1 不能使用“先 SELECT 再 INSERT”

两个并发请求都可能同时查到不存在，然后都尝试写。正确性必须由唯一约束决定。

### 9.2 推荐流程

```text
BEGIN
  INSERT ingest_batches ... ON CONFLICT DO NOTHING RETURNING ...

  if inserted:
      bulk insert all entries
      COMMIT
      return accepted

  else:
      SELECT payload_hash, hash_version, entry_count
        WHERE project_id=? AND batch_id=?
        FOR KEY SHARE
      compare hash/version/count
      ROLLBACK or COMMIT read-only transaction
      same     -> duplicate
      different -> idempotency conflict
```

PostgreSQL 在唯一冲突时会等待并发插入事务裁决。因此第一条 INSERT 返回 conflict 后，再执行一个新的 SELECT statement，在默认 `READ COMMITTED` 下能看到已提交的冲突行。

不要把 INSERT 和 SELECT 强行塞进一个 CTE 并假设同一 statement snapshot 一定能看到并发事务刚提交的行。两条 statement 更容易解释。

### 9.3 代码骨架

```go
func (r *BatchRepository) Accept(
    ctx context.Context,
    batch ingest.Batch,
    hash ingest.PayloadHash,
) (result ingest.AcceptResult, err error) {
    tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
    if err != nil {
        return ingest.AcceptResult{}, mapDatabaseError("begin ingest", err)
    }
    defer func() {
        if err != nil {
            _ = tx.Rollback(context.Background())
        }
    }()

    inserted, err := insertBatchMetadata(ctx, tx, batch, hash)
    if err != nil {
        return ingest.AcceptResult{}, mapDatabaseError("insert batch metadata", err)
    }

    if !inserted {
        existing, err := selectExistingBatchForKeyShare(ctx, tx, batch.ProjectID(), batch.ID())
        if err != nil {
            return ingest.AcceptResult{}, mapDatabaseError("read existing batch", err)
        }
        if !existing.Matches(hash, batch.EntryCount()) {
            return ingest.AcceptResult{}, ingest.ErrIdempotencyConflict
        }
        if err := tx.Commit(ctx); err != nil {
            return ingest.AcceptResult{}, mapDatabaseError("finish duplicate check", err)
        }
        return ingest.DuplicateResult(batch.ID(), batch.EntryCount()), nil
    }

    if err := insertEntries(ctx, tx, batch); err != nil {
        return ingest.AcceptResult{}, mapDatabaseError("insert entries", err)
    }

    if err := tx.Commit(ctx); err != nil {
        // Commit error is ambiguous: never ACK. Agent retries same batch ID.
        return ingest.AcceptResult{}, mapDatabaseError("commit ingest", err)
    }
    return ingest.AcceptedResult(batch.ID(), batch.EntryCount()), nil
}
```

### 9.4 defer rollback 细节

上面是教学骨架。实际 Go 实现更常见且稳健的方式是无条件 defer rollback：

```go
defer func() { _ = tx.Rollback(context.Background()) }()
```

事务 commit 后 rollback 会返回已关闭错误，忽略即可。这样即使未来增加了一个未设置命名 `err` 的 early return，也不会泄漏事务。

不过 rollback context 也应有短 timeout，避免 background 无限等待。可以创建专用 cleanup context。

### 9.5 Commit 后才能 ACK

调用链必须是：

```text
repository tx.Commit 成功
  -> service 得到 accepted/duplicate
  -> Handler 写 HTTP 200
```

禁止：

- metadata insert 后提前返回 200；
- entries 放入内存 channel 后返回 200；
- 在事务 commit 前写 response header；
- bulk writer 异步运行而 HTTP 已成功。

### 9.6 Commit 错误是不确定状态

网络断开或 context timeout 可能让客户端不知道 commit 是否成功。Server 返回/断开为失败，Agent 保留 spool 并以同一 ID 重试：

- 未提交：重试走 accepted。
- 已提交：重试走 duplicate。

这正是至少一次传输 + 幂等写入，不需要 Server 猜测 commit 结果。

## 10. Metadata INSERT

```sql
INSERT INTO ingest_batches (
    project_id,
    batch_id,
    agent_id,
    sent_at,
    entry_count,
    payload_hash,
    payload_hash_version
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, batch_id) DO NOTHING
RETURNING received_at;
```

返回一行表示首次插入；无行表示冲突。adapter 将 `pgx.ErrNoRows` 在这一特定查询中解释为“未插入”，不要全局把 no rows 都当 duplicate。

Existing 查询：

```sql
SELECT payload_hash, payload_hash_version, entry_count
FROM ingest_batches
WHERE project_id = $1 AND batch_id = $2
FOR KEY SHARE;
```

`FOR KEY SHARE` 防止该 metadata 在比较期间被 retention 删除。即便不用锁，retention 合同也必须保证幂等窗口；锁提供事务内更明确的并发边界。

如果 insert conflict 后 SELECT 意外 no rows，可能是 retention 并发删除或数据损坏。不要直接当作 accepted。可将它分类为临时并重试有限次数，并检查 retention 保证窗口。

## 11. 批量写 entries

### 11.1 两种合理实现

1. 单条多 values 的参数化 INSERT，按 batch size 分块。
2. pgx `CopyFrom` 在同一 transaction 内批量导入。

两者都必须：

- 使用当前事务，不使用 pool 另开连接。
- 任一 entry 失败导致整个 transaction rollback。
- 不每条 entry 单独一次网络 round trip。
- 保持 batch 中 sequence 与数据对应。
- 在事务前完成字段限制，数据库 constraint 只作为最后防线。

`CopyFrom` 不是绕过事务：使用 tx 的 copy API。最终 API 名称按安装的 pgx 官方文档确认，不在本文猜版本。

### 11.2 Entry ID

可在 service 或 repository mapper 中为每条 entry 生成 UUID。要求：

- 单批内唯一。
- 写入前确定。
- 不承担网络去重语义。
- 与 `observed_at` 一起提供稳定 cursor。

UUID v7 具有时间局部性，是候选；也可先使用成熟 UUID 库的随机 UUID。通过 `go get` 安装兼容版本，不手工猜。若数据库扩展生成 ID，要考虑 bulk insert 后如何拿到 ID；由 Go 生成通常更简单。

## 12. SQL 参数化与动态查询

所有值使用参数，不拼接用户输入：

```sql
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
  AND service = $4
```

动态过滤可以：

- 构造固定 SQL 的有限分支；
- 使用成熟 query builder；
- 编写小型参数 builder，只允许代码内固定列和操作符。

绝不能把 `service`、`level`、`q` 或 cursor 直接拼 SQL。排序列也只能来自代码内枚举，MVP 最好固定排序，不开放客户端选择列。

如果引入 query builder，仍通过包管理器选择兼容版本；不要为一个固定查询过度引入 ORM。ORM 也不能自动解决 project isolation 和 keyset pagination。

## 13. 数据库错误分类

adapter 需要识别少量有意义错误：

```go
var (
    ErrNotFound   = errors.New("not found")
    ErrUnavailable = errors.New("storage unavailable")
    ErrConflict   = errors.New("storage conflict")
)
```

内部可以检查 PostgreSQL SQLSTATE：

- unique violation：仅在预期约束和代码路径中解释。
- foreign key violation：通常说明 Project 不存在或程序不变量破坏。
- check violation：通常是 validation 漏洞，应记录内部错误。
- serialization/deadlock：可在 transaction 边界有限重试，但必须 context 可取消并有上限。
- connection/timeout：映射 unavailable。

不要在全局看到 unique violation 就返回 duplicate。必须确认是 `(project_id,batch_id)` 冲突，而且 hash 比较相同。

对 HTTP 永远不要返回表名、constraint 名、SQL 或参数值。

## 14. Query Repository 骨架

```go
func (r *EntryRepository) List(ctx context.Context, f query.Filter) (query.Page, error) {
    queryCtx, cancel := context.WithTimeout(ctx, r.queryTimeout)
    defer cancel()

    sqlText, args := buildListQuery(f, f.Limit+1)
    rows, err := r.pool.Query(queryCtx, sqlText, args...)
    if err != nil {
        return query.Page{}, mapDatabaseError("list entries", err)
    }
    defer rows.Close()

    // scan -> row -> domain result; check rows.Err()
}
```

关键点：

- query timeout 有界，并继承 request cancellation。
- 总是 `rows.Close()`。
- 迭代后检查 `rows.Err()`。
- 获取 `limit + 1` 判断下一页。
- project 参数固定为第一个必要条件。
- attributes scan 后验证 JSON object；对异常数据返回内部错误，而非把损坏数据原样公开。

第 11 章给出完整 SQL 与 cursor 细节。

## 15. Retention 与幂等 metadata 的关系

删除 entries 时，`ON DELETE CASCADE` 的方向是“删除 batch 会删除 entries”，而不是删除 entries 自动删 batch。因此 retention 推荐两阶段：

1. 小批量删除到期 entries。
2. 仅在幂等保证窗口之后，删除已经没有 entries 的 batch metadata。

必须定义：

```text
idempotency metadata retention >= Agent 最大合法重试/离线窗口
```

如果 metadata 太早删除，老 batch 重试会被当作 accepted，再次插入 entries。不能对外宣称无限期去重，除非 metadata 无限保留。

详细 worker 设计见[接入、查询与保留](11-ingest-query-retention.md)。

## 16. Migration 与应用版本兼容

readiness 应知道程序需要的 schema 范围，例如：

```go
type SchemaRequirement struct {
    MinVersion int64
    MaxVersion int64
}
```

- DB 低于 min：migration 未执行，not ready。
- DB 高于 max：可能是旧 Server 连接新 schema，默认 not ready，除非 migration 明确向后兼容。
- 正在 migration：不接流量或由部署顺序保护。

不要只 `SELECT 1` 就宣称 ready；数据库活着但缺表，业务仍会失败。

## 17. 集成测试环境

可选策略：

### 17.1 CI/Compose 提供 `TEST_DATABASE_URL`

优点：依赖少、与部署 Compose 一致。测试创建独立 database 或 schema，运行 migration，结束后清理明确目标。

### 17.2 Testcontainers

优点：测试自包含、版本明确；缺点：新增依赖、Docker 启动成本。若选择，使用包管理器安装兼容版本，并复用容器而不是每个 test 启一个。

无论哪种策略：

- 不连接开发者日常数据库。
- 数据库名/schema 必须是测试专用且验证路径后才清理。
- 并行测试使用独立 schema 或唯一前缀。
- migration 必须真实运行。
- 测试失败保留足够诊断，但不输出 DSN 密码。

## 18. 高价值事务测试

### 18.1 首次 accepted

- 调用 `Accept`。
- 结果为 accepted。
- batch 一行，entries 数量正确。
- 所有字段、attributes 正确。

### 18.2 同 payload duplicate

- 连续提交同一 Batch 两次。
- 第二次 duplicate。
- entries 行数不增加。

### 18.3 同 ID 不同 payload conflict

- 先提交 A。
- 修改 message，但复用 batch ID。
- 返回 `ErrIdempotencyConflict`。
- 数据库仍只有 A。

### 18.4 entry 写入中途失败

构造触发数据库 constraint 的测试 fixture，或在专用测试 adapter 注入失败：

- `Accept` 失败。
- batch metadata 不存在。
- 没有部分 entries。

不要只 mock transaction；必须至少一个真实 PostgreSQL 测试证明原子性。

### 18.5 并发重复

```go
func TestAcceptConcurrentDuplicate(t *testing.T) {
    const workers = 16
    batch := validBatch()

    results := make(chan ingest.AcceptResult, workers)
    errs := make(chan error, workers)
    var wg sync.WaitGroup
    for i := 0; i < workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            result, err := repo.Accept(context.Background(), batch, mustHash(batch))
            if err != nil { errs <- err; return }
            results <- result
        }()
    }
    wg.Wait()
    close(results)
    close(errs)

    // 断言无错误、恰好一个 accepted、其余 duplicate、数据库 entry 只一份。
}
```

这个测试保护唯一约束和事务算法，不是为了固定 goroutine 数。worker 数可适中，避免 CI 不稳定。

### 18.6 双 Project

同一 batch ID 分别提交到 Project A/B：两者都 accepted，数据各自隔离。随后 query A 不得返回 B。

### 18.7 Commit 不确定窗口

这是更难但很有价值的故障测试。可以通过代理断开连接、测试 hook 或终止 Server 响应路径模拟“数据库可能提交、客户端未收到 ACK”，然后重试相同 batch，最终数据库只有一份。不要通过在生产代码留危险 hook 实现；可在 transport/integration harness 处理。

## 19. 查询计划验证

为主要查询保存：

```sql
EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT ...
FROM log_entries
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
ORDER BY observed_at DESC, id DESC
LIMIT 101;
```

观察：

- 是否使用期望索引；
- actual rows 与 estimated rows 差距；
- buffers hit/read；
- sort 是否发生、是否落盘；
- planning/execution time；
- 不同 Project 大小是否影响计划。

测试数据库只有几十行时，优化器选择顺序扫描很正常，不能据此断言索引无效。查询计划实验需要接近目标规模和数据分布。

## 20. 备份与恢复最小意识

作为简历项目，至少文档化：

- PostgreSQL volume 是持久状态，不随普通 Compose restart 删除。
- Project、key hash、batch metadata、entries 都要备份。
- auth pepper 不在数据库，必须通过独立 secret 备份。
- 恢复后运行 schema 兼容检查。
- 恢复演练要验证 key 仍能认证、旧 batch duplicate 仍成立、entries 可查询。

不要把“使用 named volume”误写成“已备份”。volume 只防容器重建，不防磁盘损坏或误删。

## 21. ClickHouse 的演进边界

只有出现可复现证据时评估 ClickHouse：

- PostgreSQL 在已优化 SQL、索引、连接池和必要分区后，写入/查询目标仍长期不达标。
- retention/vacuum/索引体积成为主要成本。
- 产品需要大量列式聚合或更长保留期。

演进时可保持：

- Agent `/api/v1/batches` 协议。
- API Key/Project 认证。
- batch ID 与 ACK 定义。
- query API 的主要语义。

但必须重新设计：

- Server 的持久化 ACK 边界。
- PostgreSQL metadata 与 ClickHouse entries 的跨存储一致性。
- 异步可见性。
- duplicate 与重放。
- retention 协调。

不能简单把 SQL adapter 换成 ClickHouse 就声称语义不变。

## 22. 验收命令与证据

按最终包路径调整：

```text
go test ./internal/storage/postgres/... -count=1
go test -race ./internal/storage/postgres/... -count=1
go vet ./internal/storage/postgres/...
<migration-tool> up
<migration-tool> version
```

还应保存：

- 空数据库完整 migration up 结果。
- migration 重跑行为。
- 全部 repository 集成测试。
- 并发 duplicate 测试。
- 事务失败无部分数据的查询证据。
- 双 Project 隔离查询。
- 代表性数据集的 `EXPLAIN (ANALYZE, BUFFERS)`。
- Server 重启后数据仍存在的冒烟结果。

## 23. 常见坑

### 23.1 只在代码里检查重复

并发下会竞态。数据库唯一约束才是最终裁决者。

### 23.2 batch metadata 和 entries 分两个事务

可能出现 metadata 已存在但 entries 只有一部分，随后 duplicate 路径错误 ACK。必须同事务。

### 23.3 每条 entry 一个 INSERT round trip

吞吐会被网络和事务协议开销限制。使用同事务批量 INSERT 或 CopyFrom。

### 23.4 看到 commit error 就生成新 batch ID

commit 可能已经成功，新 ID 会重复写。必须让 Agent 重试同一 ID。

### 23.5 公开 driver 错误

会泄露 schema、SQL、host，且客户端依赖不稳定。adapter 分类，HTTP 返回稳定 code。

### 23.6 Repository 返回 pgx row/type

领域层被 driver 绑定，测试和存储演进困难。adapter 完成扫描和映射。

### 23.7 事务内部使用 pool 批量写

会在另一个连接、另一个事务执行，破坏原子性。所有语句必须使用 tx handle。

### 23.8 忘记 `rows.Close` 或 `rows.Err`

造成连接长期占用或漏掉迭代错误。

### 23.9 启动只 parse pool 不 Ping

进程看似成功，第一请求才发现数据库不可达。Build 时显式连通验证，readiness 检查 schema。

### 23.10 索引越多越好

写入变慢、磁盘膨胀、vacuum 成本上升。索引由查询形状和执行计划驱动。

### 23.11 retention 同时删除幂等 metadata

老 batch 重试会被重新 accepted。必须定义并保留幂等窗口。

## 24. 复盘题

1. 为什么主键是 `(project_id, batch_id)` 而非只用 batch ID？
2. 为什么先 SELECT 再 INSERT 在并发下不正确？
3. PostgreSQL unique conflict 为什么要再比较 payload hash？
4. 为什么 batch metadata 和 entries 必须同事务？
5. commit 返回错误时，Server 和 Agent 各自应该做什么？
6. `READ COMMITTED` 下为什么 conflict 后用下一条 SELECT 更容易看到并发提交？
7. 为什么 query repository 必须显式接收 ProjectID？
8. `limit + 1` 查询解决什么问题？
9. 哪些证据会触发 BRIN、分区或 ClickHouse 评估？
10. 幂等 metadata retention 和 Agent 离线重试窗口有什么约束关系？

## 25. 完成门

- [ ] migration 能从空数据库完整执行并有版本检查。
- [ ] 表约束覆盖 Project、batch 唯一性、entry sequence、level 与 attributes object。
- [ ] 连接池参数有界，Build 显式 Ping，关闭所有权清晰。
- [ ] auth/ingest/query/retention 使用窄 Repository 接口，没有万能 Store。
- [ ] payload hash 由 Server 规范化计算并保存版本。
- [ ] 首次 batch 与全部 entries 在一个事务 commit。
- [ ] 相同 ID+hash 返回 duplicate，不增加 entry。
- [ ] 相同 ID+不同 hash 返回 conflict，不覆盖原数据。
- [ ] 并发相同 batch 恰好一份有效写入。
- [ ] commit 前绝不返回 HTTP 200。
- [ ] 所有查询参数化且显式包含 ProjectID。
- [ ] 真实 PostgreSQL 测试证明原子性、幂等和跨 Project 隔离。
- [ ] 代表性查询有执行计划，而非只依据单元测试判断性能。
- [ ] migration/driver 依赖通过包管理器安装，没有手工猜版本。
- [ ] ClickHouse 只作为实测瓶颈后的演进，不作为未验证前提。

下一章进入[接入、查询与保留任务](11-ingest-query-retention.md)，把 repository 组合成完整用户用例。
