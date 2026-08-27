# 附录：术语表

这份术语表给教程和设计文档提供统一语言。定义描述 Gline 中的具体含义，不试图替代通用教材。

## A

### ACK

Agent 可以据此删除本地未确认 batch 的 Server 响应。MVP 中只有 PostgreSQL 事务提交成功，或确认相同 batch 已经提交，才可以返回 ACK。接收 JSON、放入内存 channel 或开始 SQL 事务都不是 ACK。

### Agent

部署在日志产生节点的进程，负责 Source、Parser、Enricher、Batch Builder、Spool 和 Dispatcher。Agent 不决定 Project；Project 由 Server 根据 API Key 确定。

### At-least-once delivery

至少一次传输。遇到不确定失败时 Agent 会重试，因此同一 batch 可能到达 Server 多次。它不等于数据库一定有重复，Server 可以通过幂等实现有效单次写入。

### Attributes

日志 entry 的受限动态 JSON object。必须限制总大小、深度、key 数和字符串长度；不是任意无限 Schema，也不应默认全部建立索引。

## B

### Backpressure

下游消费速度低于上游生产速度时，系统把压力向上游传播的机制。Gline 依次表现为发送变慢、spool 增长、达到上限后阻塞 Source。静默 drop 不是默认背压策略。

### Batch

Agent 作为不可变单位持久化、重试和确认的一组 entries。首次写入 spool 时获得 batch ID；重试不能改变内容、顺序或 ID。

### Batch ID

一个 Project 内的幂等身份。Server 使用 `(project_id, batch_id)` 唯一约束。它不是每次 HTTP attempt 的 request ID。

### BRIN

PostgreSQL Block Range Index。体积小，适合与物理写入顺序相关的时间列做大范围筛选，但精确过滤能力弱于 B-tree。Gline 可在 `ingested_at` 上评估使用，必须由查询计划验证。

### B-tree

PostgreSQL 常用有序索引。Gline 使用复合 B-tree 支持 project + service/level + observed time + id 的过滤和 keyset 排序。

## C

### Checkpoint

Source 已经安全交给本地持久化边界的位置，通常包含文件 identity 和 byte offset。凡消费了日志记录，Gline 都在 batch 与 checkpoint 同一 spool 事务提交后推进，而不是等待 Server ACK。没有消费新记录的 initial/rotate/truncate anchor 只能通过受限、可审计的控制过渡保存。

### ClickHouse

可作为未来日志查询存储的列式数据库。它不是 MVP 默认依赖；只有 PostgreSQL 的写入、查询或 retention 在目标负载下出现经过验证的瓶颈时才评估。

### Cursor

Query API 返回的不透明分页位置。Gline cursor 表示上一页最后一条的 `(observed_at, id)` 及版本信息，客户端不应解析或构造其内部字段。

## D

### Data transfer object (DTO)

外部协议的请求/响应类型，位于版本化 protocol 层。DTO 反映 JSON 兼容合同，不直接承担领域不变量或数据库扫描细节。

### Dispatcher

从 spool 读取不可变 batch、执行 HTTP attempt、分类响应、安排退避、ACK 删除或 quarantine 的 Agent 组件。

### Domain model

已经过协议解码、鉴权上下文注入和规范化的业务类型。例如 Server domain batch 已经带可信 project ID，但不包含 HTTP Header 或 SQL row 细节。

## E

### Effective-once write

本教程对“至少一次传输 + 幂等落库”结果的描述：网络可能重复交付，但相同 batch 的重复请求只产生一份有效数据。它不是对所有系统边界宣称 exactly-once。

### Entry

一条规范化日志事件，包括 observed time、level、service、host、message、attributes 以及 Agent/Pipeline 来源信息。

### Event time / Observed time

日志在来源处产生或被 Agent 观察的时间。客户端时钟可能错误，不能用于所有服务端运维判断。

## F

### File identity

区别文件实体而非只看路径的标识。Unix 和 Windows 实现不同，应由平台 adapter 提供。rename 后路径可以变化但旧 identity 仍存在；recreate 后同一路径可能是新 identity。

### Full jitter

指数退避每一轮在 `[0, cap]` 或既定范围内随机等待，避免多个 Agent 同时恢复造成同步重试峰值。实现必须可取消并有最大上限。

## H

### HMAC / Pepper

Server 使用秘密 pepper 对高熵 API secret 计算消息认证码，并在验证时常量时间比较。Pepper 位于环境或 secret storage，不进入数据库或仓库。API Key 不是用户密码，不需要机械套用密码哈希流程。

### High cardinality

metric label 可能值数量很大。例如 batch ID、request ID、message 和 project ID。高基数会显著增加指标系统成本，因此这些值适合日志/trace 字段，不适合作为默认 metric label。

## I

### Idempotency

重复执行同一请求得到等效结果。Gline 用 Project + batch ID 唯一约束和 payload hash：相同内容返回 duplicate，不同内容返回 conflict。

### Ingested time

Server 将 entry 持久化的时间，由 Server 生成。retention 和服务端运维通常使用它，避免客户端错误时钟影响保留策略。

### Integration test

跨越真实相邻边界的测试，例如真实 HTTP client→router→Handler，或 Repository→真实 PostgreSQL。它比纯 mock 更能发现协议、SQL、序列化和配置问题，但不必启动整个产品。

## K

### Keyset pagination

基于上一页最后一个有序键继续查询，而不是跳过 offset 行。Gline 使用 `(observed_at DESC, id DESC)`，使深分页成本和并发写入下的稳定性更可控。

## L

### Liveness

回答进程是否仍能响应、是否需要被重启。Server liveness 不依赖 PostgreSQL；外部依赖短暂失败应影响 readiness，而不是触发重启风暴。

## M

### Migration

版本控制的数据库 Schema 变化。空数据库必须能按顺序升级；Server readiness 检查当前 Schema 是否兼容。迁移不是启动时随意执行的建表字符串。

### Modular monolith

一个部署进程中的多个清晰模块。Gline Server 的 auth、ingest、query 和 health 通过包边界与窄接口隔离，但共享进程和 PostgreSQL 事务，不提前承担微服务网络成本。

## O

### OpenAPI

Server 外部 HTTP 合同的机器可读描述。它需要版本控制和 CI 校验，但不能代替行为集成测试。

## P

### Payload hash

对规范化 batch payload 计算的稳定摘要。相同 batch ID 再次到达时，Server 用它判断是合法重复还是 ID 冲突。必须定义规范化方式，不能依赖 JSON map 的偶然排列。

### Pipeline

Agent 中一个 Source + Parser + 固定来源上下文的运行单元。一个 Pipeline 失败默认不停止其他 Pipeline，但必须暴露部分失败状态。

### Project

Server 的数据和权限隔离边界。Project 由 API Key 决定，不接受客户端在上传 body 或查询参数中任意指定。

### pprof

Go 的 CPU、heap、goroutine、block 和 mutex 等诊断工具。Gline 默认关闭或限制在独立管理地址，只在受控环境启用。profile 用于证据驱动优化，不是公开产品接口。

## Q

### Quarantine

保存永久失败 batch 及其原因的本地隔离区。例如验证失败、幂等冲突或无法拆分的超大单条 entry。它避免热重试和静默删除，并需要容量上限和人工处理方式。

## R

### Readiness

回答实例当前是否应接收流量。Server readiness 检查数据库和迁移兼容；Agent readiness 可反映 spool 满、凭证失效或关键 Pipeline 状态。

### Repository

领域模块定义的窄持久化接口，以及 PostgreSQL adapter 对它的实现。不要建立包含全系统所有方法的巨大 `Store`，也不要把数据库 row 直接暴露给 Handler。

### Request ID

一次 HTTP attempt 的诊断 ID。一个 batch 重试会产生多个 request ID，但 batch ID 保持不变。

### Retention

按策略删除过期日志。MVP 使用 `ingested_at` 和有界小批删除；未来分区后可以删除过期分区。Retention 必须有成功时间、删除数量和错误指标。

## S

### Scope

API Key 的授权能力。Gline 首版至少有 `ingest` 和 `query`；Agent Key 默认只拥有 ingest。

### Source

产生 RawRecord 的 Agent 组件。FileSource 负责文件读取和位置，不负责解析、网络重试或 Server ACK。

### Spool

Agent 本地持久化的未确认 batch 队列，同时保存 checkpoint 和 quarantine 元数据。它是可靠性边界，不只是内存溢出时才使用的临时文件。

### Stable contract

由协议、领域不变量、安全边界或明确产品要求定义、应受到测试保护的行为。当前实现细节和暂时策略不自动成为稳定合同。

## T

### Transaction boundary

一组必须一起成功或一起失败的状态变化。Agent 中主要是 batch + checkpoint 的 spool 事务，也包括 compare-and-set 的无数据 checkpoint 控制过渡；Server 中是 ingest batch metadata + entries 的 PostgreSQL 事务。

## W

### WAL

Write-Ahead Log。PostgreSQL 和一些嵌入式数据库内部使用 WAL 保证事务。教程不建议首版自行发明 Agent 二进制 WAL；使用成熟事务存储实现 spool。

## 测试术语

### Unit test

验证一个小范围稳定合同，不访问真实外部系统。适合 retry 分类、cursor 编解码、配置校验和状态转换。

### End-to-end test

从用户输入跨越完整系统到用户可见结果。例如写日志文件，最终从 Query API 获取。E2E 数量应少而关键。

### Fault injection

在可控位置制造崩溃、网络失败、响应丢失或存储不可用，验证恢复合同。测试 failpoint 不能成为生产可远程触发的后门。

### Race test

使用 Go race detector 执行测试。它只能说明被执行路径没有检测到数据竞争，不证明所有路径无 race，也不证明不存在死锁或泄漏。

### Smoke test

以较低成本确认程序能够启动、响应关键入口并正常退出。Smoke 成功不等于完整业务和故障路径已经验收。
