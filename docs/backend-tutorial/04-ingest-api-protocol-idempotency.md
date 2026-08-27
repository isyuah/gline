# 04. 接入 API、协议版本与幂等事务

这一章把 Server 从“能接收一段 JSON”推进为可以被多个 Agent 长期调用的后端数据平面。重点不是把 Handler 写得更长，而是先把网络边界、确认语义、错误分类和重复请求的合同固定下来。

## 1. 本章完成后的能力

完成后，Server 应该能回答下面这些问题：

1. 一个 Agent 发送的批次在什么时刻算“已接收”？
2. 请求超时后，Agent 重试同一个批次会不会产生重复日志？
3. 同一个 `batch_id` 被不同内容占用时，服务如何阻止数据污染？
4. 一个 30 MiB 的请求、一个未知 JSON 字段、一个没有权限的 API Key，分别返回什么？
5. 运维人员能否通过 `request_id` 找到一次失败请求，而日志中不会泄露凭证？

最终链路应当是：

```text
HTTP request
  -> request id / body limit / timeout
  -> API Key authentication
  -> protocol decode and validation
  -> DTO -> domain normalization
  -> canonical payload hash
  -> PostgreSQL transaction
       insert ingest_batches
       insert log_entries
       commit
  -> response 200 accepted/duplicate
```

注意最后两步的顺序：**只有数据库提交成功后才返回可让 Agent 删除本地 spool 的 ACK**。这不是 exactly-once；它是“至少一次传输 + 服务端幂等去重”。

## 2. 先定义稳定合同

### 2.1 HTTP 路径与版本

第一版只承诺一个批次接入资源：

```text
POST /api/v1/batches
```

版本放在 URL 中，是因为它可以让路由、OpenAPI 文档和客户端选择清晰可见。`protocol_version` 仍可放在 JSON 中用于快速拒绝错版本，但不能只依靠 JSON 字段来做路由版本。

查询 API 使用同一外部版本：

```text
GET /api/v1/entries
```

未来字段不兼容时新增 `/api/v2`，而不是在 v1 中悄悄改变含义。可选字段可以向后兼容地增加；删除字段、改变时间含义、改变 ACK 语义都属于破坏性变化。

### 2.2 请求模型

协议 DTO 不要直接复用 `internal/logentry.LogEntry`。DTO 只描述线上的字段：

```go
type BatchRequestV1 struct {
	ProtocolVersion int        `json:"protocol_version"`
	BatchID         string     `json:"batch_id"`
	AgentID         string     `json:"agent_id"` // must match the authenticated agent identity
	PipelineID      string     `json:"pipeline_id"`
	Sequence        int64      `json:"sequence"`
	SentAt          time.Time  `json:"sent_at"`
	Entries         []EntryV1  `json:"entries"`
}

type EntryV1 struct {
	Sequence   int            `json:"sequence"`
	ObservedAt time.Time      `json:"observed_at"`
	Level      string         `json:"level"`
	Service    string         `json:"service"`
	Host       string         `json:"host"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"attributes"`
}
```

Server 内部再转换为：

```go
type IngestBatch struct {
	ProjectID uuid.UUID
	BatchID   uuid.UUID
	AgentID   uuid.UUID
	PipelineID uuid.UUID
	Sequence  int64
	SentAt    time.Time
	Entries   []LogEntry
	PayloadHash [32]byte
}
```

本教程统一约定：线上的字段名是 `batch_id`，领域对象字段是 `BatchID`，PostgreSQL `ingest_batches` 表使用主键列 `id` 保存它。后文出现 `project_id + id` 的 SQL 时，指的就是同一个 `(project_id, batch_id)` 幂等合同，不能再另建一套 `batch_id` 主键。

`ProjectID` 不从 JSON 解码。它来自认证上下文；客户端传一个 `project_id` 也必须被拒绝或忽略，推荐拒绝，因为静默忽略会掩盖配置错误。

### 2.3 成功响应

首次写入：

```json
{
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "status": "accepted",
  "accepted_entries": 42
}
```

同 `project_id + batch_id`、同 payload hash 的重试：

```json
{
  "batch_id": "0191f7f2-9698-7b6d-bb35-75f5035406c2",
  "status": "duplicate",
  "accepted_entries": 42
}
```

两种响应都为 HTTP 200。Agent 不需要知道这次请求是首次提交还是 ACK 丢失后的重试，只要看到 200 就可以删除本地已确认批次。

### 2.4 稳定错误码

错误响应统一为：

```json
{
  "error": {
    "code": "validation_failed",
    "message": "request contains invalid fields",
    "request_id": "req_01J..."
  }
}
```

`code` 是机器合同，`message` 只是给人看的诊断，不能让 Agent 解析 message。

| HTTP | code | 是否重试 | 典型原因 |
| --- | --- | --- | --- |
| 400 | `invalid_json` / `validation_failed` | 否 | JSON 损坏、字段缺失、序号不连续 |
| 401 | `invalid_api_key` | 否 | Key 不存在或 secret 不匹配 |
| 403 | `scope_denied` / `project_disabled` | 否 | 凭证权限不足、Project 已禁用 |
| 409 | `idempotency_conflict` | 否 | 同 batch ID 但摘要不同 |
| 413 | `body_too_large` / `batch_too_large` | 否 | 超过硬限制 |
| 422 | `unsupported_protocol` | 否 | 路由存在但版本不可处理 |
| 429 | `rate_limited` | 是 | 项目或 Key 配额耗尽 |
| 500 | `internal_error` | 是 | 未分类的服务端错误 |
| 503 | `not_ready` | 是 | 数据库未就绪或正在维护 |

不要把所有错误都返回 500。错误分类是 Agent 重试策略和后端可观测性的共同基础。

## 3. 中间件顺序和请求边界

推荐的 HTTP 链：

```text
Recover
  -> RequestID
  -> AccessLog (只记元数据)
  -> MaxBodyBytes
  -> ServerTimeout
  -> AuthenticateAPIKey
  -> RequireScope("ingest")
  -> Handler
```

顺序有原因：

- `Recover` 必须包住全部处理，防止 panic 变成连接异常。
- Request ID 应在认证失败前生成，才能追踪失败请求。
- Body limit 要在解码前生效，避免 JSON 解码先分配大内存。
- 认证前不要把 body 全部读入日志；认证失败请求也可能是攻击流量。
- Scope 检查靠近业务 Handler，但必须发生在数据库写入之前。

### 3.1 请求 ID

接受可信的 `X-Request-ID` 需要长度、字符集和前缀校验；否则客户端可以注入换行或极长值。更简单的策略是：始终由 Server 生成内部 ID，若请求带合法 ID 则把它作为外部关联 ID 单独保存。

```go
type RequestMeta struct {
	RequestID string
	ExternalID string
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID() // 随机、短、不可预测；实现时使用项目现有随机工具
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

日志字段可包含 `request_id`、路由、状态码、耗时、project ID、batch ID、entry count；不能包含 Authorization 原文、secret、原始 message 和完整 attributes。

### 3.2 严格 JSON 解码

不要只调用框架的 `ShouldBindJSON` 就结束。必须同时实现：

1. body 硬上限；
2. `DisallowUnknownFields`，防止客户端误拼字段后被静默忽略；
3. 只接受一个 JSON value，拒绝 `{} {}`；
4. 必须检查 EOF；
5. 解码错误映射到稳定 code，而不是返回库错误字符串。

代码骨架：

```go
func decodeBatch(w http.ResponseWriter, r *http.Request, limit int64) (BatchRequestV1, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req BatchRequestV1
	if err := dec.Decode(&req); err != nil {
		return BatchRequestV1{}, ErrInvalidJSON
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return BatchRequestV1{}, ErrTrailingJSON
	}
	return req, nil
}
```

对 `http.MaxBytesReader` 返回的错误要识别为 `body_too_large`。不要把 `io.EOF` 当成空请求成功；空 body 是 400。

### 3.3 Content-Type、方法和压缩

v1 只接受 `Content-Type: application/json`，拒绝任意浏览器表单。请求方法不匹配由路由层返回 405。第一阶段不要支持 gzip request body；压缩会增加解压炸弹风险和 CPU 复杂度。需要时单独定义解压后的上限、压缩比和指标。

## 4. API Key、Project 和 Scope 注入

认证器的输出不是“一个 bool”，而是不可伪造的上下文：

```go
type Principal struct {
	KeyID    uuid.UUID
	ProjectID uuid.UUID
	AgentID  *uuid.UUID // set for an agent-scoped key; nil for project/admin keys
	Scopes   map[string]struct{}
}

func (p Principal) Has(scope string) bool {
	_, ok := p.Scopes[scope]
	return ok
}
```

认证流程：

1. 从 `Authorization: Bearer ...` 解析 public key ID 和 secret。
2. 用 key ID 查 `api_keys`，只取需要的列。
3. 使用服务端 pepper 计算 HMAC，与数据库中的 `secret_hash` 做常量时间比较。
4. 检查 disabled、过期和 Project 状态。
5. 将 `Principal` 放入 context。
6. `RequireScope("ingest")` 或 `RequireScope("query")` 再放行。

业务代码只读取 `Principal`，不能自己从 Header 解码 project。这样可保证每一条 SQL 都从可信上下文获得 `project_id`。如果是 agent-scoped key，还要要求请求的 `agent_id` 与 `Principal.AgentID` 相等；project/admin ingest key 则只能写入已注册且属于该 Project 的 Agent。

API Key 的 `last_used_at` 不要在每请求同步 UPDATE 主事务，否则高并发时会产生热点。可以做有界的异步合并更新，但必须接受它是近似统计，不把它当安全判断。

## 5. 批次校验：先协议，后业务

校验分两层：

### 5.1 协议层

- `protocol_version == 1`；
- `batch_id` 是合法 UUID；
- `agent_id` 是合法 UUID，并且必须属于认证 Principal 对应的 Agent；
- `sent_at` 为 RFC3339 时间；
- `entries` 非空且不超过上限；
- `sequence` 从 0 开始连续递增；
- 每个 entry 的时间、level、service、host 和 message 满足长度约束；
- attributes 是 object，深度和字节数有界。

### 5.2 领域层

- 同一个 batch 内不允许重复 sequence；
- 不允许客户端覆盖 server-owned 字段（project、ingested_at、数据库 id）；
- 规范化 level 到有限集合；
- 时间必须转换到 UTC；
- message 保留原文但禁止 NUL 等不可存储字符；
- 计算 entry_count，不能信任客户端额外提供的计数。

### 5.3 错误返回策略

把所有校验错误聚合为字段路径列表，但限制最多返回若干条，防止攻击者用超大错误响应拖垮 Server：

```json
{
  "error": {
    "code": "validation_failed",
    "fields": [
      {"path": "entries[3].sequence", "reason": "expected 3"}
    ],
    "request_id": "req_..."
  }
}
```

不要返回 SQL constraint 名、文件路径、堆栈或 API Key 信息。

## 6. canonical payload hash

幂等判断必须回答“这是同一个批次，还是同一个 ID 被复用了”。因此要比较规范化后的 payload 摘要。

### 6.1 规范化原则

hash 输入必须明确：

- 不包含 Authorization、request ID、接收时间和数据库生成 id；
- `project_id` 作为数据库命名空间，不必重复放入 JSON，但冲突键必须包含它；
- 时间统一 UTC，并使用固定精度；
- level 使用规范化后的大写值；
- attributes 的 object key 按字典序递归编码；
- batch 中 entry 顺序和 sequence 保持不变；
- 字符串使用 UTF-8，数值格式固定，禁止同一含义多种编码。

### 6.2 不要依赖“看起来稳定”的 map 序列化

如果 `attributes` 是 `map[string]any`，必须明确实现递归排序的 canonical encoder，或者将协议层限制为可排序的结构。不要把“当前 JSON 库恰好按 key 排序”当作长期合同而不写测试向量。

代码形状：

```go
func CanonicalPayload(b BatchRequestV1, normalized []LogEntry) ([]byte, error) {
	// 按协议规定的字段顺序写入；attributes 递归按 key 排序。
	// 这里返回的是 hash 的唯一输入，不是 HTTP 响应 JSON。
}

func PayloadHash(payload []byte) [32]byte {
	return sha256.Sum256(payload)
}
```

### 6.3 hash 不是安全签名

payload hash 只用于幂等和数据一致性检查。它不能替代 API Key 身份认证，也不能防止拥有该 Key 的调用者伪造内容。若未来需要端到端签名，应增加独立的签名字段和密钥轮换设计。

## 7. 幂等事务与 ACK 边界

### 7.1 事务状态机

```text
不存在
  -> BEGIN
  -> INSERT ingest_batches
  -> INSERT log_entries
  -> COMMIT
  -> 200 accepted

已存在 + hash 相同
  -> 读取已提交记录
  -> 200 duplicate

已存在 + hash 不同
  -> 409 idempotency_conflict
```

不要用“先 SELECT，没查到再 INSERT”的两步逻辑决定首次请求；并发请求会同时通过 SELECT。数据库唯一约束才是最终仲裁者。

### 7.2 SQL 骨架

```sql
BEGIN;

INSERT INTO ingest_batches
    (id, project_id, agent_id, pipeline_id, sequence_no, payload_hash,
     entry_count, payload_bytes, status, created_at, committed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'committed', $9, $10)
ON CONFLICT (project_id, id) DO NOTHING;

-- 通过 row count 判断是否首次插入。
-- 首次插入才批量写 log_entries。

INSERT INTO log_entries
    (project_id, batch_id, batch_sequence, agent_id, pipeline_id,
     observed_at, level, service, host, message, attributes)
VALUES ...;

COMMIT;
```

实际实现应在事务对象上完成全部写入，并在 commit 返回错误时把请求判为失败；commit 错误不能被当成成功。

### 7.3 冲突处理

数据库报告唯一键冲突后，读取同一 `project_id + id`（线上的 `batch_id`）的已存 hash：

- 相同：返回 duplicate；
- 不同：返回 409，写审计事件并增加冲突指标；
- 找不到：可能是并发事务回滚或隔离时序，重新按事务策略处理，不直接假设成功。

冲突 409 是不可重试错误。Agent 应把原始批次保留到 quarantine，供检查 batch ID 生成或 spool 恢复逻辑。

### 7.4 不是 exactly-once

窗口仍然存在：Server commit 成功后，在 HTTP 响应到达 Agent 前进程或网络可能失败。Agent 会重试，Server 看到 duplicate。数据库可能暂时不可达时，Agent 只把 batch 保留在 spool。正确的面试表述是：

> 传输语义是 at-least-once；服务端使用项目作用域的 batch ID、payload hash 和唯一约束实现幂等效果。不能宣称分布式 exactly-once。

## 8. 真实实现顺序

按下面的垂直切片推进，每一步都能编译和验收：

1. 定义 `protocol/ingestv1` DTO、错误码和限制常量。
2. 写严格解码器测试：未知字段、尾随 JSON、body 超限、空 body。
3. 写 DTO 到 domain 的转换和校验，不接数据库。
4. 写 canonical hash 固定向量测试。
5. 定义 `IngestRepository` 窄接口和 `Accept` use case。
6. 用内存 fake 验证 accepted、duplicate、conflict 三条路径。
7. 加 API Key middleware 和 scope 测试，确认 project 由上下文注入。
8. 添加 PostgreSQL migration 与真实 repository。
9. 用 Compose PostgreSQL 运行接入集成测试。
10. 最后接到 Agent dispatcher，验证 200/409/429/503 的行为。

不要一开始同时改 router、SQL、Agent 重试和部署文件；每个阶段失败时很难知道边界在哪里。

## 9. 测试矩阵与验收证据

### 单元测试

- 解码拒绝未知字段和第二个 JSON value；
- body 超限映射为 `body_too_large`；
- sequence 缺失、重复、乱序被拒绝；
- attributes 深度/字节限制；
- canonical hash 对 key 顺序不同但语义相同的 attributes 相同；
- 不同 message 或不同 sequence 产生不同 hash；
- scope 缺失不能调用 use case。

### 集成测试

- 第一次 batch 返回 accepted 并写入 batch/entries；
- 同 batch 同 hash 返回 duplicate，entry 数不增加；
- 同 batch 不同 hash 返回 409；
- 两个并发相同请求最多产生一批 entries；
- commit 失败时无成功响应；
- 不同 Project 即使 batch ID 相同也互不冲突；
- 数据库重启后已经提交的 batch 仍可被识别为 duplicate。

### E2E 测试

用真实 Agent 或最小 HTTP client：发送 batch，模拟响应丢失，再重试，查询结果必须只有一次。记录请求 ID、服务端状态、数据库行数和 Agent spool 状态，作为验收证据。

### 完成门

- `POST /api/v1/batches` 的成功/错误合同写入 OpenAPI；
- 任何 ACK 前都能在 PostgreSQL 中查到已提交 batch；
- duplicate 和 conflict 行为有真实数据库测试；
- 日志和指标不泄露 Authorization 或 message；
- Agent 能根据稳定错误码选择隔离或重试；
- 至少有一次故障注入证明 commit 后 ACK 丢失不会重复写入。

## 10. 常见错误与复盘题

常见错误：

- 直接把 `[]logentry.LogEntry` 作为公开请求模型；
- 使用内存 map 做幂等表；
- 返回 202 但没有持久化接入队列；
- 以 `batch_id` 全局唯一而忘记 Project 命名空间；
- 计算 hash 时包含接收时间，导致每次重试都被判冲突；
- 409 后让 Agent 无限重试；
- 把完整 Authorization 头写到 access log；
- 无时间范围的查询或无限 page size；
- 让客户端传 `project_id` 决定写入目标。

复盘题：

1. Server 在 commit 后、响应前崩溃，下一次请求经过哪条路径？
2. 如果两个 Project 使用同一个 batch ID，为什么不能互相冲突？
3. 如果 canonicalizer 只排序顶层 attributes key，嵌套对象会发生什么？
4. 为什么 request ID 不能参与 payload hash？
5. 当数据库唯一键冲突但读取不到原记录时，你会如何处理事务隔离？
