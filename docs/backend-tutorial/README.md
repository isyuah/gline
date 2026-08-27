# Gline 后端项目完整实现教程

这套教程以 Gline Server 为主角，把 Agent 视为运行在边缘节点上的可靠客户端。目标不是再做一个“日志上传 CRUD”，而是一步一步实现一个可以作为后端简历主项目的多租户日志管理后端：它有控制平面、数据接入平面、查询平面、后台任务、可观测性、交付流程，以及最后才引入的水平扩展和高可用能力。

> 状态提醒：本目录描述的是新的后端中心目标和实现路线。章节中的代码、SQL、配置和命令骨架，除非明确标注“当前源码”，都需要你在后续阶段实现并验证。当前工作树事实仍以 [现状评估](../01-current-state-assessment.md) 和源码为准。

## 1. 最终系统定位

最终的 Gline Server 是：

> 面向小型服务集群的自托管、多租户日志管理后端，提供 Agent 注册与状态管理、版本化批次接入、幂等事务写入、受限检索、保留清理、失败批次隔离与重放，以及可证明积压、恢复和性能行为的运维接口。

Agent 负责边缘文件采集、checkpoint、持久化 spool 和网络投递；Server 负责业务边界、租户隔离、数据持久化、查询、控制和运行治理。

最终后端包含四个逻辑平面，但第一阶段仍部署为一个模块化单体：

```text
Control Plane
  Project / API Key / Agent / Pipeline / Audit

Ingest Plane
  Batch Protocol / Validation / Idempotency / PostgreSQL Commit

Query Plane
  Filter / Keyset Pagination / Query Limits / Export

Operations Plane
  Retention / Quarantine / Replay / Usage / Health / Metrics
```

逻辑分层比为了“显得高级”拆多个微服务更重要。只有真实负载、故障域或团队边界证明需要独立扩缩容时，才在最后阶段拆分进程。

## 2. 最终用户体验

完成教程后，用户应该能完成：

```text
管理员创建 Project
  -> 创建 ingest/query API Key
  -> 注册 Agent，看到 Agent 和 Pipeline 状态
  -> Agent 从文件读取并把 batch 写入 Server
  -> Server 校验 Project、scope、大小和协议版本
  -> PostgreSQL 事务写入 batch metadata + entries
  -> commit 后返回 accepted / duplicate
  -> 查询客户端按项目和时间范围使用 cursor 分页
  -> 运维者看到积压、错误、配额、保留和 Quarantine 状态
  -> 修复后可重放隔离批次
```

故障时也必须有明确体验：

- Server 短暂不可用时，Agent 保留同一个 batch 重试；
- PostgreSQL commit 后响应丢失时，重复请求返回 duplicate；
- 单个坏批次不会被无限重试，也不会静默删除；
- Project A 的 Key 看不到 Project B 的日志；
- Retention 不用一条巨大 DELETE 阻塞接入；
- readiness、限流、后台任务和数据库状态都可观测。

## 3. 分阶段路线

不要从微服务或 Kubernetes 开始。每个阶段都必须形成可运行、可解释、可回退的纵向切片。

| 阶段 | 目标 | 主要结果 | 不进入本阶段 |
| --- | --- | --- | --- |
| 0. 基线 | 让仓库可复现 | 依赖边界、Router、配置、测试基线 | 新业务功能堆叠 |
| 1. 后端核心 | 建立真正的 Server 数据闭环 | Project、Key、Batch、Entry、PostgreSQL、Ingest、Query | Agent 高级轮转 |
| 2. 控制平面 | 让后端可以被运营 | Agent 注册、Pipeline 状态、审计、Key 生命周期 | 动态下发复杂配置 |
| 3. 数据治理 | 让数据系统可长期运行 | Retention、Quota、Usage、Quarantine、Replay | 无边界全文分析 |
| 4. Agent 可靠性 | 让接入端可恢复 | spool、checkpoint、背压、重试、故障注入 | 进程内直传降级 |
| 5. 交付与证明 | 让项目可演示、可复现 | Compose、CI、Release、E2E、基准报告 | 未测量的性能数字 |
| 6. 扩展与高可用 | 在证据基础上扩大规模 | Server 水平扩展、读副本、故障域、滚动升级 | 为简历强行引入 Kafka/微服务 |

阶段 6 不是项目开始时的前置条件，而是建立在正确性和可观测性已经成立之后的最终演进。

## 4. 阅读和实现顺序

1. [00. 最终系统与后端面试目标](./00-system-vision.md)
2. [01. 模块化单体与部署架构](./01-backend-architecture.md)
3. [02. 领域模型与 PostgreSQL 数据模型](./02-domain-model-and-data-model.md)
4. [03. Control Plane：Project、Key、Agent 与 Pipeline](./03-control-plane.md)
5. [04. Ingest API、协议和幂等事务](./04-ingest-api-protocol-idempotency.md)
6. [05. Query API、搜索和稳定分页](./05-query-search-pagination.md)
7. [06. Repository、Migration 和存储实现](./06-storage-repository-migrations.md)
8. [07. Retention、Quarantine 和 Replay](./07-retention-quarantine-replay.md)
9. [08. Agent Runtime、Spool 和 Checkpoint](./08-agent-runtime-spool.md)
10. [09. 可观测性、限流和后台任务](./09-observability-limits-and-background-jobs.md)
11. [10. 测试、故障注入和可靠性证明](./10-testing-fault-injection.md)
12. [11. 部署、CI、水平扩展和高可用](./11-deployment-ci-and-scale-ha.md)
13. [12. 最终集成、演示和后端面试](./12-final-integration-and-interview.md)
14. [术语表](./appendix-glossary.md)

如果你只想先完成一个有说服力的后端版本，做到第 10 章并通过阶段 0-5 就已经足够；第 11 章是最终增强，不应阻塞核心项目完成。

## 5. 每章固定学习循环

```text
读当前代码和本章目标
  -> 写本章稳定合同
  -> 画数据流和失败路径
  -> 先写高价值测试
  -> 实现一个最小纵向切片
  -> build / test / race / vet
  -> 做真实集成或故障实验
  -> 记录证据、限制和下一完成门
```

目标代码块是教学骨架，不保证复制后直接编译；每章会说明哪些部分必须完整实现，哪些只是帮助理解依赖方向的伪代码。

## 6. 不是技术名词清单

本教程会主动拒绝几种常见的“看起来高级”：没有证据就加入 Kafka；没有瓶颈就加入 ClickHouse；没有独立扩缩容需求就拆微服务；没有恢复验证就声称高可用；没有基线和原始结果就声称性能提升；用大量 CRUD 端点掩盖没有事务、权限和故障语义。

真正要形成的后端能力是：模块边界、状态机、数据库约束、事务边界、租户隔离、后台任务、故障处理、运行证据和演进判断。

## 7. 最终简历表达

在全部阶段完成并有证据后，项目可以这样介绍：

> 使用 Go 设计并实现面向小型服务集群的多租户日志管理后端，包含 Agent 控制平面、版本化批次接入、PostgreSQL 幂等事务、Keyset 日志检索、Retention/Quarantine 后台任务和 Project 级配额；在 Agent 侧通过持久化 spool 与 checkpoint 处理网络、进程和文件轮转故障，并使用故障注入、竞态测试和固定环境基准验证恢复及性能行为。

方括号中的性能、规模和恢复时间只能在真实实验后填写。

