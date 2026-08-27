# 05. Spool 与 Checkpoint：建立本地可靠边界

> 本章描述目标实现。当前工作树尚未具备这里的磁盘 spool 和 durable checkpoint。请先完成[上一章的运行时边界](./04-agent-runtime.md)，再开始引入存储依赖。

## 1. 本章目标

完成本章后，你应该能够：

1. 准确定义 spool、checkpoint、batch identity 和 ACK；
2. 设计一个事务，使 batch 与对应 checkpoint 要么都提交，要么都不提交；
3. 解释 Agent 在四个关键崩溃窗口中的恢复行为；
4. 保证同一 durable batch 的所有 HTTP 重试使用完全相同的身份和 payload；
5. 实现有界容量、默认阻塞、隔离区、schema version 和重启恢复；
6. 区分逻辑队列大小、数据库文件大小和磁盘剩余空间；
7. 用真实临时数据库和子进程故障注入证明持久化语义。

## 2. 当前代码定位与缺口

当前 Agent 的批次只存在于 `internal/agent/sender/tickOrBatch.go` 的内存 slice 中：

```text
Pipeline 写 entry channel
  -> TickOrBatchSender 在内存聚合
  -> Destination.SendEntries
  -> 成功后丢弃 slice
```

这个实现适合协议原型，但不具备以下能力：

- 进程重启后恢复未发送 batch；
- 判断文件中的哪些字节已安全转移到本地存储；
- HTTP 结果未知时保持相同 `batch_id` 重试；
- Server 长期不可用时用磁盘容量形成有界背压；
- 将永久失败 batch 隔离并保留诊断元数据。

目标不是给 `TickOrBatchSender` 增加一个“失败时写文件”的补丁。spool 必须成为发送前的必经路径，否则系统在正常和故障路径上会有两套不同保证。

## 3. 前置知识

建议先理解：

- ACID 事务，尤其 atomicity 与 durability；
- write-ahead 思想：先建立可恢复状态，再推进外部进度；
- 单写者 KV 数据库的事务与文件锁；
- little-endian/big-endian 对可排序整数 key 的影响；
- `fsync`、进程退出与操作系统缓存不是同一层保证；
- SHA-256 是内容指纹，不是身份生成器；
- 幂等不是“先查询再插入”，而是依赖唯一约束处理竞态。

同时阅读：

- [上传协议 v1](../04-domain-api-and-storage.md#3-上传协议-v1)；
- [Agent spool 设计](../05-reliability-security-observability.md#3-agent-spool-设计)；
- [ADR-0003](../adr/0003-at-least-once-idempotency.md)。

## 4. 关键定义

### 4.1 Spool

Spool 是 Agent 本地的持久、有序、有界待发送队列。一个 batch 在 spool 事务 commit 后，才算跨过 Agent 的本地可靠边界。

Spool 不是：

- 网络失败后的可选缓存；
- 无限增长的本地日志副本；
- 用于查询历史日志的数据库；
- Server ACK 之前可以随意重编码的对象仓库。

### 4.2 Checkpoint

Checkpoint 表示某个 Pipeline 的 Source 数据已经安全进入 spool 的恢复位置。

它不表示：

- Server 已收到数据；
- Server 已写 PostgreSQL；
- 文件当前 handle 的即时读取位置；
- 用户已经查询到数据。

对于文件 Source，checkpoint 至少包含：

```go
type FileCheckpoint struct {
	PipelineID       string
	SourceFingerprint string
	Identity         source.FileIdentity
	Offset           int64
	UpdatedAt        time.Time
}
```

`SourceFingerprint` 用于检测 Pipeline ID 未变但路径或关键采集策略已经改变的情况。它应来自规范化后的非敏感配置，不包含 token。

### 4.3 Batch identity

至少区分三种序号：

| 名称 | 作用域 | 作用 |
| --- | --- | --- |
| `batch_id` | Agent 生成的全局稳定 ID | Server 幂等键的一部分 |
| local queue sequence | 单个 spool 内单调递增 | 保持待发送顺序、作为 KV 排序 key |
| entry `sequence` | 单个 batch 内从 0 连续递增 | 保持 entry 顺序，形成 batch 内稳定标识 |

不要把 local queue sequence 直接当 `batch_id`。Agent 重装、spool 重建或多 Agent 并行时，它不具备全局唯一性。

### 4.4 ACK

只有 Server 在 PostgreSQL 事务提交后返回的以下结果，才是可删除本地 batch 的 ACK：

- `accepted`：首次成功写入；
- `duplicate`：相同项目、相同 `batch_id`、相同 payload 已存在。

HTTP 200 但响应结构不合法，不能视为 ACK。连接关闭、timeout 或客户端取消都属于“结果未知”，必须保留并用同一 batch 重试。

## 5. 核心不变量

| 编号 | 不变量 |
| --- | --- |
| S1 | 任何因消费记录而推进的 checkpoint，都和对应 batch 在同一个本地事务中提交 |
| S2 | checkpoint 只有在事务成功返回后才对恢复逻辑可见 |
| S3 | Dispatcher 不能读取未提交事务中的 batch |
| S4 | batch commit 后的协议 payload 是不可变字节串 |
| S5 | `batch_id` 在首次尝试本地 commit 前生成，此后该内存 batch 重试 commit 时也不改变 |
| S6 | Agent 重启后按 spool 中保存的 payload 重发，不重新从领域对象编码 |
| S7 | `Ack` 必须同时核对 local sequence 与 `batch_id`，避免删除错误队首 |
| S8 | 容量满时默认阻塞新的 commit，不删除旧 batch、不绕过 spool |
| S9 | 单个 batch 大于总容量是永久配置/输入错误，不能永远等待容量 |
| S10 | quarantine 是一次原子移动，不是先删 pending 再尝试写另一个文件 |
| S11 | 持久化引擎损坏或无法保证 durability 时停止采集，而不是降级 |

## 6. 存储引擎如何选择

### 6.1 第一版推荐 bbolt 的理由

对于单进程 Agent，本地 spool 的负载形状是：

- 单个进程打开；
- 小量 writer、顺序读取；
- 需要原子更新多个 key；
- 不需要 SQL 查询；
- 希望一个文件即可备份和定位。

bbolt 与这个形状匹配。它提供进程内事务、文件锁和按 key 排序的 bucket。引入时应通过 Go 包管理器选择与当前 Go 版本兼容的版本，例如先查询模块信息，再执行 `go get`；不要在教程中硬编码一个未经验证的最新版本，也不要手改 `go.sum`。

### 6.2 SQLite 也可以，但不要双实现

SQLite 的事务、工具生态和可检查性也很好，尤其当你希望用 SQL 查看队列时。代价是 driver 选择、CGO/纯 Go 取舍和跨平台发布验证更多。

简历项目不需要同时维护 bbolt 和 SQLite adapter。先选一个，使用接口隔离上层；只有真实需求出现时再实现第二个。

### 6.3 不要用“每个 batch 一个 JSON 文件”起步

文件方案看似简单，但很快需要自己解决：

- batch 文件与 checkpoint 文件如何原子提交；
- 目录 fsync；
- 临时文件 rename；
- 崩溃残留清理；
- 数十万小文件性能；
- 容量统计和隔离移动。

这些问题并非不能解决，但会把教学重点变成自研存储事务。

## 7. 推荐包与类型布局

目标包可以采用：

```text
internal/agent/
  checkpoint/       # checkpoint 值类型、比较和配置指纹
  spool/            # 持久化实现、schema、事务、容量与恢复
  pipeline/         # 形成 PreparedBatch 并调用 Committer
  dispatcher/       # Next/Ack/Quarantine 的消费者
```

`checkpoint` 可以是独立的领域类型包，但 checkpoint 的持久化必须由 `spool` 的同一个数据库事务完成。不要创建 `checkpoint.db` 和 `spool.db` 两个独立文件，然后声称两个写入是原子的。

### 7.1 存储值

```go
type BatchID string

type StoredBatch struct {
	QueueSequence uint64
	BatchID       BatchID
	PipelineID    string
	Payload       []byte
	WireSHA256    [32]byte
	EntryCount    int
	LogicalBytes  int64
	CreatedAt     time.Time
}

type CommitRequest struct {
	Batch         PreparedBatch
	Payload       []byte
	WireSHA256    [32]byte
	LogicalBytes  int64
	Checkpoint    checkpoint.Value
}
```

`Payload` 应是实际要发送的协议 body。保存最终字节最容易保证“每次重试完全一致”。`WireSHA256` 只用于 Agent 检测本地保存的 wire bytes 是否损坏；Server 的幂等 hash 必须按[协议章节定义的规范化领域内容](./03-protocol-domain-contracts.md#11-payload-hash定义的是内容不是请求字节)自行计算，不能信任 Agent checksum，也不能把两种 hash 混为一谈。

### 7.2 Store 接口

接口不要泄漏 bbolt 的 `*bolt.Tx`：

```go
var (
	ErrClosed        = errors.New("spool closed")
	ErrBatchTooLarge = errors.New("batch exceeds spool capacity")
	ErrCorrupt       = errors.New("spool corrupt")
)

type Store interface {
	Commit(ctx context.Context, req CommitRequest) (StoredBatch, error)
	Transition(ctx context.Context, transition checkpoint.Transition) error
	Next(ctx context.Context) (StoredBatch, error)
	Ack(ctx context.Context, sequence uint64, id BatchID) error
	Quarantine(ctx context.Context, sequence uint64, id BatchID, reason string) error
	Checkpoint(ctx context.Context, pipelineID string) (checkpoint.Value, bool, error)
	Stats(ctx context.Context) (Stats, error)
	Close() error
}
```

`Transition` 不是绕过 `Commit` 的通用 checkpoint setter。它只处理不对应新日志记录的、经过验证的控制状态变化，例如：用户显式选择 `start_position: end` 时建立初始 anchor，或旧文件已经稳定 EOF 后把 identity 切换到新文件的 offset 0。实现必须在同一个 write transaction 中比较预期旧值、校验 `Reason` 和新值，再写入新 checkpoint；不匹配就返回状态冲突。

只要消费了哪怕一条记录，就必须走 `Commit(batch + checkpoint)`。禁止用 `Transition` 单独保存一个更靠后的读取 offset，否则会重新制造永久丢失窗口。

如果第一版只有一个 Dispatcher，`Next` 可以始终返回最小 queue sequence 且不删除。这样进程崩溃后仍会取到同一队首。未来并发 Dispatcher 才需要 lease/claim 状态机；不要提前增加 lease 过期、心跳和乱序确认。

## 8. 协议 payload 何时冻结

建议在 `Commit` 前完成以下步骤：

1. 为内存 batch 生成一次 `batch_id`；
2. 为 entry 分配从 0 开始的连续 `sequence`；
3. 固定 `agent_id`、`pipeline_id` 和时间字段；
4. 使用协议 DTO 编码 payload；
5. 对最终 wire bytes 计算本地完整性 checksum；Server 稍后独立计算规范化内容 hash；
6. 在同一事务保存 payload 与 checkpoint。

现有协议示例包含 `sent_at`。若它属于幂等 payload，则它必须在 batch 首次形成时冻结，重试时不能更新为“本次发送时间”。更清晰的命名是 `created_at` 或 `queued_at`；真正的每次 attempt 时间只记录在 Agent 指标/日志中，不进入幂等 body。

不要在 Dispatcher 每次发送前执行：

```go
request.SentAt = time.Now() // 错误：同一 batch 的 payload 发生变化
```

否则“Server 已提交但 ACK 丢失”后，重试会使用相同 ID、不同 hash，正确的 Server 应返回 409。

## 9. 建议的 bbolt schema

第一版可使用这些 bucket：

```text
meta
  schema_version       -> uint32
  logical_bytes        -> uint64
  next_queue_sequence  -> uint64（也可使用 bucket sequence）

pending_batches
  big_endian(sequence) -> encoded StoredBatch

checkpoints
  pipeline_id          -> encoded checkpoint.Value

quarantine
  big_endian(sequence) -> encoded QuarantinedBatch
```

为什么 pending key 使用 big-endian uint64？因为 bbolt 按字节序排序；固定宽度大端编码能让字节顺序等于数字顺序。

批次 value 可以先使用带显式 schema version 的 JSON，便于调试；吞吐成为已测瓶颈后再评估 protobuf/msgpack。无论选择什么编码，都必须：

- 拒绝未知或未来 schema；
- 对缺失必要字段报 corruption；
- 解码后验证本地 `WireSHA256`；
- 限制 value 大小，避免损坏数据触发巨量分配。

## 10. 最重要的事务

### 10.1 事务伪代码

这是本项目最值得完整实现和评审的代码之一：

```go
func (s *BoltStore) commitOnce(req CommitRequest) (StoredBatch, error) {
	var stored StoredBatch

	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		pending := tx.Bucket(bucketPending)
		checkpoints := tx.Bucket(bucketCheckpoints)

		used := decodeUint64(meta.Get(keyLogicalBytes))
		if req.LogicalBytes > s.maxBytes {
			return ErrBatchTooLarge
		}
		if used+uint64(req.LogicalBytes) > s.maxBytes {
			return errCapacityUnavailable
		}

		sequence, err := pending.NextSequence()
		if err != nil {
			return fmt.Errorf("allocate queue sequence: %w", err)
		}

		stored = freeze(req, sequence)
		batchBytes, err := encodeStoredBatch(stored)
		if err != nil {
			return fmt.Errorf("encode batch: %w", err)
		}
		checkpointBytes, err := encodeCheckpoint(req.Checkpoint)
		if err != nil {
			return fmt.Errorf("encode checkpoint: %w", err)
		}

		if err := pending.Put(sequenceKey(sequence), batchBytes); err != nil {
			return fmt.Errorf("store batch: %w", err)
		}
		if err := checkpoints.Put([]byte(req.Batch.PipelineID), checkpointBytes); err != nil {
			return fmt.Errorf("store checkpoint: %w", err)
		}
		if err := meta.Put(keyLogicalBytes, encodeUint64(used+uint64(req.LogicalBytes))); err != nil {
			return fmt.Errorf("update logical bytes: %w", err)
		}
		return nil
	})
	if err != nil {
		return StoredBatch{}, err
	}
	return stored, nil
}
```

如果 `checkpoints.Put` 失败，整个事务回滚，之前的 `pending.Put` 不可见；checkpoint 也不会推进。这正是使用事务的原因。

### 10.2 容量等待不应占着写事务

`errCapacityUnavailable` 应退出事务，然后在事务外等待容量通知：

```go
func (s *BoltStore) Commit(ctx context.Context, req CommitRequest) (StoredBatch, error) {
	for {
		batch, err := s.commitOnce(req)
		if err == nil {
			s.notifyDataAvailable()
			return batch, nil
		}
		if !errors.Is(err, errCapacityUnavailable) {
			return StoredBatch{}, err
		}

		wait := s.capacityChanged()
		select {
		case <-ctx.Done():
			return StoredBatch{}, ctx.Err()
		case <-wait:
		}
	}
}
```

不要在 bbolt write transaction 内等待 Dispatcher ACK。那会阻塞 `Ack` 自己需要的 write transaction，形成死锁。

通知实现可以用“关闭当前 channel，再创建新 channel”的 generation 模式，并由 mutex 保护。不要只用一次性非阻塞 send；多个等待者可能错过容量变化。即使收到通知，也必须重新检查容量，因为其他 Pipeline 可能先抢到空间。

### 10.3 bbolt 调用与 context

bbolt 的 `Update` 本身不是可中断 I/O。调用前检查 context，事务必须保持短小，不做 JSON 解析、网络调用或等待。若底层磁盘卡死，context 不能强行中断系统调用；shutdown deadline 只能限制上层等待，不能承诺立刻终止内核 I/O。

## 11. Checkpoint 单调性与配置变更

### 11.1 正常追加

对于同一个文件 identity，新 checkpoint offset 应不小于旧 offset。若更小，只有显式 truncate/重置状态才能允许。

在事务内校验：

```text
same identity + new offset >= old offset -> 正常推进
same identity + new offset < old offset  -> 拒绝，除非带 validated truncate transition
new identity                            -> 按轮转状态机验证
```

不要让任意调用方直接覆盖 checkpoint。可以把允许的状态转换编码成类型或命令：

```go
type Transition struct {
	From   checkpoint.Value
	To     checkpoint.Value
	Reason checkpoint.Reason // append, rotate, truncate, initial
}
```

该类型位于 `checkpoint` 包时，Store 签名中的名字就是 `checkpoint.Transition`。`append` reason 只由 `Commit` 随 batch 使用；独立 `Store.Transition` 必须拒绝它，只接受经过验证的无数据 reason。

### 11.2 Pipeline 配置变化

若用户保留 `pipeline_id`，却把 path 从 `app.log` 改成 `audit.log`，旧 checkpoint 不能静默套用。启动时比较 `SourceFingerprint`：

- 相同：恢复；
- 不同且无历史数据：按初始策略启动；
- 不同且已有 checkpoint：启动失败并给出可执行提示，要求用户改 Pipeline ID 或显式 reset。

### 11.3 无数据控制过渡

以下状态变化可能没有 batch，但仍需持久化，避免崩溃后把已经验证过的 Source 状态重新解释一遍：

- `initial`：用户明确选择从当前文件末尾开始，保存首次 anchor；
- `rotate`：旧 identity 已稳定 EOF、pending 半行已按策略处理，新 identity 已从打开的 handle 验证，保存新 identity 的 offset 0；
- `truncate`：确认同一 identity 已截短后，保存经过策略许可的 reset 状态。

它们必须使用独立的 `Transition` API、封闭的 reason 枚举和 compare-and-set 语义。审计信息要能区分“数据随 batch 推进”和“用户策略/文件状态导致的控制过渡”。

“猜一个合理 offset”会让简历项目的可靠性叙事失去依据。

## 12. 容量、文件大小与磁盘空间

### 12.1 逻辑容量

`max_bytes` 建议限制 pending batch 的逻辑大小，而不是只看 entry 数。计量规则必须稳定，例如：

```text
logical_bytes = len(stored payload) + fixed metadata estimate
```

在文档中说明它是近似的队列预算，不等于数据库文件的精确字节数。

配置至少包括：

```yaml
agent:
  spool:
    path: ./data/spool.db
    max_bytes: 1073741824
    high_watermark: 0.8
    full_policy: block
```

第一版只实现 `block` 也可以。若未来增加 `drop_oldest`，必须是用户显式选择，并记录丢失批次/entry/byte 数；它不再具备默认可靠保证。

### 12.2 为什么 DB 文件可能不缩小

KV 数据库删除 key 后通常会复用空闲页，但文件不一定立即缩小。因此至少区分：

- `gline_agent_spool_logical_bytes`：pending payload 预算；
- `gline_agent_spool_file_bytes`：数据库文件实际大小；
- 主机磁盘剩余空间：由系统指标或额外检查提供。

只限制 logical bytes 不能防止磁盘被旧页、quarantine 或其他文件耗尽。生产式实现需要磁盘低水位保护和可控 compact/备份流程；MVP 至少要把这个限制写清楚并暴露指标。

### 12.3 高水位与满载

```text
used < high watermark -> running
used >= high watermark -> warning，仍允许 commit
next batch exceeds max -> commit 等待，Pipeline backpressured
Ack 释放空间 -> 唤醒等待者并竞争重试
```

高水位是可观测状态，不应该提前丢数据。满载 readiness 失败不等于进程不存活；liveness 仍可成功。

## 13. ACK 与删除事务

`Ack` 应核对队首身份：

```go
func (s *BoltStore) Ack(ctx context.Context, seq uint64, id BatchID) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		stored, err := loadBatch(tx, seq)
		if err != nil {
			return err
		}
		if stored.BatchID != id {
			return fmt.Errorf("ack identity mismatch: %w", ErrCorrupt)
		}
		if err := deleteBatchAndDecreaseLogicalBytes(tx, stored); err != nil {
			return err
		}
		return nil
	})
}
```

收到 ACK 后本地删除失败，不要再次请求 Server 来“确认是否真的成功”。保留 batch 并在重启后重发即可；Server 的幂等约束会返回 duplicate。

## 14. Quarantine 设计

永久无效 batch 不能热重试，也不能静默删除。隔离记录至少包含：

- 原 `StoredBatch` 或其可恢复 payload；
- 稳定错误类别和 Server error code；
- 首次/最后失败时间；
- 尝试次数；
- request ID（若有）；
- 不含 token、Authorization header 或额外日志正文副本的诊断摘要。

在一个 write transaction 中将 batch 写入 quarantine 并从 pending 删除，再更新容量统计。若隔离也受 `max_bytes` 管理，要决定它是否释放 pending 容量；更实用的设计是 quarantine 有独立上限和人工导出/清理命令，防止无效数据再次堵死采集。

401/403 不应直接 quarantine 队首，因为更换凭证后相同 batch 可能成功。它属于 Dispatcher 暂停状态，详见[第七章](./07-dispatch-retry-backpressure.md)。

## 15. Schema version、迁移与单实例锁

### 15.1 首次打开

打开流程建议是：

1. 校验父目录与权限；
2. 以超时方式获取数据库文件锁；
3. 创建必要 bucket；
4. 读取 `schema_version`；
5. 新库写当前版本，旧库执行显式迁移，未来版本拒绝打开；
6. 扫描并校验 meta 统计与队首有限数据；
7. 返回可运行 Store。

第二个 Agent 实例打不开同一个 spool 时，应给出“spool 已被另一进程占用”的可执行错误，而不是无限等待。

### 15.2 迁移原则

- 迁移前可创建安全备份；
- 每一步在事务内完成；
- 不修改 batch payload，只转换外围 metadata；
- 失败保持旧版本可诊断，不自动删库重建；
- migration 测试使用真实旧 fixture。

### 15.3 durability 选项

不要为了 benchmark 开启会跳过 fsync 的选项，然后仍宣称崩溃恢复。若提供性能模式，必须改变保证说明，并在默认配置中保持 durability。

## 16. 四个关键崩溃窗口

| 崩溃点 | durable 状态 | 重启行为 | 为什么不丢/不重复写入 |
| --- | --- | --- | --- |
| Source 已读，spool 事务未提交 | 旧 checkpoint，无新 batch | 从旧位置重读 | 可能重复解析，但此前未发送 |
| batch + checkpoint 已提交，尚未 HTTP | 新 checkpoint，有 pending batch | 直接从 spool 发送 | 不需要重读文件 |
| Server DB 已提交，ACK 丢失 | 本地仍有同一 batch | 原样重试 | Server 返回 duplicate，不重复写 entries |
| 收到 ACK，本地删除前崩溃 | 本地仍有同一 batch | 原样重试 | 同上，删除只是可重复的清理动作 |

还应理解第五个边界：若应用刚写文件、Agent 尚未读到就整机磁盘损坏，Agent 没有能力保证该数据。Gline 的保证从 batch 进入本地 durable spool 开始，不是从业务进程调用日志库开始。

## 17. 分小步实现顺序

### 步骤 1：定义值类型与编码，不打开数据库

先实现并测试：

- `BatchID` 生成器接口；
- protocol DTO 到不可变 payload 的编码；
- wire bytes checksum（它与 Server 的规范化内容 hash 是两个概念）；
- checkpoint 值类型与状态转换校验；
- queue sequence key 的大端编码。

UUID 生成使用成熟库或标准能力，不手写随机格式。测试稳定合同：ID 非空/可解析、同一个 prepared batch 只生成一次；不要测试随机值恰好是什么。

### 步骤 2：实现 schema 创建与只读检查

实现 `Open`、bucket 创建、schema version 和 `Close`。此时还不接 Pipeline。

验证：新库打开/关闭/重开；未来 schema 拒绝；同路径第二实例失败；构造失败无句柄泄漏。

### 步骤 3：实现原子 `Commit`

先不做容量等待，只实现单次事务。使用故障注入让 checkpoint put 返回错误，确认 batch put 也回滚。

不要通过在生产代码中散落 `if testHook != nil` 实现故障注入。更干净的方式是把事务内部的编码/校验在事务前完成，并用一个极小 storage adapter 或测试数据库错误制造失败。

### 步骤 4：实现 `Next` 与 `Ack`

`Next` 返回最小 sequence，不删除。空队列时等待 data notification，等待可被 context 取消。`Ack` 核对 ID、删除、更新逻辑字节并通知容量等待者。

### 步骤 5：加入容量与默认 block

覆盖：单 batch 超限、多个 Pipeline 竞争、取消等待、ACK 唤醒、关闭唤醒。确保等待发生在事务外。

### 步骤 6：加入 checkpoint 恢复

Pipeline 启动时先读取 checkpoint，再让 FileSource 打开正确 identity/offset。若无 checkpoint，才使用 `start_position`。同时实现受限 `Transition`，覆盖 initial anchor、稳定 EOF 后的 rotate anchor 和 validated truncate；不要暴露任意覆盖 checkpoint 的方法。

### 步骤 7：加入 quarantine

先支持查看统计与导出，再讨论清理。没有可见性的隔离区只是更隐蔽的丢数据位置。

### 步骤 8：接入运行时并删除旧直传路径

通过编译错误逐步查找所有仍让 entry 直接进入网络 Sender 的调用。可靠模式下应只剩：

```text
Pipeline -> Store.Commit
Dispatcher -> Store.Next -> Transport.Send -> Store.Ack/Quarantine
```

## 18. 错误、取消与资源关闭

### 18.1 错误分类

| 错误 | 调用方动作 |
| --- | --- |
| `ErrBatchTooLarge` | 停对应 Pipeline 或按 batch byte limit 提前切分；不能无限 block |
| capacity unavailable | 等待 `Ack` 释放空间，可取消 |
| context canceled | 保持未提交 batch 由旧 checkpoint 重读；正常 shutdown 路径处理 |
| database I/O/corruption | 停整个 Agent，保留文件供诊断 |
| checkpoint transition invalid | 停 Pipeline/Agent 并报告状态冲突，不能覆盖 |
| schema too new | 拒绝启动，提示使用兼容版本 |

### 18.2 Close 的合同

`Close` 应幂等或至少明确重复调用结果。调用前必须保证无 Pipeline/Dispatcher 使用 Store。关闭时：

- 唤醒所有等待 data/capacity 的 goroutine，使其返回 `ErrClosed`；
- 等待已进入的短事务退出；
- 关闭数据库；
- 不删除 spool 文件；
- 不因为“正常关闭”清空 pending batch。

### 18.3 不记录敏感内容

Spool 本身保存日志正文，这是预期风险，部署文档需要求目录权限。运行日志不要额外打印完整 payload。错误上下文使用 `batch_id`、pipeline ID、entry count、byte count 和 hash 前缀即可。

## 19. 哪些代码值得完整给出

实现教学中应完整展示：

- schema 初始化与版本检查；
- `CommitBatchAndCheckpoint` 整个事务；
- 容量等待循环；
- 非破坏性 `Next` 与核对身份的 `Ack`；
- quarantine 原子移动；
- checkpoint 状态转换验证；
- `Close` 如何唤醒等待者。

普通 DTO、编码 tag 和简单 Stats getter 可以只解释字段含义，不需要大量样板代码。

## 20. 测试设计

### 20.1 单元测试

| 测试 | 保护的合同 |
| --- | --- |
| sequence key 排序 | queue 顺序稳定 |
| payload freeze | 重试读取到完全相同 bytes/hash |
| checkpoint transition | 非 truncate 情况不允许 offset 回退 |
| control transition CAS | 旧值/reason 不匹配时不覆盖 checkpoint，且不能携带已消费记录 |
| batch too large | 不会永远等待 |
| capacity wait cancel | shutdown 不挂住 |
| ACK identity mismatch | 不删除错误 batch |

### 20.2 真实数据库集成测试

不要 mock 掉事务：

1. `t.TempDir()` 创建 spool；
2. commit 两批并关闭；
3. 重开后按 sequence 读取；
4. checkpoint 等于第二批末尾；
5. ACK 第一批并再次重开；
6. 只剩第二批；
7. logical bytes 与实际 pending 内容一致。

另建原子回滚测试：让事务在 batch put 之后返回错误，重开数据库确认 batch 和 checkpoint 都没有变化。再分别提交 initial/rotate/truncate 控制过渡，验证 compare-and-set、reason 白名单、回滚和 reopen 后状态；任意 forward offset 或夹带记录的调用必须被拒绝。

### 20.3 子进程崩溃测试

正常 `Close` 不是 crash。高价值测试可使用测试二进制的 helper-process 模式：

- 子进程 commit 成功后通过 stdout/文件信号通知父进程；
- 父进程强制终止子进程；
- 新进程打开同一 spool，验证 batch/checkpoint；
- 测试只操作 `t.TempDir()`，绝不指向真实用户 spool。

“在 commit 中间 kill”不容易稳定定位，而且数据库事务本身应保证未 commit 不可见。可以通过事务回滚测试保护原子性，用 commit 后 kill 测 durability；不要为了制造精确纳秒窗口在生产代码加入复杂 hook。

### 20.4 端到端故障测试

与 Server 合并后覆盖：DB commit 后断开响应、Agent 重启、duplicate ACK、本地删除。用带 `run_id` 和连续序号的合成日志检查集合差异，不能只比较总行数。

## 21. 验收命令与证据

依赖应由包管理器加入。实际包名落定后执行类似：

```powershell
go mod tidy
gofmt -w .\internal\agent\spool .\internal\agent\checkpoint
go test ./internal/agent/spool ./internal/agent/checkpoint -count=1
go test -race ./internal/agent/spool ./internal/agent/checkpoint -count=1
go test ./internal/agent/... -count=1
go vet ./internal/agent/...
```

验收证据至少包括：

- commit/reopen 测试；
- batch 与 checkpoint 回滚一致性测试；
- 同一 batch payload/wire checksum 重启后不变；
- 小容量下默认阻塞、ACK 后恢复；
- 等待能被 context 和 Close 唤醒；
- schema 不兼容时拒绝启动；
- DB commit 后 ACK 丢失的端到端重复上传最终只写一份。

## 22. 常见错误

1. **spool 和 checkpoint 各写一个文件。** 两次成功之间崩溃无法原子恢复。
2. **先写 checkpoint 再异步写 batch。** 这是永久丢失窗口。
3. **Dispatcher 从内存 batch 发送，失败才落盘。** Server 提交后本地落盘前崩溃会破坏身份稳定性。
4. **每次重试重新 marshal 并更新 `sent_at`。** 相同 ID 产生不同 payload hash。
5. **HTTP timeout 后生成新 ID。** Server 若已提交，会写出重复日志。
6. **容量不足时在 write transaction 内等待。** ACK 无法获得写事务释放空间。
7. **只统计数据库文件大小。** 删除后文件可能不缩，且不能准确代表 pending 队列预算。
8. **只统计逻辑大小。** 无法防止真实磁盘被数据库页或其他文件耗尽。
9. **遇到 corruption 自动删库。** 这会把可诊断的数据损坏变成确定的数据丢失。
10. **把 401 放 quarantine。** 修复 token 后本可成功的数据被错误隔离。
11. **让多个 Agent 共用一个 spool。** 文件锁、Agent identity 与 queue sequence 的语义都会混乱。
12. **为了吞吐关闭 fsync。** benchmark 变快，但保证已经改变。

## 23. 复盘题

1. 为什么 batch 与 checkpoint 必须在同一个事务，而不仅是“先后紧挨着写”？
2. batch ID、local queue sequence、entry sequence 各解决什么问题？
3. 为什么 Dispatcher 重试时应读取已保存的 payload bytes，而不是重新构造 DTO？
4. `sent_at` 为什么可能破坏幂等？你会如何命名或冻结它？
5. 为什么容量等待不能发生在 bbolt write transaction 内？
6. 收到 ACK 后本地删除失败，重启会发生什么？为什么仍然正确？
7. logical bytes 与 spool file bytes 为什么不同？分别用于什么告警？
8. Pipeline path 改变但 ID 不变时，为什么不能静默复用 checkpoint？
9. `copytruncate` 后 offset 回退应怎样通过状态转换表达？
10. 正常关闭测试为什么不能替代进程崩溃测试？

## 24. 进入下一章前的完成条件

- [ ] batch + checkpoint 的单事务代码已经实现并经过回滚测试；
- [ ] initial/rotate/truncate 控制过渡使用独立 CAS API，并有崩溃恢复测试；
- [ ] durable batch 包含稳定 ID、稳定 payload 和 hash；
- [ ] 重启后 `Next` 返回相同 batch bytes；
- [ ] `Ack` 核对 queue sequence 与 batch ID；
- [ ] 默认容量策略为 block，等待可取消；
- [ ] 单 batch 超限不会永久阻塞；
- [ ] quarantine 是原子移动且具备可见统计；
- [ ] schema version、单实例锁和不兼容错误清晰；
- [ ] spool corruption 不会触发自动清空或内存直传；
- [ ] 能解释并验证四个关键崩溃窗口。

下一章将让文件 Source 产生可信的 identity 与 checkpoint：[06. 文件 Tail 与轮转](./06-file-tail-rotation.md)。
