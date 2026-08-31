# 00. 后端系统愿景：把 Gline 做成一个可运营的日志管理后端

本章先回答一个关键问题：**Gline Server 最终到底是什么后端系统**。后面的章节会逐个实现它，但如果没有统一的产品边界、领域语言和完成标准，很容易在“上传日志”这个局部功能上反复加字段，最后仍然像一个 CRUD 服务。

本教程的目标不是把组件名称堆到简历上，而是让你可以从空白的当前基线，逐阶段实现一个能够解释以下问题的后端项目：

* 谁可以把数据写进来，写入的数据属于哪个项目？
* Agent 离线、Server 崩溃、响应丢失时，数据如何恢复？
* 为什么同一个批次重试不会产生重复日志？
* Project、Agent、Pipeline、Batch、Entry 的生命周期和权限边界是什么？
* 查询为什么不会因为深分页拖垮数据库？
* 失败批次如何隔离、修复、重放并留下审计记录？
* 当单机容量或吞吐不足时，哪些指标证明应该水平扩展？

## 1. 实现前基线

> 本节保留实现开始前的差距表，用来说明为什么要按可靠性、控制面和
> 数据边界分阶段建设。当前实现状态请以根目录 `STATUS.md` 为准。

实现前仓库已经有 Agent 生命周期、Source/EntrySink 抽象和一个非常早期的上传 HTTP 入口，但 Server 还没有形成完整业务闭环。下表保留这份历史快照，不代表当前工作树：

| 区域 | 当前状态 | 目标状态 |
| --- | --- | --- |
| Server 进程 | 能启动基础 HTTP 入口，部分 Handler/上传模块存在 | 模块化单体，拥有控制、接入、查询、运维四个平面 |
| 鉴权 | 原型级 API Key/中间件边界 | Key 哈希存储、Project 上下文、Scope、轮换/吊销/审计 |
| 接入 | 上传路由和协议方向已确定 | PostgreSQL 事务幂等写入，commit 后 ACK |
| 数据库 | 尚未成为可迁移、可查询的稳定合同 | migrations、repositories、约束、索引和 retention |
| 查询 | 设计中 | 有界过滤、keyset cursor、权限隔离、导出限制 |
| 控制平面 | Project 概念存在，管理 API 未完成 | Project/Key/Agent/Pipeline 生命周期 |
| 运维 | 健康/指标边界已出现 | readiness、metrics、quarantine/replay、usage、audit |
| 扩展 | 无证据，不讨论拆分 | 先完成单体证据，再按实测演进到水平扩展和高可用 |

第一阶段的任务不是“把所有表建出来”，而是先建立可以失败、重试和恢复的后端主链路。每一阶段都要有运行证据，而不是只提交接口定义。

## 2. 最终定位

### 2.1 产品定位

Gline 是一个面向个人项目和小型服务集群的自托管日志管理后端。它允许多个项目接入来自多个 Agent 的结构化日志，并提供隔离检索、生命周期治理和运行状态观察。

### 2.2 工程定位

Gline Server 是一个**多租户日志管理后端**，由四个逻辑平面组成：

```text
Gline Server
├── Control Plane       项目、凭证、Agent 和配置生命周期
├── Ingest Plane        批次接入、幂等、限额和失败隔离
├── Query Plane         有界检索、游标分页和导出
└── Operations Plane    retention、usage、audit、health、metrics、replay
```

Agent 是边缘客户端，不是 Server 内的一个“后台线程”。它运行在产生日志的机器上，负责文件读取、轮转识别、checkpoint、持久化 spool 和网络投递。Agent 与 Server 之间的 HTTPS 批次协议是一个真实的分布式故障边界。

### 2.3 简历定位

在核心闭环完成后，可以这样描述项目：

> 使用 Go 设计并实现一个面向小型服务集群的多租户日志管理后端。边缘 Agent 通过文件 identity、checkpoint 和持久化 spool 实现可恢复采集；Server 采用模块化单体，提供 Project/Scope 鉴权、版本化批次协议、PostgreSQL 事务幂等写入、keyset 查询、retention、quarantine/replay 和运行指标；通过故障注入、race 检测和基准测试验证网络失败、进程重启、重复投递与查询性能行为。

不要在尚未实测之前写“百万日志每秒”“高可用”“零丢失”之类的结论。简历中只写已经有测试或压测报告支撑的数字；其余写设计目标和已覆盖的故障场景。

## 3. 用户和使用体验

后端模块要围绕真实使用流程组织，而不是围绕表名组织。最终用户可以是项目维护者、Agent 进程和运维人员。

### 3.1 项目维护者的主流程

```text
创建 Project
  -> 创建带 ingest/query Scope 的 API Key
  -> 在一台机器注册 Agent
  -> 配置日志路径、service 和 pipeline
  -> 查看 Agent 在线状态及积压
  -> 查询某服务的错误日志
  -> 发现失败批次并检查 Quarantine
  -> Replay 或标记已处理
  -> 设置 retention 和项目配额
```

操作应该有明确的结果：创建 Key 只在响应中显示一次明文；之后只能看到前缀和状态。禁用 Project 后，所有属于该 Project 的 Key 立即失效。删除日志要受到 retention 或显式运维权限约束，并记录 Audit Event。

### 3.2 Agent 的主流程

Agent 并不创建 Project，也不自行决定 `project_id`。它使用 API Key 认证，Server 从认证上下文得到 Project。Agent 先把不可变 Batch 写入本地 spool，再推进 checkpoint；Server 只有在 PostgreSQL 事务提交成功后才返回 ACK。响应丢失时，Agent 使用相同的 `batch_id` 和 payload 重试。

### 3.3 运维人员的主流程

运维人员能够区分三种状态：

* **Live**：进程能响应基础探活。
* **Ready**：依赖（至少 PostgreSQL）已连接且可以接受业务流量。
* **Degraded**：进程仍在运行，但某个后台任务、数据库连接池或配额窗口不可用。

他们可以查看每个 Project 的接入量、失败量、query latency、spool backlog 和 Agent 最近心跳，但指标和日志不能泄露原始 API Key 或日志正文。

## 4. 四个后端平面

### 4.1 Control Plane

控制平面管理“谁在使用系统以及如何使用”：

* Project 创建、禁用和元数据；
* API Key 创建、哈希存储、轮换、吊销和 Scope；
* Agent 注册、心跳、版本和最后状态；
* Pipeline 配置版本和启用/暂停；
* 后续阶段的 quota 与 retention policy。

控制平面不接收高吞吐日志，不把日志正文放进控制请求。

### 4.2 Ingest Plane

接入平面处理高频数据写入：

* 版本化 `POST /api/v1/batches`；
* body、entry count、timestamp 和 metadata 校验；
* Project/Scope 已在认证上下文中确定；
* `(project_id, batch_id)` 幂等键和 payload hash 冲突检测；
* Batch 与 Entry 同一 PostgreSQL 事务；
* commit 后 ACK；
* 配额、限流和永久失败隔离。

接入服务不直接解析文件，也不负责 Agent 的重试等待。

### 4.3 Query Plane

查询平面面向人和工具：

* 时间范围、service、level、source 和关键词等过滤；
* 参数化 SQL；
* `(observed_at, id)` keyset pagination；
* 单页最大条数和最大时间窗口；
* 查询超时、并发限制和审计；
* 后续加入导出任务，而不是让 HTTP 请求无限持有连接。

### 4.4 Operations Plane

运维平面处理不会由单个 HTTP 请求完成的事情：

* retention 小批量删除；
* usage 按 Project 聚合；
* Agent 离线状态判定；
* quarantine 查看和 replay；
* health、metrics、pprof（只在受控环境启用）；
* migrations、备份和发布检查。

## 5. 分阶段路线

不要一开始拆微服务。按下面的阶段实现，每阶段结束都应有可以演示的垂直切片。

### 阶段 A：核心可靠闭环

目标是证明“一个 Agent 能把日志可靠地写入一个 Server 数据库”：

1. 固化批次协议 DTO、错误码和版本；
2. 建立 `projects`、`api_keys`、`ingest_batches`、`log_entries`；
3. API Key 认证得到 Project/Scope；
4. 接入事务在 commit 后 ACK；
5. 重复相同批次返回 duplicate，不重复写 Entry；
6. Query API 使用 keyset cursor；
7. retention 任务可安全重跑；
8. live/readiness/metrics 可观察；
9. Agent spool、checkpoint 和 dispatcher 与 Server 联调。

阶段 A 的面试重点是事务边界、幂等、重试和恢复，而不是功能数量。

### 阶段 B：平台后端能力

目标是让 Server 具备可运营的控制平面：

1. Project CRUD 与禁用状态；
2. API Key 一次性展示、轮换、吊销和过期；
3. Agent 注册和心跳；
4. Pipeline 状态、配置版本和错误摘要；
5. Quarantine 与受控 Replay；
6. Audit Event；
7. Project quota、查询限制和 usage；
8. Admin CLI 或 Admin API。

阶段 B 的面试重点是领域生命周期、权限、审计和后台任务的资源控制。

### 阶段 C：交付与质量

目标是任何人按 README 都可以启动并复现：

* Docker Compose 提供 PostgreSQL 和 Server；
* migration 在启动或独立命令中可控执行；
* CI 执行 format、test、race、vet、构建和 migration smoke；
* 故障注入覆盖数据库失败、ACK 丢失、重复批次和 spool 满；
* 有基准测试报告和资源边界；
* 配置、秘密、备份和升级说明完整。

阶段 C 的面试重点是可重复交付和证据，而不是“本地能跑”。

### 阶段 D：实测驱动的水平扩展和高可用

只有阶段 C 之后，才根据数据决定：

* 多个无状态 Server 实例挂在负载均衡器后；
* PostgreSQL 主从/托管高可用；
* 接入和查询分离扩缩容；
* 按时间分区或归档；
* 在数据库写入成为瓶颈且需要独立消费者时引入 Kafka/NATS；
* 日志规模和聚合查询证明 PostgreSQL 不再合适时评估 ClickHouse。

每次扩展必须先写 ADR：瓶颈证据、替代方案、数据一致性变化、回滚方案和新增运维成本。

## 6. 为什么不是“微服务越多越高级”

Server 先做模块化单体，是因为当前问题的主要难点是可靠写入和领域一致性，不是组织边界或独立扩缩容。Ingest、Query、Control 和 Operations 可以拥有清晰的包边界，同时共享一个事务抽象和 PostgreSQL schema。现在拆进程会立即引入：

* API Key 和 Project 状态的跨服务读取；
* 批次接收、入队、落库的分布式确认语义；
* 本地开发和集成环境的额外组件；
* 版本、超时、重试和部署矩阵。

模块化单体不是永远不拆，而是把拆分时机交给指标。真正值得在面试中讲的是：你知道什么边界已经存在，也知道什么边界尚未被证据证明需要网络化。

## 7. 前置知识

开始实现前，应能读懂：

* Go interface、context、goroutine、channel 和 `database/sql`；
* HTTP 方法、状态码、请求体限制和中间件；
* PostgreSQL 事务、唯一约束、索引和隔离级别；
* HMAC/哈希、API Key 生命周期和最小权限；
* 分布式系统中的 at-least-once、幂等和故障窗口。

不熟悉时，先回看旧教程中的并发和协议章节，再进入本教程的领域实现。

## 8. 建议目录

目标代码布局如下。可以分阶段搬迁，不要为了目录树一次性重写所有文件：

```text
cmd/
  server/
internal/
  protocol/ingestv1/
  server/
    bootstrap/
    httpapi/
    control/
    ingest/
    query/
    operations/
    auth/
  storage/postgres/
  platform/
    logging/
    metrics/
migrations/
deployments/compose/
```

模块只依赖向内的领域接口；`cmd/server` 负责组装具体 adapter。不要让 Handler 直接 import PostgreSQL driver，也不要让数据库 row 结构体成为 JSON 响应。

## 9. 实现顺序

按以下顺序推进，每次只完成一个可运行切片：

1. 读当前代码、锁定基线和依赖；
2. 建立领域 ID、状态枚举和错误分类；
3. 实现 Project/API Key 认证上下文；
4. 写 migration 与 repository contract；
5. 实现 Ingest 事务和重复批次测试；
6. 实现 Query keyset 与 Project 隔离测试；
7. 实现 Agent/Agent Pipeline 控制平面；
8. 实现 quarantine/replay、retention、usage、audit；
9. Compose/CI/故障注入/基准测试；
10. 基于结果决定水平扩展或高可用方案。

每完成一个切片，记录“实现、验证、集成、可运行、已接受”中的证据等级。代码存在不等于功能已经完成。

## 10. 测试和验收证据

最小证据矩阵：

| 能力 | 必须有的证据 |
| --- | --- |
| Project 隔离 | 两个 Project 的 Key 互查得到 401/403 或空结果，且 SQL 带认证上下文 |
| 幂等 | 同一 `batch_id` 重试两次，Entry 只有一份；payload 不同返回 409 |
| commit 后 ACK | 注入 commit 前数据库错误，客户端不会收到成功 ACK |
| 查询 | 多页结果无重复、无遗漏；cursor 不接受跨 Project 复用 |
| retention | 到期数据删除，不到期数据保留；重复执行安全 |
| control | Key 吊销立即失效；Agent 心跳更新状态；Audit 有操作者和对象 |
| 失败恢复 | Server 重启后 Agent spool 继续投递；永久失败进入 quarantine |
| 资源边界 | body、limit、时间窗口、连接池、后台队列均有上限 |
| 并发 | `go test -race ./...` 通过，并发重复接入不破坏唯一约束 |

## 11. 常见坑

* 把 `project_id` 放在请求 JSON 里并信任客户端，造成租户越权；
* 返回 202 后把批次放入内存 channel，进程崩溃即丢数据；
* 用 `batch_id` 唯一约束却不比较 payload hash，导致重用 ID 的数据被静默接受；
* 用 `OFFSET` 做深分页，数据写入期间出现重复或跳过；
* 让 retention 直接 `DELETE` 全表，长事务锁住接入；
* 把 API Key 明文写数据库、日志或指标标签；
* 先做微服务、Kafka、ClickHouse，反而没有一条可证明的可靠写入链路；
* 把“有 health endpoint”误认为“依赖可用”；
* 把当前阶段的吞吐数字写进简历，却没有固定数据集和测试环境。

## 12. 复盘题

1. 如果客户端在 PostgreSQL commit 成功后、收到响应前断网，重试会发生什么？为什么不会重复写入？
2. 为什么 Project 必须来自认证上下文，而不是 Batch 中的字段？
3. 为什么 Control Plane 和 Ingest Plane 逻辑分开，但第一版不拆进程？
4. 哪一个指标会证明 Query 需要单独扩容？哪一个指标只能说明 SQL 需要优化？
5. 引入消息队列后，ACK 应该确认“入队”还是“落库”？这会改变谁的恢复责任？

## 13. 本章完成门

读者只有在能够口述下面这段话后，才进入下一章：

> Gline Server 是 PostgreSQL-first 的模块化多租户后端。认证上下文决定 Project 和 Scope；Agent 发送不可变批次；Server 在同一事务中写入 Batch 与 Entry，commit 后才 ACK，并用 `(project_id,batch_id)` 与 payload hash 实现幂等。Control 管理 Project、Key、Agent 和 Pipeline，Query 提供有界 keyset 检索，Operations 负责 retention、usage、quarantine、audit 和观测。第一版不拆微服务，水平扩展和高可用只在实测瓶颈后进入最终阶段。

