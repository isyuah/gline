# 06. PostgreSQL 存储、Repository、Migration 与连接池

本章负责把 Server 的领域合同落到可以恢复、迁移、测试和观测的 PostgreSQL。后端项目是否“像一个真正的后端”，很大程度取决于数据模型、事务边界、迁移纪律和资源管理，而不取决于是否添加了更多框架。

## 1. PostgreSQL-first 的理由

第一阶段选择 PostgreSQL，是因为它同时提供：

- 事务和唯一约束，适合 batch 幂等；
- JSONB，能保存受控的动态 attributes；
- B-tree/BRIN/全文扩展等渐进式索引能力；
- 本地 Compose 易于复现；
- 关系模型适合 Project、Agent、Audit 等控制平面实体。

这不是宣称 PostgreSQL 永远足够。后续是否使用队列、ClickHouse 或分区，必须由吞吐、延迟、保留周期和查询计划证据触发。

## 2. 数据模型：把平台生命周期建出来

### 2.1 核心表

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
    id           uuid PRIMARY KEY,
    project_id   uuid NOT NULL REFERENCES projects(id),
    name         text NOT NULL,
    hostname     text NOT NULL,
    version      text NOT NULL,
    status       text NOT NULL CHECK (status IN ('active', 'stale', 'disabled')),
    last_heartbeat_at timestamptz,
    last_seen_ip inet,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
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
    reported_status text NOT NULL DEFAULT 'stopped' CHECK (reported_status IN ('running', 'stopped', 'error')),
    reported_at     timestamptz,
    last_error      text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, agent_id, name)
);

-- 单列外键不能阻止跨 Project 的 Agent/Pipeline/Key 引用。
ALTER TABLE agents ADD CONSTRAINT agents_project_id_id_uq UNIQUE (project_id, id);
ALTER TABLE pipelines
    ADD CONSTRAINT pipelines_agent_same_project_fk
    FOREIGN KEY (project_id, agent_id)
    REFERENCES agents(project_id, id);
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_agent_same_project_fk
    FOREIGN KEY (project_id, agent_id)
    REFERENCES agents(project_id, id);

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
    id             bigserial PRIMARY KEY,
    project_id     uuid NOT NULL REFERENCES projects(id),
    batch_id       uuid NOT NULL,
    batch_sequence integer NOT NULL CHECK (batch_sequence >= 0),
    agent_id       uuid NOT NULL REFERENCES agents(id),
    pipeline_id     uuid NOT NULL REFERENCES pipelines(id),
    observed_at    timestamptz NOT NULL,
    ingested_at    timestamptz NOT NULL DEFAULT now(),
    level          text NOT NULL,
    service        text NOT NULL,
    host           text NOT NULL,
    message        text NOT NULL,
    attributes     jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (project_id, batch_id, batch_sequence),
    FOREIGN KEY (project_id, batch_id)
      REFERENCES ingest_batches(project_id, id)
      ON DELETE CASCADE
);
```

和第 02、04 章一致，HTTP 的 `batch_id` 在这套迁移中存为 `ingest_batches.id`；`log_entries.batch_id` 只保存这个 ID。Repository 对外仍可使用 `BatchID` 命名，但 SQL 不要再混用两套主键列名。

### 2.2 运营表

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

CREATE TABLE usage_buckets (
    project_id      uuid NOT NULL REFERENCES projects(id),
    bucket_start    timestamptz NOT NULL,
    entries         bigint NOT NULL DEFAULT 0 CHECK (entries >= 0),
    bytes           bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    failed_batches  bigint NOT NULL DEFAULT 0 CHECK (failed_batches >= 0),
    PRIMARY KEY (project_id, bucket_start)
);
```

`quarantine_batches.payload` 要有严格大小上限；它不是无限的备份系统。大批次可以存储在受保护的文件对象中，表里只放引用和 hash，但那是后续演进，不要在第一阶段模糊 ACK 语义。

## 3. Migration 纪律

### 3.1 一个迁移一个目的

建议目录：

```text
migrations/
  0001_projects_keys.sql
  0002_agents_pipelines.sql
  0003_ingest_batches.sql
  0004_log_entries.sql
  0005_quarantine_retention_audit_usage.sql
```

每个文件应明确 up/down 或采用项目选定的不可逆迁移策略。不要把格式化、无关索引和业务变更混在一次迁移里。

### 3.2 向前兼容的发布顺序

对线上表结构，遵循：

```text
expand: 增加可选列/新表/新索引
  -> deploy code that writes old and new safely
  -> backfill with bounded jobs
  -> enforce constraint after data is clean
  -> contract: remove old column in later release
```

即使项目是个人项目，也要按这个顺序写，因为它直接展示你理解无停机演进，而不是只会初始化数据库。

### 3.3 Migration 验收

CI 至少执行：

1. 空数据库向前迁移；
2. 全部迁移后启动 Server；
3. 测试数据写入和查询；
4. 回滚/重建路径（若工具支持）；
5. 重复执行迁移时行为明确。

不要把“本机已经手动建过表”作为迁移通过的证据。

## 4. Repository 接口：按用例切窄

业务层需要的是能力，不是 SQL driver：

```go
type IngestRepository interface {
	InsertBatch(ctx context.Context, tx Tx, batch BatchRow) (InsertResult, error)
	InsertEntries(ctx context.Context, tx Tx, rows []EntryRow) error
	FindBatch(ctx context.Context, projectID, batchID uuid.UUID) (BatchRow, error)
}

type EntryRepository interface {
	List(ctx context.Context, q EntryQuery) (EntryPage, error)
}

type RetentionRepository interface {
	DeleteEntriesBefore(ctx context.Context, projectID uuid.UUID, before time.Time, limit int) (int, error)
}

type QuarantineRepository interface {
	ClaimPending(ctx context.Context, limit int) ([]QuarantineRow, error)
	MarkReplayResult(ctx context.Context, id uuid.UUID, result ReplayResult) error
}
```

不要创建：

```go
type Store interface {
	CreateProject(...)
	InsertBatch(...)
	ListEntries(...)
	DeleteRetention(...)
	// 还会不断增长
}
```

大接口会让测试 fake、事务边界和模块依赖一起膨胀。

### 4.1 事务抽象

事务应由 use case 拥有，而不是 repository 自己偷偷开启：

```go
type DB interface {
	BeginTx(ctx context.Context, opts TxOptions) (Tx, error)
}

type Tx interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}
```

具体项目可以选择 `database/sql`、某个 PostgreSQL driver 或其他 adapter，但将其依赖限制在 `internal/storage/postgres`。不要在领域接口暴露 driver 专用的 `Rows`、`TxOptions` 或 SQL builder 类型。

## 5. Row mapper 与 DTO 隔离

数据库 row 可能有 `[]byte` hash、nullable time、内部状态字段；HTTP response 则需要公开格式。分开定义：

```go
type ingestBatchRow struct {
	ProjectID  uuid.UUID
	BatchID    uuid.UUID
	PayloadHash []byte
	EntryCount int
	CreatedAt  time.Time
	CommittedAt *time.Time
	Status     string
}

func toDomain(row ingestBatchRow) (IngestBatch, error) {
	if len(row.PayloadHash) != sha256.Size {
		return IngestBatch{}, ErrCorruptRow
	}
	// 复制 bytes，避免复用 driver buffer。
}
```

Row mapper 要检查数据库中不应该出现的状态。数据库约束是第一道防线，mapper 的检查是发现损坏和迁移错误的第二道防线。

## 6. Ingest 事务实现重点

接入 use case 的伪代码：

```go
func (s *IngestService) Accept(ctx context.Context, principal Principal, batch Batch) (Result, error) {
	if !principal.Has("ingest") {
		return Result{}, ErrScopeDenied
	}
	tx, err := s.db.BeginTx(ctx, TxOptions{})
	if err != nil {
		return Result{}, classifyDBError(err)
	}
	defer tx.Rollback(ctx) // commit 后 rollback 是无害清理

	inserted, err := s.repo.InsertBatch(ctx, tx, batch.BatchRow())
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		existing, err := s.repo.FindBatchTx(ctx, tx, batch.ProjectID, batch.BatchID)
		if err != nil {
			return Result{}, err
		}
		if existing.PayloadHash != batch.PayloadHash {
			return Result{}, ErrIdempotencyConflict
		}
		return Result{Status: Duplicate, Count: existing.EntryCount}, nil
	}
	if err := s.repo.InsertEntries(ctx, tx, batch.EntryRows()); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, classifyDBError(err)
	}
	return Result{Status: Accepted, Count: len(batch.Entries)}, nil
}
```

实际代码需处理数据库唯一冲突、context cancellation 和 commit 状态未知等 driver 细节。原则是不在无法确认提交结果时返回 accepted；Agent 的重试会通过 idempotency 重新确认。

批量写入可使用 driver 的批量能力或多值 INSERT，但要在集成测试验证参数数量、事务回滚和大批次内存使用。不要为了“看起来高性能”直接引入未经评估的批量库。

## 7. 连接池和数据库生命周期

连接池不是越大越好。总连接数应低于 PostgreSQL 实际可用连接，并给 migration、管理连接和其他进程留余量。

配置项至少包括：

```text
DB_MAX_OPEN_CONNS
DB_MAX_IDLE_CONNS
DB_CONN_MAX_LIFETIME
DB_CONN_MAX_IDLE_TIME
DB_PING_TIMEOUT
```

启动顺序：

1. 解析 DSN，但不要打印完整 DSN（可能含密码）；
2. 创建池；
3. 在 startup context 中 Ping；
4. 执行已经授权的 migration；
5. 构造 repositories 和 services；
6. 仅在依赖就绪后报告 ready。

关闭顺序：

1. readiness 先变为 false；
2. 停止接收新请求；
3. 等待正在进行的事务到 deadline；
4. 关闭 HTTP listener；
5. 关闭 worker；
6. 关闭连接池。

`running` 不等于 `ready`。健康检查必须区分进程存活和数据库可用性。

## 8. 索引设计与迁移成本

基础查询索引：

```sql
CREATE INDEX log_entries_project_time_idx
  ON log_entries (project_id, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_service_time_idx
  ON log_entries (project_id, service, observed_at DESC, id DESC);
CREATE INDEX log_entries_project_level_time_idx
  ON log_entries (project_id, level, observed_at DESC, id DESC);
CREATE INDEX log_entries_ingested_at_brin_idx
  ON log_entries USING brin (ingested_at);
```

索引顺序要匹配最常见的 where 和 order by。每多一个索引，写入和保留删除都会变贵。通过 `EXPLAIN (ANALYZE, BUFFERS)`、索引大小和写入延迟证明收益，不要在文档中填未经测试的“QPS 提升 10 倍”。

如果未来按时间分区：

- 重新审查唯一约束是否包含分区键；
- 重新审查 cursor 的排序和跨分区查询；
- 先在影子环境回放真实查询；
- 明确旧数据 backfill 和失败恢复路径。

## 9. 测试矩阵

### Repository 单元测试

- SQL 参数顺序和 project 条件；
- row mapper 对 null、短 hash、未知状态的处理；
- rollback 路径不会返回 accepted；
- batch duplicate/conflict 的领域结果。

### PostgreSQL 集成测试

使用可复现的 Compose 数据库：

- 每次测试执行 migration；
- 第一次 batch 产生一行 batch 和 N 行 entries；
- 重复 batch 不增加 entries；
- hash 冲突返回 409；
- 任一 entry 插入失败时整批回滚；
- 不同 Project 的相同 batch ID 可以同时存在；
- keyset 查询使用正确索引和边界。

### 故障测试

- 数据库启动慢时 readiness 不提前返回成功；
- 事务期间断开数据库后没有半批次可见；
- commit 响应丢失后 Agent 重试返回 duplicate；
- 连接池达到上限时请求有界等待并可取消；
- migration 失败时 Server 不启动成“假 ready”。

## 10. 完成门与面试证据

- 所有 schema 由 migration 重建，不依赖手工 SQL；
- Repository 接口按用例切窄，业务层不依赖 SQL driver；
- ingest 的 ACK 边界有集成测试；
- connection pool、timeout、shutdown 有配置和指标；
- 通过 EXPLAIN 解释至少三种查询索引；
- 能演示一次事务回滚和一次数据库重启恢复；
- 能清楚说明何时继续 PostgreSQL，何时才进入分区/队列/分析库阶段。

## 11. 复盘题

1. 为什么事务应该由 use case 拥有，而不是每个 repository 方法各自开启？
2. 如果 commit 返回网络错误，客户端和 Server 分别应该做什么？
3. 为什么 `project_id` 必须进入每一个日志查询索引的前缀？
4. 迁移为何要采用 expand/contract，而不是一次删除旧列？
5. 连接池开到 CPU 核数的十倍为什么可能使延迟变差？
