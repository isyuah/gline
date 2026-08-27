# 04. 领域模型、API 与存储设计

## 1. 统一语言

| 名称 | 含义 |
| --- | --- |
| Project | 日志隔离与权限边界，一个 API Key 只属于一个 Project |
| Agent | 一个采集进程实例，拥有稳定 `agent_id` |
| Pipeline | Agent 内一个 Source + Parser 配置，拥有配置内唯一 ID |
| Entry | 一条规范化日志事件 |
| Batch | Agent 持久化并作为整体重试的一组 Entry |
| Spool | Agent 本地保存未确认 Batch 的持久化队列 |
| Checkpoint | Source 已经安全转换并进入 spool 的读取位置 |
| ACK | Server 已把 Batch 提交到其持久化边界后的确认 |

## 2. 协议模型与领域模型分离

推荐定义三类类型：

1. `protocol/ingestv1`：JSON 字段、协议版本、请求响应和公开错误码。
2. Server domain：已经鉴权并规范化的 Batch/Entry，`project_id` 由服务端上下文注入。
3. PostgreSQL row：数据库列和扫描细节。

不能直接把当前 `internal/logentry.LogEntry` 同时当作 Agent 内部对象、外部协议、Server domain 和数据库 row。否则任何字段调整都会同时破坏四层。

## 3. 上传协议 v1

### 3.1 请求

```http
POST /api/v1/batches
Authorization: Bearer glk_<key-id>_<secret>
Content-Type: application/json
X-Request-ID: optional-client-request-id
```

```json
{
  "protocol_version": 1,
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "agent_id": "host-a-agent",
  "sent_at": "2026-08-23T14:20:00Z",
  "entries": [
    {
      "sequence": 0,
      "pipeline_id": "orders-file",
      "observed_at": "2026-08-23T14:19:59.123Z",
      "level": "INFO",
      "service": "orders",
      "host": "host-a",
      "message": "order created",
      "attributes": {
        "order_id": "o-123"
      }
    }
  ]
}
```

设计要点：

- `batch_id` 在 Agent 首次落入 spool 时生成，所有重试保持不变。
- `sequence` 在 batch 内唯一且从 0 连续递增，形成廉价幂等键。
- `project_id` 不接受客户端输入，由 API Key 决定。
- `sent_at` 用于诊断排队时间，不替代日志的 `observed_at`。
- `ingested_at` 由 Server 写入。
- v1 固定允许的 level；未知文本由 Agent 规范化为 `UNKNOWN`。
- `attributes` 只允许 JSON object，限制深度、key 数、key 长度和值大小。

### 3.2 成功响应

```json
{
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "status": "accepted",
  "accepted_entries": 1
}
```

重复请求且内容一致：

```json
{
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "status": "duplicate",
  "accepted_entries": 1
}
```

两者都返回 200，因为 Agent 都可以安全删除本地批次。

### 3.3 错误响应

```json
{
  "error": {
    "code": "batch_too_large",
    "message": "batch exceeds the configured limit",
    "request_id": "req_..."
  }
}
```

错误映射：

| HTTP | 稳定 code 示例 | Agent 行为 |
| --- | --- | --- |
| 400 | `invalid_json`, `validation_failed` | 不重试，移入隔离区并报警 |
| 401 | `invalid_api_key` | 暂停发送并高等级报警 |
| 403 | `key_disabled` | 暂停发送并高等级报警 |
| 409 | `idempotency_conflict` | 不重试；表示本地状态或协议实现错误 |
| 413 | `body_too_large`, `batch_too_large` | 不原样重试；需拆批或隔离 |
| 429 | `rate_limited` | 按 `Retry-After` 重试 |
| 500 | `internal_error` | 退避重试 |
| 503 | `not_ready` | 退避重试 |

Server 不把 JSON 解码器、SQL 或内部错误文本直接返回给客户端。

## 4. 输入限制建议

初始默认值应可配置，但必须有硬上限：

| 项目 | 建议默认值 | 目的 |
| --- | --- | --- |
| HTTP body | 2 MiB | 防止内存放大 |
| 每批 entries | 500 | 控制事务与失败重试粒度 |
| 单条 message | 64 KiB | 防止异常行拖垮系统 |
| attributes JSON | 16 KiB | 避免动态字段失控 |
| attributes 深度 | 4 | 限制解析复杂度 |
| service/host/pipeline ID | 128 字符 | 控制索引与存储 |
| Agent 时间偏差 | 24 小时告警、可配置拒绝阈值 | 识别错误时钟 |
| 查询 page size | 默认 100，最大 500 | 控制数据库和响应 |
| 查询时间范围 | 默认 15 分钟，最大 7 天 | 防止无界扫描 |

具体数字不是永久协议，可以由配置演进；“必须有界”才是稳定原则。

## 5. 查询 API

### 5.1 查询日志

```http
GET /api/v1/entries?from=...&to=...&service=orders&level=ERROR&q=timeout&limit=100&cursor=...
Authorization: Bearer ...
```

响应：

```json
{
  "entries": [
    {
      "id": 123456,
      "observed_at": "2026-08-23T14:19:59.123Z",
      "ingested_at": "2026-08-23T14:20:00.041Z",
      "level": "ERROR",
      "service": "orders",
      "host": "host-a",
      "agent_id": "host-a-agent",
      "pipeline_id": "orders-file",
      "message": "request timeout",
      "attributes": {}
    }
  ],
  "next_cursor": "opaque-base64url-value"
}
```

### 5.2 过滤条件

MVP 支持：

- 必填 `from`、`to`；
- 可重复或逗号分隔的 `service`、`level`；
- 精确 `host`；
- `q` 对 message 的受限包含搜索；
- cursor + limit。

先不支持任意布尔 DSL。复杂 DSL 会同时扩大解析、安全、索引和兼容性成本。

### 5.3 游标分页

不要用大 offset。稳定排序键为：

```sql
ORDER BY observed_at DESC, id DESC
```

cursor 编码上一页最后一行的 `observed_at` 和 `id`，下一页条件为：

```sql
AND (observed_at, id) < ($cursor_time, $cursor_id)
```

cursor 应视为不透明值，可包含版本和签名，避免客户端依赖内部格式。

## 6. PostgreSQL 模型

### 6.1 表

```sql
CREATE TABLE projects (
    id          uuid PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           uuid PRIMARY KEY,
    project_id   uuid NOT NULL REFERENCES projects(id),
    key_prefix   text NOT NULL UNIQUE,
    secret_mac   bytea NOT NULL,
    name         text NOT NULL,
    scopes       text[] NOT NULL,
    disabled_at  timestamptz,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ingest_batches (
    project_id    uuid NOT NULL REFERENCES projects(id),
    batch_id      uuid NOT NULL,
    agent_id      text NOT NULL,
    payload_hash  bytea NOT NULL,
    entry_count   integer NOT NULL,
    sent_at       timestamptz NOT NULL,
    ingested_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, batch_id)
);

CREATE TABLE log_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id     uuid NOT NULL REFERENCES projects(id),
    batch_id       uuid NOT NULL,
    batch_sequence integer NOT NULL,
    agent_id       text NOT NULL,
    pipeline_id    text NOT NULL,
    observed_at    timestamptz NOT NULL,
    ingested_at    timestamptz NOT NULL DEFAULT now(),
    level          text NOT NULL,
    service        text NOT NULL,
    host           text NOT NULL,
    message        text NOT NULL,
    attributes     jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (project_id, batch_id, batch_sequence),
    FOREIGN KEY (project_id, batch_id)
        REFERENCES ingest_batches(project_id, batch_id)
        ON DELETE CASCADE
);
```

实际迁移还应添加长度/枚举约束，并由集成测试验证。这里表达的是关系与一致性边界，不是可以直接复制上线的最终 DDL。

### 6.2 初始索引

```sql
CREATE INDEX log_entries_project_time_idx
    ON log_entries (project_id, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_service_time_idx
    ON log_entries (project_id, service, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_level_time_idx
    ON log_entries (project_id, level, observed_at DESC, id DESC);

CREATE INDEX log_entries_ingested_brin_idx
    ON log_entries USING brin (ingested_at);
```

不要一开始给 `attributes` 建一个覆盖所有内容的 GIN 索引。先记录真实查询，再决定哪些属性值得索引。message 包含搜索可从 `pg_trgm` GIN 演进，但应先通过查询计划和写放大测量判断。

### 6.3 为什么初期不分区

百万级演示数据不需要先引入分区。分区会影响唯一约束、迁移、删除和查询计划。先用单表建立基线；当保留清理变慢或数据量达到明确阈值时，再按 `ingested_at` 做日/月分区，并重新设计包含分区键的唯一约束。

## 7. 幂等事务

推荐事务流程：

1. 对规范化协议负载计算稳定摘要。
2. 尝试插入 `ingest_batches(project_id, batch_id, payload_hash, ...)`。
3. 若主键冲突，读取已有摘要：相同则返回 duplicate，不同则返回 409。
4. 首次插入时使用 `COPY FROM` 或多值参数批量写 `log_entries`。
5. entry 数量与 batch 记录一致后提交。
6. 只有提交成功才响应 200。

不要使用“先查询是否存在，再插入”的非原子流程；并发重试会产生竞态。冲突判断应以数据库唯一约束为准。

## 8. API Key 设计

建议 key 格式：

```text
glk_<public-key-id>_<high-entropy-secret>
```

验证流程：

1. 解析公开 key ID，查询单行记录。
2. 使用服务端 secret pepper 对完整 secret 做 HMAC-SHA-256。
3. 用常量时间比较数据库中的 MAC。
4. 检查 `disabled_at`。
5. 检查路由需要的 `ingest` 或 `query` scope。
6. 把 `project_id` 与 scopes 放入 request context。
7. 访问日志只记录 key ID 的短前缀，不记录 secret 或完整 Header。

API Key 使用高熵随机 secret，不是用户密码。HMAC 查验比每请求运行昂贵的密码 KDF 更适合该场景；server pepper 必须来自 secret storage/environment，不能进入仓库。

Agent 使用只带 `ingest` scope 的 Key；查询客户端使用 `query` scope。开发演示可以创建同时拥有两个 scope 的 Key，但不能把这变成生产默认值。

## 9. 数据保留

MVP 先提供全局 `retention_days`，由定时 job 小批量删除旧日志：

- 每次删除固定数量，避免长事务；
- 暴露上次成功时间、删除数量和错误指标；
- 删除依据使用 `ingested_at`，避免客户端错误时钟导致永久保留；
- 删除 batch metadata 前先确保没有 entries 引用；
- 备份与恢复策略写入运维文档。

当数据增长到分区阶段，保留实现可改为 detach/drop 过期分区，但外部产品语义不变。

## 10. OpenAPI 与兼容性

- 维护一个版本控制的 OpenAPI 文档，CI 校验格式。
- 破坏性协议变化使用新 URL 或新 `protocol_version`。
- 只对外部可观察合同做兼容承诺；内部 package 不需要过早稳定。
- Agent 应记录 Server 返回的稳定错误 code，而不是解析 message。
- 至少保留一项兼容测试：当前 Agent 构造的 v1 batch 能被当前 Server 接收并查询。
