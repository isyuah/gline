# 术语表

### Control Plane

管理 Project、API Key、Agent、Pipeline、配置版本和审计状态的后端模块。它决定“谁可以接入、谁可以查询、哪些 Agent 属于哪个 Project”。

### Data Plane

承载日志实际流动的路径。Gline 中包含 Agent 采集/投递和 Server ingest/query；它决定“数据如何进入、持久化、查询和恢复”。

### Project

Gline 的隔离租户边界。API Key 属于 Project，Batch 和 Entry 也必须带 Project 维度。客户端不能通过 body 或 query 参数覆盖认证上下文中的 Project。

### API Key

用于 Agent 或查询客户端访问 Server 的机器凭证。secret 只在创建成功后显示一次，数据库保存带 pepper 的 HMAC 摘要，不保存 plaintext。

### Agent

运行在日志产生端的边缘客户端。负责文件读取、解析、批处理、spool、checkpoint、重试和背压，不负责 Project 权限或数据库事务。

### Batch

一次上传的不可变日志集合。Batch 首次进入 spool 时生成稳定 ID，之后的重试必须使用相同 ID、entry 顺序、payload bytes 和 hash。

### Idempotency

重复执行同一业务请求得到等效结果。Gline 使用 `(project_id, batch_id)` 唯一约束和 Server 规范化 payload hash：相同内容为 duplicate，不同内容为 conflict。

### ACK

允许 Agent 删除本地 pending batch 的 Server 响应。只有 PostgreSQL ingest transaction commit 成功，或 Server 已确认相同 batch 已经提交，才能返回 ACK。

### Spool

Agent 本地有界持久队列。它保存尚未得到 Server ACK 的不可变 batch，并与 checkpoint 共同构成进程崩溃后的恢复边界。

### Checkpoint

表示 Source 数据已经安全交给本地 spool 后可以从哪里恢复的位置。消费记录时必须随 batch 同一事务推进；initial、rotate、truncate 只允许使用受限、可审计的控制过渡。

### Quarantine

无法按原 payload 自动成功投递、但又不应静默删除的批次隔离区。Quarantine 记录稳定错误、原始 batch 身份和处理时间，后续可人工修复或 Replay。

### Keyset Pagination

使用排序键继续查询下一页的方法。Gline 使用 `(observed_at DESC, id DESC)`，cursor 保存上一页末尾键，避免深 offset 的扫描和页漂移。

### Retention

按明确时间策略删除过期日志和幂等 metadata 的后台任务。删除使用小批量、短事务、可取消和可观测方式，不用一个无限大的 DELETE。

### Usage

按 Project 或有限维度汇总接入量、字节数、查询量和错误量的统计。Usage 用于配额、审计和容量判断，不应把高基数 ID 直接放进 Prometheus label。

### Modular Monolith

一个进程内包含多个清晰模块的 Server。模块通过窄接口隔离，但共享部署和数据库事务；它不是“没有架构”，也不是已经拆成微服务。

### Horizontal Scaling

增加多个无状态 Server 副本并通过负载均衡分发请求。前提是 Server 不依赖本地业务文件、migration 兼容、readiness 正确、数据库连接池和幂等事务经过验证。

### High Availability

在明确故障模型、冗余拓扑、恢复流程和验收证据后，系统在部分组件故障时仍能提供约定服务。多副本进程本身不等于 HA；数据库、迁移、备份、网络和观测同样属于可用性边界。

