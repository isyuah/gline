# 07. Dispatcher、HTTP 重试与端到端背压

> 本章描述目标实现。当前 `TickOrBatchSender` 在内存中聚合 entry，任何 Destination 错误都会结束 Sender；它尚未具备本章的 durable queue、错误分类和可恢复重试。实现前应完成[Spool 与 Checkpoint](./05-spool-checkpoint.md)。

## 1. 本章目标

完成本章后，你应该能够：

1. 让 Dispatcher 只发送已经持久化的不可变 batch；
2. 把“一次 HTTP 尝试”和“跨尝试重试策略”拆成 Transport 与 Dispatcher；
3. 分类 accepted、duplicate、temporary、rate-limited、auth、permanent 和 conflict；
4. 实现可取消的指数退避、full jitter 与 `Retry-After`；
5. 保证 timeout、ACK 丢失和 ACK 后本地删除失败都不会产生重复写入；
6. 让网络故障最终通过 spool 容量反向阻塞 Source，而不是耗尽内存；
7. 定义 shutdown 中 in-flight batch 的行为；
8. 用 `httptest.Server`、真实 spool 和故障 Server 验证完整链路。

## 2. 当前代码定位

### 2.1 `TickOrBatchSender`

`internal/agent/sender/tickOrBatch.go` 当前同时负责：

- 从共享 channel 消费 entry；
- 按条数和 interval 组成 batch；
- 调用 Destination；
- Destination 错误时返回并使 Agent 停止。

可靠架构中，这些职责要拆开：

- 每个 Pipeline 的 BatchBuilder 负责组成单 Pipeline batch；
- Spool 负责 batch + checkpoint 的 durable commit；
- Dispatcher 负责消费 durable batch 和策略状态机；
- Transport 只执行一次 HTTP request。

### 2.2 `GlineDest`

`internal/agent/destination/gline.go` 已经做到：

- 使用 request context；
- 设置 Content-Type 与 Bearer token；
- 配置 10 秒 client timeout；
- 关闭 response body。

但目标实现还需要：

- 使用 spool 保存的完整 payload，不再每次 marshal entries；
- 调用版本化 `/api/v1/batches`；
- 解析 accepted/duplicate response；
- 校验响应 `batch_id` 与 entry count；
- 限量读取错误 body；
- 返回稳定、可分类、可 `errors.Is/As` 的结果；
- 处理 429 `Retry-After`；
- 不自动跨 host 跟随 redirect 并携带 Authorization；
- 区分 timeout、用户取消与永久协议错误。

## 3. 前置知识

建议掌握：

- HTTP 请求的“不确定结果”：客户端没收到响应不等于 Server 没提交；
- 指数退避、jitter、thundering herd；
- `Retry-After` 的秒数和 HTTP-date 两种格式；
- Go `http.Client`、`http.Transport`、request context 的 timeout 层次；
- response body 关闭、有限 drain 与连接复用；
- [上传协议错误映射](../04-domain-api-and-storage.md#33-错误响应)；
- [HTTP 重试分类](../05-reliability-security-observability.md#5-http-重试分类)；
- [至少一次传输 ADR](../adr/0003-at-least-once-idempotency.md)。

## 4. 关键定义

### 4.1 Transport

Transport 执行**恰好一次尝试**：构造 request、发送、有限读取响应、解析并返回分类结果。

Transport 不负责：

- sleep；
- 无限 retry；
- 从 spool 删除 batch；
- 生成新 batch ID；
- 修改 payload；
- 决定 quarantine 或 readiness。

### 4.2 Dispatcher

Dispatcher 是 durable queue 的策略消费者：

```text
Next pending batch
  -> Transport.Send once
  -> accepted/duplicate: Ack local batch
  -> temporary/rate-limited: wait and retry same batch
  -> auth: pause, retain same batch
  -> permanent: quarantine atomically
  -> conflict/internal invariant: fail loudly, retain or quarantine by explicit policy
```

### 4.3 背压

背压不是“发生错误后 sleep”本身，而是下游容量约束能沿依赖链传播到上游：

```text
Server 慢/不可用
  -> Dispatcher 无法 ACK
  -> pending spool 增长
  -> spool 达到 max_bytes
  -> Pipeline Commit 阻塞
  -> Pipeline 不再 NextRecord
  -> Source 停止继续读文件
```

只要每层内存都有限，这条链可以让 Agent 长期故障时保持资源有界。

## 5. 必须保持的不变量

| 编号 | 不变量 |
| --- | --- |
| D1 | Dispatcher 只读取 spool 中已提交的 batch |
| D2 | 同一 batch 每次 attempt 使用相同 ID、entry 顺序、payload bytes 和 hash |
| D3 | Server 只有在 PostgreSQL 事务提交后才返回 accepted/duplicate ACK |
| D4 | 只有 accepted/duplicate 且响应身份匹配时才能调用本地 Ack |
| D5 | timeout、网络错误、5xx 和 429 不删除、不重建 batch |
| D6 | 401/403 保留 batch 并暂停，不能热循环攻击 Server |
| D7 | permanent batch 不热重试、不静默删除；进入可见 quarantine |
| D8 | idempotency conflict 不能通过换新 ID 绕过 |
| D9 | 所有 retry wait 都受 context 控制并有上限 |
| D10 | spool 满时默认 block，上游不能绕过 durable path |
| D11 | shutdown 取消 in-flight request 后 batch 仍在 spool |
| D12 | Transport 读取的响应体、重定向和并发均有界 |

## 6. 推荐结果与错误类型

不要只返回一个字符串 error 再在 Dispatcher 中匹配文本。可以定义：

```go
type Outcome string

const (
	OutcomeAccepted    Outcome = "accepted"
	OutcomeDuplicate   Outcome = "duplicate"
	OutcomeTemporary   Outcome = "temporary"
	OutcomeRateLimited Outcome = "rate_limited"
	OutcomeAuth         Outcome = "auth"
	OutcomePermanent    Outcome = "permanent"
	OutcomeProtocol     Outcome = "protocol"
	OutcomeConflict     Outcome = "conflict"
)

type SendResult struct {
	Outcome         Outcome
	BatchID         spool.BatchID
	AcceptedEntries int
	HTTPStatus      int
	ServerCode      string
	RequestID       string
	RetryAfter      time.Duration
}

type SendError struct {
	Outcome    Outcome
	StatusCode int
	Code       string
	RequestID  string
	RetryAfter time.Duration
	Err        error
}

func (e *SendError) Error() string { /* 脱敏摘要 */ }
func (e *SendError) Unwrap() error { return e.Err }
```

也可以让非成功情况全部使用 `SendError`，成功才返回 `SendResult`。关键是只有一个明确分类来源，Dispatcher 不再重复解释 status code。

不要把 Server response message 原样放进 Agent 错误或 metrics label。message 可能高基数，也可能含不适合记录的内部内容；保留稳定 `code`、status 和 request ID 即可。

## 7. HTTP 状态与动作矩阵

| 条件 | Outcome | Dispatcher 动作 |
| --- | --- | --- |
| 200 + accepted + 身份匹配 | accepted | `Ack` |
| 200 + duplicate + 身份匹配 | duplicate | `Ack` |
| 200 但 JSON/身份/entry count 不合法 | protocol | 保留队首并进入 failed/paused；不能 Ack |
| 400 validation/invalid JSON | permanent | 原子 quarantine，继续下一批或按策略停 |
| 401/403 | auth | 暂停；等待凭证变化或低频探测 |
| 409 idempotency conflict | conflict | 原子移入专用隔离区并进入 failed；绝不换 ID 或自动继续 |
| 413 batch too large | permanent | quarantine；修正未来 batch limit |
| 429 | rate-limited | 至少等待 `Retry-After`，再重试同一 batch |
| 500/502/503/504 | temporary | full-jitter backoff |
| 其他 5xx | temporary | 默认同上并记录 code |
| redirect，或未定义的其他 3xx/4xx | protocol | 保留队首并进入 failed/paused；默认不跟随 redirect |
| DNS/connect/TLS/EOF | temporary | backoff，保留 batch |
| request deadline exceeded | temporary | 结果未知，同 batch 重试 |
| Dispatcher context canceled | canceled | 立即退出，不计失败、不 quarantine |

### 7.1 200 响应仍需验证

成功状态至少检查：

- JSON 只能有已知字段或按协议兼容规则解析；
- `batch_id` 等于请求 batch；
- `status` 只能是 accepted/duplicate；
- `accepted_entries` 与本地 entry count 一致；
- body 大小在上限内，且没有意外 trailing JSON。

一个反向代理返回 HTML 200 不能删除本地 batch。

### 7.2 409 的处理

409 表示 Server 已有相同 `(project_id, batch_id)`，但 payload hash 不同。可能原因包括：

- Agent 在重试前改变 payload；
- spool 损坏；
- batch ID 生成器碰撞或被错误复用；
- Server/Agent hash 规范不一致。

这不是普通脏数据。默认建议把它原子移入专门的 conflict quarantine，随后让 Dispatcher 进入 failed，高等级告警并保留现场。这样既符合“永久失败不能热重试”的隔离合同，也不会自动继续大量发送，直到确认不是系统性损坏。

### 7.3 413 为什么不能原地拆批

durable batch 的 ID 与 payload 已冻结。把它拆成两个新 batch 会改变 checkpoint、entry identity 和 Server 幂等语义。第一版应在本地成批前用与 Server 一致的 byte limit 防止 413；如果仍发生，隔离并修正配置/协议。未来若设计 re-batch，必须有显式 supersession 记录，不能在重试分支临时切 slice。

## 8. Transport 接口与一次尝试

```go
type Transport interface {
	Send(ctx context.Context, batch spool.StoredBatch) (SendResult, error)
}

type HTTPTransport struct {
	endpoint    *url.URL
	client      *http.Client
	credentials CredentialProvider
	maxBodyBytes int64
}
```

Endpoint 应在构造时解析和校验：scheme、host、固定 path；不要每次请求拼字符串。生产配置要求 HTTPS，允许 localhost 开发例外时要写清。

### 8.1 Request 构造

```go
req, err := http.NewRequestWithContext(
	ctx,
	http.MethodPost,
	t.endpoint.String(),
	bytes.NewReader(batch.Payload),
)
if err != nil { /* wrap */ }

req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept", "application/json")
req.Header.Set("Authorization", "Bearer "+token)
req.Header.Set("X-Request-ID", requestID)
```

不要把 token 存进 spool payload。Credential 在每次 attempt 构造 header 时读取，因此更换 key 后可以重试同一业务 payload。

`X-Request-ID` 可以每次 attempt 不同，用于追踪网络尝试；`batch_id` 才是业务幂等身份。不要把 request ID 混入 payload hash。

### 8.2 Client timeout 层次

建议同时有：

- dial timeout；
- TLS handshake timeout；
- response header timeout；
- idle connection timeout；
- 整次 attempt 的 context deadline。

`http.Client.Timeout` 可以作为总兜底，但不要叠出互相矛盾的多个极短 deadline。Transport 接收的 context 由 Dispatcher 为每次 attempt 派生：

```go
attemptCtx, cancel := context.WithTimeout(ctx, cfg.AttemptTimeout)
defer cancel()
```

Server 的写入超时应与客户端 attempt timeout 协调，但客户端 timeout 仍可能发生在 DB commit 之后，所以幂等不可省略。

### 8.3 Redirect

默认禁止自动 redirect：

```go
client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}
```

否则错误配置或恶意 redirect 可能把 Authorization 发往其他 host。若未来允许同源 redirect，要显式校验 scheme/host 并限制次数。

### 8.4 有限读取与关闭

无论 status 如何都 `defer resp.Body.Close()`。成功/error body 用 `io.LimitReader(max+1)` 读取并检测超限。

为了连接复用，可以在小响应范围内读完；若 body 超上限，不要无限 drain，关闭连接即可。不要使用无界 `io.ReadAll(resp.Body)`。

## 9. Dispatcher 单批状态机

第一版使用单 Dispatcher、严格队首处理：

```text
waiting_batch
  -> sending
      -> accepted/duplicate -> acking -> waiting_batch
      -> temporary          -> backing_off -> sending
      -> rate_limited       -> backing_off -> sending
      -> auth               -> auth_paused -> sending
      -> permanent          -> quarantining -> waiting_batch
      -> protocol           -> failed（batch 仍在 pending）
      -> conflict           -> conflict_quarantine -> failed
      -> canceled           -> stopped
```

单队首的优点：

- 不需要 lease、in-flight 恢复或乱序 ACK；
- 同 Pipeline 顺序自然保持；
- 更容易证明重启语义。

代价是 head-of-line blocking：一个临时失败 batch 会阻止后续 batch。对于小型日志系统是合理起点。并发发送只有在实测吞吐证明单 Dispatcher 是瓶颈后再引入。

## 10. Dispatcher 循环骨架

```go
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		batch, err := d.queue.Next(ctx)
		if err != nil {
			return classifyQueueExit(ctx, err)
		}

		if err := d.dispatchOne(ctx, batch); err != nil {
			return err
		}
	}
}
```

`dispatchOne` 才包含重试状态：

```go
func (d *Dispatcher) dispatchOne(ctx context.Context, batch spool.StoredBatch) error {
	for attempt := 0; ; attempt++ {
		result, err := d.transport.Send(ctx, batch)
		outcome := classify(result, err)

		switch outcome.Kind {
		case OutcomeAccepted, OutcomeDuplicate:
			return d.ack(ctx, batch, result)
		case OutcomeTemporary:
			if err := d.wait(ctx, d.backoff.Delay(attempt)); err != nil {
				return err
			}
		case OutcomeRateLimited:
			delay := maxDuration(outcome.RetryAfter, d.backoff.Delay(attempt))
			if err := d.wait(ctx, delay); err != nil {
				return err
			}
		case OutcomeAuth:
			if err := d.waitForCredentialChangeOrProbe(ctx); err != nil {
				return err
			}
		case OutcomePermanent:
			return d.quarantine(ctx, batch, outcome)
		case OutcomeProtocol:
			return fmt.Errorf("batch %s received an invalid protocol response: %w", batch.BatchID, ErrProtocol)
		case OutcomeConflict:
			return d.quarantineConflictAndFail(ctx, batch, outcome)
		default:
			return fmt.Errorf("unknown dispatch outcome: %w", ErrInvariant)
		}
	}
}
```

最终代码需要注意 attempt 溢出、metrics、状态更新、错误脱敏和 ACK cleanup context。这个状态机值得完整给出；简单的 constructor 不值得。

## 11. 指数退避与 Full Jitter

### 11.1 算法

经典 full jitter：

```text
cap(attempt) = min(maxDelay, baseDelay * 2^attempt)
delay        = random duration in [0, cap(attempt)]
```

实现时不要直接移位到溢出：

```go
func exponentialCap(base, max time.Duration, attempt uint) time.Duration {
	if base <= 0 || max <= 0 {
		return 0
	}
	cap := base
	if cap > max {
		cap = max
	}
	for i := uint(0); i < attempt; i++ {
		if cap >= max || cap > max/2 {
			return max
		}
		cap *= 2
	}
	if cap > max { // 防御未来修改破坏上面的溢出检查
		return max
	}
	return cap
}
```

随机源通过极小接口注入，生产使用并发安全 RNG，测试使用固定序列：

```go
type JitterSource interface {
	Duration(max time.Duration) time.Duration
}
```

不要在测试中断言某次随机 delay 恰好等于 731ms；断言它在范围内、cap 正确、相同 batch 重试、等待可取消。

### 11.2 重置 attempt

以下时机重置：

- 当前 batch ACK，切换下一 batch；
- permanent batch quarantine 后切换下一 batch。

不要因为一次连接建立成功但收到 503 就重置。是否在进程重启后保存 attempt 不是正确性要求；MVP 可重新从 base delay 开始，但要避免 auth 重启导致热循环。

### 11.3 Retry-After

解析：

- 整数秒；
- HTTP-date 相对当前时间；
- 负值视为 0；
- 非法值回退普通 backoff；
- 超出协议允许的最大未来时间时进入可见的 rate-limit paused/failed 状态并保留 batch，不能截短后提前请求。

429 的 `Retry-After` 表示至少等待多久。建议使用：

```text
delay = max(retryAfter, jitterBackoff)
```

`Retry-After` 解析器先保证结果不溢出且处于配置允许的范围，因此这里不再 `clamp`。不要为了 jitter 或本地 backoff 上限让实际等待短于 Server 明确要求。若希望长等待也能周期性刷新状态，可以把它拆成多个有上限、可取消的 timer step，但在目标时间前不得重新发请求。

### 11.4 可取消等待

```go
func wait(ctx context.Context, clock Clock, delay time.Duration) error {
	timer := clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}
```

使用 fake clock 或 `testing/synctest`，不要让单元测试真实 sleep 数秒。

## 12. Auth 暂停与凭证轮换

401/403 与 5xx 不同：持续快速重试既无用，也可能形成自我攻击。

推荐定义：

```go
type Credential struct {
	Token   string
	Version uint64
}

type CredentialProvider interface {
	Current(ctx context.Context) (Credential, error)
	Changed() <-chan struct{}
}
```

Dispatcher 收到 auth outcome 后：

1. batch 留在队首；
2. 状态变为 `auth_paused`；
3. readiness 失败，liveness 继续；
4. 等待 credential version 改变，或等待一个低频 probe interval；
5. 变化后使用新 header 重试同一 payload；
6. 不把旧/新 token 打进日志。

如果 MVP 不支持热加载，至少使用分钟级而不是毫秒级探测，并提示用户重启 Agent；重启后 spool 仍保留 batch。不要把“进程需要重启”伪装成自动恢复已完成。

## 13. Permanent 与 Quarantine

### 13.1 可隔离并继续的错误

例如单个 batch 的 schema validation、字段超限或 413。动作：

1. 将 pending batch 原子移动至 quarantine；
2. 保存稳定 code、status、request ID、attempt count；
3. 更新 lost/isolated 风险指标；
4. 继续下一 batch；
5. readiness 是否失败由策略决定，但必须可见。

### 13.2 不应自动继续的错误

idempotency conflict、payload hash mismatch、spool decode corruption 表示系统不变量可能破坏。默认停止 Dispatcher，保留现场并让 Agent 返回/进入 failed。

不要提供“忽略 conflict 并换 ID”开关。那会把一个可诊断缺陷变成重复数据。

### 13.3 Poison batch 与队首阻塞

如果 permanent batch 不隔离，严格队首会永远阻塞。quarantine 的价值是显式承认该 batch 无法按原 payload 送达，并让后续健康数据继续。它不是成功 ACK，演示/指标中要把隔离数作为数据质量事件。

## 14. ACK 后本地处理

### 14.1 响应先验证

Transport 返回 accepted/duplicate 后，Dispatcher再次检查结果与 `StoredBatch` 一致，再调用：

```go
queue.Ack(ctx, batch.QueueSequence, batch.BatchID)
```

### 14.2 外部 context 已取消

可能出现：HTTP ACK 刚解析完成，用户同时取消。可以为本地 Ack 创建非常短的 cleanup context：

```go
ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.LocalAckTimeout)
defer cancel()
err := queue.Ack(ackCtx, batch.QueueSequence, batch.BatchID)
```

Ack 超时或失败时不要把 Server 请求视为失败并改变 batch。保留本地数据，重启后原样发送，Server 返回 duplicate。这是安全但可能多一次网络请求的选择。

`WithoutCancel` 只能用于这个有界本地收尾，不能让整个 Dispatcher 忽略 shutdown。

### 14.3 ACK 删除错误

Spool I/O/corruption 是全局错误。不要在内存里标记“已经 ack”后跳过磁盘；下次 `Next` 必须仍从 durable state 决定。

## 15. 端到端资源边界

可靠 Agent 的内存上限可以近似拆解：

```text
每个 Pipeline：
  <= 1 个 pending batch byte limit
  + 1 个 max_line_bytes
  + bufio reader 固定 buffer

Dispatcher：
  <= 1 个 in-flight stored payload
  + 有限 response body

Spool：
  <= max logical bytes（默认 full 时 block）
  + 数据库页/文件开销（另监控）
```

若仍保留大容量共享 `entries chan`，需要把它也计入上限。目标设计可以删除这层，让每个 Pipeline 直接 batch + commit，背压路径更清晰。

### 15.1 Spool 水位

| 状态 | 用户体验 |
| --- | --- |
| 正常 | Pipeline 持续 commit，Dispatcher 逐步 ACK |
| 高水位 | warning/metric，仍采集；提示 Server/网络变慢 |
| 满载 | 新 commit block，Pipeline backpressured，readiness 失败 |
| ACK 恢复 | capacity notification 唤醒等待者，自动继续 |
| 磁盘错误 | 停止可靠采集，Agent failed，不转内存模式 |

### 15.2 为什么默认 block

默认 block 使“数据没有被静默丢弃”成为可解释行为。它会让 Agent 落后于文件增长，最终还受业务日志保留/轮转速度限制，因此不是无限保证；指标必须暴露 source lag 和 oldest pending age，让用户在旧文件被删除前处理容量问题。

## 16. Shutdown 协议

关闭由 Agent 顶层协调，Dispatcher 自身遵守：

1. 外部 signal 使 Pipeline 停止读取新记录；
2. Pipeline 在本地 deadline 内 commit 已形成的完整 batch；
3. 不再产生新 batch 后，给 Dispatcher 一个有界 drain 窗口；
4. drain 窗口内继续发送队首；
5. deadline 到达时取消 in-flight HTTP 与 retry timer；
6. 未 ACK batch 保留在 spool；
7. Dispatcher 退出后再关闭 spool。

不要要求 shutdown 一定清空全部 spool。Server 若离线数小时，清空会让退出永远无法完成。正确目标是：已进入 spool 的 batch 可恢复，未提交的完整内存 batch尽量在本地 deadline 内 commit。

Dispatcher 收到 context canceled 时：

- 不增加 permanent failure；
- 不 quarantine 当前 batch；
- 不删除 batch；
- 停止 timer；
- 关闭当前 response body（request context 会取消）；
- 返回可由顶层识别的 cancellation。

## 17. 指标与状态

建议至少暴露：

```text
gline_agent_spool_batches
gline_agent_spool_logical_bytes
gline_agent_spool_oldest_seconds
gline_agent_batches_acked_total{result="accepted|duplicate"}
gline_agent_batches_quarantined_total{reason="validation|too_large|..."}
gline_agent_upload_attempts_total{outcome="..."}
gline_agent_upload_duration_seconds
gline_agent_retry_delay_seconds
gline_agent_dispatcher_state{state="running|backoff|auth_paused|failed"}
gline_agent_pipeline_backpressured{pipeline_id="..."}
```

谨慎 label 基数：

- 可以用固定 outcome/code；
- Pipeline ID 数量由配置有界，可接受但仍需审查；
- 不使用 batch ID、request ID、URL、错误全文或日志 message 作为 label；
- batch/request ID 只进入采样或结构化日志字段。

结构化日志应记录状态转换，而不是每次 retry 都刷屏。可以在第 1 次、退避级别变化、状态恢复时记录，并用 counter 保留完整次数。

## 18. 是否需要 Circuit Breaker

第一版通常不需要额外 circuit breaker。单 Dispatcher + 指数退避已经限制请求频率；auth 有独立暂停。再加入 breaker 会增加 open/half-open 状态、恢复探测和配置复杂度。

只有出现以下证据再考虑：

- 多 Dispatcher/多 endpoint 导致失败请求仍很多；
- 下游明确要求客户端熔断；
- retry 状态无法表达服务级暂停；
- 压测显示失败流量影响 Agent 本地工作。

不要为了简历关键词加入没有解决实际问题的组件。

## 19. 分小步实现顺序

### 步骤 1：先固化当前 Agent -> Server 合同

在引入重试前，用 `httptest.Server` 让真实 HTTP client 发送一个 batch，Server recording sink/repository 验证：

- path、method、Content-Type；
- Authorization 存在但测试不打印 secret；
- DTO 解码正确；
- 非 200 能返回分类基础信息。

这也是[开发路线图 GL-003](../06-development-roadmap.md#gl-003-固化-v1-最小上传合同)的前置工作。

### 步骤 2：让 Transport 发送 immutable payload

把 API 从 `SendEntries([]LogEntry)` 改为 `Send(StoredBatch)`。去掉每次 attempt 的 payload 重建和时间更新。

先只实现 accepted/duplicate；任何不完整 200 都不 Ack。

### 步骤 3：集中实现 HTTP 分类

用 table-driven tests 覆盖所有稳定 status/code。Dispatcher 只认 Outcome，不认 HTTP 细节。

### 步骤 4：从真实 Spool 读取、成功后 Ack

实现无 retry 的最小 Dispatcher：Next -> Send -> Ack。故意让 Agent 在 ACK 前后退出，验证 batch 是否正确保留/删除。

### 步骤 5：加入 temporary retry 与 fake clock

实现 full jitter、cap、可取消 timer。Transport 连续返回 temporary N 次再 accepted，断言始终是相同 payload 和 ID。

### 步骤 6：加入 429 Retry-After

覆盖 delta-seconds、HTTP-date、invalid、past date 和过大值。断言实际 delay 不早于合法 server 下限。

### 步骤 7：加入 auth pause

先支持低频探测，再视配置系统增加 credential change signal。验证长时间 401 不产生热循环且 spool 不删除。

### 步骤 8：加入 permanent quarantine 与 conflict fail

让普通 poison batch 不阻塞后续；让 409 停止并保留高质量诊断证据。

### 步骤 9：接入背压状态与 metrics

用很小 `max_bytes` 让 Server 下线：spool 填满 -> Pipeline block；恢复 Server -> ACK -> Pipeline 自动继续。

### 步骤 10：接入完整 shutdown

覆盖 retry wait、auth pause、in-flight request、Ack cleanup、空队列等待五种取消位置。

## 20. 错误、取消与资源关闭

### 20.1 错误包装

Transport 错误示例：

```go
return SendResult{}, &SendError{
	Outcome: OutcomeTemporary,
	Err:     fmt.Errorf("execute ingest request: %w", err),
}
```

Dispatcher 记录 batch ID 与 attempt，但不再重复输出 response message。使用 `errors.Is(err, context.Canceled)` 区分用户取消；request attempt deadline exceeded 且顶层 context 未取消时仍是 temporary。

### 20.2 Response body

所有分支都关闭。若 JSON decode 提前失败，仍只在上限内处理剩余数据；不要为连接复用读取攻击者提供的无限 body。

### 20.3 HTTP Transport 关闭

Agent 停止时在 in-flight requests 结束后调用 `CloseIdleConnections()`。它不取消正在进行的 request，取消由 context 负责。

### 20.4 Timer

每个 timer 都由创建它的 Dispatcher 停止。Reset 前正确 drain 取决于 clock/timer API；可以封装一个只在状态机中使用的 timer adapter，减少散落错误。

## 21. 哪些代码值得完整给出

在实现教学和评审中，优先完整展示：

- HTTP `Send` 的 request/response 全部收尾路径；
- status + server code 分类函数；
- `Retry-After` 解析；
- overflow-safe full-jitter backoff；
- `dispatchOne` 状态机；
- auth pause 等待；
- accepted/duplicate 后的本地 Ack；
- permanent quarantine 与 conflict fail；
- shutdown 中 drain/cancel 的边界。

简单 DTO、metric 注册和 constructor 可以只给结构定义与关键字段。

## 22. 测试设计

### 22.1 Transport 单元/HTTP 集成测试

使用 `httptest.Server`，不要 mock `http.Client.Do` 到完全看不到协议：

| 场景 | 断言 |
| --- | --- |
| accepted | batch ID/entry count 匹配，OutcomeAccepted |
| duplicate | OutcomeDuplicate，同样允许 Ack |
| HTML 200 | 解析失败，不 Ack |
| 错 batch ID | protocol failure，不 Ack |
| 400 stable code | permanent |
| 401/403 | auth |
| 409 | conflict |
| 413 | permanent |
| 429 seconds/date | 正确 RetryAfter |
| 503 | temporary |
| delayed response beyond timeout | temporary，结果未知 |
| oversized body | 有界失败，无巨量分配 |
| redirect to second server | 不携带 token 跟随 |

验证请求 body bytes 与 spool 中 `Payload` 完全相同，而不只反序列化后“语义相同”。

### 22.2 Dispatcher 确定性测试

使用 fake queue、scripted Transport、fake clock/random：

- temporary、temporary、accepted：三次相同 batch，最后 Ack 一次；
- duplicate：Ack；
- cancel during backoff：及时返回，不 Ack；
- auth 连续返回：只按 credential change/低频 probe 重试；
- permanent：Quarantine 一次，继续下一批；
- protocol failure：batch 留在 pending，不 Ack，Dispatcher failed/paused；
- conflict：原子进入专用 quarantine，不换 ID、不 Ack，Dispatcher failed；
- Ack 失败：返回 spool 错误，batch durable state 保留；
- queue empty + cancel：无 goroutine 泄漏。

不要测试内部 helper 恰好调用几次，除非次数本身就是外部合同（例如 Transport attempt 和 Ack）。

### 22.3 真实 Spool + HTTP 集成

1. commit batch + checkpoint；
2. Server 第一次写入后故意断开连接/不返回完整 ACK；
3. Dispatcher timeout 后重发相同 bytes；
4. Server 返回 duplicate；
5. Dispatcher Ack；
6. 重开 spool 为空；
7. Server entries 只有一份。

模拟“DB commit 后断响应”应在 Server repository commit 之后注入，不能只让 handler 在写入前返回 500。

### 22.4 背压端到端

使用小容量 spool 和连续序号发生器：

1. Server 正常，确认 steady state；
2. 停 Server；
3. 持续追加日志；
4. 观察 spool 到高水位和上限；
5. 证明 Pipeline commit 阻塞、Agent 内存保持有界；
6. 恢复 Server；
7. spool 逐步归零，Pipeline 自动继续；
8. 查询 Server，按 `(run_id, sequence)` 做集合差异；
9. 区分文件仍保留的可靠范围与轮转删除造成的外部边界。

### 22.5 Race 与泄漏

在 retry、auth pause、queue wait、in-flight HTTP 各状态反复启动/取消。执行 race test；确认测试 server 连接关闭、spool 可重开、临时目录可删除。

## 23. 验收命令与证据

```powershell
gofmt -w .\internal\agent\dispatcher .\internal\agent\transport
go test ./internal/agent/dispatcher ./internal/agent/transport -count=1
go test -race ./internal/agent/dispatcher ./internal/agent/transport -count=1
go test ./internal/agent/... -count=1
go test -race ./internal/agent/... -count=1
go vet ./internal/agent/... ./cmd/agent
go build ./cmd/agent
```

完成本章需要的证据：

- 真实 HTTP accepted/duplicate 合同测试；
- status/code 分类表测试；
- 同 batch 多次 attempt 的原始 body 完全相同；
- `Retry-After` 与 full-jitter 的确定性测试；
- 401/403 无热循环；
- DB commit 后 ACK 丢失仍只有一份 entries；
- Server 离线时 spool 满后 Pipeline block、内存有界；
- Server 恢复后 backlog 自动清空；
- shutdown 在所有等待状态下于 deadline 内退出；
- 日志和测试失败输出不包含 token、Authorization 或完整 payload。

## 24. 常见错误

1. **Transport 内部无限 retry。** Dispatcher 失去取消、指标和 quarantine 控制。
2. **每次 retry 重新生成 batch ID。** ACK 丢失后产生重复写入。
3. **每次 retry 更新 payload 时间。** 相同 ID 触发 hash conflict。
4. **timeout 当作 Server 未处理。** DB 可能已经 commit。
5. **收到任意 200 就删除。** 代理 HTML、截断 JSON 或错误 batch ID 都可能被误 ACK。
6. **401/403 按普通 5xx 高频重试。** 无助恢复并持续攻击下游。
7. **429 jitter 后早于 Retry-After。** 违反 Server 的最小等待要求。
8. **413 时临时拆 slice 换新 ID。** 破坏 durable batch 和 checkpoint 合同。
9. **409 时换 ID 绕过。** 隐藏 payload 可变/损坏并制造重复。
10. **永久错误直接删除。** 数据消失且无法解释。
11. **response body 无界 ReadAll。** 错误下游可让 Agent OOM。
12. **自动跨 host redirect Bearer token。** 凭证泄漏风险。
13. **取消时 quarantine in-flight batch。** 用户 shutdown 不是数据永久无效。
14. **shutdown 等待清空整个 spool。** Server 离线时进程永远退不出。
15. **spool 满时改为内存直传。** 故障时保证发生隐式变化。
16. **metrics label 放 batch/request ID。** 产生无界时序基数。
17. **过早增加并发 Dispatcher/circuit breaker。** 在没有吞吐证据前引入 lease、乱序 ACK 与更多状态。

## 25. 复盘题

1. Transport 与 Dispatcher 各自负责什么？为什么必须拆开？
2. 为什么 request timeout 属于“结果未知”而不是“Server 未写入”？
3. accepted 与 duplicate 为什么都允许 Ack？
4. 200 response 还需要验证哪些字段？
5. `batch_id` 与每次 attempt 的 request ID 有何区别？
6. full jitter 如何避免大量 Agent 同时重试？
7. 429 的实际等待为何不能短于合法 Retry-After？
8. auth 错误为什么暂停而不是 quarantine？
9. 409 为什么比普通 400 更严重？
10. ACK 后本地删除失败，重启后会怎样恢复？
11. spool 满如何逐层传播到 FileSource？
12. 为什么默认 block 仍不能保证“文件永远不会因外部轮转删除而丢数据”？
13. shutdown 为什么不要求清空全部 spool？
14. 什么证据出现后才值得增加并发 Dispatcher？

## 26. 本专题完成条件

- [ ] Pipeline 只写 spool，Dispatcher 只读 spool；
- [ ] Transport 每次调用只执行一次 HTTP attempt；
- [ ] durable payload 在所有 retry 中 byte-for-byte 相同；
- [ ] accepted/duplicate 只有身份与计数验证通过后才 Ack；
- [ ] timeout/5xx/429 使用同一 batch 可取消退避；
- [ ] Retry-After 两种格式正确解析并有上限；
- [ ] 401/403 进入 auth pause，无热循环且不删除 batch；
- [ ] permanent batch 原子 quarantine；
- [ ] protocol response 无效时保留 batch 且不 Ack；
- [ ] conflict 原子隔离、不换 ID，并停止自动继续；
- [ ] 默认 spool 满时 block 并沿链路背压；
- [ ] in-flight request、retry timer、auth wait、empty queue wait 都能在 shutdown deadline 内退出；
- [ ] Server 确认严格发生在 PostgreSQL 事务提交之后；
- [ ] ACK 丢失端到端测试证明 Server 最终只有一份数据；
- [ ] 日志、错误和 metrics 均不暴露 token 或制造高基数。

完成后，回到[教程目录](./README.md)继续 Server 持久化、查询、可观测性和最终交付章节。Agent 专题的最终验收不只是“能上传”，而是能用故障证据解释每一个状态转换和数据边界。
