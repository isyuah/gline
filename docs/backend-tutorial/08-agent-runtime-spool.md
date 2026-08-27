# 08. Agent 运行时、Spool 与 Server ACK 边界

本章的目的不是把 Gline 变成“另一个 Agent 教程”，而是解释一个后端工程师必须能够说清楚的事实：Server 的幂等接入语义，只有在 Agent 能够可靠地保存并重放批次时才成立。

如果 Agent 读到一行就直接发 HTTP，请求超时后再从文件当前位置继续，那么 Server 即使有唯一索引，也无法判断丢失、重复和未知提交分别发生在哪里。本章把 Agent 设计成 Server 的可靠客户端，并明确所有状态的所有权。

> 本章中的代码是实现骨架。代码存在、单元测试通过，都不等于故障恢复已经完成；完成门要求用真实进程、临时目录和可注入故障证明状态转换。

## 8.1 当前差距与本章完成结果

当前仓库已经有 Agent、SourcePipeline、Parser 和生命周期测试，这些测试证明了部分并发和错误隔离行为。但在开始本章前，不要默认下列能力已经存在：

- 任何读到的记录都已经进入磁盘级 durable spool；
- checkpoint 与 spool 写入具有同一提交边界；
- batch 拥有稳定的 `batch_id` 和不可变 payload；
- dispatcher 能区分 ACK、重复 ACK、临时错误和永久错误；
- spool 达到上限时，采集会施加背压而不是无限占用内存；
- 文件 rename、recreate、truncate 后仍能恢复到正确位置。

本章完成后，Agent 应该具备以下可验证的边界：

```text
FileSource
   -> record buffer
   -> immutable batch
   -> durable spool commit
   -> checkpoint commit
   -> Dispatcher
       -> same batch_id retry
       -> Server commits PostgreSQL
       -> ACK / duplicate ACK
       -> mark delivered and reclaim
```

Server 的合同是：只有批次已经提交到 PostgreSQL，才返回成功 ACK。Agent 的合同是：只要没有看到可接受的 ACK，就保留原始 batch，并使用同一个 `batch_id` 和 payload 重试。

## 8.2 前置知识

开始前应能解释：

1. Go 中 channel 的发送、接收和关闭责任；
2. `context.Context` 的取消传播与超时；
3. `io.Reader`、文件 offset、`fs.Stat` 和文件 identity 的区别；
4. `os.Rename`、`fsync`、目录落盘在“进程崩溃”和“机器断电”场景下的差别；
5. HTTP 状态码、请求超时和“服务端可能已经提交”的不确定性；
6. 至少一次传输与幂等消费者的关系。

如果这些概念不熟悉，先阅读项目中关于 Go 并发和领域协议的基础章节，再回到本章。不要通过增加重试次数来掩盖状态模型没有定义的问题。

## 8.3 定义：四种位置与三个时间点

不要把“文件位置”“spool 位置”和“Server 位置”混成一个 offset。至少有四个概念：

| 概念 | 含义 | 由谁拥有 | 能否代表 Server 已写入 |
| --- | --- | --- | --- |
| source position | Source 已经读到的文件位置 | FileSource | 不能 |
| checkpoint | 已安全进入本地 spool 的位置 | Spool/Checkpoint store | 不能 |
| pending batch | 已落盘、尚未得到可接受 ACK 的不可变批次 | Spool | 不能 |
| server receipt | PostgreSQL 已接受的 `(project_id,batch_id)` | Server DB | 能 |

三个关键时间点：

```text
T1: Source 读出记录
T2: batch + checkpoint 原子落盘
T3: Server transaction commit 后返回 ACK
```

`T1` 与 `T2` 之间崩溃，记录应从旧 checkpoint 重读；`T2` 与 `T3` 之间崩溃，pending batch 应重放；`T3` 与 Agent 删除本地 batch 之间崩溃，重放会得到 duplicate，但不能产生第二份有效日志。

这就是为什么 checkpoint 不能等 ACK：checkpoint 的语义是“已经有本地恢复依据”，不是“远端已经写入”。

## 8.4 Batch 合同与不可变性

建议 v1 batch 至少包含以下字段。字段名必须与 Server 的协议 DTO 统一，不能在 Agent 与 Server 中各自“差不多地定义”。

```go
type Batch struct {
    BatchID   string       `json:"batch_id"`
    ProjectID string       `json:"project_id"`
    AgentID   string       `json:"agent_id"`
    Pipeline  string       `json:"pipeline"`
    CreatedAt time.Time    `json:"created_at"`
    Entries   []LogEntry   `json:"entries"`
    Hash      string       `json:"payload_hash"`
}

type LogEntry struct {
    ObservedAt time.Time `json:"observed_at"`
    Service    string    `json:"service"`
    Host       string    `json:"host"`
    Level      string    `json:"level"`
    Content    string    `json:"content"`
}
```

实现时需要注意：

- `BatchID` 在 batch 第一次落盘前生成，重试不重新生成；
- `Hash` 对规范化 JSON 或协议编码后的 payload 计算，不能对随机字段计算；
- batch 一旦进入 spool，不再追加 entries、不重新排序、不修改时间；
- 服务端返回“重复且 hash 相同”时可以确认清理本地 batch；
- 服务端返回“同 batch_id 但 hash 不同”是协议冲突，应进入 quarantine，不能覆盖远端数据；
- 单个 batch 的条数、字节数和编码后的请求体必须有上限。

可用一个构造函数把不可变性变成约束。Go 的 slice 仍可被调用方修改，因此构造时要复制数据或在内部使用只读编码结果：

```go
func NewBatch(projectID, agentID, pipeline string, entries []LogEntry, now time.Time) (Batch, error) {
    if len(entries) == 0 {
        return Batch{}, errors.New("batch must contain at least one entry")
    }
    copied := append([]LogEntry(nil), entries...)
    payload, err := canonicalJSON(copied)
    if err != nil {
        return Batch{}, fmt.Errorf("encode batch payload: %w", err)
    }
    return Batch{
        BatchID:   newUUID(),
        ProjectID: projectID,
        AgentID:   agentID,
        Pipeline:  pipeline,
        CreatedAt: now.UTC(),
        Entries:   copied,
        Hash:      sha256Hex(payload),
    }, nil
}
```

## 8.5 Spool 的本地数据模型

先选一个能解释、能恢复和能测试的模型。第一版不需要把本地 spool 做成分布式队列，也不要先引入 Kafka。可采用一个目录和一个事务性元数据文件（例如 SQLite）管理 payload 文件：

```text
<spool-root>/
  data/<batch-id>.json
  data/<batch-id>.json.tmp
  quarantine/<batch-id>.json
  meta.db
  LOCK
```

`meta.db` 中至少需要：

```sql
CREATE TABLE spool_batches (
    batch_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    pipeline TEXT NOT NULL,
    payload_path TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    entry_count INTEGER NOT NULL,
    payload_bytes INTEGER NOT NULL,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL,
    last_error_code TEXT,
    last_error_at TIMESTAMP
);

CREATE TABLE checkpoints (
    source_key TEXT PRIMARY KEY,
    file_identity TEXT NOT NULL,
    offset_bytes INTEGER NOT NULL,
    observed_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
```

状态只允许向前转换：

```text
pending -> sending -> pending
pending -> delivered -> reclaimed
pending -> quarantined
```

如果 `sending` 状态的 Agent 在请求中途崩溃，启动恢复时必须把超时的 `sending` 批次改回 `pending`。不要把“进程退出时恰好是 sending”当作数据已经发送成功。

写入顺序要明确。推荐的最小事务：

1. 生成 batch payload 临时文件；
2. 写完整 payload，并调用文件同步；
3. 原子 rename 为最终文件名；
4. 在元数据事务中插入 `spool_batches`；
5. 在同一个元数据事务中更新 checkpoint；
6. 提交元数据事务；
7. 同步 spool 目录（如果目标平台和实现需要）；
8. 只有提交成功后才向上游报告“已消费”。

如果第 4 步失败，不能推进 checkpoint。若 payload 文件已存在但元数据事务失败，启动扫描应将孤儿文件移动到 `lost+found` 或 quarantine，并记录可诊断的事件，而不是静默删除。

## 8.6 文件 Source、identity 与轮转

文件路径不是文件身份。日志轮转通常会出现：

- 原文件 rename 成 `.1`，新文件以原路径创建；
- 原文件被 truncate 后继续写；
- 文件被删除后重建；
- 多个路径短时间指向不同 inode 或 Windows 文件 ID；
- 进程只看到路径变化，却没有看到旧文件末尾。

checkpoint 至少需要 `source_key`、`file_identity` 和 `offset_bytes`。`source_key` 可以是配置项的稳定 ID；`file_identity` 则来自平台可用的 inode/file ID，不能只使用 path hash。

处理步骤：

1. 根据 `source_key` 打开并 stat 当前路径；
2. identity 相同且 size 大于等于 checkpoint offset：从 offset 继续；
3. identity 相同但 size 小于 offset：判定 truncate，按策略从头或新 epoch 开始；
4. identity 不同：关闭旧 handle，保存旧文件最后可读位置，建立新 identity 的 checkpoint；
5. 旧文件仍可能有尾部数据时，先 drain 旧 handle，再切换新文件；
6. 每个身份都产生可审计的 rotate/truncate 事件。

不要在没有证据时声称“支持所有轮转方式”。应在测试矩阵中列出具体方式和未覆盖方式。尤其是 copytruncate：复制和 truncate 之间的写入可能丢失，Agent 无法单凭 tail 逻辑保证零丢失。文档和运行指标必须诚实表达这一边界。

## 8.7 Dispatcher、ACK 和重试分类

Dispatcher 不解析日志正文，也不修改 batch。它只负责把 spool 中的不可变 payload 交给 Transport，并根据响应更新状态。

```go
type SendResult struct {
    Class      ResultClass
    RetryAfter time.Duration
    Code       string
    Message    string
}

type ResultClass int

const (
    ResultAck ResultClass = iota
    ResultDuplicate
    ResultTemporary
    ResultPermanent
    ResultConflict
)
```

建议分类：

| 响应 | Agent 动作 | 是否删除 batch |
| --- | --- | --- |
| 2xx 新写入 | 标记 delivered，随后 reclaim | 是 |
| 2xx duplicate 且 hash 相同 | 标记 delivered，reclaim | 是 |
| 超时、连接失败、429、5xx | 保留，按退避重试 | 否 |
| 400 schema、413 超大、403 scope | quarantine 或暂停配置 | 否，除非策略明确丢弃 |
| 同 ID 不同 hash | quarantine 并报警 | 否 |

超时尤其重要：客户端不知道 Server 是否已经提交，因此必须按同一 batch 重试。不能看到超时就生成新 batch；那会绕过幂等合同。

退避函数要可测试，不依赖真实 sleep：

```go
func Backoff(attempt int, base, max time.Duration, jitter float64, r *rand.Rand) time.Duration {
    if attempt < 1 {
        attempt = 1
    }
    n := math.Min(float64(max), float64(base)*math.Pow(2, float64(attempt-1)))
    if jitter <= 0 || r == nil {
        return time.Duration(n)
    }
    factor := 1 + (r.Float64()*2-1)*jitter
    d := time.Duration(n * factor)
    if d < 0 {
        return 0
    }
    if d > max {
        return max
    }
    return d
}
```

不要把 `Retry-After` 当作无限期等待，也不要在 401/403 上快速热循环。认证配置错误应暂停发送、暴露 readiness 或高等级事件，等待凭证修复。

## 8.8 背压与关闭

spool 的容量是资源治理合同，不是一个可以忽略的配置。推荐三个水位：

- 低水位：正常发送；
- 高水位：暂停低优先级 flush，增加告警和采集延迟；
- 满水位：阻塞新 batch 的落盘或停止 Source 读取，返回明确状态。

默认策略应是 `block`。`drop_oldest` 或 `drop_newest` 只能作为显式配置，并必须记录丢弃条数、字节数、最老时间和原因；不能把丢弃伪装成成功。

关闭顺序：

```text
停止新 Source 读取
  -> 让 Parser/Batch Builder 完成已取数据
  -> 将可形成的 batch 原子写入 spool
  -> 停止 Dispatcher 接收新工作
  -> 等待有限时间发送 pending
  -> 超时则保留本地 pending
  -> 关闭 transport、文件、spool
```

关闭不是“尽可能把数据发完”，而是在 bounded deadline 内完成。超过 deadline 后强行退出不能删除 pending 文件，也不能推进未提交的 checkpoint。

## 8.9 实施顺序

按垂直切片实施：

1. 定义 batch DTO、hash 计算和状态枚举；
2. 实现内存 spool，先验证状态转换和 dispatcher 分类；
3. 替换为临时目录中的持久化 payload + 元数据事务；
4. 把 Source checkpoint 接到 spool commit；
5. 引入 fake Transport，验证 timeout、duplicate、429、413 和 conflict；
6. 接入真实 Server HTTP；
7. 增加文件 identity、truncate 和 rename 测试；
8. 增加容量水位和 graceful shutdown；
9. 最后再调 batch 大小和并发，不先猜性能数字。

每一步都保留一个可运行状态。不要在 spool、轮转、HTTP 重试和 metrics 同一个提交里同时改动，否则故障定位会非常困难。

## 8.10 测试与故障注入

高价值测试不是检查某个 helper 被调用了几次，而是检查恢复后外部结果：

| 注入点 | 预期结果 |
| --- | --- |
| payload 写一半进程退出 | 启动时临时文件不被当作完整 batch |
| payload 已同步、元数据事务前退出 | checkpoint 不前进，孤儿文件可诊断 |
| metadata commit 后 HTTP 未发送 | 重启后发送同一 batch |
| HTTP 请求超时但 Server 已 commit | 重试返回 duplicate，数据库只有一份 |
| ACK 后本地 reclaim 前退出 | 重启重发，最终仍只有一份 |
| 429 且带 Retry-After | 不热循环，按服务端建议延迟 |
| 400 schema | 进入 quarantine，不无限重试 |
| spool 满 | Source 停止继续增长，状态和指标可见 |
| rename/recreate | 新旧 identity 不串 offset |
| truncate | 按明确策略处理，不出现负 offset |

故障注入接口可以是函数而不是全局变量：

```go
type FaultHooks struct {
    AfterPayloadSync func(batchID string) error
    BeforeMetaCommit  func(batchID string) error
    AfterHTTPWrite    func(batchID string) error
}
```

生产构建不得从环境变量无条件打开破坏性故障。测试只在显式注入时启用，并确保 hook 在测试结束时释放。

## 8.11 验收证据

本章不是看到 `spool/` 目录就算完成。至少收集：

- 一个 batch 从文件写入到 Server duplicate 的日志和数据库查询结果；
- 进程在四个关键窗口退出后的重启结果；
- rename、truncate 和权限错误的测试输出；
- spool 高低水位变化和满盘时的 readiness/metrics；
- `go test -race` 下没有数据竞争；
- 关闭超时后 pending 文件仍存在且可恢复；
- 文档记录明确的 copytruncate 和机器断电边界。

不要写“保证零丢失”，除非实验覆盖了所声明的故障模型。更准确的表述是：“在 Agent 进程崩溃、网络超时和可检测的文件轮转场景下，依靠本地持久化 spool 与 Server 批次幂等实现可恢复的至少一次传输；对 copytruncate 窗口和存储介质损坏另有明确边界。”

## 8.12 常见坑、复盘题与完成门

常见坑：

- checkpoint 先写、spool 后写，造成不可恢复的丢失；
- retry 时重新生成 batch ID，导致重复写入；
- 将 HTTP 200 当成“响应已到达”而忽略客户端超时；
- 只按路径识别文件，轮转后跳过新文件或重复旧文件；
- `sending` 状态永久卡住，重启不扫描 lease；
- spool 满时继续从 Source 读入无界 channel；
- quarantine 只有日志没有可重放 payload；
- shutdown 直接删除临时文件和 pending 元数据；
- 把 copytruncate 当成 rename 的等价物；
- metrics 标签包含 `batch_id` 或原始路径，造成高基数和信息泄露。

复盘题：

1. 为什么 checkpoint 的安全点是 spool commit，而不是 Server ACK？
2. Server 已提交但 Agent 没收到 ACK 时，哪个组件负责去重？
3. `sending` lease 的超时时间如何和 HTTP timeout、最大重试间隔协调？
4. spool 满时阻塞 Source 会如何影响内存、文件句柄和可观测性？
5. 如果两条配置 pipeline 指向同一文件，`source_key` 应如何定义？
6. 什么证据能支持“重启后无丢失、最多重复一次”的表述？

完成门：

- [ ] batch payload 和 `batch_id` 跨重试不可变；
- [ ] spool metadata 和 checkpoint 有明确提交边界；
- [ ] timeout、duplicate、temporary、permanent、conflict 五类结果都有动作；
- [ ] Server 未确认时任何路径都不会删除 pending batch；
- [ ] spool 满会产生背压和可观测状态；
- [ ] 轮转和 truncate 测试覆盖声明过的场景；
- [ ] 崩溃窗口测试可以在干净环境中重复运行；
- [ ] 已知限制和未验证性能写入文档。

