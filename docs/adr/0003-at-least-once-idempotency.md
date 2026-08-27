# ADR-0003：至少一次传输与幂等写入

- 状态：建议采纳
- 日期：2026-08-23

## 背景

Agent 通过不可靠网络向 Server 上传日志。请求超时可能发生在数据库提交之前或之后，Agent 无法仅凭超时判断 Server 是否已经写入。如果失败后不重试，会丢数据；如果生成新请求重试，会产生重复。

## 决策

- Agent 在 batch 首次写入本地 spool 时生成稳定 `batch_id`。
- 同一 batch 的所有重试使用相同 payload、batch ID 和 entry sequence。
- Server 在 `(project_id, batch_id)` 上建立唯一约束，并保存规范化 payload hash。
- batch 与 entries 在一个 PostgreSQL 事务中提交。
- 相同 ID + 相同 hash 返回 200 duplicate；相同 ID + 不同 hash 返回 409。
- Server 仅在事务提交后返回成功。
- Agent 收到 accepted/duplicate 后删除 spool 中的 batch。

系统语义表述为“至少一次传输，有效单次写入”，不宣称端到端 exactly-once。

## 原因

- 正确处理“服务端已提交但响应丢失”的经典不确定状态。
- 唯一约束避免先查后写竞态。
- batch 级幂等比每条日志单独网络确认更高效。
- 语义可以通过重复请求和故障注入测试证明。

## 备选方案

### 最多一次：失败不重试

实现简单，但网络抖动直接造成日志丢失，不符合项目的可靠采集目标。

### 每条 entry 使用全局 event ID

可以提供更细粒度去重，但增加协议、索引和存储成本。MVP 使用 batch ID + sequence 已足够；跨 batch 合并不是当前需求。

### 分布式事务或 exactly-once 消息系统

复杂度与当前规模不匹配，而且不能消除 Source 到 Agent 本地状态等全部边界的不确定性。

## 影响

正面：网络重试安全，确认边界明确，故障测试容易设计。

负面：

- 需要保存 batch metadata 和 payload hash；
- batch ID 不能被重用；
- 幂等记录需要与日志保留策略协调；
- 永久失败 batch 需要 quarantine 和人工可见性。

## 必须保持的合同

- 进入 spool 后 batch payload 不可变。
- HTTP timeout 后使用同一 batch ID 重试。
- duplicate 与 accepted 都允许 Agent 删除本地 batch。
- 409 不能自动生成新 ID 绕过，否则会掩盖数据损坏并产生重复。
- 清理 idempotency metadata 前，必须保证不会再收到对应 batch 的合法重试，或明确缩短保证窗口。

