# 05. 查询、搜索、Keyset 分页与查询成本治理

接入完成后，Server 还不能称为日志管理后端。后端面试中，查询 API 的价值不在于“拼一条 SELECT”，而在于你能否同时处理租户隔离、稳定分页、搜索成本、并发资源和可解释的失败行为。

## 1. 目标与边界

本章实现 `GET /api/v1/entries`，第一版支持：

- Project 由 API Key 决定；
- 必填时间范围；
- service、host、level 精确过滤；
- message 的受限包含搜索；
- 稳定的 keyset pagination；
- page size、查询时间和并发上限；
- 返回不透明 cursor；
- 记录耗时、结果数、超时和拒绝原因。

第一版明确不支持任意 SQL、任意布尔 DSL、无限时间范围、任意 JSONPath 和用户自定义排序。功能少一些，反而能把性能和安全合同讲清楚。

## 2. 查询模型和权限模型

### 2.1 Query DTO

HTTP 参数先解码为传输 DTO，再转换为领域 Query：

```go
type EntryQueryParams struct {
	From    string
	To      string
	Service []string
	Host    []string
	Level   []string
	Q       string
	Limit   int
	Cursor  string
}

type EntryQuery struct {
	ProjectID   uuid.UUID
	From        time.Time
	To          time.Time
	Services    []string
	Hosts       []string
	Levels      []Level
	MessageTerm string
	Limit       int
	After       *CursorPosition
}
```

`ProjectID` 从 `Principal` 注入，不能出现在客户端参数中。若 URL 带 `project_id`，推荐返回 `400 unsupported_filter`，让错误尽早暴露。

### 2.2 过滤合同

推荐请求示例：

```http
GET /api/v1/entries?from=2026-08-24T00:00:00Z&to=2026-08-24T01:00:00Z&service=orders&level=ERROR&limit=100
Authorization: Bearer glk_...
```

响应示例：

```json
{
  "entries": [
    {
      "id": 123,
      "observed_at": "2026-08-24T00:40:00.000Z",
      "ingested_at": "2026-08-24T00:40:01.000Z",
      "level": "ERROR",
      "service": "orders",
      "host": "node-a",
      "agent_id": "agent-a",
      "pipeline_id": "orders-file",
      "message": "request timeout",
      "attributes": {}
    }
  ],
  "next_cursor": "eyJ2IjoxLCJ0Ijoi..."
}
```

返回的 cursor 为空字符串或 null 表示没有下一页。不要把数据库内部 offset 暴露给客户端。

## 3. 参数校验和查询成本预算

### 3.1 必填时间范围

`from` 和 `to` 必须同时存在，使用 RFC3339 解析并转换到 UTC，要求 `from < to`。限制最大时间跨度，例如默认 7 天或由配置控制。这个数字是部署策略，不是永远不变的业务合同；但“必须有上限”是稳定不变量。

拒绝：

- 缺少任一时间；
- `from >= to`；
- 超过最大范围；
- 年份明显超出支持范围；
- 时区解析失败。

返回 400 `invalid_time_range`，不要让数据库自己处理模糊的日期。

### 3.2 limit 与集合过滤

- 缺省 limit 100；
- 最大 limit 500；
- service、host、level 列表分别限制数量；
- 去重并排序集合值，形成稳定查询指纹；
- 空字符串过滤项直接拒绝；
- `q` 长度有限，禁止把整个日志正文当搜索词。

### 3.3 查询预算

每个查询都应有预算：

```text
时间跨度预算
返回行数预算
SQL statement_timeout
单请求占用的连接时间
并发查询槽位
响应体大小预算
```

预算超限是 400（客户端参数不合理）或 429（资源暂时不足），不是 500。数据库错误和 timeout 才是 5xx/可重试类别。

可以在事务或连接上设置：

```sql
SET LOCAL statement_timeout = '2s';
```

实际值由配置读取，不能在每次查询中拼接用户字符串。超时错误映射为稳定的 `query_timeout`，日志只记录请求元数据。

### 3.4 并发门控

一个 Project 的查询不能占满整个连接池。应用层可以按 Project 设置有界 semaphore：

```go
type QueryLimiter interface {
	Acquire(ctx context.Context, projectID uuid.UUID) (release func(), err error)
}
```

获取不到槽位时返回 429，并设置合理的 `Retry-After`。release 必须通过 `defer` 保证，包含 decode、repository 和 response 写入的全路径。

## 4. Keyset Pagination

### 4.1 为什么不用 OFFSET

`OFFSET 100000` 要让数据库先找到并跳过前 100000 行；数据不断写入时，前一页和后一页之间还可能出现重复或跳过。日志检索通常是按时间倒序翻页，适合使用稳定排序键。

### 4.2 排序键

采用：

```sql
ORDER BY observed_at DESC, id DESC
```

`observed_at` 允许多条日志相同，所以必须加单调递增的 `id` 作为 tie-breaker。下一页使用上一页最后一行：

```sql
AND (observed_at, id) < ($cursor_observed_at, $cursor_id)
```

方向与排序必须匹配；把 `DESC` 写成 `ASC` 但继续使用 `<` 会产生漏行或重复。

### 4.3 cursor 内容

cursor 是协议字段，不是数据库实现细节。推荐携带：

```json
{
  "v": 1,
  "project_id": "...",
  "filter_hash": "...",
  "observed_at": "2026-08-24T00:40:00Z",
  "id": 123
}
```

编码为 base64url。为了防止客户端改写 cursor 访问别的 Project 或跳过过滤条件，可以附加 server-side HMAC：

```text
base64url(payload) + "." + base64url(mac)
```

验签失败返回 400 `invalid_cursor`。如果不做签名，也必须验证 cursor 中的 project、filter hash 与当前请求一致；不能直接信任客户端传来的 project ID。

`filter_hash` 来自规范化后的 `from/to/services/hosts/levels/q`，不包括 cursor 自身和 limit。limit 改变时可以允许继续翻页，也可以视为新查询，必须在文档中固定一种策略。

### 4.4 cursor 代码骨架

```go
type Cursor struct {
	Version     int       `json:"v"`
	ProjectID   uuid.UUID `json:"project_id"`
	FilterHash  [32]byte  `json:"filter_hash"`
	ObservedAt  time.Time `json:"observed_at"`
	ID          int64     `json:"id"`
}

func DecodeCursor(raw string, expected uuid.UUID, filterHash [32]byte) (Cursor, error) {
	// base64url decode -> JSON decode -> version/project/filter 校验 -> 可选 HMAC 验证。
}
```

不要把 cursor 解码成 SQL 片段。它只能产生参数值。

## 5. 参数化 SQL 和过滤实现

Repository 接收已经验证过的 `EntryQuery`，不接收原始 query string：

```go
type EntryRepository interface {
	List(ctx context.Context, query EntryQuery) (EntryPage, error)
}
```

SQL 的核心形状：

```sql
SELECT id, observed_at, ingested_at, level, service, host,
       agent_id, pipeline_id, message, attributes
FROM log_entries
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
  AND ($4::text[] IS NULL OR service = ANY($4))
  AND ($5::text[] IS NULL OR host = ANY($5))
  AND ($6::text[] IS NULL OR level = ANY($6))
  AND ($7 = '' OR message ILIKE $8 ESCAPE '\\')
  AND ($9::timestamptz IS NULL OR (observed_at, id) < ($9, $10))
ORDER BY observed_at DESC, id DESC
LIMIT $11;
```

这里的 `$8` 应是经过转义的包含模式，而不是直接拼接：

```go
func containsPattern(term string) string {
	term = strings.ReplaceAll(term, `\`, `\\`)
	term = strings.ReplaceAll(term, `%`, `\%`)
	term = strings.ReplaceAll(term, `_`, `\_`)
	return "%" + term + "%"
}
```

参数化可以阻止 SQL 注入，但不能自动解决通配符造成的全表扫描，所以仍需长度限制、时间范围和查询计划检查。

`ANY($n::text[])` 的具体写法要根据所选 PostgreSQL 驱动适配；不要在没有确定驱动合同前在 go.mod 中猜依赖版本。Repository 的 SQL 单元测试可以先验证参数和逻辑，真实数组绑定放到 PostgreSQL 集成测试中验证。

## 6. 索引和搜索演进

第一版索引：

```sql
CREATE INDEX log_entries_project_time_idx
  ON log_entries (project_id, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_service_time_idx
  ON log_entries (project_id, service, observed_at DESC, id DESC);

CREATE INDEX log_entries_project_level_time_idx
  ON log_entries (project_id, level, observed_at DESC, id DESC);
```

索引选择要从查询证据开始：

1. 记录真实查询形状和耗时分布；
2. 用 `EXPLAIN (ANALYZE, BUFFERS)` 在脱敏数据上检查扫描行数；
3. 观察写入放大和索引大小；
4. 只为高频、稳定且无法接受顺序扫描的过滤组合增加索引。

message 的 `%term%` 在规模变大后可能无法使用普通 B-tree。可演进为 PostgreSQL trigram 或全文搜索，但必须先测量索引占用、写入成本、中文分词效果和查询语义。不要仅因为“有搜索功能”就无条件建立 GIN。

attributes 查询先只支持有限的精确键，或暂时不支持任意属性搜索。动态 JSONB DSL 会把成本治理、索引和兼容性一起复杂化。

## 7. 查询服务的职责

Query Service 应按以下顺序工作：

1. 从 context 读取 Principal，要求 `query` scope；
2. 解析并验证时间、filters、limit；
3. 规范化过滤集合，计算 filter hash；
4. 解码并验证 cursor；
5. 获取 Project 查询槽位；
6. 调 Repository；
7. 构造 response DTO 与 next cursor；
8. 记录 metrics 和审计元数据。

它不负责：

- 解析 Authorization；
- 拼 SQL；
- 直接返回数据库 row；
- 修改日志数据；
- 根据客户端传入的 project_id 越权切换租户。

## 8. 错误、并发和取消边界

- 客户端断开连接时，所有 DB 调用使用 `r.Context()`，及时取消；
- Repository 不把取消当作服务故障，记录为请求中止；
- 查询槽位 release 使用 `defer`；
- cursor 无效、时间范围非法、limit 越界为 400；
- 配额暂时耗尽为 429；
- DB 未就绪为 503；
- statement timeout 为 `query_timeout`，可重试但不能承诺原查询一定快速成功；
- 不能把数据库错误内容返回给客户端。

如果响应写入失败，不能回滚已经完成的只读查询，也不需要重试查询；只需要记录客户端中止和耗时。

## 9. 真实实现顺序

1. 定义 `EntryQuery`、`EntryPage` 和错误类型。
2. 实现 query 参数解析和时间/limit 校验。
3. 实现 cursor 编码、解码、filter hash 和失效测试。
4. 使用内存 repository 验证 Project 隔离、排序和下一页逻辑。
5. 写 PostgreSQL repository 的参数化 SQL。
6. 用真实 PostgreSQL 验证数组过滤、keyset 条件和 cursor 边界。
7. 加 statement timeout、并发 limiter 和 metrics。
8. 接入 HTTP router 与 OpenAPI。
9. 用 Agent 写入的真实批次执行 E2E：从第一页走到末页，期间继续写入新日志。

## 10. 测试与验收

### 单元测试

- 缺少 from/to、from >= to、超过最大范围；
- limit 缺省、最大值和越界；
- filters 去重、空值和规范化；
- cursor 篡改、版本错误、Project 不匹配、filter hash 不匹配；
- message `%`、`_`、反斜杠按字面搜索；
- 下一页排序不会重复或跳过同一时间戳的行。

### 集成测试

- 两个 Project 写入相同 service 和时间，互相不可见；
- 空结果有正确的 `next_cursor`；
- 插入与分页并发时，每条已提交记录最多出现在一次分页遍历中；
- statement timeout 被映射为稳定错误；
- 查询取消会释放连接和 limiter 槽位；
- EXPLAIN 证明时间范围查询使用目标索引（不要硬编码某个成本数字）。

### 故障测试

- 数据库重启期间查询返回 503 或受控重试，而不是挂死；
- 并发查询超过 Project 上限时出现 429 且最终都会 release；
- 客户端中途中断，服务端 goroutine 和连接数回落。

### 完成门

- 查询永远带 Project 条件和有限时间范围；
- 不使用大 OFFSET；
- cursor 不暴露可修改的 SQL 或跨 Project 信息；
- 查询成本有可配置预算和指标；
- 集成测试覆盖 keyset 边界、权限和取消；
- 面试演示能展示“写入一批、分页查询、继续写入、继续翻页”的稳定结果。

## 11. 常见错误与复盘题

常见错误：

- `GET /entries?project_id=...` 直接决定租户；
- `SELECT ... OFFSET $n LIMIT $m` 作为长期分页方案；
- 用日志 message 拼接 SQL；
- 让空时间范围代表“查全部”；
- cursor 只保存 id，不保存排序时间；
- cursor 不验证当前过滤器；
- 为每种可能组合创建大量索引；
- 把 query timeout 当成数据库永久故障；
- 查询取消后忘记释放连接或并发槽位。

复盘题：

1. 如果两条日志的 `observed_at` 一样，为什么仍需要 id？
2. 新写入一条更晚的日志，会不会让正在翻页的客户端看到重复？为什么？
3. 为什么 limit 不能只在 SQL 中限制，Handler 也要验证？
4. 什么证据能证明某个索引值得保留？
5. 如果未来改成按时间分区，cursor 和唯一约束需要重新检查哪些地方？
