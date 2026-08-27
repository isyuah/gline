# 02. 领域模型与 PostgreSQL 数据模型

后端项目是否“像平台”，很大程度取决于领域模型是否能表达完整生命周期。这里不从表出发，而是先定义对象、状态和不变量，再把它们映射到 PostgreSQL。表只是实现领域约束的一种工具。

## 1. 当前代码差距

当前仓库还没有稳定的数据库 schema 和 repository 合同。已有的协议和 Agent 结构可以提供字段来源，但不能直接把 JSON DTO 落成一张大表。需要明确分离：

```text
wire DTO        -> 外部协议版本，允许兼容演进
domain object   -> 业务规则、不变量、状态转换
database row    -> 索引、约束、事务和存储优化
response DTO    -> 面向调用者的稳定输出
```

如果跳过这一层，最容易出现三类问题：Project 由客户端指定造成越权；Batch 重试只按 ID 去重却未验证内容；控制平面删除/禁用数据时没有生命周期和审计依据。

## 2. 前置知识

* 值对象、聚合边界和不变量；
* PostgreSQL `uuid`、`timestamptz`、`jsonb`、唯一约束、部分索引；
* 事务原子性、唯一冲突和并发写入；
* 状态机和幂等操作；
* SQL 参数化与查询计划。

## 3. 领域语言

### 3.1 Project

Project 是租户隔离的最小边界。日志 Entry、Agent、Pipeline、API Key、Usage、Retention Policy 都归属于一个 Project。客户端不能通过请求体改变认证得到的 Project。

```go
type Project struct {
    ID        ProjectID
    Slug      string
    Name      string
    Status    ProjectStatus // active, disabled
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

不变量：slug 在全局唯一；disabled Project 不能接收新 Batch，也不能通过普通 Query Key 查询；已有数据是否可被管理员查询由 Scope 决定。

### 3.2 APIKey

API Key 是认证凭证，不是领域用户密码。数据库只保存不可逆 hash、前缀、Scope 和生命周期状态。

```go
type APIKey struct {
    ID          APIKeyID
    ProjectID   ProjectID
    AgentID     *AgentID // nil for project/admin keys; set for agent-scoped keys
    Prefix      string
    SecretHash  []byte // never serialize to response
    Scopes      []Scope
    Status      KeyStatus // active, revoked, expired
    ExpiresAt   *time.Time
    LastUsedAt  *time.Time
    CreatedAt   time.Time
    RevokedAt   *time.Time
}
```

创建时返回一次明文 secret，之后只能返回 `prefix` 和状态。认证 lookup 使用 constant-time hash 比较；不要把完整 Key 放在 metrics label 或普通日志中。

### 3.3 Agent

Agent 是部署在边缘主机上的逻辑客户端。它可以在进程重启后重新注册，但 `agent_id` 必须稳定，不能每次启动随机生成，否则无法判断一个节点是否离线。

```go
type Agent struct {
    ID             AgentID
    ProjectID      ProjectID
    Name           string
    Hostname       string
    Version        string
    Status         AgentStatus // active, disabled, stale
    LastHeartbeat  *time.Time
    LastSeenIP     *net.IP // optional, do not treat as identity
    CreatedAt      time.Time
    UpdatedAt      time.Time
}
```

Agent 状态是控制平面的观察结果，不能直接等同于进程真实存活。`stale` 由后台任务依据 `last_heartbeat_at` 和配置阈值计算。

### 3.4 Pipeline

Pipeline 表示一个 Agent 内的采集配置实例，例如读取某个 service 的某类文件。它拥有自己的 source identity、parser 配置和运行状态。

```go
type Pipeline struct {
    ID          PipelineID
    ProjectID   ProjectID
    AgentID     AgentID
    Name        string
    Service     string
    Config      json.RawMessage
    ConfigVer   int64
    Status      PipelineStatus // enabled, paused, error, disabled
    ReportedStatus string      // running, stopped, error; agent observation
    ReportedAt   *time.Time
    LastError   *string
    UpdatedAt   time.Time
}
```

配置版本递增而不是原地覆盖审计历史。Agent 当前使用的版本由心跳或状态上报提供；第一版可以只记录版本，不实现动态下发。

### 3.5 Batch

Batch 是 Agent 向 Server 发送的不可变传输单元。它有稳定的 `batch_id`，同一 Agent 重试时不改变 ID、Entry 顺序或 payload hash。

```go
type Batch struct {
    ID          BatchID
    ProjectID   ProjectID // server fills from AuthContext
    AgentID     AgentID
    PipelineID  PipelineID
    Sequence    int64
    PayloadHash [32]byte
    Entries     []Entry
    CreatedAt   time.Time
}
```

`sequence` 只用于诊断和排序，不替代幂等键。不要把客户端提供的 `project_id` 当作租户边界。

### 3.6 Entry

Entry 是最终可检索日志记录。建议把检索高频字段做成列，把不可预测扩展字段放进受控 `jsonb`。

```go
type Entry struct {
    ID          EntryID
    ProjectID   ProjectID
    BatchID     BatchID
    AgentID     AgentID
    PipelineID  PipelineID
    Sequence    int
    Service     string
    Host        string
    Level       string
    Message     string
    ObservedAt  time.Time // source observation time
    IngestedAt  time.Time // server persistence time
    Attributes  map[string]any
}
```

时间含义必须固定：`observed_at` 用于用户按日志发生时间检索，`ingested_at` 用于排查延迟和 retention 兜底。不要用一个 `created_at` 同时承载两种语义。

### 3.7 QuarantineBatch

永久失败的批次不能无限重试，也不能静默丢弃。Quarantine 记录原始 payload 引用、失败分类、尝试次数和人工处理状态。

```go
type QuarantineBatch struct {
    ID          QuarantineID
    ProjectID   ProjectID
    BatchID     BatchID
    Payload     []byte // encrypt or external blob later; bounded in v1
    PayloadHash [32]byte
    ErrorCode   string
    ErrorDetail string // sanitized, no secret
    Status      QuarantineStatus // pending, replaying, resolved, discarded
    Attempts    int
    CreatedAt   time.Time
    ClaimedAt   *time.Time
    ResolvedAt  *time.Time
}
```

同一 `(project_id,batch_id)` 只允许一个 active quarantine。Replay 必须使用原始 batch ID 和 payload，成功后将状态置为 resolved，并写 Audit Event。

### 3.8 AuditEvent

Audit Event 记录控制或破坏性操作，而不是每一条日志接入。至少包含操作者身份类型、Project、动作、对象和结果。

```go
type AuditEvent struct {
    ID          AuditID
    ProjectID   *ProjectID
    ActorType   string // api_key, admin, agent, system
    ActorID     string
    Action      string // project.disable, key.revoke, quarantine.replay
    Resource    string
    ResourceID  string
    Outcome     string // success, rejected, failed
    Metadata    json.RawMessage
    CreatedAt   time.Time
}
```

Metadata 不放 secret、原始 payload 或完整 Authorization header。审计不可被普通 Project Key 删除。

### 3.9 Usage 和 RetentionPolicy

Usage 是按时间桶聚合的观测数据；RetentionPolicy 是 Project 级治理配置。它们不应阻塞接入主事务。

```go
type UsageBucket struct {
    ProjectID      ProjectID
    BucketStart    time.Time
    Entries        int64
    Bytes          int64
    FailedBatches  int64
}

type RetentionPolicy struct {
    ProjectID       ProjectID
    MaxAge          time.Duration
    MaxBytes        *int64
    Enabled         bool
    UpdatedAt       time.Time
}
```

## 4. 聚合边界和状态机

### 4.1 聚合边界

`Project` 是控制平面根；`Batch` 是接入事务根；`Pipeline` 属于 Agent；Entry 不独立改变自身状态。一次接入事务只允许操作一个 Project，写入一个 Batch 及其 Entries。

```text
Project
├── APIKey
├── Agent
│   └── Pipeline
├── RetentionPolicy
└── UsageBucket / AuditEvent

Batch
└── Entry[]
```

不要在一个 HTTP 请求中跨 Project 批量操作；管理员批量操作也应在服务层显式逐 Project 执行并记录结果。

### 4.2 Project 状态

```text
active -> disabled
disabled -> active       (需要 admin scope，并记录审计)
```

disabled 时：ingest 直接拒绝；普通 query 也拒绝；审计和受控导出可以由 admin scope 继续读取。删除 Project 不是 v1 的普通 API 行为，先通过 retention/备份策略处理。

### 4.3 API Key 状态

```text
active -> revoked
active -> expired         (由时间判定，不必定时更新)
revoked / expired -> terminal
```

吊销是幂等的；再次吊销返回当前 terminal 状态，不重复生成错误审计。轮换创建新 Key，再显式撤销旧 Key，不在同一字段覆盖 secret。

### 4.4 Agent/Pipeline 状态

```text
Agent: active -> stale -> disabled
Agent: stale -> active    (收到有效心跳)

Pipeline: enabled -> paused -> enabled
Pipeline: enabled -> error -> paused
Pipeline: * -> disabled
```

状态转换必须由 service 方法执行，不能让 Handler 随意写枚举。`disabled` 表示控制平面禁止运行；`stale` 只是系统根据心跳推断，不应被 Agent 自己声明为 active 覆盖。

### 4.5 Batch 接入状态

```text
received -> committed
received -> rejected
received -> quarantined
```

数据库中可以只保存 `committed` 记录和错误信息；如果需要记录接入尝试，另建 `batch_attempts`，不要把状态更新和 Entry 插入拆成无法解释的多个事务。重复 committed 批次返回 duplicate；相同 ID 不同 hash 返回 idempotency conflict。

### 4.6 Quarantine 状态

```text
pending -> replaying -> resolved
pending -> discarded
replaying -> pending       (可重试的 replay 失败)
```

抢占 replay 使用带条件的 `UPDATE ... WHERE status = 'pending'`，避免多个 worker 同时重放。进程崩溃后，超时的 `replaying` 可由恢复任务改回 `pending`。

## 5. PostgreSQL schema 骨架

下面是 v1 迁移的教学骨架。真正执行前需要根据 PostgreSQL 版本、迁移工具和项目命名规范调整。所有时间使用 `timestamptz`，ID 使用 UUID；不要在手工 SQL 中依赖本地时区。

命名约定只有一套：协议字段 `batch_id` 和领域字段 `Batch.ID` 落库到 `ingest_batches.id`；`log_entries.batch_id` 是指向它的外键。文中说 `(project_id, batch_id)` 幂等键时是在描述协议合同，对应 SQL 唯一约束 `(project_id, id)`。

```sql
CREATE TABLE projects (
    id          uuid PRIMARY KEY,
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL,
    status      text NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    agent_id        uuid,
    prefix          text NOT NULL,
    secret_hash     bytea NOT NULL,
    scopes          text[] NOT NULL,
    status          text NOT NULL CHECK (status IN ('active', 'revoked', 'expired')),
    expires_at      timestamptz,
    last_used_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    UNIQUE (project_id, prefix)
);

CREATE INDEX api_keys_active_lookup
    ON api_keys (prefix)
    WHERE status = 'active';

CREATE TABLE agents (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id),
    name                text NOT NULL,
    hostname            text NOT NULL,
    version             text NOT NULL,
    status              text NOT NULL CHECK (status IN ('active', 'stale', 'disabled')),
    last_heartbeat_at   timestamptz,
    last_seen_ip        inet,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE TABLE pipelines (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    agent_id        uuid NOT NULL REFERENCES agents(id),
    name            text NOT NULL,
    service         text NOT NULL,
    config          jsonb NOT NULL,
    config_version  bigint NOT NULL CHECK (config_version > 0),
    status          text NOT NULL CHECK (status IN ('enabled', 'paused', 'error', 'disabled')),
    reported_status text NOT NULL CHECK (reported_status IN ('running', 'stopped', 'error')) DEFAULT 'stopped',
    reported_at     timestamptz,
    last_error      text,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, agent_id, name)
);

CREATE TABLE ingest_batches (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    agent_id        uuid NOT NULL REFERENCES agents(id),
    pipeline_id     uuid NOT NULL REFERENCES pipelines(id),
    sequence_no     bigint NOT NULL CHECK (sequence_no >= 0),
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    entry_count     integer NOT NULL CHECK (entry_count > 0),
    payload_bytes   integer NOT NULL CHECK (payload_bytes > 0),
    status          text NOT NULL CHECK (status IN ('committed', 'rejected', 'quarantined')),
    created_at      timestamptz NOT NULL,
    committed_at    timestamptz,
    error_code      text,
    CHECK ((status = 'committed') = (committed_at IS NOT NULL)),
    UNIQUE (project_id, id)
);

CREATE TABLE log_entries (
    id              bigserial PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    batch_id        uuid NOT NULL,
    batch_sequence  integer NOT NULL CHECK (batch_sequence >= 0),
    agent_id        uuid NOT NULL,
    pipeline_id     uuid NOT NULL,
    service         text NOT NULL,
    host            text NOT NULL,
    level           text NOT NULL,
    message         text NOT NULL,
    observed_at     timestamptz NOT NULL,
    ingested_at     timestamptz NOT NULL DEFAULT now(),
    attributes      jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (project_id, batch_id, batch_sequence),
    FOREIGN KEY (project_id, batch_id)
      REFERENCES ingest_batches(project_id, id)
);

CREATE INDEX log_entries_project_time
    ON log_entries (project_id, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_service_time
    ON log_entries (project_id, service, observed_at DESC, id DESC);
```

注意：PostgreSQL 要求被外键引用的列具有唯一约束。若 `ingest_batches(id)` 已经全局唯一，实际迁移可以改用单列外键并在应用层验证 Project；保留复合外键的目的，是把跨租户引用错误交给数据库拒绝。迁移时二选一，不能写出无效的外键定义。

## 6. 其余表和约束

```sql
CREATE TABLE quarantine_batches (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    batch_id        uuid NOT NULL,
    payload         bytea NOT NULL,
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    error_code      text NOT NULL,
    error_detail    text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'replaying', 'resolved', 'discarded')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    resolved_at     timestamptz,
    UNIQUE (project_id, batch_id)
);

CREATE TABLE retention_policies (
    project_id      uuid PRIMARY KEY REFERENCES projects(id),
    max_age_seconds bigint NOT NULL CHECK (max_age_seconds > 0),
    max_bytes       bigint CHECK (max_bytes IS NULL OR max_bytes > 0),
    enabled         boolean NOT NULL DEFAULT true,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE usage_buckets (
    project_id       uuid NOT NULL REFERENCES projects(id),
    bucket_start     timestamptz NOT NULL,
    entries          bigint NOT NULL DEFAULT 0 CHECK (entries >= 0),
    bytes            bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    failed_batches   bigint NOT NULL DEFAULT 0 CHECK (failed_batches >= 0),
    PRIMARY KEY (project_id, bucket_start)
);

CREATE TABLE audit_events (
    id              bigserial PRIMARY KEY,
    project_id      uuid REFERENCES projects(id),
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    action          text NOT NULL,
    resource        text NOT NULL,
    resource_id     text NOT NULL,
    outcome         text NOT NULL CHECK (outcome IN ('success', 'rejected', 'failed')),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_project_time
    ON audit_events (project_id, created_at DESC);
```

### 6.1 跨 Project 的外键

`pipeline.project_id` 必须与 `agent.project_id` 相同，单独两个外键并不能完全表达这一点。v1 有三种选择：

1. 在 Agent/ Pipeline 建表时使用 `(project_id, id)` 复合唯一，并在 Pipeline 上建复合外键；
2. 使用 PostgreSQL trigger；
3. 在 service 事务中查询并验证。

教学实现优先选择复合唯一/复合外键，因为约束显式且无需隐式触发器：

```sql
ALTER TABLE agents ADD CONSTRAINT agents_project_id_id_uq UNIQUE (project_id, id);
ALTER TABLE pipelines
  ADD CONSTRAINT pipelines_agent_same_project_fk
  FOREIGN KEY (project_id, agent_id)
  REFERENCES agents(project_id, id);
ALTER TABLE api_keys
  ADD CONSTRAINT api_keys_agent_same_project_fk
  FOREIGN KEY (project_id, agent_id)
  REFERENCES agents(project_id, id);
```

这会多占一个唯一索引，但能把租户串写从“代码约定”提升为数据库保护。

## 7. 事务和幂等实现

### 7.1 新批次

```sql
BEGIN;

-- project_id 来自认证上下文，不能来自 JSON
INSERT INTO ingest_batches (
    id, project_id, agent_id, pipeline_id, sequence_no,
    payload_hash, entry_count, payload_bytes, status, created_at, committed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'committed', $9, $10)
ON CONFLICT (project_id, id) DO NOTHING;

-- 应用根据 row count 判断是否首次插入；只有首次才插入 Entries
INSERT INTO log_entries (...)
SELECT ...;

COMMIT;
```

实际 SQL 不要在 `ON CONFLICT DO NOTHING` 后无条件插 Entry；需要先判断是否插入成功。也可以 `INSERT ... ON CONFLICT DO UPDATE SET id = EXCLUDED.id RETURNING payload_hash`，但必须区分新记录、相同 hash 和冲突 hash。

### 7.2 重复和冲突

```go
switch result {
case Inserted:
    return AcceptResult{Status: "accepted"}, nil
case DuplicateSameHash:
    return AcceptResult{Status: "duplicate"}, nil
case DuplicateDifferentHash:
    return AcceptResult{}, ErrIdempotencyConflict
default:
    return AcceptResult{}, err
}
```

同 hash 的重复请求不需要再次写 Entry，也不应该被当作错误导致 Agent 无限重试。不同 hash 的同 ID 是协议或客户端 bug，应返回 409 并触发隔离/告警策略。

### 7.3 使用 advisory lock 还是唯一约束？

默认不用应用锁。唯一约束是所有 Server 实例共享的事实来源，能在并发和重启后继续生效。只有在需要串行推进某个昂贵状态机且无法用条件更新表达时，才评估 advisory lock，并为它写等待指标和超时。

## 8. Keyset 查询

第一页：

```sql
SELECT id, project_id, service, level, message, observed_at, ingested_at, attributes
FROM log_entries
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
ORDER BY observed_at DESC, id DESC
LIMIT $4;
```

下一页 cursor 包含上一页最后一条 `(observed_at, id)`：

```sql
... AND (observed_at, id) < ($4, $5)
ORDER BY observed_at DESC, id DESC
LIMIT $6;
```

`id` 是同一时间戳下的稳定 tie-breaker。cursor 应带签名或至少绑定规范化查询摘要，防止把一个查询的 cursor 用在另一个 Project 或过滤条件上。不要把 cursor 解码后直接拼 SQL。

## 9. Repository 逐步实现

1. 先为每张表写 row struct 和 mapper；
2. 为每个 use case 定义窄 interface；
3. 用 fake 实现 service 单元测试；
4. 用真实 PostgreSQL integration test 验证约束和索引；
5. 再实现批量 insert（COPY 或分批 VALUES），保留事务语义；
6. 为 retention 和 usage 增加行数/耗时指标；
7. 迁移脚本采用编号、可回滚说明和 smoke test。

Repository 不负责鉴权，也不负责决定 Project。它接收已经确定的 `projectID` 并在 SQL 中始终带租户谓词。查询方法命名要包含这个边界，例如 `ListEntries(ctx, projectID, filter)`，不要提供一个没有 Project 参数的 `ListAllEntries`。

## 10. Retention 设计

Retention 的删除应是小批量、可恢复和可重复的：

```sql
WITH victims AS (
    SELECT id
    FROM log_entries
    WHERE project_id = $1
      AND ingested_at < $2
    ORDER BY ingested_at
    LIMIT $3
)
DELETE FROM log_entries e
USING victims v
WHERE e.id = v.id;
```

每轮提交一个短事务，直到没有受影响行或达到 worker deadline。`observed_at` 可能被客户端伪造或严重滞后，保留策略优先使用 `ingested_at`，必要时再增加两者的兜底规则。

## 11. 测试策略

高价值测试包括：

* Project disabled 时 ingest 和 query 的错误分类；
* API Key revoked/expired 的认证失败；
* 同一 Project 同一 Batch ID 相同 hash 的并发请求只产生一个 Batch/Entries；
* 同 ID 不同 hash 返回冲突且不改变原记录；
* 跨 Project 的 Agent/Pipeline 关系被数据库或 service 拒绝；
* keyset 分页在插入新日志期间没有重复/遗漏；
* retention 只删除到期数据，重复执行无副作用；
* Quarantine replay 的状态抢占和崩溃恢复；
* Audit Event 在成功、拒绝、失败三种结果下都有正确 actor/resource。

不要为每个 getter 写测试；要保护的是隔离、幂等、状态转换和持久化边界。

## 12. 验收证据

保存以下证据：

* migration 在空数据库和已有数据库上均可执行；
* `\d+ log_entries` 或等效 schema 导出显示 Project/时间索引；
* `EXPLAIN (ANALYZE, BUFFERS)` 证明 keyset 查询使用预期索引（记录环境和数据量）；
* integration test 显示重复/冲突批次结果；
* retention 一轮的删除行数和耗时指标；
* 约束失败时返回经过分类的业务错误，而不是裸数据库字符串。

## 13. 常见坑

* `api_keys.secret` 明文存储或在日志打印；
* 只有 `(batch_id)` 唯一而没有 Project 维度，迁移时与协议语义不一致；
* 同一批次重复请求仍插入 Entries；
* 使用 `SELECT max(sequence)` 判断重复，遭遇并发和重启后失效；
* `OFFSET` 深分页；
* 用户输入直接拼接 `LIKE`、ORDER BY 或 JSON path；
* retention 单次删除百万行，造成长锁和 WAL 峰值；
* 认为 `jsonb` 可以替代所有列，导致无法建立稳定索引；
* 误把 `observed_at` 当作服务器接收时间。

## 14. 复盘题

1. 为什么 Batch 的 payload hash 必须在 Server 按规范化 payload 计算或验证？
2. 如果数据库只保留 `log_entries.project_id` 而不保留 Batch 外键，哪些审计/删除能力会变差？
3. 为什么 retention 使用 `ingested_at` 更安全？什么场景下需要按 `observed_at` 辅助删除？
4. 复合外键带来的索引成本是什么？为什么这里仍值得付出？
5. 查询 cursor 只包含时间戳不包含 id 会发生什么？

## 15. 本章完成门

* 能写出 Project、Batch、Entry、Quarantine、Audit、Usage、Retention 的不变量；
* 能完成第一版 migrations 并让数据库阻止明显的跨租户引用；
* 能用一次真实事务证明“Batch 元数据和 Entries 同生共死”；
* 能解释重复相同 hash、重复不同 hash 和数据库失败三种分支；
* 能实现并测试 keyset cursor 和有界 retention；
* 能说清哪些规则在 domain/service，哪些规则在 SQL constraint/index。
