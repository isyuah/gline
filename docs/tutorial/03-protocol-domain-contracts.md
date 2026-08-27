# 03. 协议、领域模型与稳定合同

> 本章是目标设计的实现教学，不代表仓库当前已经具备这些能力。开始编码前，先阅读[目标架构](../03-target-architecture.md)和[领域、API 与存储设计](../04-domain-api-and-storage.md)。

## 1. 本章完成后你应当得到什么

这一章不急着连接数据库，也不急着写 Gin Handler。先把跨模块边界定义清楚，因为协议一旦被 Agent、Server、测试和演示脚本共同使用，修改成本会迅速升高。

完成本章后，应当得到：

1. 一个版本化的 `POST /api/v1/batches` 上传合同。
2. 相互分离的协议 DTO、Server 领域对象和 PostgreSQL row。
3. Server 能稳定返回、Agent 能稳定分类的错误码。
4. 明确且集中管理的请求限制。
5. 可复现的 batch payload hash 规则。
6. 查询过滤器、稳定排序和不透明 cursor 的合同。
7. 不依赖 HTTP 或数据库的协议与领域单元测试。

这里最重要的学习目标不是“多定义几个 struct”，而是理解三类变化为什么不应该互相拖累：

- JSON 字段为了兼容旧 Agent 而保留，不代表领域层也必须永远保留。
- 领域模型增加不变量，不代表数据库列名要成为公开 API。
- 数据库为了索引拆列或增加内部字段，不代表客户端需要知道这些细节。

## 2. 当前代码与目标之间的差距

当前实现位于：

- `internal/logentry.LogEntry`：同时承担 Agent 内部数据和上传 JSON。
- `internal/agent/destination.GlineDest`：发送 `{"entries": [...]}`。
- `internal/server/modules.UploadEntriesRequest`：直接包含 `[]logentry.LogEntry`。
- `internal/server/sink.EntrySink`：接收同一个 `LogEntry`。
- `POST /api/v1/entries/upload`：成功只表示打印型 Sink 返回 nil。

这能验证最早期链路，但还缺少几个决定可靠性的字段：

- 没有 `protocol_version`，无法演进协议。
- 没有稳定 `batch_id`，超时重试无法精确幂等。
- 没有 `agent_id` 与 `sequence`，无法定位来源或检查批内顺序。
- 没有项目上下文，无法做隔离。
- 没有公开错误码，Agent 只能把“非 200”混成一种失败。
- 没有统一限制，超大 body、空 batch 或无界 attributes 可能进入下游。
- 传输对象、领域对象、数据库对象混在一起。

因此，本章建议建立新边界，再由后续章节逐步把旧 endpoint 迁移过去。不要先在旧 `LogEntry` 上不断加数据库 tag、校验 tag 和内部状态字段。

## 3. 先建立统一语言

| 名称 | 精确定义 | 所有者 |
| --- | --- | --- |
| Project | 日志、凭证和查询的隔离边界 | Server |
| API Key | 绑定一个 Project，并携带 scope 的凭证 | Server |
| Agent | 一个采集进程实例，拥有稳定 `agent_id` | Agent 配置 |
| Pipeline | 一个 Source + Parser 的采集配置 | Agent 配置 |
| Entry | 一条经过规范化的日志事件 | 领域层 |
| Batch | 一组作为整体持久化、重试和确认的 Entry | Agent spool |
| `batch_id` | Batch 首次写入 spool 时生成、后续永不改变的 ID | Agent |
| `sequence` | Entry 在 Batch 内从 0 开始的稳定序号 | Agent |
| payload hash | Server 对规范化 Batch 内容计算的摘要 | Server |
| ACK | Batch 已越过 Server 持久化边界，可以从 Agent spool 删除 | Server |
| accepted | Batch 首次提交并写入成功 | Server |
| duplicate | 同一 Project 下，相同 ID 和相同摘要已写入 | Server |
| conflict | 同一 Project 下，相同 ID 对应不同摘要 | Server |

请特别注意：`project_id` 不属于上传 JSON。它只能由已经认证的 API Key 注入 request context。若允许客户端在 body 中发送 `project_id`，开发者很容易漏掉一次授权比较，从而产生跨项目写入。

## 4. 三层模型为何必须分离

### 4.1 协议 DTO

DTO 只关心网络合同：JSON 字段名、可选性、协议版本和公开格式。推荐包：

```text
internal/protocol/ingestv1
```

它可以同时被 Agent transport 和 Server HTTP adapter 引用，但不能依赖 Server 的数据库包。

### 4.2 Server 领域模型

领域对象表示“已经解码、鉴权、规范化并满足业务不变量的数据”。推荐包：

```text
internal/server/ingest
internal/server/query
```

领域对象可以使用强类型 `ProjectID`、`BatchID`，并且通过构造函数建立不变量。它不应该带 JSON tag 或数据库 tag。

### 4.3 PostgreSQL row

row 只服务 SQL 写入和扫描，例如数据库 UUID、`jsonb` 字节、内部创建时间。推荐包：

```text
internal/storage/postgres
```

row 不是 API response。查询返回前要从 row 映射到 query result，再映射到 response DTO。

### 4.4 依赖方向

```text
Agent transport ---> protocol/ingestv1
                         |
Server HTTP adapter -----+
          |
          v
Server ingest domain <--- Repository interface
          ^                    ^
          |                    |
          +------- PostgreSQL adapter
```

`protocol` 不导入 `server/ingest`；`server/ingest` 不导入 Gin 或 pgx；PostgreSQL adapter 可以导入领域包来实现其窄接口。

## 5. 上传协议 v1

### 5.1 HTTP 合同

```http
POST /api/v1/batches
Authorization: Bearer glk_<key-id>_<secret>
Content-Type: application/json
X-Request-ID: optional-client-request-id
```

请求示例：

```json
{
  "protocol_version": 1,
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "agent_id": "host-a-agent",
  "sent_at": "2026-08-23T08:30:00.123456Z",
  "entries": [
    {
      "sequence": 0,
      "observed_at": "2026-08-23T08:29:59.900000Z",
      "event_time": "2026-08-23T08:29:59.800000Z",
      "pipeline_id": "orders-file",
      "service": "orders",
      "host": "host-a",
      "level": "ERROR",
      "message": "payment provider timeout",
      "attributes": {
        "trace_id": "abc123",
        "attempt": 3
      }
    }
  ]
}
```

建议响应：

```json
{
  "status": "accepted",
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "accepted_entries": 1
}
```

重复请求：

```json
{
  "status": "duplicate",
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "accepted_entries": 1
}
```

`accepted` 和 `duplicate` 都是成功 ACK。Agent 收到任一结果后都可以删除 spool 中的 batch。两者使用 HTTP 200，是因为客户端的目标“确保这批数据已存在”已经满足。

### 5.2 DTO 骨架

以下代码是推荐骨架，不是要求逐字照抄：

```go
package ingestv1

import (
    "encoding/json"
    "time"
)

const ProtocolVersion = 1

type BatchRequest struct {
    ProtocolVersion int        `json:"protocol_version"`
    BatchID         string     `json:"batch_id"`
    AgentID         string     `json:"agent_id"`
    SentAt          time.Time  `json:"sent_at"`
    Entries         []EntryDTO `json:"entries"`
}

type EntryDTO struct {
    Sequence   int64           `json:"sequence"`
    ObservedAt time.Time       `json:"observed_at"`
    EventTime  *time.Time      `json:"event_time,omitempty"`
    PipelineID string          `json:"pipeline_id"`
    Service    string          `json:"service"`
    Host       string          `json:"host"`
    Level      string          `json:"level"`
    Message    string          `json:"message"`
    Attributes json.RawMessage `json:"attributes,omitempty"`
}

type BatchResponse struct {
    Status          BatchStatus `json:"status"`
    BatchID         string      `json:"batch_id"`
    AcceptedEntries int         `json:"accepted_entries"`
}

type BatchStatus string

const (
    BatchAccepted  BatchStatus = "accepted"
    BatchDuplicate BatchStatus = "duplicate"
)
```

为什么 DTO 的 `Attributes` 可以先使用 `json.RawMessage`：

- HTTP 层可以先限制原始 JSON 大小。
- 可以显式拒绝数组、字符串等非 object 值。
- 领域映射时再解码到受限值类型。
- 避免解码到 `map[string]any` 后数字一律变为 `float64`。

另一种可行方式是使用 `map[string]json.RawMessage`，逐值验证允许的 JSON 类型。不要在协议层接受任意深度、任意大小的 JSON，然后原样写入数据库。

## 6. 字段语义与不变量

### 6.1 Batch 级字段

| 字段 | 要求 | 原因 |
| --- | --- | --- |
| `protocol_version` | 必须等于 1 | 未知版本应明确拒绝 |
| `batch_id` | 合法 UUID，首次入 spool 后稳定 | 幂等主键 |
| `agent_id` | 非空、长度有限、字符集有限 | 来源识别和诊断 |
| `sent_at` | 合法 UTC 时间，可允许有限时钟偏差 | 诊断 Agent 延迟，不作为日志排序主键 |
| `entries` | 非空且条数有上限 | 空请求无意义，超大批次破坏延迟和内存上界 |

### 6.2 Entry 级字段

| 字段 | 要求 | 原因 |
| --- | --- | --- |
| `sequence` | 从 0 连续递增，批内唯一 | `(batch_id, sequence)` 可稳定标识 entry |
| `observed_at` | 必填 | Agent 实际观察时间，查询稳定排序依据 |
| `event_time` | 可选 | 原日志自带时间可能缺失或解析失败 |
| `pipeline_id` | 非空且有界 | 关联采集配置 |
| `service` | 非空且有界 | 核心过滤维度 |
| `host` | 非空且有界 | 核心过滤维度 |
| `level` | 枚举值 | 避免无界 level 基数 |
| `message` | 可为空但长度有界 | 某些结构化事件可能只有 attributes |
| `attributes` | object、深度/键数/总字节受限 | 控制存储与查询风险 |

`observed_at` 与 `event_time` 不应混淆。前者由 Agent 生成，保证每条记录有值；后者来自日志内容，可能错误、缺失或来自错误时区。默认查询排序使用 `observed_at`，用户查看时仍可以展示 `event_time`。

### 6.3 推荐 level 集合

```go
type Level uint8

const (
    LevelUnknown Level = iota
    LevelTrace
    LevelDebug
    LevelInfo
    LevelWarn
    LevelError
    LevelFatal
)
```

协议字符串与领域枚举通过显式函数转换：

```go
func ParseLevel(raw string) (Level, error) {
    switch strings.ToUpper(raw) {
    case "TRACE": return LevelTrace, nil
    case "DEBUG": return LevelDebug, nil
    case "INFO":  return LevelInfo, nil
    case "WARN":  return LevelWarn, nil
    case "ERROR": return LevelError, nil
    case "FATAL": return LevelFatal, nil
    case "UNKNOWN": return LevelUnknown, nil
    default:
        return 0, fmt.Errorf("unsupported level")
    }
}
```

不要悄悄把任意字符串降级为 `UNKNOWN`。如果协议允许未知值，应明确规定是兼容策略；否则拼写错误会静默污染数据。

## 7. 领域对象与构造边界

推荐让领域对象尽量不可被不受控地构造：

```go
package ingest

type ProjectID uuid.UUID
type BatchID uuid.UUID

type Batch struct {
    projectID ProjectID
    id        BatchID
    agentID   string
    sentAt    time.Time
    entries   []Entry
}

type Entry struct {
    sequence   int64
    observedAt time.Time
    eventTime  *time.Time
    pipelineID string
    service    string
    host       string
    level      Level
    message    string
    attributes Attributes
}

func NewBatch(
    projectID ProjectID,
    id BatchID,
    agentID string,
    sentAt time.Time,
    entries []Entry,
) (Batch, error) {
    // 统一执行领域不变量校验，并复制 entries，避免调用方后续修改。
}
```

这里不必为了“面向对象”给每个字段都加 getter。真正有价值的是：

- 构造完成的 Batch 一定带 `projectID`。
- `entries` 非空且 sequence 连续。
- 时间统一为 UTC。
- `attributes` 已经验证并规范化。
- 创建后不会被 HTTP 层意外修改。

如果 Go 中大量私有字段导致 mapper 非常繁琐，也可以使用公开只读约定的字段，但必须把校验集中在 `NewBatch`，且不要让 Handler 绕过构造函数直接调 Repository。

## 8. DTO 到领域对象的映射

建议 HTTP adapter 负责协议解码，mapper 负责格式到领域的转换：

```go
func ToDomain(projectID auth.ProjectID, req ingestv1.BatchRequest) (ingest.Batch, error) {
    if req.ProtocolVersion != ingestv1.ProtocolVersion {
        return ingest.Batch{}, ingest.ErrUnsupportedProtocol
    }

    batchID, err := uuid.Parse(req.BatchID)
    if err != nil {
        return ingest.Batch{}, fieldError("batch_id", "must be a UUID")
    }

    entries := make([]ingest.Entry, 0, len(req.Entries))
    for i, dto := range req.Entries {
        entry, err := toDomainEntry(dto)
        if err != nil {
            return ingest.Batch{}, prefixFieldError(fmt.Sprintf("entries[%d]", i), err)
        }
        entries = append(entries, entry)
    }

    return ingest.NewBatch(
        projectID,
        ingest.BatchID(batchID),
        req.AgentID,
        req.SentAt,
        entries,
    )
}
```

mapper 错误应携带安全的字段路径和稳定分类，但不应把整个日志 message 或 attributes 拼进错误文本。

## 9. 请求限制是一部分合同

不要把限制散落成 Handler 中的魔法数字。定义配置和绝对安全上限：

```go
type IngestLimits struct {
    MaxBodyBytes       int64
    MaxEntries         int
    MaxAgentIDBytes    int
    MaxPipelineIDBytes int
    MaxServiceBytes    int
    MaxHostBytes       int
    MaxMessageBytes    int
    MaxAttributesBytes int
    MaxAttributeKeys   int
    MaxAttributeDepth  int
}
```

限制值需要通过负载测试决定，本章不替你猜数字。实现时遵循：

1. 配置值必须大于 0。
2. 配置值不能高于编译期绝对安全上限。
3. HTTP body 限制必须在 JSON 解码前生效。
4. batch 条数和每字段限制在进入事务前完成。
5. Server 公开限制时，可以提供文档或响应错误，不必增加动态 discovery API。

字节长度与字符数要明确。网络与数据库成本更接近 UTF-8 字节，因此建议限制以 bytes 表示；UI 截断可另按 rune 处理。

## 10. 严格 JSON 解码

Gin 的普通绑定不足以表达全部合同。推荐统一 helper：

```go
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
    r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()

    if err := dec.Decode(dst); err != nil {
        return classifyDecodeError(err)
    }

    var trailing json.RawMessage
    if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
        if err == nil {
            return ErrTrailingJSON
        }
        return classifyDecodeError(err)
    }
    return nil
}
```

必须覆盖：

- 空 body；
- malformed JSON；
- 未知字段；
- body 超限；
- 一个对象后跟第二个 JSON 值；
- 错误 `Content-Type`；
- `entries: null` 与空数组的策略。

不要把 Go decoder 的原始错误直接回给客户端。它可能泄露内部类型名，也不稳定，无法作为 Agent 的长期合同。

## 11. payload hash：定义的是内容，不是请求字节

### 11.1 为什么不能直接 hash 原始 body

下面两个 JSON 在语义上相同，但字节不同：

```json
{"batch_id":"...","entries":[]}
```

```json
{
  "entries": [],
  "batch_id": "..."
}
```

如果直接 hash 原始 body，同一个 Batch 经不同 JSON encoder 重试时会被误判为冲突。反过来，如果随意 marshal `map[string]any`，数字类型和时间格式也可能变化。

### 11.2 推荐规则

Server 在 DTO 转领域并完成规范化后，按固定字段顺序写入摘要：

```text
hash-version
batch-id
agent-id
sent-at-as-unix-microseconds
entry-count
for each entry ordered by sequence:
  sequence
  observed-at-as-unix-microseconds
  optional event-time marker + value
  length-prefixed pipeline-id/service/host/level/message
  canonical attributes JSON
```

所有字符串使用 UTF-8，并使用长度前缀，避免 `ab + c` 与 `a + bc` 产生边界歧义。attributes 至少要做到：

- object key 按字节序排序；
- 数字规范化规则固定；
- 不保留无意义空白；
- 不允许重复 object key；
- 递归深度有界。

推荐为 hash 格式加入内部版本：

```go
const payloadHashVersion byte = 1
```

数据库保存 `payload_hash_version` 与 32 字节 SHA-256。不要信任客户端上传的 hash 作为冲突判断依据；客户端可以发送诊断摘要，但 Server 必须自己计算。

### 11.3 摘要接口

```go
type PayloadHasher interface {
    Sum(batch Batch) ([32]byte, error)
}
```

如果只有一种稳定实现，也不必为了 mock 而定义接口；纯函数更简单：

```go
func PayloadSHA256(batch Batch) ([32]byte, error)
```

摘要测试必须使用固定向量，并验证：字段顺序或 JSON 空白不改变语义摘要，任一真实字段变化会改变摘要。

## 12. 稳定错误合同

### 12.1 错误响应

```json
{
  "error": {
    "code": "invalid_request",
    "message": "request validation failed",
    "request_id": "01J...",
    "details": [
      {
        "field": "entries[0].service",
        "reason": "must not be empty"
      }
    ]
  }
}
```

稳定的是 `code`、HTTP status 和字段语义，不是英文 `message` 的逐字文本。

### 12.2 推荐错误码

| HTTP | code | Agent 行为 | 说明 |
| --- | --- | --- | --- |
| 400 | `invalid_json` | 永久失败，隔离 | JSON 语法或结构错误 |
| 400 | `invalid_request` | 永久失败，隔离 | 字段不满足合同 |
| 400 | `unsupported_protocol` | 永久失败，升级 Agent | 未支持的协议版本 |
| 401 | `authentication_required` | 暂停发送，提示配置 | 缺少凭证 |
| 401 | `invalid_api_key` | 暂停发送，提示配置 | key 不存在或 secret 错 |
| 403 | `insufficient_scope` | 暂停发送，提示权限 | key 无 ingest/query scope |
| 409 | `idempotency_conflict` | 隔离，不换 ID 重试 | 同 ID 不同 payload |
| 413 | `request_too_large` | 拆批或隔离 | body 超限 |
| 415 | `unsupported_media_type` | 修正客户端 | 非 JSON |
| 429 | `rate_limited` | 遵守 `Retry-After` | 临时限流 |
| 500 | `internal_error` | 退避重试 | 未知内部错误 |
| 503 | `service_unavailable` | 退避重试 | DB/迁移未就绪 |

不要为 PostgreSQL unique violation、context deadline、JSON decoder 文本分别公开新 code。内部错误类型可以细，外部错误合同应少而稳定。

### 12.3 领域错误

```go
var (
    ErrUnsupportedProtocol = errors.New("unsupported protocol")
    ErrInvalidBatch        = errors.New("invalid batch")
    ErrIdempotencyConflict = errors.New("idempotency conflict")
    ErrUnavailable         = errors.New("ingest unavailable")
)

type ValidationError struct {
    Fields []FieldViolation
}
```

HTTP adapter 使用 `errors.Is`/`errors.As` 映射，不通过字符串比较错误。存储 adapter 必须把 driver 错误翻译为领域可理解的类别，不能让 Handler 判断 PostgreSQL SQLSTATE。

## 13. 查询协议合同

推荐 endpoint：

```http
GET /api/v1/entries?from=...&to=...&service=orders&level=ERROR&host=host-a&q=timeout&limit=100&cursor=...
```

### 13.1 过滤器

```go
type ListEntriesParams struct {
    From    time.Time
    To      time.Time
    Service string
    Level   string
    Host    string
    Query   string
    Limit   int
    Cursor  string
}
```

HTTP adapter 解析后，构造 query domain：

```go
type Filter struct {
    ProjectID ProjectID
    From      time.Time
    To        time.Time
    Service   *string
    Level     *Level
    Host      *string
    Text      *string
    Limit     int
    After     *Position
}

type Position struct {
    ObservedAt time.Time
    EntryID    uuid.UUID
}
```

规则建议：

- `project_id` 仍只来自认证 context。
- `from` 和 `to` 必填或有明确默认窗口。
- `from < to`。
- 查询窗口有最大跨度。
- `limit` 有默认值和硬上限。
- `q` 长度有界，并明确是普通包含搜索还是完整 DSL；MVP 不提供 DSL。
- `cursor` 与其他过滤条件组合时，必须保持同一过滤器，否则可拒绝或将过滤器摘要放入 cursor。

### 13.2 稳定排序

统一使用：

```sql
ORDER BY observed_at DESC, id DESC
```

下一页条件：

```sql
AND (observed_at, id) < ($cursor_time, $cursor_id)
```

仅按时间排序不稳定，因为多条日志可能拥有相同微秒时间。`id` 是 tie-breaker。

### 13.3 不透明 cursor

cursor v1 可以编码：

```go
type cursorV1 struct {
    Version    int       `json:"v"`
    ObservedAt time.Time `json:"t"`
    EntryID    string    `json:"id"`
    FilterHash string    `json:"f,omitempty"`
}
```

对 JSON 使用 base64url 编码。它不必承载权限，因为 project 始终来自 key；但必须：

- 限制解码后的字节大小；
- 拒绝未知版本和额外字段；
- 验证时间与 UUID；
- 可选地绑定规范化过滤器摘要，避免把 A 查询的 cursor 用于 B 查询。

若以后 cursor 含有不希望客户端修改的内部状态，再增加 HMAC 签名。不要把 base64 误认为加密。

### 13.4 查询响应

```json
{
  "entries": [
    {
      "id": "0191...",
      "batch_id": "0191...",
      "sequence": 0,
      "observed_at": "2026-08-23T08:29:59.900000Z",
      "event_time": "2026-08-23T08:29:59.800000Z",
      "agent_id": "host-a-agent",
      "pipeline_id": "orders-file",
      "service": "orders",
      "host": "host-a",
      "level": "ERROR",
      "message": "payment provider timeout",
      "attributes": {"trace_id":"abc123"}
    }
  ],
  "next_cursor": "eyJ2IjoxLCJ0I..."
}
```

`next_cursor` 只有在可能存在下一页时返回。Repository 推荐读取 `limit + 1` 条：多出的 1 条只用于判断是否还有下一页，不返回给客户端。

## 14. 推荐包与接口清单

```text
internal/protocol/ingestv1/
  request.go
  response.go
  errors.go
  validation.go

internal/protocol/queryv1/
  request.go
  response.go
  cursor.go

internal/server/ingest/
  batch.go
  entry.go
  errors.go
  hash.go
  service.go

internal/server/query/
  filter.go
  result.go
  errors.go
  service.go
```

文件拆分是建议，不是硬指标。初期类型较少时可以合并，但包边界应保持。不要建立一个模糊的 `models`、`common` 或 `utils` 包来容纳所有类型。

## 15. 分步实现顺序

### 步骤 1：记录当前协议行为

先保留一个现有 Agent 到 Server 的合同测试，确认迁移前行为。测试不要依赖打印输出，只使用 recording sink。

### 步骤 2：建立 `ingestv1` DTO

只定义 JSON 合同和基础枚举。此时不连接 Gin 和数据库。为 request/response 添加 round-trip 与拒绝未知字段测试。

### 步骤 3：建立领域构造函数

实现 level、ID、时间、sequence 和 attributes 校验。保持纯函数，测试快速。

### 步骤 4：建立 mapper

输入必须包含认证产生的 `projectID`，不从 DTO 读取。验证错误返回安全字段路径。

### 步骤 5：实现 payload hash

先写规范，再写固定向量测试，最后实现。hash 规则一旦进入持久化数据库就属于兼容合同，变更时必须增加版本。

### 步骤 6：建立 query DTO 与 cursor codec

先只处理时间窗口、核心过滤条件和 keyset cursor，不添加动态排序或复杂 DSL。

### 步骤 7：迁移 endpoint

后续 Server 章节把新 Handler 挂到 `/api/v1/batches`。旧 `/entries/upload` 可以在一个明确迁移期内保留，也可以在未发布项目前直接删除；不要让两个 endpoint 长期拥有不同可靠性语义。

## 16. 测试策略

### 16.1 应当测试的稳定合同

- v1 正常请求能转换成带 Server project context 的领域 Batch。
- 客户端无法通过 body 指定 project。
- 未知协议版本被拒绝。
- sequence 缺失、重复、跳号被拒绝。
- 超限 body/entries/message/attributes 被拒绝。
- 未知 JSON 字段和 trailing JSON 被拒绝。
- accepted/duplicate response 可被 Agent 正确识别为 ACK。
- 409 被识别为不可自动绕过的 conflict。
- payload hash 固定向量稳定。
- query cursor round-trip，错误/超限 cursor 被拒绝。
- 相同 `observed_at` 下用 `id` 保证分页位置稳定。

### 16.2 不值得固定的细节

- Go struct 的字段排列。
- mapper 内部调用了几个 helper。
- 错误 message 的完整英文文本。
- JSON 缩进或字段输出空白。
- cursor base64 的具体字符，只要版本化 round-trip 合同成立；固定向量只在格式需要跨版本兼容时保留。

### 16.3 代表性测试骨架

```go
func TestToDomainUsesAuthenticatedProject(t *testing.T) {
    req := validBatchRequest()
    projectID := mustProjectID("11111111-1111-1111-1111-111111111111")

    batch, err := ToDomain(projectID, req)
    if err != nil {
        t.Fatal(err)
    }
    if diff := cmp.Diff(projectID, batch.ProjectID()); diff != "" {
        t.Fatalf("project mismatch (-want +got):\n%s", diff)
    }
}

func TestPayloadHashChangesWhenPayloadChanges(t *testing.T) {
    first := validDomainBatch()
    second := validDomainBatch()
    second = second.WithMessageForTest(0, "different")

    firstHash, err := PayloadSHA256(first)
    if err != nil { t.Fatal(err) }
    secondHash, err := PayloadSHA256(second)
    if err != nil { t.Fatal(err) }

    if firstHash == secondHash {
        t.Fatal("different payloads produced the same test hash")
    }
}
```

示例中的 `WithMessageForTest` 只是表达测试意图，不建议给生产领域对象加入测试专用 API。实际测试可以通过 fixture builder 构造两份对象。

## 17. 验收证据

本章完成时至少保存这些证据：

```text
go test ./internal/protocol/... ./internal/server/ingest/... ./internal/server/query/... -count=1
go test -race ./internal/protocol/... ./internal/server/ingest/... ./internal/server/query/... -count=1
go vet ./internal/protocol/... ./internal/server/ingest/... ./internal/server/query/...
```

另外人工核对：

- `rg "json:" internal/server/ingest internal/server/query` 不应发现领域对象直接承担外部 DTO。
- `rg "gin|pgx|database/sql" internal/server/ingest internal/server/query` 不应发现 transport/driver 泄漏。
- 上传请求中不存在 `project_id`。
- error code 已集中定义，不是 Handler 临时拼字符串。
- hash 规范和固定向量测试同时存在。

命令路径需按最终包布局调整。不能运行的命令要明确记录原因，不要将“代码存在”写成“合同已验证”。

## 18. 常见错误与为什么错误

### 18.1 一个 struct 贯穿四层

短期少写 mapper，长期任何字段变化都变成破坏性修改。尤其危险的是给 API struct 加数据库内部字段，随后因为 `omitempty` 或 tag 疏漏暴露出去。

### 18.2 让客户端传 project ID

这把隔离安全依赖于每个 Handler 都记得比较。正确方式是认证一次并将 Project 放入 context，所有 service method 强制接收 ProjectID。

### 18.3 对原始 body 做 hash

相同语义可能因为空白、字段顺序或 encoder 不同产生不同摘要。应 hash 规范化领域内容。

### 18.4 先查 batch 再写入

两个并发请求都可能查到“不存在”，随后重复插入。幂等最终必须由数据库唯一约束裁决。

### 18.5 409 后生成新 batch ID

这会把数据损坏伪装成一次新写入，直接制造重复。409 必须进入 quarantine 或人工处理。

### 18.6 cursor 只包含时间

时间相同时会跳过或重复数据。必须包含与 SQL 排序完全一致的 `(observed_at, id)`。

### 18.7 把错误文本当合同

数据库和 decoder 错误文本会变化，也可能泄露内部信息。客户端应依赖稳定 `code` 与 status。

### 18.8 为“未来兼容”接受所有未知字段

服务端悄悄忽略拼错字段，例如 `servcie`，比明确失败更难排查。v1 默认严格解码；真正新增字段时通过兼容设计或新协议版本处理。

## 19. 复盘题

1. 为什么 `project_id` 不应出现在上传 DTO？
2. DTO、domain、row 各自允许依赖哪些包？
3. 为什么 `accepted` 和 `duplicate` 都能让 Agent 删除 spool batch？
4. 如果数据库已提交但 200 响应丢失，下一次请求会走什么路径？
5. 为什么 hash 原始 JSON body 会误报冲突？
6. `observed_at` 与 `event_time` 分别服务什么问题？
7. 为什么 keyset cursor 需要时间和 ID 两个值？
8. 哪些错误应重试，哪些错误应隔离，哪些错误应暂停等待人工修复？
9. 为什么错误 code 应稳定而 message 不必逐字稳定？
10. 增加 ClickHouse 时，哪些外部协议可以保持不变？

如果你不能不看文档清楚回答第 3、4、5 题，不要进入数据库幂等实现。

## 20. 完成门

只有同时满足以下条件，才算完成本章，而不是“定义了一些 struct”：

- [ ] `/api/v1/batches` v1 request/response/error 合同有代码和测试。
- [ ] DTO、domain、PostgreSQL row 位于不同边界。
- [ ] 上传 body 不接受 `project_id`。
- [ ] 限制在解码与领域校验两个层次生效。
- [ ] payload hash 规则有版本、有固定向量、有变更敏感性测试。
- [ ] accepted/duplicate/conflict 的语义不含糊。
- [ ] query filter 和 `(observed_at, id)` cursor 有 round-trip 测试。
- [ ] 不依赖 Gin 或数据库即可运行协议/领域测试。
- [ ] 未把任何目标能力描述成当前已完成。

下一步进入[Server 启动、HTTP 边界与生命周期](08-server-bootstrap-http.md)，把这些合同挂到一个有界、可关闭、可测试的 HTTP 进程中。
