# 04. Agent 运行时：并发、所有权与生命周期

> 本章是实现教学，不是现状声明。文中标为“目标”的类型和包尚不代表当前工作树已经存在。当前代码事实以[现状评估](../01-current-state-assessment.md)为准，最终架构合同以[目标架构](../03-target-architecture.md)和[至少一次传输 ADR](../adr/0003-at-least-once-idempotency.md)为准。

## 1. 本章目标

完成本章后，你应该能够：

1. 画出 Agent 中每个长期运行组件的所有权关系；
2. 解释“读取成功”“进入 channel”“进入 spool”“Server ACK”分别意味着什么；
3. 把现有 `SourcePipeline -> entries channel -> Sender` 演进为可靠的 `Pipeline -> Spool -> Dispatcher`；
4. 明确哪个错误只停止一个 Pipeline，哪个错误必须停止整个 Agent；
5. 写出可取消、可关闭、不会遗留 goroutine 或文件句柄的运行时骨架；
6. 用行为测试验证生命周期，而不是绑定内部 goroutine 的精确调度顺序。

本章不实现磁盘格式、文件轮转或 HTTP 退避算法。它只先确定这些模块将来可以正确接入的运行时边界。

## 2. 当前代码定位

阅读以下文件，并在纸上画出调用方向：

| 文件 | 当前职责 | 值得保留的部分 | 目标阶段的问题 |
| --- | --- | --- | --- |
| `internal/agent/agent.go` | 启动 Pipelines 与共享 Sender | Pipeline 故障隔离、Sender 故障取消全局、等待生产者后关闭 channel | Sender 直接消费内存 entry，尚无可靠持久化边界 |
| `internal/agent/sourcePipeline.go` | Source 读取、Parser 解析、补充来源字段 | 临时/致命 Source 错误分类；解析失败保留原始消息 | `RawRecord` 不携带 checkpoint；发送到 channel 不代表安全落盘 |
| `internal/agent/source/file.go` | 从文件末尾轮询完整行 | 等待可取消；半行暂存在内存 | 打开即跳末尾、无身份/checkpoint/轮转；`time.After` 每轮分配 timer |
| `internal/agent/sender/tickOrBatch.go` | 按大小或时间聚合并调用 Destination | 批量与定时 flush 的基本行为 | 网络错误直接终止；取消时内存 batch 不落盘；batch 无稳定 ID |
| `internal/agent/destination/gline.go` | JSON + HTTP 上传 | context、超时、关闭 response body | 只接受 200、错误不可分类、协议没有稳定 batch 身份 |
| `internal/agent/build/*.go` | 从配置构造运行时 | 构造逻辑与运行逻辑初步分离 | 构造失败时资源回收不完整，尚无 spool/runtime 配置 |

现有测试已经保护了几个有价值的合同：

- 一个 Pipeline 的致命错误或 panic 不应停止其他 Pipeline；
- 临时 Source 错误等待后继续；
- Parser 失败产生 `LevelUnknown` 并继续；
- Sender 致命错误会停止 Agent；
- 正常关闭生产端后，Sender 会 flush 剩余内存 batch。

不要删除这些合同。重构时应把它们迁移到新结构，只调整已经不再成立的“Sender 直接接收 entry”实现细节。

## 3. 前置知识

建议先掌握以下 Go 概念：

- `context.Context` 的取消传播与 `context.WithCancelCause`；
- channel 的所有者应负责关闭 channel；
- `sync.WaitGroup` 或 `errgroup` 的生命周期协调；
- `errors.Is`、`errors.As` 和带语义的错误类型；
- 接口由使用方定义，而不是为每个实现预先造大接口；
- `defer` 的执行顺序和构造失败时显式回滚资源；
- `os.File.Close`、HTTP response body、ticker/timer 都属于必须释放的资源。

同时先阅读：

- [目标架构的 Agent 部分](../03-target-architecture.md#3-agent-内部架构)；
- [可靠性状态机](../05-reliability-security-observability.md#2-数据状态机)；
- [开发路线图阶段 2](../06-development-roadmap.md#4-阶段-2agent-可靠传输)。

## 4. 先建立正确的心智模型

### 4.1 四个不同的“成功”

一条日志在 Agent 中会跨过四个边界：

| 边界 | 能证明什么 | 崩溃后能否恢复 |
| --- | --- | --- |
| `Source.NextRecord` 返回 | 当前进程从文件读到了字节 | 不能；除非旧 checkpoint 允许重读 |
| entry 写入内存 channel | 另一个 goroutine 有机会处理 | 不能；进程退出后 channel 消失 |
| batch 与 checkpoint 在 spool 事务提交 | Agent 的本地可靠边界成立 | 能；重启可从 spool 恢复 batch |
| Server 在 PostgreSQL 提交后 ACK | 远端持久化边界成立 | 能；本地 batch 可以删除 |

最常见的错误，是把第二行当成第三行。channel 只协调并发，不是持久化协议。

### 4.2 目标数据流

第一版可靠实现建议让每个 batch 只包含一个 Pipeline 的 entry：

```text
FileSource(pipeline A)
  -> Parser / Enricher
  -> Pipeline-local BatchBuilder
  -> Spool.Commit(batch A + checkpoint A)  [一个本地事务]

FileSource(pipeline B)
  -> Parser / Enricher
  -> Pipeline-local BatchBuilder
  -> Spool.Commit(batch B + checkpoint B)  [一个本地事务]

Spool durable queue
  -> Dispatcher
  -> HTTP Transport
  -> Server database commit
  -> ACK
  -> Spool.Ack(batch)
```

这里有两个重要选择：

1. Pipeline 直接把完整 batch 提交给 spool，而不是先汇入一个共享 entry channel；
2. Dispatcher 只读取已经提交的 batch，不接触 Source、Parser 或文件 offset。

这样，采集进度与发送进度被明确拆开：网络变慢只会让 spool 增长，不会让“哪些字节已经安全保存”变得含糊。

## 5. 关键定义

### 5.1 运行时角色

| 角色 | 唯一职责 | 不应负责 |
| --- | --- | --- |
| `Agent` | 构造后的顶层监督、取消、等待和关闭顺序 | 解析日志、决定 HTTP 重试 |
| `Pipeline` | 串联 Source、Parser、Enricher、BatchBuilder 与本地提交 | 删除远端已确认 batch |
| `Source` | 从一个输入产生带位置的原始记录 | 更新 durable checkpoint |
| `BatchBuilder` | 按条数/字节/时间形成单 Pipeline batch | 网络发送、磁盘事务 |
| `Spool` | 原子保存 batch 与 checkpoint，提供有界持久队列 | 解析 HTTP 状态码 |
| `Dispatcher` | 按顺序读取 durable batch、上传、重试、确认或隔离 | 读取日志文件 |
| `Transport` | 执行一次上传并返回可分类结果 | 自己无限重试 |
| `HealthRegistry` | 汇总状态快照供日志、指标、readiness 使用 | 反向控制业务流程 |

### 5.2 所有权

“拥有”不仅表示持有字段，还表示谁负责关闭和改变状态：

| 资源/状态 | 所有者 | 规则 |
| --- | --- | --- |
| 日志文件 handle | 对应 FileSource | 只由该 Source 读取和关闭 |
| Pipeline 内存 batch | 对应 Pipeline | 不与其他 Pipeline 共享底层 slice |
| durable batch bytes | Spool | commit 后任何调用者都不能原地修改 |
| durable checkpoint | Spool | 消费记录时只随 batch commit 的同一事务推进；无数据 initial/rotate/truncate 只能走受限控制过渡 |
| HTTP request/response | Transport | 每次调用内创建并完整收尾 |
| retry timer | Dispatcher | 每次等待都必须能被 context 取消 |
| 顶层 context | `cmd/agent` 创建，Agent 派生 | 子组件不得自行替换为 `Background()` 逃逸 |
| channel | 创建它并决定“不再发送”的组件 | 只有发送方关闭；接收方不关闭 |

### 5.3 进度的两种含义

不要只使用一个含糊的 `offset`：

- **volatile cursor**：当前进程的文件 handle 已经读到哪里；可能领先于 durable 状态；
- **durable checkpoint**：相关数据已经随 batch 安全进入 spool 后，可以从哪里恢复。未消费新记录的 initial/rotate/truncate anchor 是显式例外，必须经过校验、compare-and-set 和独立 reason，不能保存任意读取位置。

进程崩溃后只信 durable checkpoint。volatile cursor 可以在运行期间领先一个有界 batch；这批数据如果还未 commit，重启后会被重读，而不是丢失。

## 6. 必须保持的不变量

建议把下面内容写进代码附近的 package 文档或 ADR，而不是只留在脑中：

| 编号 | 不变量 |
| --- | --- |
| A1 | durable checkpoint 绝不能指向一个尚未进入 spool 的位置 |
| A2 | 同一 batch 首次 commit 后，`batch_id`、entry 顺序和 payload 必须不可变 |
| A3 | Dispatcher 只发送 spool 中已提交的 batch |
| A4 | 只有 Server 明确返回 `accepted` 或 `duplicate` 后才能删除 batch |
| A5 | HTTP timeout 不是失败确认；它是结果未知，必须用同一 batch 重试 |
| A6 | 一个 Pipeline 的 Source/Parser 致命错误默认只终止该 Pipeline，并使健康状态可见 |
| A7 | spool 无法继续可靠写入时，所有采集必须停止或阻塞，不能退化成内存直传 |
| A8 | Agent 停止前必须先停止生产者，再关闭其依赖的 spool；不能在 Pipeline 仍写入时关闭 store |
| A9 | 所有队列、batch、行、timer 等待和 shutdown 时间都有上限 |
| A10 | 运行时日志和指标可以描述失败，但不能成为正确性所依赖的状态存储 |

第 A1、A2、A4 是整个至少一次语义的核心。如果实现选择与它们冲突，应先改设计，不要用更多重试代码掩盖。

## 7. 推荐类型与接口骨架

以下代码是目标骨架。字段会在后续章节展开，不建议一次性照抄全部实现。

### 7.1 Source 输出必须携带“记录之后的位置”

```go
package source

type Position struct {
	Identity FileIdentity
	Offset   int64 // 读取完本条记录后，下次应从这里继续
}

type RawRecord struct {
	ObservedAt time.Time
	Content    string
	After      Position
	Partial    bool
}

type Source interface {
	NextRecord(ctx context.Context) (RawRecord, error)
	Close() error
}
```

`After` 必须表示完整处理当前记录后的位置，而不是记录起点。这样一批记录的 checkpoint 就是最后一条记录的 `After`。

`Close` 是否进入接口需要结合构造方式决定。当前只有文件 Source，可以直接要求；如果未来有不需要关闭的 Source，也可以由 Builder 返回 `io.Closer`，避免给所有 Source 填一个空实现。

### 7.2 Pipeline 内部候选数据

```go
type PendingEntry struct {
	Entry logentry.LogEntry
	After source.Position
}

type PreparedBatch struct {
	PipelineID     string
	BatchID        string
	Entries        []logentry.LogEntry
	CheckpointAfter source.Position
	CreatedAt      time.Time
}
```

`PreparedBatch` 在 commit 前可以存在于内存；commit 后应视为不可变值。最稳妥的做法是 Spool 编码并复制 payload，不把调用方的 slice 直接留在内部。

### 7.3 最小的 Pipeline 依赖

```go
type BatchCommitter interface {
	Commit(ctx context.Context, batch PreparedBatch) error
}

type Pipeline struct {
	ID        string
	Source    source.Source
	Parser    parser.Parser
	Committer BatchCommitter
	Batcher   *BatchBuilder
	Logger    zerolog.Logger
}
```

接口由 Pipeline 这个使用方定义，只暴露 `Commit`。Pipeline 不需要知道 bbolt bucket、磁盘路径、发送队列游标或重试次数。

### 7.4 Dispatcher 面向 durable queue

```go
type BatchQueue interface {
	Next(ctx context.Context) (StoredBatch, error)
	Ack(ctx context.Context, id BatchID) error
	Quarantine(ctx context.Context, id BatchID, reason string) error
}

type Transport interface {
	Send(ctx context.Context, batch StoredBatch) (SendResult, error)
}
```

`Next` 的具体语义必须在第五章确定：是非破坏性 `Peek`、带 lease 的 `Claim`，还是单 Dispatcher 的顺序读取。第一版只有一个 Dispatcher 时，非破坏性读取 + 成功后 `Ack` 最容易证明。

### 7.5 运行状态快照

不要把一个随处写入的 `bool healthy` 当状态模型。可以定义有限状态：

```go
type ComponentState string

const (
	StateStarting     ComponentState = "starting"
	StateRunning      ComponentState = "running"
	StateBackpressured ComponentState = "backpressured"
	StatePaused       ComponentState = "paused"
	StateFailed       ComponentState = "failed"
	StateStopped      ComponentState = "stopped"
)

type PipelineStatus struct {
	ID           string
	State        ComponentState
	LastError    string
	LastRecordAt time.Time
	UpdatedAt    time.Time
}
```

对外暴露不可变快照。`LastError` 应是脱敏摘要，不保存日志原文或 token。

## 8. 运行时状态机

### 8.1 Agent 状态

```text
constructed
    -> starting
        -> running
            -> stopping -> stopped
            -> failed   -> stopped
        -> failed       -> stopped
```

只有以下情况建议把 Agent 判为全局失败：

- spool 无法打开、事务提交或读取，可靠边界失效；
- Dispatcher 自身出现内部不变量错误；
- 配置或构造错误使运行时无法启动；
- 关键监督 goroutine panic 且无法隔离。

以下情况通常不应立即杀死 Agent：

- 单个日志文件权限永久失败：停止该 Pipeline，其他 Pipeline 继续；
- 单个 Parser panic：捕获、标记该 Pipeline 失败，其他 Pipeline 继续；
- Server 暂时不可用：Dispatcher 重试，Pipeline 直到 spool 满才受到背压；
- API Key 失效：Dispatcher 暂停并使 readiness 失败，batch 留盘。

### 8.2 Pipeline 状态

```text
starting -> running <-> backpressured
                    \-> failed
running/backpressured -> stopping -> stopped
```

“backpressured”不是错误恢复策略，它只是说明 Pipeline 正在等待 spool 容量。不要为绕过阻塞而改成内存直传或丢弃。

### 8.3 错误传播表

| 来源 | 示例 | 局部动作 | 顶层动作 |
| --- | --- | --- | --- |
| Source temporary | 文件暂时不可读 | 可取消退避并重试 | 记录状态，不停 Agent |
| Source fatal | 权限永久失败 | 停该 Pipeline | 其他 Pipeline 继续，readiness 可降级 |
| Parser error | 单行 JSON 无效 | 产生 Unknown entry | 继续 |
| Parser panic | 代码缺陷 | recover + stack，停 Pipeline | 其他 Pipeline 继续 |
| Spool capacity | 达到 `max_bytes` | `Commit` 阻塞 | 显示背压，等待空间 |
| Spool corruption/I/O | 校验失败、磁盘错误 | 不再采集 | 取消全局并返回错误 |
| Transport temporary | timeout、5xx | 同 batch 退避重试 | Agent 继续 |
| Transport auth | 401/403 | 暂停 Dispatcher | 保留 batch，readiness 失败 |
| Transport permanent | batch 无法接受 | 隔离或停住队首 | 按策略显式处理，不静默删除 |

## 9. 推荐实现顺序

每一步都应该保持代码可构建、可测试，不要一次同时重写 Source、spool、协议和 Server。

### 步骤 1：冻结现有生命周期合同

先运行并理解现有测试。补测试只针对稳定合同，例如：

- 构造中途失败会关闭此前已打开的 Source 和日志文件；
- Pipeline 结束后它拥有的 Source 一定关闭；
- Agent 取消后所有长期 goroutine 在超时内退出。

不要测试“恰好启动三个 goroutine”或 helper 调用次数，那是实现细节。

### 步骤 2：让 RawRecord 携带来源位置

先只增加位置数据流：FileSource 产生 `After`，Pipeline 保留它，但仍可用当前 Sender。此时不要声称已经可靠；这一步只是让位置不会在 Parser 后丢失。

验收点：Parser 成功和失败降级时都保留相同的 `After`。

### 步骤 3：把 BatchBuilder 移入每个 Pipeline

当前共享 Sender 汇总多个 Pipeline 的 entry。改为每个 Pipeline 独立聚合：

- batch 不跨 Pipeline；
- 最后一个 entry 的 `After` 成为 `CheckpointAfter`；
- 批次受 entry 数、编码后字节数和 flush interval 三个边界约束；
- interval 用可注入 clock/timer 测试，不依赖真实睡眠。

这会增加一些 HTTP 请求，但显著简化 checkpoint 原子性，适合第一版。

### 步骤 4：在运行时引入 `BatchCommitter`

先用内存 fake 实现接口，证明 Pipeline 的规则：

```text
读取完整记录
  -> parse/enrich
  -> append pending batch
  -> 达到 flush 条件
  -> Commit(batch)
  -> Commit 成功后清空内存 batch
```

若 `Commit` 因容量满而阻塞，Pipeline 自然停在这里。若因持久化错误返回，Pipeline 不应继续读 Source；顶层应将可靠性基础设施错误视为全局失败。

### 步骤 5：接入真正的 Spool

第五章实现磁盘事务。接入后，删除任何“spool 失败就回退到直接网络发送”的路径。

关键检查：只有本地事务 commit 成功，durable checkpoint 才推进。进程内 FileSource 的 volatile cursor 可以更靠前，但崩溃恢复不能使用它。

### 步骤 6：将 Sender 替换为 Dispatcher

Dispatcher 从 spool 读取完整 batch。现有 `TickOrBatchSender` 的“聚合”职责已经移动到 Pipeline，它不应继续作为可靠运行时核心。

可以暂时保留 terminal destination 作为开发工具，但它的 ACK 语义要明确：写 stdout 成功不等价于 Server 持久化。

### 步骤 7：重写顶层监督与关闭顺序

目标顺序：

```text
启动：配置校验 -> 打开日志 -> 打开 spool -> 恢复状态
     -> 启动 Dispatcher -> 打开并启动 Pipelines -> running

停止：取消 Pipelines -> 等待 Pipeline 完成当前本地 commit
     -> 停止产生新 batch -> 给 Dispatcher 一个有界 drain 窗口
     -> 取消 Dispatcher -> flush/close spool -> close logger
```

注意，“等待 Pipeline 完成当前本地 commit”不等于无限等待。磁盘调用应受 shutdown deadline 约束；如果超时，旧 durable checkpoint 保证未提交数据会在重启后重读。

### 步骤 8：最后改 Builder 与配置

构造顺序与运行顺序同样重要。推荐使用局部 cleanup 栈：

```go
func Build(cfg Config) (_ *Agent, err error) {
	var closers []io.Closer
	defer func() {
		if err != nil {
			closeReverse(closers)
		}
	}()

	// 每成功打开一个资源就登记；全部成功后将所有权移交给 Agent。
	return agent, nil
}
```

不要在 Builder 中启动 goroutine。构造完成不应产生后台活动；`Run` 才是启动边界。

## 10. 一个可推导的 `Run` 骨架

下面只展示控制结构，省略状态上报和错误包装：

```go
func (a *Agent) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- a.dispatcher.Run(runCtx)
	}()

	var pipelines sync.WaitGroup
	for i := range a.pipelines {
		pipeline := &a.pipelines[i]
		pipelines.Go(func() {
			a.runPipelineSafely(runCtx, pipeline)
		})
	}

	producersDone := make(chan struct{})
	go func() {
		pipelines.Wait()
		close(producersDone)
	}()

	select {
	case <-ctx.Done():
		cancel(context.Cause(ctx))
	case err := <-dispatchDone:
		if err != nil {
			cancel(err)
		}
	}

	<-producersDone
	// 实际实现还要区分 Dispatcher 是否已退出，并执行有界 drain。
	return context.Cause(runCtx)
}
```

不要直接复制为最终实现，因为它尚未表达：

- Pipeline 的本地 commit 收尾窗口；
- Dispatcher 的独立 drain context；
- 谁关闭 spool；
- 局部失败状态；
- 多个错误同时发生时的主错误选择。

值得完整实现并仔细审查的是最终 `Agent.Run`，因为它是资源所有权的总账。简单的字段赋值 constructor 不需要大量测试。

## 11. 取消、错误与资源关闭

### 11.1 不要滥用 `context.WithoutCancel`

现有代码用它让 Sender 在 Pipeline 取消后继续排空 channel，这是特定生命周期需求。可靠版本中，Dispatcher 的 drain 也可能需要独立 context，但必须满足：

- 有明确 timeout；
- 上游已经不会产生新 batch；
- timeout 后退出不会丢 durable batch；
- 不会把用户取消永久屏蔽。

推荐创建 `context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)`，只用于有界收尾，不传给整个运行期。

### 11.2 关闭顺序来自依赖方向

若 `Pipeline -> Spool <- Dispatcher`，则 Spool 必须最后关闭：

1. 停 Pipeline；
2. 等 Pipeline 不再调用 `Commit`；
3. 停 Dispatcher；
4. 关闭 Spool。

反过来会产生 `database not open`、use-after-close 或丢失最后一次 commit。

### 11.3 返回哪个错误

定义可解释的优先级：

1. spool corruption/I/O 等全局可靠性错误；
2. Dispatcher 内部不变量错误；
3. 外部 context 的 deadline/cancel；
4. 单 Pipeline 错误只进入状态与日志，不作为 Agent 返回值，除非所有 Pipeline 都失败且产品策略如此规定。

使用 `%w` 保留原因，但不要在每一层重复打印同一个错误。通常由最终处理该错误的边界记录一次结构化日志。

### 11.4 panic 不是普通错误

Pipeline 外层可以 recover 来隔离一个插件/Parser 缺陷，并记录 `debug.Stack()`。Spool 事务核心若 panic，通常不应假装局部恢复；让顶层失败并保留磁盘数据更安全。

## 12. 哪些代码值得完整给出

在实际实现和代码评审中，优先完整展示这些代码：

- `Agent.Run`：启动、取消、等待、drain 与关闭顺序；
- `Pipeline.Run`：读取、解析、batch、commit 之间的状态转换；
- Spool 的 `CommitBatchAndCheckpoint` 事务；
- Dispatcher 的单 batch 状态机；
- FileSource 的轮转状态机；
- 构造失败时的资源回滚。

以下内容通常不值得为了教学写成大段完整代码：

- 只赋字段的 constructor；
- 配置结构体的所有 tag；
- 机械的 metric 注册；
- 简单 getter/setter；
- 为测试语言或标准库行为编写的样板。

## 13. 测试设计

### 13.1 单元测试

保留或新增以下高价值行为：

| 场景 | 断言 |
| --- | --- |
| Parser 返回错误 | Unknown entry 仍保留原始消息、时间、来源位置 |
| 单 Pipeline fatal | 该 Pipeline 标记 failed，其他 Pipeline 继续 commit |
| Pipeline panic | 捕获堆栈，其他 Pipeline 不受影响 |
| Spool commit 阻塞 | Pipeline 不继续无限读取，内存 batch 有界 |
| Spool commit 失败 | Agent 退出且不会绕过 spool 发送 |
| 外部取消 | Source、Pipeline、Dispatcher 都在 deadline 内退出 |
| 构造第 N 个 Source 失败 | 前 N-1 个资源全部关闭 |

可使用 `testing/synctest` 测 timer 和 goroutine 稳定点，但断言应针对结果，不应锁死内部调度步骤。

### 13.2 集成测试

用真实临时 spool + fake Transport 验证：

1. Pipeline 提交 batch；
2. Dispatcher 能看到该 batch；
3. Transport 未 ACK 时 batch 仍在；
4. 重启运行时仍可看到；
5. ACK 后 batch 才删除。

### 13.3 泄漏与竞态

至少执行 race test。对于反复启动/停止，可以用 goroutine profile 辅助检查，但不要把某个绝对 goroutine 数写成脆弱断言；更稳定的断言是所有自有 `done` channel 都关闭、临时文件可删除、端口可重新绑定。

## 14. 验收命令与证据

在仓库可复现性问题（本机 `replace`）解决后，建议逐步执行：

```powershell
gofmt -w .\internal\agent .\cmd\agent
go test ./internal/agent/... -count=1
go test -race ./internal/agent/... -count=1
go vet ./internal/agent/... ./cmd/agent
go build ./cmd/agent
```

本章的“完成”不能只由命令退出码证明，还应保存以下证据：

- 一张实际运行时所有权图，与代码字段和关闭顺序一致；
- 一组覆盖局部失败、全局失败、取消和构造回滚的测试；
- 一个小容量 fake spool 测试，证明背压时读取不会无界增长；
- 日志中能用 `pipeline_id` 定位局部失败，但不包含日志正文或 token。

如果 `go test` 仍依赖 `E:/Proj/testx`，只能记录“本机通过”，不能把它写成全新 clone 已验证。

## 15. 常见错误

1. **把 channel buffer 调大当可靠性。** 它只推迟 OOM，进程崩溃仍全部丢失。
2. **共享一个跨 Pipeline batch。** 需要一次事务推进多个文件 checkpoint，恢复模型迅速复杂化。
3. **先推进 checkpoint，再写 spool。** 两步之间崩溃会永久丢数据。
4. **spool 写失败时退回 HTTP 直传。** 这让同一配置下的保证随故障改变，无法解释。
5. **Transport 内部自己无限重试。** Dispatcher 无法观察状态、取消等待或执行隔离策略。
6. **接收方关闭 channel。** 生产者仍发送时会 panic；关闭权属于发送方。
7. **为每个 goroutine 使用 `context.Background()`。** 顶层取消无法传递，进程退出挂住。
8. **取消时直接关闭 spool。** Pipeline 可能正在 commit，Dispatcher 可能正在读取。
9. **把所有 Pipeline 错误都吞掉。** 隔离不等于不可见，必须进入状态、日志与指标。
10. **只测 happy path。** 生命周期 bug 多发生在取消、构造到一半和两个错误并发发生时。

## 16. 复盘题

1. 为什么 entry 已经写入 channel 仍不能推进 durable checkpoint？
2. 为什么第一版让 batch 只包含一个 Pipeline 能显著降低原子性复杂度？
3. FileSource 的 volatile cursor 可以领先 durable checkpoint 多远？这个距离如何保持有界？
4. Server timeout 后，为什么不能生成新 `batch_id` 再发送？
5. 一个 Pipeline permission denied 与 spool database corruption 的错误作用域有何不同？
6. 为什么 Dispatcher 应在 Pipeline 停止之后、Spool 关闭之前结束？
7. `context.WithoutCancel` 在什么情况下合理，怎样防止它形成永不结束的 shutdown？
8. 你会如何证明构造第 3 个 Source 失败时，前两个 Source 已关闭？

如果不能不看文档回答第 1、2、4、6 题，先不要进入磁盘 spool 实现。

## 17. 进入下一章前的完成条件

- [ ] 能从当前 `Agent.Run` 指出现有生命周期优点和可靠性缺口；
- [ ] 目标运行时已明确拆为 Pipeline、Spool、Dispatcher；
- [ ] 每个长期资源和 channel 都有唯一关闭者；
- [ ] 错误传播表已经转化为测试或明确的实现任务；
- [ ] `RawRecord` 的位置语义是“本条记录之后的位置”；
- [ ] batch 第一版不跨 Pipeline；
- [ ] 任何网络路径都不能绕过 spool；
- [ ] shutdown 有 deadline，未发送 batch 依然留盘；
- [ ] 现有 Pipeline 隔离与 Parser 降级合同没有在重构中丢失。

下一章将把 `BatchCommitter` 从接口变成真正的本地事务边界：[05. Spool 与 Checkpoint](./05-spool-checkpoint.md)。
