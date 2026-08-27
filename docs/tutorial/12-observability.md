# 12. 可观测性、健康检查与安全诊断

> 本章是实现教程，不是现状声明。文中的指标、端点和代码均表示目标实现；只有完成本章验收并保存证据后，才能在 README 或简历中写成已实现能力。

相关设计：[目标架构](../03-target-architecture.md)、[可靠性、安全与可观测性](../05-reliability-security-observability.md)、[开发路线图](../06-development-roadmap.md)。

## 1. 本章目标

完成本章后，你应能做到：

1. 用结构化日志回答“哪一类操作失败、失败发生在哪个组件”。
2. 用低基数 Prometheus 指标定位 Agent 积压、HTTP 错误、数据库变慢和查询退化。
3. 正确区分 liveness 与 readiness，避免数据库故障引发进程重启风暴。
4. 在明确授权的诊断网络上临时启用 pprof，并保证它默认关闭、独立监听。
5. 通过故障操作证明观测信号与真实系统状态一致。
6. 保存可审查的指标样本、日志样本和验证记录，而不是只截一张“看起来正常”的面板。

这一章不会先引入完整的日志平台、告警平台或分布式追踪后端。Gline 的首要目标是让自身主链路可解释，而不是堆叠观测组件。

## 2. 前置条件

开始前至少应具备：

- Agent 已有明确的 Pipeline、batch、spool、Dispatcher 状态边界。
- Server 已使用显式 `http.Server` 启动，并支持有界关闭。
- PostgreSQL 连接池与 migration 版本检查已经封装，不散落在 Handler 中。
- 上传、查询和鉴权错误已经有稳定分类，例如 `invalid_api_key`、`not_ready`、`idempotency_conflict`。
- 日志输出有一个统一构造入口，能够注入 `component` 和运行环境字段。
- 本机 `replace` 已移除，全新环境可以构建和测试。

如果持久化闭环还没有实现，可以先设计接口和指标名，但不要添加永远为零的“装饰性指标”，也不要把本章标记为完成。

## 3. 先定义观测问题

可观测性不是“把所有变量导出”。先列出操作人员真正要回答的问题：

| 问题 | 首选信号 | 辅助信号 |
| --- | --- | --- |
| Agent 为什么没有继续采集？ | Pipeline 状态、spool 水位 | 分类日志、source lag |
| 日志还在 Agent，还是已到 Server？ | 待发送批次数、最老批次年龄 | batch 生命周期日志 |
| Server 是拒绝请求还是处理变慢？ | HTTP 状态分类、路由延迟 | request ID 日志 |
| 数据库是否成为瓶颈？ | DB 操作延迟、错误分类、连接池 | 慢查询、查询计划 |
| 查询为何没有结果？ | 返回行数、filter shape | 项目隔离与时间范围日志 |
| 服务能否继续接流量？ | `/readyz` | DB ping 与 migration 检查结果 |
| 进程是否还能响应？ | `/livez` | 进程日志 |
| 优化是否真的有效？ | 同条件指标与 profile 对比 | 原始 benchmark 结果 |

每个信号都要有消费者。没人会查看、不能对应行动的指标会增加维护成本，应删除或暂缓。

## 4. 三类信号的职责

### 4.1 日志

日志回答离散事件：哪一次配置加载失败、哪个 Pipeline 停止、哪个 batch 被隔离。它适合保留上下文，但不适合计算长期错误率。

### 4.2 指标

指标回答趋势和聚合问题：积压是否持续上升、错误率是否突增、p99 是否退化。它不能携带每个 batch 的全部身份。

### 4.3 Trace

Trace 回答一次请求跨边界花在哪里。它应排在日志、指标和持久化闭环之后；否则你会先维护一套复杂链路，却仍无法证明 ACK 和数据安全。

推荐实现顺序：

```text
稳定错误分类
  -> 结构化日志
  -> 健康检查
  -> 核心指标
  -> 面板与告警规则
  -> pprof 验证
  -> 必要时引入 OpenTelemetry
```

## 5. 结构化日志合同

### 5.1 稳定字段

统一字段建议如下：

```text
component, operation, request_id, project_id, agent_id,
pipeline_id, batch_id, error_kind, duration_ms
```

不要要求每条日志都有全部字段。字段只在对应上下文存在时出现，但名称和含义必须稳定。

建议的事件名：

```text
agent.pipeline.started
agent.pipeline.stopped
agent.spool.high_watermark
agent.batch.retry_scheduled
agent.batch.quarantined
server.request.completed
server.ingest.committed
server.ingest.duplicate
server.query.failed
server.readiness.failed
```

事件名比自由文本更适合过滤；自由文本负责给人阅读。

### 5.2 敏感信息边界

默认禁止记录：

- `Authorization` Header、API Key secret、数据库 DSN；
- 上传 body、日志正文、完整 attributes；
- 查询关键词 `q`；
- 完整文件内容或解析失败原文；
- 数据库原始错误中可能带出的 SQL 参数。

可记录：

- 内部 UUID，或仅用于关联的短前缀；
- message 长度、attributes 大小、batch entry 数；
- 稳定错误分类；
- 文件路径经过部署模型评估后的逻辑名称。若项目面向多用户，默认只记录 Pipeline ID。

错误链在底层用 `%w` 保留，由进程/请求边界记录一次。每层都打印同一个错误会制造重复噪声。

### 5.3 一条日志的目标形态

以下只是字段示例，不代表当前已存在：

```json
{
  "level": "warn",
  "event": "agent.batch.retry_scheduled",
  "component": "dispatcher",
  "pipeline_id": "orders-file",
  "batch_id": "019...",
  "error_kind": "server_unavailable",
  "attempt": 3,
  "retry_after_ms": 1840
}
```

不要加入 `message` 正文，也不要把 `error.Error()` 直接当作 metric label。

## 6. 指标设计原则

### 6.1 类型选择

- Counter：只增加的事件总数，例如读取记录数、重试次数。
- Gauge：可增可减的当前状态，例如 spool bytes、待发送 batch 数。
- Histogram：持续时间或大小分布，例如上传耗时、batch entry 数。
- Summary：客户端计算分位数，跨实例难聚合；本项目优先使用 Histogram。

### 6.2 低基数要求

标签值集合必须可预估且有上限。安全的例子：

- `route`：路由模板，如 `/api/v1/batches`；
- `method`：有限 HTTP method；
- `status_class`：`2xx`、`4xx`、`5xx`；
- `result`：`accepted`、`duplicate`、`rejected`；
- `reason`：代码中封闭枚举；
- `operation`：`insert_batch`、`query_entries` 等固定集合。

禁止作为标签：

- request ID、batch ID、agent ID；
- 原始 URL、查询字符串、文件路径；
- 错误文本、日志 message；
- 默认情况下的 project ID；
- 无上限的 service、host、用户输入 attributes。

`pipeline` 标签是否允许取决于单 Agent 的配置上限。如果允许，必须设置 Pipeline 数上限并在文档中说明；Server 侧不要按所有 Agent 的 Pipeline 建标签。

### 6.3 名称与单位

遵循：

```text
<namespace>_<subsystem>_<name>_<unit>
```

- Counter 以 `_total` 结尾。
- 时间统一用秒 `_seconds`。
- 字节统一 `_bytes`。
- 名称表达观测量，不表达图表样式。

## 7. Agent 指标清单

第一批只实现能支持故障判断的指标：

```text
gline_agent_records_read_total{pipeline}
gline_agent_records_parse_failed_total{pipeline,reason}
gline_agent_batches_spooled_total
gline_agent_batches_sent_total{result}
gline_agent_batches_retried_total{reason}
gline_agent_batches_quarantined_total{reason}
gline_agent_spool_bytes
gline_agent_spool_batches
gline_agent_oldest_pending_seconds
gline_agent_pipeline_up{pipeline}
gline_agent_upload_duration_seconds
```

关键语义：

- `records_read_total` 在 Source 产生 RawRecord 后增加，不代表已进入 spool。
- `batches_spooled_total` 只在 spool 事务提交后增加。
- `batches_sent_total{result="accepted|duplicate"}` 只在合法 200 响应解析成功后增加。
- `spool_bytes` 和 `spool_batches` 从已提交状态计算，不能只统计当前进程的增减量。
- `oldest_pending_seconds` 在没有积压时为 `0`，定义写入帮助文本。
- `pipeline_up` 表示 Pipeline 能否继续工作，不等于进程存活。

避免把 `batch_id` 放入标签。要排查单个 batch，使用结构化日志关联。

## 8. Server 指标清单

```text
gline_server_http_requests_total{route,method,status_class}
gline_server_http_request_duration_seconds{route,method}
gline_server_ingest_entries_total{result}
gline_server_ingest_batches_total{result}
gline_server_ingest_batch_size
gline_server_db_operation_duration_seconds{operation}
gline_server_db_errors_total{operation,class}
gline_server_query_duration_seconds{filter_shape}
gline_server_query_rows
gline_server_auth_failures_total{reason}
gline_server_retention_last_success_timestamp_seconds
```

`filter_shape` 只表达是否存在固定类型的过滤器，例如：

```text
time_only
service_time
level_time
service_level_time
message_time
```

不能把具体 service、level 列表或关键词拼进去。未知组合归入 `other_bounded`，并定期检查是否需要新增固定形状。

HTTP middleware 应在路由匹配后取得路由模板。不要以 `/api/v1/entries?service=orders` 或 404 的任意用户路径作为 label；未匹配请求统一标成 `unmatched`。

## 9. Go 指标实现骨架

### 9.1 依赖与注册表

依赖必须通过 Go 工具添加。先查看项目兼容版本，再安装选定版本：

```powershell
go list -m -versions github.com/prometheus/client_golang
go get github.com/prometheus/client_golang@<已评估版本>
go mod tidy
```

CI/Linux 使用相同 Go 命令。不要手工编辑 `go.sum`。

不要把全局默认注册表散布到业务代码中。为 Agent 和 Server 分别构造 `prometheus.Registry`，测试时可以创建隔离实例：

```go
type AgentMetrics struct {
	RecordsRead     *prometheus.CounterVec
	BatchesRetried  *prometheus.CounterVec
	SpoolBytes      prometheus.Gauge
	SpoolBatches    prometheus.Gauge
	OldestPending   prometheus.Gauge
	UploadDuration  prometheus.Histogram
}

func NewAgentMetrics(reg prometheus.Registerer) *AgentMetrics {
	m := &AgentMetrics{
		RecordsRead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gline", Subsystem: "agent", Name: "records_read_total",
			Help: "Records emitted by a source before parsing.",
		}, []string{"pipeline"}),
		// 其余指标按同样方式定义，label 必须来自封闭集合。
	}
	reg.MustRegister(m.RecordsRead /* 其余 collectors */)
	return m
}
```

构造函数集中注册可以在启动时暴露重名、漏注册等配置错误。若组件测试需要重复构造，给每个测试独立 registry，不要捕获 panic 后继续运行。

### 9.2 依赖注入位置

- Source 只接收与读取/解析有关的窄记录器。
- Spool 在事务成功后更新容量指标。
- Dispatcher 负责 attempt、结果、退避原因和上传耗时。
- HTTP middleware 负责路由级请求指标。
- Ingest/Query service 负责业务结果。
- PostgreSQL adapter 负责 DB operation 指标。

不要让领域逻辑直接依赖整个 Prometheus API。可定义很窄的接口，或把观测调用放在 adapter 边界。

### 9.3 Histogram bucket

bucket 必须围绕目标延迟和实际分布设计。最初可以使用明确记录的保守集合，但第一次基准后要复核。不要因为默认 bucket 方便就永久保留，也不要按每个毫秒创建数百个 bucket。

## 10. `/metrics` 暴露方式

开发期可与业务 HTTP Server 共用监听器，但生产边界更推荐独立、受网络策略保护的运维监听器。无论哪种方式都不能要求公网访问。

```go
registry := prometheus.NewRegistry()
// 注册 process/go collectors 是否需要，由部署与面板需求决定。
opsMux := http.NewServeMux()
opsMux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
```

如果 Agent 和 Server 都使用独立运维监听地址，应提供：

```text
ops.enabled
ops.listen_address
```

默认监听 loopback 或受控内部网络。配置摘要只打印地址与开关，不打印 secret。

## 11. 健康检查

### 11.1 精确定义

`/livez` 回答：进程事件循环是否仍能处理请求？

- 不检查数据库。
- 不检查 migration。
- 不因为某个 Agent Pipeline 失败就让整个进程 liveness 失败。
- 正常返回简单的 200；不要执行慢操作。

`/readyz` 回答：当前实例是否应继续接收业务流量？

Server readiness 至少检查：

- 启动配置已完成；
- PostgreSQL 在短 timeout 内可达；
- 当前 migration 版本与二进制兼容；
- 进程没有进入 shutdown draining 状态。

Agent readiness 至少检查：

- 配置加载完成；
- spool 可读写且未达到阻塞上限；
- Dispatcher 未因 auth/permanent config 错误暂停；
- 必需 Pipeline 没有全部失效。

### 11.2 Handler 骨架

```go
type ReadinessChecker interface {
	Check(ctx context.Context) error
}

func LiveHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func ReadyHandler(checker ReadinessChecker, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		if err := checker.Check(ctx); err != nil {
			// 日志记录稳定 error_kind；响应不回显 DSN、SQL 或内部路径。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not_ready"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}
}
```

真实实现应统一 JSON error writer，正确处理写响应错误。这里强调的是依赖方向：Handler 只调用 Checker，数据库和 migration 细节由组合层提供。

### 11.3 迁移兼容性

不要只检查“migration 表存在”。Checker 应知道：

- 二进制要求的最小/精确 schema 版本；
- 当前数据库版本；
- dirty/半迁移状态；
- 是否正在 shutdown。

数据库不可用时 `/livez` 仍为 200，`/readyz` 应在短 timeout 后返回 503。这个行为必须有集成测试和运行证据。

## 12. pprof：默认关闭且独立监听

pprof 可能暴露 goroutine 堆栈、内存内容轮廓和内部路径，不能挂在公开业务地址上，也不能默认启用。

配置建议：

```yaml
diagnostics:
  pprof_enabled: false
  pprof_listen_address: "127.0.0.1:6060"
```

使用独立 mux，避免 `net/http/pprof` 的隐式默认路由污染业务 Server：

```go
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	return mux
}
```

只在配置显式开启时创建第二个 `http.Server`。它需要自己的错误通道、timeout 与 shutdown；启动失败不能被静默忽略。默认地址使用 loopback，远程诊断通过受控隧道或内部网络完成。

Windows PowerShell 采样：

```powershell
go tool pprof -http=:0 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
go tool pprof -http=:0 'http://127.0.0.1:6060/debug/pprof/heap'
```

CI/Linux：

```bash
go tool pprof -http=:0 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
go tool pprof -http=:0 'http://127.0.0.1:6060/debug/pprof/heap'
```

只有在稳定、可复现负载下采样。profile 文件可能含敏感上下文，不默认提交仓库；若作为证据保存，应先检查并按项目安全策略处理。

## 13. 最小面板与告警思路

第一张面板只回答四件事：

1. Agent 是否在积压：spool bytes、batch 数、最老 pending age。
2. 上传是否健康：accepted/duplicate/retry/quarantine 比率与耗时。
3. Server 是否健康：按路由的 2xx/4xx/5xx 与延迟。
4. PostgreSQL 是否退化：操作延迟、错误、连接池等待。

示意 PromQL：

```promql
sum(rate(gline_agent_batches_retried_total[5m])) by (reason)

histogram_quantile(
  0.95,
  sum(rate(gline_server_http_request_duration_seconds_bucket[5m])) by (le, route)
)

max(gline_agent_oldest_pending_seconds)
```

告警必须对应行动。例如：

- pending age 连续增长：检查 Server readiness、429/5xx、凭证和 DB。
- quarantine 增加：检查协议兼容或 batch 限制，不能自动删除。
- readiness 失败但 liveness 正常：不要重启风暴，先修复依赖。
- retention 最后成功时间过旧：检查 job 和数据库膨胀风险。

阈值要由部署目标和基线确定，本教程不编造固定秒数或吞吐阈值。

## 14. 分步实施

### 步骤 1：建立观测词汇表

列出 component、operation、error kind、result 的封闭枚举。审查任何用户输入是否可能进入 label 或日志。

验证：代码搜索能找到每个枚举的定义；没有以 `err.Error()` 构造标签。

### 步骤 2：统一结构化日志边界

在 bootstrap 构造 logger；组件派生固定字段；只在请求、Pipeline 或后台 job 边界记录错误。

验证：用错误 Key、数据库断开和 Parser 错误各触发一次，日志可关联且不含 secret/正文。

### 步骤 3：引入隔离 registry

分别构造 Agent/Server metrics；用单元测试 gather registry，断言关键 metric family 存在。测试指标名和 label 集合属于稳定运维合同，具体内部调用次数不是。

### 步骤 4：在状态提交后更新指标

例如 `batches_spooled_total` 必须在事务成功后增加，不能在尝试前增加。失败和成功分别有明确结果。

验证：让 spool commit 失败，确认成功计数不增加，错误分类增加。

### 步骤 5：实现健康检查

建立独立 Checker；设置短 timeout；加入 shutdown draining 状态。Server 的 livez 不依赖 DB，readyz 检查 DB 与 migration。

### 步骤 6：独立暴露运维端点

配置监听地址，默认不对公网；metrics 和健康端点根据部署需要决定是否同一 ops Server。pprof 始终独立且默认关闭。

### 步骤 7：建立故障到信号的映射

逐项执行下一章的故障测试，保存故障前、故障中、恢复后三段指标样本。确认报警信号在恢复后回落。

## 15. 手工验证命令

以下端口只是示例，以实际配置为准。

PowerShell：

```powershell
$opsBase = 'http://127.0.0.1:9091'
Invoke-WebRequest "$opsBase/livez" | Select-Object StatusCode, Content
Invoke-WebRequest "$opsBase/readyz" | Select-Object StatusCode, Content
(Invoke-WebRequest "$opsBase/metrics").Content |
  Select-String 'gline_(agent|server)_'
```

CI/Linux：

```bash
curl --fail --show-error http://127.0.0.1:9091/livez
curl --fail --show-error http://127.0.0.1:9091/readyz
curl --fail --show-error http://127.0.0.1:9091/metrics \
  | grep -E 'gline_(agent|server)_'
```

数据库故障验证：先确认目标是本项目 Compose service，再停止 PostgreSQL service；不要删除容器或 volume。预期：

- `/livez` 继续 200；
- `/readyz` 在短 timeout 内 503；
- 上传得到稳定的可重试错误，而不是挂死；
- 恢复数据库后 readiness 自动恢复；
- 日志与指标不包含 DSN。

## 16. 自动化验证重点

高价值测试包括：

- registry gather 后不存在禁止标签；
- route label 使用模板而不是原始 URL；
- spool commit 失败不增加成功指标；
- 相同错误映射到稳定 reason；
- DB 不可用时 livez 200、readyz 503；
- migration dirty/incompatible 时 readyz 503；
- shutdown 开始后 readyz 503；
- pprof 默认端口不可访问，显式启用后仅独立地址可访问；
- 日志捕获器确认 Authorization、secret、message 正文未出现。

不要编写“Prometheus Counter 会增加”这种只验证第三方库的测试。测试 Gline 何时认为操作成功、如何分类以及暴露边界。

## 17. 失败处理与排查

### 指标端点为空

依次检查：registry 是否是同一个实例、collector 是否注册、代码路径是否执行、Handler 是否使用对应 registry。不要直接切回全局默认注册表掩盖组装错误。

### 指标基数快速增长

抓取 metric family，统计 label value 数量。常见原因是原始 URL、错误字符串、project/batch ID。立即改成路由模板或封闭枚举；仅缩短保留时间不能解决生成端设计错误。

### readiness 偶发超时

检查 timeout 是否过短、DB pool 是否耗尽、migration 检查是否执行了重查询。readiness 应做轻量检查，但不能为了“常绿”而跳过 DB/schema 合同。

### liveness 跟着数据库失败

这是语义错误。移除外部依赖检查，仅保留进程响应能力；数据库状态放到 readiness。

### pprof 无法关闭

检查是否误用了全局 DefaultServeMux 或在 import 时自动注册。改成独立 mux，保存 server 引用并在进程 shutdown 中显式关闭。

### 日志量过大

不要简单关闭错误日志。检查是否每层重复记录、重试是否每次都打高等级日志、是否缺少聚合/采样。状态转换必须保留，重复 attempt 可以降级或采样，但关键首次、状态改变和最终失败不能消失。

## 18. 常见错误

- 把 project、batch、request ID 放进 metric label。
- 用原始错误文本作为 `reason`。
- 在事务提交前增加 accepted 指标。
- 只看进程存活，不暴露 Pipeline 部分失败。
- `/livez` 检查 PostgreSQL，导致外部依赖故障触发重启风暴。
- `/readyz` 永远 200，无法阻止未迁移实例接流量。
- pprof 与公开 API 共用地址并默认启用。
- 记录请求 body、查询词或 Authorization 帮助调试。
- 在没有基线时设置“漂亮”的告警阈值。
- 把一次 profile 截图当作性能结论。

## 19. 验收证据

建议在后续约定的验证目录保存以下产物；目录名应由项目统一，不在本章擅自创建：

- 执行命令、commit 和配置摘要；
- Agent 与 Server 的 `/metrics` 样本；
- 正常、DB 故障、恢复三段 `/livez` 与 `/readyz` 结果；
- 脱敏后的结构化日志样本；
- 标签基数审查表；
- pprof 默认关闭和独立监听的验证记录；
- 失败项、修复 commit 与复测结果。

证据中不得包含真实 API Key、DSN、日志正文或生产数据。

## 20. 复盘题

1. 为什么 liveness 不应检查 PostgreSQL，而 readiness 必须检查？
2. `spool_bytes` 为什么是 Gauge，`batches_spooled_total` 为什么是 Counter？
3. 为什么不能用 batch ID 作为指标标签？单 batch 又如何排查？
4. 在什么状态提交后才能增加 accepted 指标？
5. `pipeline` 标签在什么条件下可以接受？
6. 为什么 pprof 必须默认关闭并使用独立监听器？
7. 如果查询 p99 上升，你会按什么顺序关联 HTTP、DB 和查询计划证据？
8. 为什么日志脱敏不能只依靠部署人员“不要上传敏感日志”？
9. 哪些观测合同值得测试，哪些只是第三方库实现细节？

## 21. 完成门

只有同时满足以下条件，本章才算完成：

- [ ] Agent 和 Server 核心指标已实现，名称、单位和状态提交点有文档。
- [ ] 所有 label 来自封闭低基数集合，不泄露项目、凭证或日志内容。
- [ ] `/livez` 不依赖数据库；`/readyz` 检查数据库、migration 与 draining。
- [ ] 数据库故障和恢复行为已有自动化或可重复运行证据。
- [ ] pprof 默认关闭、独立监听、可随进程有界关闭。
- [ ] 结构化日志可以关联故障，同时不包含 secret、正文和查询词。
- [ ] 至少一张最小面板能解释积压、上传、Server 与 DB 四类状态。
- [ ] 验证证据对应当前 commit，失败项没有被省略。

通过本章只代表“系统状态可观察”。它不自动证明系统可靠或性能足够；可靠性需要下一章的故障验证，性能需要第 15 章的可复现实验。
