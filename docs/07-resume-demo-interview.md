# 07. 简历、演示与面试手册

## 1. 简历写法原则

一条可信的项目描述应包含：问题、你的设计、关键取舍、验证结果。不要只列框架，也不要把路线图写成已完成能力。

### 当前阶段可以诚实描述的内容

在 Server 闭环完成前，可以写：

> 使用 Go 实现多源日志采集 Agent，通过 context 与有界 channel 编排多个采集 Pipeline 和批量 Sender；设计临时/致命错误分类、单 Pipeline 故障隔离、panic 边界恢复与优雅排空，并以竞态测试验证并发生命周期。

这段描述与当前代码和测试基本一致。不要写“实现日志平台”“保证日志不丢”“支持分布式检索”。

### 阶段 1 完成后可以增加

> 设计版本化批次接入协议和 PostgreSQL 事务写入，使用项目级 API Key 隔离数据，并以批次唯一约束实现重试幂等；提供按时间、服务、级别和关键词过滤的游标分页查询。

前提是这些能力已实现并通过真实数据库集成测试。

### 阶段 2、3 完成后可以增加

> 在 Agent 侧引入持久化 spool、checkpoint 和指数退避，使网络中断与进程重启后的批次可恢复；通过 Prometheus 指标和故障测试验证积压、恢复和无重复写入。

> 针对固定数据集完成接入与查询基准，在【硬件与负载条件】下达到【实测吞吐/p95】，并根据 `EXPLAIN ANALYZE` 优化【具体索引或 SQL】。

方括号必须替换为真实结果。如果没有结果，就不写数字。

## 2. 推荐简历项目结构

```text
Gline | Go, Gin/net/http, PostgreSQL, Prometheus, Docker
一句话：面向小型服务集群的自托管日志采集与检索平台。

- 关键数据链路与并发生命周期；
- 可靠性与幂等设计；
- 存储、索引和查询；
- 可观测性与实测结果。
```

技术栈只列实际直接使用且能深入解释的组件。若 Gin 只承担路由，不应让面试叙事停留在“会用 Gin”。真正值得讲的是 `net/http` 生命周期、context、事务、幂等、索引和故障恢复。

## 3. 五分钟演示脚本

### 第 0:00 - 0:40：说明问题与架构

- 小型服务也需要统一日志，但部署完整大平台成本高。
- 展示一张 Agent→Server→PostgreSQL 图。
- 明确项目保证：至少一次传输 + 幂等落库，而不是模糊的 exactly-once。

### 第 0:40 - 1:30：启动

- `docker compose up -d`。
- 展示 `/readyz`。
- 创建/加载 demo project 和 API Key。
- 启动 Agent 并指定 demo 配置。

### 第 1:30 - 2:20：正常链路

- 追加几种级别的日志和一条异常格式。
- 调用 Query API，展示 service/level/time filter 和 cursor。
- 解释 `observed_at` 与 `ingested_at`。

### 第 2:20 - 3:40：故障恢复

- 暂停 Server 或阻断地址。
- 继续产生日志。
- 查看 Agent 的 spool bytes、oldest pending、retry 指标。
- 恢复 Server，观察积压清空。
- 查询记录并证明没有重复。

### 第 3:40 - 4:30：幂等与隔离

- 重放同一 batch，数据库行数不变。
- 使用另一个 Project 的 Key 查询，结果隔离。
- 解释唯一约束和事务为什么比“先查再插”可靠。

### 第 4:30 - 5:00：性能与边界

- 展示一张 benchmark 表和查询计划。
- 说明当前非目标：没有做 UI、告警和大规模分布式存储。
- 说明什么指标会触发 ClickHouse 或队列演进。

## 4. README 应提供的证据

项目首页按下面顺序组织：

1. 项目名和一句话定位。
2. 30 秒能看懂的架构图。
3. 三个明确特性：可靠采集、幂等接入、受限查询。
4. 五分钟 quickstart。
5. 一次正常链路和一次故障恢复示例。
6. API 示例或 OpenAPI 链接。
7. 可靠性保证与不保证什么。
8. 实测性能，包含环境链接。
9. 开发/测试命令。
10. 当前限制与路线图。

避免用大段愿景代替运行说明，也不要展示尚未实现的功能列表而不标状态。

## 5. 面试叙事主线

### 5.1 为什么做这个项目

参考回答结构：

> 我想做一个能够覆盖真实后端边界的项目，而不只是 CRUD。日志链路天然包含文件状态、并发、批处理、网络失败、幂等、时序数据查询和运行诊断。为了把范围控制住，我把目标限定为小型自托管场景，先完成单 Server + PostgreSQL 的闭环。

### 5.2 为什么是模块化单体

> 当前接入、鉴权和查询共享同一数据模型与数据库事务，部署规模也不要求独立扩缩容。拆成微服务会提前引入服务发现、网络失败、分布式追踪和数据一致性成本，但没有解决用户问题。我通过包边界保留拆分可能，等指标证明写入与查询需要独立扩缩容时再拆。

### 5.3 为什么先选 PostgreSQL

> MVP 既有日志 append/query，也有 Project、API Key 和幂等批次等强一致元数据。PostgreSQL 可以在一个事务中完成确认边界，JSONB 保留属性扩展性，组合索引和 BRIN 足以建立百万级基线。只有分析型查询或保留成本出现实测瓶颈时，才评估 ClickHouse。

### 5.4 为什么不是 exactly-once

> 网络超时时客户端无法知道 Server 是否已提交，传输层天然可能重试。我让 Agent 持久化同一个 batch ID，Server 用项目 + batch ID 唯一约束和 payload hash 识别重复，因此语义是至少一次传输、有效单次写入。它比笼统宣称 exactly-once 更准确。

### 5.5 如何处理背压

> 内存 channel 只吸收短波动，持久积压进入有容量上限的磁盘 spool。达到上限默认停止读取而不是静默丢弃，并通过水位、最老批次年龄和 pipeline readiness 暴露。用户可以显式选择 drop-oldest，但该策略必须有丢弃指标。

### 5.6 如何保证查询稳定分页

> 不使用 offset，因为数据增长时扫描成本和页间漂移都会上升。使用 `(observed_at DESC, id DESC)` 作为全序，通过不透明 cursor 执行 keyset pagination，并把 project 和时间范围放在匹配索引前缀中。

## 6. 高频追问与答题骨架

这些不是需要背诵的标准答案。每次回答都应按“当前代码或目标合同 → 为什么这样设计 → 代价与验证证据”展开；没有实现的部分必须使用未来时。

### 6.1 Agent 与 Go 并发

#### Channel 谁关闭，为什么？

答题骨架：

- N 个 Pipeline 是生产者，一个 Sender 是消费者。
- 关闭权属于生产者集合：`pipelineWg.Wait()` 后关闭 entries channel。
- Sender 通过 channel close 得知不再有新数据，发送最后一批后退出。
- 若由消费者关闭，仍在生产的 Pipeline 可能触发 send on closed channel。
- 当前证据是多 Pipeline、取消排空和 Sender 错误测试；未来 spool 引入后还要验证持久化批次不依赖内存 channel 存活。

#### 为什么 Sender 使用 `context.WithoutCancel`？

答题骨架：

- 外部取消首先停止 Pipeline 继续生产。
- Sender 不能同时被取消，否则 channel 中已产生的 entry 会直接留在内存中丢失。
- Sender 使用独立生命周期，等待生产者退出和 channel 关闭，再完成有界排空。
- 这不意味着 shutdown 可以无限等待；目标设计会增加总 shutdown deadline，超时后未确认 batch 已在 spool 中。

#### 为什么一个 Pipeline 失败不取消其他 Pipeline？

答题骨架：

- Pipeline 对应独立日志源，一个文件权限或格式问题不应让其他服务停止采集。
- Sender 是共享出口；它失败意味着继续生产只会制造无人消费的数据，因此会取消全部 Pipeline。
- panic recover 放在 Pipeline goroutine 边界，既隔离故障，又能记录完整堆栈。
- 代价是 Agent 可能处于部分可用状态，所以必须暴露每个 Pipeline 的健康状态，而不是只报告进程存活。

#### 如何证明没有 goroutine 泄漏和数据竞争？

答题骨架：

- `go test -race` 只能证明被执行路径没有检测到 race，不代表全系统天然无竞争。
- 生命周期测试要覆盖正常关闭、Pipeline 错误、Sender 错误、panic 和重复启停。
- 使用同步测试工具或显式事件协调，避免用长时间 sleep 猜测状态。
- 集成阶段用 goroutine profile 比较运行前后数量，并检查 timer、response body、文件 handle 和数据库资源所有权。

### 6.2 文件采集与可靠性

#### 为什么 checkpoint 在进入 spool 后推进，而不是 Server ACK 后推进？

答题骨架：

- 可靠性边界是本地事务 spool，不是进程内存。
- batch 与 checkpoint 同事务提交后，即使 Agent 崩溃，也能从 spool 恢复，不需要重新读取原文件。
- 等 Server ACK 才推进会造成不必要的大量重读；先推进再写 spool 则会在中间崩溃时永久丢失。
- 代价是 spool 成为关键状态，需要 schema version、容量限制、损坏处理和备份边界。

#### rename/recreate 与 copytruncate 分别如何处理？

答题骨架：

- rename/recreate：路径指向新 identity，但旧 handle 仍有效；旧文件读到稳定 EOF，同时开始追踪新文件，二者 checkpoint 分离。
- copytruncate：identity 不变但 size 小于 offset，检测后从头继续，并记录截断事件。
- copytruncate 的复制与截断之间存在 Agent 无法消除的竞态，所以不能承诺绝对零丢失；推荐 rename/recreate。
- Windows 和 Unix 的文件 identity 不同，应通过平台 adapter 隔离并用真实文件系统测试。

#### spool 满了怎么办？

答题骨架：

- 内存 channel 只吸收短期波动，磁盘 spool 吸收持久积压，两者都有上限。
- 默认 `block`：停止从 Source 读取，文件继续保留原始数据，并使 Agent readiness 失败。
- 可选 `drop_oldest` 是显式产品策略，必须记录丢弃条数、字节和最老时间。
- 观察 `spool_bytes`、`oldest_pending_seconds` 和 source lag，不能只看进程是否存活。

### 6.3 协议与一致性

#### 为什么不宣称 exactly-once？

答题骨架：

- 网络超时时，Agent 不知道 Server 是提交前失败还是提交后响应丢失。
- Agent 因此必须重试，传输层天然是至少一次。
- 同一 batch 在首次进入 spool 时生成 ID，所有重试保持 payload 不变。
- Server 使用 `(project_id, batch_id)` 唯一约束和 payload hash，使重复传输产生有效单次写入。
- “至少一次传输 + 幂等写入”比 exactly-once 更准确，也能通过故障窗口测试证明。

#### 为什么相同 batch ID、不同内容返回 409？

答题骨架：

- batch ID 是不可变请求的幂等身份。
- 内容不同意味着 Agent 状态损坏、ID 生成错误或实现违反合同。
- 自动换新 ID 重试会掩盖问题并写入重复/冲突数据。
- 409 进入 quarantine 并报警，由操作人员检查，而不是热循环。

#### 为什么 ACK 必须在 PostgreSQL commit 后？

答题骨架：

- ACK 是 Agent 删除本地 batch 的授权。
- 如果数据只进入 Server 内存 channel 就返回 200，Server 随后崩溃会造成已确认数据丢失。
- MVP 直接在请求事务中写入 PostgreSQL，提交后响应，使边界简单、可测试。
- 如果未来引入持久化队列，可以把 ACK 边界移动到队列提交，但必须重新定义查询可见性。

### 6.4 存储与查询

#### 为什么先用 PostgreSQL，而不是直接上 ClickHouse？

答题骨架：

- MVP 同时包含日志 entries、Project、API Key 和幂等 batch，既有时序数据也有强一致元数据。
- PostgreSQL 可以在一个事务中形成准确 ACK，唯一约束直接解决并发幂等竞态。
- 组合索引、JSONB 和 BRIN 足以建立目标数据量的第一条基线。
- ClickHouse 对大规模追加写和分析查询有优势，但它应由实际查询/保留瓶颈触发，而不是由“简历含金量”触发。
- 重新评估信号包括查询 p95、扫描量、索引体积、retention 成本和稳定接入吞吐。

#### 为什么使用 keyset pagination？

答题骨架：

- offset 越深，数据库通常要跳过越多行；并发写入还会造成页间漂移。
- 使用 `(observed_at DESC, id DESC)` 形成全序，cursor 保存上一页最后一个键。
- 下一页使用 tuple comparison，查询成本更稳定。
- cursor 对客户端保持不透明并带版本；时间范围和 limit 仍必须有上限。

#### 组合索引如何设计？

答题骨架：

- 先从真实过滤形状出发：所有查询都有 project 和时间范围，常见附加条件是 service 或 level。
- 因此建立 project+time、project+service+time、project+level+time 等少量索引。
- 不给每个 attributes 字段预建索引，避免写放大和空间失控。
- 用 `EXPLAIN (ANALYZE, BUFFERS)` 证明命中情况；索引不是看到字段就添加。

### 6.5 安全、运维与演进

#### API Key 如何隔离权限和项目？

答题骨架：

- Key 映射到唯一 Project，并包含 `ingest`/`query` scope。
- Agent 默认只有 ingest，不能读取日志。
- Project ID 由鉴权上下文注入，客户端 body/query 无权指定。
- Repository 方法显式接收 project ID，集成测试使用两个 Project 验证隔离。
- secret 只在创建时展示，数据库保存 HMAC，支持禁用和轮换。

#### 为什么 liveness 不检查数据库？

答题骨架：

- liveness 回答进程是否需要被重启；数据库短暂不可用不代表进程已经失去响应能力。
- 如果 liveness 依赖数据库，外部依赖故障可能触发所有 Server 重启，形成故障放大。
- readiness 检查数据库和迁移版本，用于停止接收流量。

#### 如果要支持一千个 Agent，如何演进？

答题骨架：

1. 先观察接入 p99、429、连接池、数据库 WAL/IO 和恢复流量。
2. 优化 batch、SQL、连接池和限流，避免先拆系统。
3. Server 保持无本地业务状态，通过负载均衡水平扩展。
4. 若接入确认和索引需要解耦，引入持久化消息队列，并保留 batch 协议。
5. 若查询/保留成为瓶颈，评估分区或 ClickHouse 查询存储。
6. 只有 Agent 配置和升级成为真实问题时，才增加 fleet 管理。

#### Gline 与 Filebeat/Loki 的差距是什么？

答题骨架：

- Gline 没有广泛输入生态、fleet 管理、完整查询语言、告警、集群 HA 和成熟 UI。
- 定位不是产品替代，而是小型自托管闭环和工程学习项目。
- 价值在于把一个窄范围的采集、可靠性、接入、存储和验证做完整，并诚实说明保证边界。
- 对标用于解释设计来源和差距，不用于暗示达到成熟项目规模。

## 7. 设计复盘模板

每完成一个阶段，写一页短复盘：

```text
背景：原来有什么可观察问题？
约束：规模、部署、兼容、安全边界是什么？
方案：数据流和所有权如何变化？
备选：为什么没有选择另一个方案？
失败：实现中哪条假设被证伪？
验证：哪些测试、指标、运行结果支持结论？
限制：仍然不能保证什么？
演进：什么信号会触发下一步？
```

这会自然形成面试中的 STAR/设计故事，也比背诵概念更可靠。

## 8. Git 历史与评审面

公开前整理原则：

- 不改写包含他人工作的历史；当前只有本地早期历史，可在明确边界后继续用清晰提交推进。
- 一个提交代表一个可验证行为切片。
- commit message 说明结果，不写“update”“fix test”。
- PR/commit 描述包含问题、设计、验证和遗留项。
- 不提交本地绝对路径、真实 token、日志、数据库 volume 或 benchmark 临时数据。
- tag/release 只指向通过完整验证的 commit。

## 9. 不应声称的内容

除非有对应实现与证据，不要写：

- “高并发”“高可用”“海量数据”；
- “零丢失”“exactly once”；
- “分布式日志平台”；
- “支持多租户”但只有一个非空 Header；
- “微服务架构”但所有模块同进程；
- “性能提升 X%”但没有基线、环境和原始结果；
- “生产可用”但没有备份、迁移、限流、关闭和故障测试。

克制不会削弱简历。能够准确说出保证边界、非目标和演进条件，通常比夸大规模更能体现工程判断。

## 10. 项目自评量表

每项 0 到 2 分：0 未实现，1 有实现但证据不足，2 有自动化或运行证据。

| 维度 | 问题 |
| --- | --- |
| 可复现 | 新环境能否独立构建并运行？ |
| 功能闭环 | 日志能否从文件进入数据库并被查询？ |
| 一致性 | 重试、超时和重复请求是否有确定语义？ |
| 可靠性 | 断网、重启、轮转、spool 满是否可解释？ |
| 安全 | Key、隔离、limits、脱敏是否真实存在？ |
| 数据库 | Schema、事务、索引、分页是否有证据？ |
| 可观测性 | 能否定位积压、错误和慢查询？ |
| 性能 | 数字是否可复现且结论诚实？ |
| 交付 | Compose、迁移、CI、Release 是否一致？ |
| 表达 | README、ADR、演示和简历是否与实现一致？ |

达到 16 分以上且“可复现、功能闭环、一致性、安全”没有 0 分时，才适合把它作为简历主项目。分数本身不是目标，缺口定位才是。
