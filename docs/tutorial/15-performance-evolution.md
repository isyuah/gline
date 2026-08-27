# 15. 性能工程、PostgreSQL 优化与架构演进

> 本章不会提供未经测量的吞吐数字，也不会把“使用 ClickHouse/Kafka/微服务”当作目标。性能结论必须绑定 commit、硬件、软件版本、数据集、配置和原始结果；架构演进必须由证据触发。

相关设计：[目标架构与演进路径](../03-target-architecture.md)、[PostgreSQL 模型](../04-domain-api-and-storage.md)、[PostgreSQL-first ADR](../adr/0002-postgresql-first.md)、[可靠性与性能方法](../05-reliability-security-observability.md)。入口：[教程目录](./README.md)、[如何使用本教程](./00-how-to-use-this-tutorial.md)。

## 1. 本章目标

完成后你应能：

1. 从用户场景定义容量目标、延迟目标和资源约束，而不是先追求一个大数字。
2. 建立 Agent、接入、查询、混合负载与恢复流量的可复现实验。
3. 使用 pprof、Prometheus、PostgreSQL 指标和 `EXPLAIN (ANALYZE, BUFFERS)` 定位瓶颈。
4. 按低风险顺序优化 batch、SQL、索引、连接池和数据生命周期。
5. 区分峰值、稳定值、饱和点与故障恢复速率。
6. 用同一数据集对比修改前后，并保存原始结果。
7. 只有 PostgreSQL 已有明确证据不足时，才评估 ClickHouse、持久化队列或服务拆分。

## 2. 前置条件

- 功能与可靠性测试已通过，尤其是幂等和四个崩溃窗口。
- 指标、健康检查和 pprof 已按第 12 章实现；pprof 默认关闭且独立监听。
- 测试数据生成器能输出唯一 run ID 和连续 sequence。
- Query API 有固定过滤和 keyset pagination 合同。
- PostgreSQL migration、索引和 retention 行为可追踪。
- 负载生成器、系统被测实例和指标采集不会共享不可控资源。
- 工作树有明确 commit；未提交修改必须记录为 dirty，不能伪装成可复现结果。

性能测试不能替代正确性测试。一个“很快但丢数据”的结果没有优化价值。

## 3. 从场景定义目标

先写容量模型，不填猜测数字：

| 输入 | 说明 | 你的值与依据 |
| --- | --- | --- |
| Agent 数 | 同时连接的采集节点 | 待填写 |
| 每 Agent Pipeline 数 | 文件源数量 | 待填写 |
| 平均/峰值 records/s | 正常与突发速率 | 待填写 |
| 平均/p95 行大小 | 含 attributes 后大小 | 待填写 |
| 保留天数 | 决定总数据量 | 待填写 |
| 查询并发 | 开发者/工具同时查询数 | 待填写 |
| 常见时间范围 | 15 分钟、小时或天 | 待填写 |
| 常见过滤形状 | service/level/message 等 | 待填写 |
| 故障窗口 | Server 最长不可用时间 | 待填写 |
| 单机资源预算 | CPU、内存、磁盘、IOPS | 待填写 |

由这些输入推导：

```text
daily_entries = average_records_per_second * 86400
daily_raw_bytes = daily_entries * average_encoded_entry_bytes
spool_required ≈ peak_records_per_second * outage_seconds * encoded_entry_bytes
```

这些只是容量估算，不是实测。数据库索引、WAL、行开销、压缩和副本都会改变实际存储量，必须用真实装载测量修正。

## 4. 定义 SLO 与停止条件

不要只写“高并发”。为实验定义：

| 目标 | 指标 | 测量边界 | 目标值 |
| --- | --- | --- | --- |
| 接入确认延迟 | p50/p95/p99 | Agent 发请求到收到 durable ACK | 由场景填写 |
| 端到端可见延迟 | p50/p95/p99 | 写入文件到 Query 可见 | 由场景填写 |
| 稳定接入吞吐 | entries/s、bytes/s | 持续窗口内 spool 不增长 | 由场景填写 |
| 常用查询延迟 | p50/p95/p99 | 固定数据量与 filter shape | 由场景填写 |
| 恢复能力 | drain rate / recovery time | 固定 outage 与积压 | 由场景填写 |
| 资源上限 | CPU、RSS、disk、IOPS | 稳定负载窗口 | 由场景填写 |
| 错误率 | HTTP/业务错误 | 排除客户端故意错误 | 由场景填写 |

每个实验提前定义停止条件，例如：

- missing/duplicate 非零；
- 错误率超过目标；
- spool 持续单调增长；
- readyz 失败；
- 磁盘空间低于安全余量；
- query p99 超出目标；
- CPU/内存/IO 达到资源预算。

达到停止条件就结束并记录，不把系统拖到影响其他开发服务。

## 5. 可复现报告合同

每份结果必须包含以下元数据：

```yaml
experiment_id: <unique-id>
started_at: <UTC timestamp>
git_commit: <full-sha>
git_dirty: false
os: <name/version>
cpu: <model/core-count>
memory_bytes: <value>
disk: <model/filesystem/medium>
go_version: <value>
agent_version: <value>
server_version: <value>
postgres_version: <value>
container_images:
  server: <tag-and-digest>
  postgres: <tag-and-digest>
dataset:
  generator_version: <value>
  seed: <value>
  run_id: <value>
  entry_count: <value>
  encoded_size_distribution: <summary>
  level_service_attribute_distribution: <summary>
configuration:
  batch_size: <value>
  flush_interval: <value>
  http_concurrency: <value>
  db_pool: <redacted-summary>
  retention: <value>
warmup: <duration>
measurement: <duration>
result_files:
  - <raw-json-or-csv>
  - <metrics-snapshot>
  - <query-plans>
notes: <limitations-and-anomalies>
```

缺少 commit、硬件、版本、数据集、配置或原始结果的数字不能进入 README/简历。

### 5.1 采集环境信息

PowerShell 示例：

```powershell
git rev-parse HEAD
git status --short
go version
docker version
docker compose version
Get-CimInstance Win32_Processor | Select-Object Name,NumberOfCores,NumberOfLogicalProcessors
Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory
Get-Volume | Select-Object DriveLetter,FileSystem,Size,SizeRemaining
```

CI/Linux 示例：

```bash
git rev-parse HEAD
git status --short
go version
docker version
docker compose version
uname -a
lscpu
free -b
lsblk -o NAME,MODEL,SIZE,ROTA,FSTYPE,MOUNTPOINTS
```

PostgreSQL 版本和关键配置应通过受控查询获取，例如 `SHOW server_version`、连接上限、shared buffers 等。报告不保存 database URL 或密码。

## 6. 数据集设计

一个数据集要能代表查询与写入成本，而不是只有完全相同的短字符串。

固定并记录：

- entry 数与编码后 byte 数；
- message 长度分布；
- service、host、level 基数与倾斜；
- attributes key 数、深度和大小；
- observed_at 分布与乱序程度；
- batch size 与 Agent 数；
- 可命中的关键词比例；
- run ID 与连续 sequence。

建议至少三类合成数据：

1. small-line：短 message、少量 attributes，用于测协议/事务上限。
2. mixed-realistic：多种长度、service/level 倾斜，用于主报告。
3. boundary：接近 message/attributes/batch 限制，用于资源和拒绝行为。

不要用真实生产日志做公开 benchmark。合成内容必须可由 seed 重新生成。

## 7. 测试环境隔离

- 固定 CPU/内存限制，或明确记录没有限制。
- 关闭会竞争同一磁盘/CPU 的不相关任务，或记录干扰。
- load generator 尽量与 Server 分离；若同机，必须记录并承认竞争。
- PostgreSQL 使用专用测试 volume/database，不删除开发数据。
- 每轮恢复到相同 schema、索引和数据量。
- 明确冷缓存、暖缓存实验；不能混在一张统计表。
- 时间同步，统一使用 UTC。
- 预热与测量窗口分开。

共享 CI runner 适合检查严重回归趋势，不适合发布绝对性能数字。最终简历数字应来自可控环境。

## 8. 实验 1：Agent 采集与本地路径

目的：分离 Source、Parser、batch 和 spool 成本，不先把 HTTP/DB 混进来。

### 8.1 子实验

| 子实验 | 保留组件 | 回答问题 |
| --- | --- | --- |
| Source + Parser | 文件读取、解析 | 单 Pipeline CPU/分配在哪里？ |
| + Batch Builder | 条数/字节/时间 flush | batch 策略成本与延迟？ |
| + Spool | 真实磁盘事务 | durable boundary 的吞吐/IO？ |
| + Dispatcher fake ACK | 从 spool 读取和删除 | 本地恢复/清理是否成为瓶颈？ |

fake ACK 只用于隔离本地成本，不能用于系统最终吞吐结论。

### 8.2 观察指标

- records/s、bytes/s；
- source lag；
- spool commit duration（若新增，标签仍需低基数）；
- spool bytes/batches；
- CPU、RSS、allocation、GC；
- 磁盘写入与 fsync 行为；
- goroutine 数是否稳定。

### 8.3 Go benchmark 骨架

对于纯 Parser/Batch 算法，可以使用：

```bash
go test ./internal/agent/... -run '^$' -bench 'Benchmark(Parser|Batch)' \
  -benchmem -count=10
```

PowerShell 将续行改为反引号，或写成单行。完整 Agent 使用独立负载程序与进程指标，不要把包含真实文件/磁盘的测试强塞进 `testing.B` 后忽略环境。

## 9. 实验 2：batch 大小与实时性

变量：

- max entries；
- max encoded bytes；
- flush interval；
- 并发上传数。

响应变量：

- entries/s、requests/s；
- upload p50/p95/p99；
- 端到端可见延迟；
- CPU、allocation；
- PostgreSQL transaction/WAL/IO；
- 失败重试粒度；
- Agent spool age。

方法：一次只改变一个主要变量；每组使用同一 dataset/seed，先 warmup，再持续测量。batch 越大通常减少请求开销，但会增加等待、重试成本和事务峰值；最终选择必须说明这个交换，而不只是最高吞吐。

## 10. 实验 3：Server 接入

使用能复现完整 Agent payload、稳定 batch ID 和 auth 的负载生成器。不要用无限并发 `curl` 循环作为正式结论。

逐级提高：

```text
HTTP concurrency
  -> Agent count
  -> batch size
  -> entry size
```

每一级保持其他参数固定。观察：

- HTTP route latency/status；
- accepted/duplicate/conflict；
- Ingest service 与 DB operation duration；
- DB pool active/idle/wait；
- transaction rate、WAL bytes、disk latency；
- Server CPU/RSS/GC；
- Agent retry/spool；
- sequence validator。

### 10.1 峰值与稳定值

峰值是短窗口最大值；稳定值要求在预设持续窗口内：

- spool 不持续增长；
- 错误率在目标内；
- p95/p99 不持续恶化；
- 资源不单调上涨；
- 数据无缺失/重复。

报告必须分别标注。不能用一次短跑峰值替代稳定能力。

## 11. 实验 4：查询与索引

固定数据库规模和数据分布，为每种 filter shape 准备查询：

```text
project + time
project + service + time
project + level + time
project + service + level + time
project + message contains + time
cursor next page
```

分别测热缓存和冷/受控缓存。清空系统缓存是具有全局影响的操作，不应在共享开发机随意执行；更安全的是重启专用测试数据库或明确只报告暖缓存。

### 11.1 查询计划

对固定参数保存：

```sql
EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT ...
FROM log_entries
WHERE project_id = $1
  AND observed_at >= $2
  AND observed_at < $3
ORDER BY observed_at DESC, id DESC
LIMIT $4;
```

在安全测试库执行。`ANALYZE` 会真的运行语句；只用于 SELECT 或明确安全的语句。

阅读重点：

- actual vs estimated rows；
- Seq Scan / Index Scan / Bitmap；
- rows removed by filter；
- shared hit/read/dirtied；
- sort 方法和内存/磁盘；
- planning vs execution；
- limit 前扫描了多少行。

不要只看“用了索引”。索引扫描可能仍读取大量行，估算错误也可能导致坏计划。

## 12. 实验 5：混合负载

接入与查询共享 Server/DB 时，单独测试都快不代表同时运行仍满足目标。

矩阵：

| 接入比例 | 查询比例 | 数据量 | 目的 |
| --- | --- | --- | --- |
| 低 | 低 | 基线 | 验证环境 |
| 高 | 低 | 固定 | 接入饱和 |
| 低 | 高 | 固定 | 查询饱和 |
| 目标 | 目标 | 固定 | 真实场景 |
| 恢复峰值 | 目标 | 固定 | outage 后竞争 |

观察接入 p99、查询 p99、pool wait、WAL/IO、spool age 和 readiness。若恢复流量压垮正常查询，应先考虑 Dispatcher 恢复限速和 Server admission control，而不是立即拆微服务。

## 13. 实验 6：故障恢复性能

可靠性测试证明“能恢复”，本实验测“恢复是否可控”。

步骤：

1. 以目标速率稳定运行。
2. 停止 Server 固定时长，继续生成连续 sequence。
3. 记录 spool 增长率、最终大小、最老 batch age。
4. 恢复 Server，记录 drain rate 与查询延迟。
5. 观察是否出现重试同步风暴、DB pool 饱和或正常新流量饥饿。
6. spool 清空后继续观察资源是否回落。
7. sequence validator 验证不缺不重。

调整候选：

- full jitter；
- 每 Agent 恢复并发上限；
- 全局/每 key 限流；
- batch 与 flush；
- 新流量和恢复流量公平策略。

每次只改一个主要因素并复测。

## 14. pprof 方法

pprof 只在独立 loopback/受控监听器显式开启。

### 14.1 CPU

在稳定测量窗口采集固定时长：

```text
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=<fixed>
```

查看 top、flame graph、调用树。先判断时间属于 JSON、hash、日志格式化、锁、syscall、SQL driver 还是业务校验，再提出修改。

### 14.2 Heap 与 allocation

- `heap`：当前存活对象；
- `allocs`：累计分配热点；
- 在相同负载、相同时间点比较；
- 关注 batch/message 是否不必要复制、响应 body 是否泄漏。

### 14.3 goroutine、mutex、block

用于定位：

- Dispatcher/Source 是否泄漏；
- channel/backpressure 是否阻塞在预期位置；
- 全局锁是否串行化 ingest；
- DB pool 等待是否被误判为 CPU 问题。

mutex/block profile 有运行开销，只在受控实验启用并记录配置。

### 14.4 证据链

每个优化必须形成：

```text
基线结果
  -> profile / metric / query plan 指向热点
  -> 单一修改
  -> 相同环境与数据复测
  -> 收益、代价、正确性验证
```

没有这条链，就不能写“优化后提升 X%”。

## 15. PostgreSQL 优化阶梯

按以下顺序推进，每一级都需要证据。

### 15.1 SQL 往返与事务

- 确认不是每 entry 一次 insert。
- 对比 multi-values、COPY 或 driver 支持的 batch 方式。
- batch metadata 与 entries 仍在同一事务。
- 失败保持原子，不为吞吐牺牲 ACK 语义。

### 15.2 batch 策略

调整 batch entries/bytes/flush interval，平衡吞吐、延迟、内存和重试粒度。请求体与 Server 限制保持一致。

### 15.3 连接池

观察 active、idle、wait duration、DB max connections。pool 更大可能增加数据库竞争，并非总是更快。设置有界 connection lifetime/idle，并在 shutdown 关闭。

### 15.4 索引

从真实 filter shape 和查询计划添加少量组合索引。记录：

- 建索引前后查询时间/buffers；
- 索引大小；
- ingest/WAL 代价；
- retention 删除代价。

删除索引也要基于使用与影响证据，不能只看一次统计快照。

### 15.5 统计信息与参数

如果 estimate 严重偏离，检查 analyze、数据倾斜和列统计。数据库配置修改要记录旧值、新值、原因和复测；不要复制网络“推荐配置”。

### 15.6 retention 与 vacuum

小批删除时测锁、WAL、膨胀和查询影响。调整 batch/频率，并观察 autovacuum。只有保留成本真实不可接受时再考虑分区。

### 15.7 分区

触发信号：

- retention 删除持续成为主要成本；
- 单表/索引大小导致明确管理或查询瓶颈；
- 时间分区能显著减少目标查询扫描；
- 唯一约束、幂等和 migration 影响已有设计。

分区会改变唯一约束、路由、DDL 和运维，不是免费性能开关。先在相同数据集建立非分区/分区对照。

## 16. 不要过早优化

以下修改必须先有 profile/plan 证据：

- 更换 JSON 库；
- 引入对象池；
- 手写 unsafe/零拷贝；
- 为所有 attributes 建 GIN；
- 提高全部 channel/pool 容量；
- 关闭 fsync/durability；
- 将 ingest 改成未持久化内存队列后立即 ACK。

尤其不能为了 benchmark 关闭 PostgreSQL durability，然后仍声称原有可靠性合同成立。

## 17. 何时评估 ClickHouse

只有同时满足：

1. 目标数据量/查询形状已经明确；
2. PostgreSQL 基线可复现；
3. SQL、索引、pool、retention/分区等合理优化已验证；
4. 瓶颈仍持续违反目标；
5. 运维复杂度、迁移与可靠性成本可接受。

可能信号：

- 大时间范围分析/聚合是核心需求；
- 索引与 retention 成本超出资源预算；
- PostgreSQL 查询 p95/p99 在目标数据量下持续不满足；
- ClickHouse 对同一数据集有显著且稳定的实测优势。

### 17.1 对照实验

相同：

- commit/协议规范；
- 数据集、seed、run ID；
- 硬件资源预算；
- 查询语义与结果校验；
- warmup、测量窗口；
- durability 设置与故障场景。

比较：

- ingest ACK 延迟与吞吐；
- 查询 p50/p95/p99；
- 存储与压缩；
- CPU/内存/IO；
- retention 成本；
- 重复处理与故障恢复；
- 备份、迁移、监控、日常运维复杂度。

不能只比较一条聚合查询的峰值。

### 17.2 可靠性边界必须重写

若 PostgreSQL 保留 Project/API Key/idempotency metadata，而 entries 进入 ClickHouse，需要重新回答：

- ACK 在哪个持久化提交后返回？
- metadata 已提交但 entries 未写入如何恢复？
- 同 batch 重试如何精确判重？
- 查询可见性是同步还是最终一致？
- retention/删除如何跨存储协调？

没有明确答案时，不进行双写。可以先做离线 prototype 评估查询，不接入主 ACK 链路。

## 18. 何时评估持久化队列

Agent spool 已经吸收短期 Server 不可用。只有出现以下证据才考虑 Kafka、NATS JetStream 或其他持久队列：

- 接入确认和数据库索引必须独立扩缩容；
- 同一 batch 有多个独立消费者；
- Server 端需要吸收比 Agent 单机 spool 更长的维护窗口；
- 数据库写入延迟使 HTTP 接入长期超时，合理优化仍不满足；
- 有明确运营能力维护额外状态组件。

引入后 ACK 合同改为“持久化队列提交成功”，不再等同于“日志可查询”。必须增加：

- queue message 与 batch ID 的幂等；
- consumer retry/DLQ；
- queue lag 与查询可见延迟指标；
- replay 与 schema compatibility；
- 队列不可用时的 Agent 行为；
- 查询存储失败后的恢复。

不要使用进程内 channel 伪装持久化队列并提前 ACK。

## 19. 何时拆微服务

模块化单体先保留清晰包边界。拆分信号：

- ingest 与 query 的资源/扩缩容目标长期不同；
- 故障隔离有明确收益；
- 团队所有权与发布节奏确实独立；
- 已有队列/存储边界能承载跨进程一致性；
- 监控、部署、认证和网络成本有预算。

“CPU 高”通常先通过 profile、横向扩展无状态 Server、SQL/连接池优化解决。拆服务会新增网络失败、版本兼容、部署和追踪成本，不能仅为简历名词使用。

## 20. 演进决策表

| 观测证据 | 先做 | 仍不满足时 |
| --- | --- | --- |
| HTTP CPU 高 | profile、编码/校验/日志热点 | 评估水平扩展 |
| DB pool wait 高 | SQL 时长、pool/DB容量 | 分离负载或扩容 |
| ingest 写入慢 | batch/bulk SQL/WAL/IO | 队列 prototype |
| 时间查询扫描大 | 组合索引、统计、时间范围 | 分区/ClickHouse prototype |
| retention 膨胀 | 小批删除、vacuum、指标 | 时间分区 |
| 恢复流量压垮服务 | jitter、限速、公平性 | Server buffer/持久队列评估 |
| query 影响 ACK | 限制查询、pool/资源隔离 | 独立 query 进程/存储 |
| 多消费者需求 | 明确事件合同 | 持久化队列 |

每项决策写 ADR：问题、基线、备选、实验、结果、代价、回退条件。

## 21. 性能回归策略

### 21.1 微基准

稳定算法可在 CI 保存 `go test -bench` 原始结果，用统计工具离线比较。不要因共享 runner 单次波动就阻断发布。

### 21.2 宏观实验

在固定 runner/机器定期执行，保存 JSON/CSV、metrics 和 profile。设置报警阈值前先了解自然方差。

### 21.3 正确性门

任何性能结果只有在以下通过时有效：

- sequence 无 missing/duplicate；
- error rate 合法；
- readyz 稳定；
- spool 最终收敛；
- 数据库约束与 durability 未被关闭；
- 结果对应当前 commit/config。

## 22. 结果分析与呈现

### 22.1 表格

```text
Experiment: <id>
Commit: <sha>
Hardware: <link/summary>
Dataset: <link/summary>

Variant | Stable entries/s | ACK p95 | Query p95 | CPU | RSS | DB I/O | Errors
baseline| measured value   | ...     | ...       | ... | ... | ...    | ...
change  | measured value   | ...     | ...       | ... | ... | ...    | ...
```

### 22.2 结论句式

```text
在 <commit / hardware / version / dataset / config> 下，系统在 <测量窗口>
内稳定达到 <实测结果>。限制因素由 <profile/query plan/metric> 定位为
<具体瓶颈>。修改 <单一变量> 后，<指标> 从 <基线> 变为 <结果>，代价是
<延迟/内存/运维复杂度>。原始结果位于 <artifact>。
```

没有测量前只能写“目标”“实验方案”“待验证”，不能预填“数万行/秒”“毫秒级”或“提升 50%”。

### 22.3 README 只放摘要

README 放一张最重要的表/图和原始报告链接。完整 metadata、原始数据和失败实验保留在版本化报告或 CI artifact；失败结果有助于证明决策，不应全部隐藏。

## 23. 分步执行流程

### 步骤 1：冻结正确性基线

运行 unit、race、integration、四窗口故障测试，记录 commit。失败时停止性能工作。

### 步骤 2：填写容量模型与目标

给每个值写依据和可接受边界。没有产品数据时，明确这是简历演示目标，不伪装成生产需求。

### 步骤 3：固定环境与数据集

记录硬件、版本、镜像 digest、seed 和配置；创建独立测试数据库/volume。

### 步骤 4：建立未优化基线

依次执行 Agent、本地 spool、Server ingest、query、mixed、recovery。保存 raw results。

### 步骤 5：定位瓶颈

结合 metrics、pprof、DB pool、WAL/IO 和 query plan。用一个主假设解释证据。

### 步骤 6：单一修改

只改一个主要变量或实现切片。运行正确性测试，再按相同条件复测。

### 步骤 7：记录代价

吞吐提高是否牺牲 p99、内存、磁盘、恢复公平性或运维复杂度？全部记录。

### 步骤 8：决定停止或演进

若 PostgreSQL 满足目标就停止。只有持续不满足且优化证据完整，才创建 ClickHouse/队列/拆分 ADR 与 prototype。

## 24. 失败处理

### 同一配置结果波动很大

检查 warmup、测量窗口、后台任务、CPU频率/电源策略、共享磁盘、容器限额、DB cache 和生成器是否饱和。增加重复次数并报告方差，不挑最好的一次。

### 负载发生器先饱和

观察 generator CPU/network 和实际发送速率。将生成器移到独立机器或优化生成方式；在确认前，结果只能称为“生成器限制下的下界”。

### pprof 没有明显热点

系统可能在 IO、DB pool 或锁等待。结合 block/mutex、DB 指标、系统 I/O；不要据此更换 JSON 库。

### 查询计划与预期索引不符

检查参数、数据分布、统计信息、类型转换、排序和实际选择性。不要用强制 hint（若依赖支持）掩盖统计/设计问题。

### 优化提高吞吐但出现缺失

立即判定实验无效并回到最近正确基线。检查是否提前 ACK、跳过 fsync、共享 buffer 复用或错误 batch ID。

### ClickHouse prototype 更快

先检查是否使用相同 durability、查询语义、硬件和数据量，再评估迁移、幂等与运维成本。单项更快不自动成为架构结论。

## 25. 常见错误

- 不记录 commit、硬件、软件版本或配置。
- 只保存截图，不保存原始结果。
- 用峰值代替稳定值。
- generator 与 Server 同机，却忽略资源竞争。
- 一次同时修改 batch、索引、pool 和 JSON 库。
- 只测正常流量，不测恢复流量。
- 只测 ingest，不验证 Query 与 sequence。
- 关闭 durability 获得漂亮数字，却沿用原可靠性叙事。
- 在共享机器清系统缓存或删除开发 volume。
- PostgreSQL 尚未分析就引入 ClickHouse。
- 用消息队列或微服务作为简历装饰。
- 看到 CPU 高就对象池化，未检查生命周期与数据竞争风险。
- 固定 CI 绝对阈值，因 runner 噪声产生频繁误报。

## 26. 验收证据

每个正式报告必须具备：

- [ ] full commit SHA 与 dirty 状态；
- [ ] CPU、内存、磁盘、OS；
- [ ] Go、Agent、Server、PostgreSQL、镜像版本/digest；
- [ ] generator 版本、seed、run ID、数据分布；
- [ ] 完整配置摘要且已脱敏；
- [ ] warmup、测量时长、重复次数；
- [ ] p50/p95/p99、吞吐、错误率、CPU/RSS/IO；
- [ ] Query SQL/filter shape 与 `EXPLAIN (ANALYZE, BUFFERS)`；
- [ ] raw JSON/CSV、metrics/profile/query plan artifact；
- [ ] sequence missing/duplicate 结果；
- [ ] 基线、修改、收益、代价和限制；
- [ ] 失败实验与异常说明。

缺一项时，明确报告边界，不补猜测。

## 27. 复盘题

1. 为什么稳定吞吐必须要求 spool 不持续增长？
2. 为什么 batch 越大不一定越好？
3. 如何区分 Server CPU 瓶颈、DB pool 等待和磁盘瓶颈？
4. `EXPLAIN ANALYZE BUFFERS` 中哪些信息比“是否用了索引”更重要？
5. 为什么恢复流量必须单独测试？
6. 哪些条件满足后才值得评估 ClickHouse？
7. 引入持久化队列后 ACK 与查询可见性如何变化？
8. 为什么关闭 durability 的 benchmark 不能用于原可靠性合同？
9. 哪些性能结果适合放 README，哪些应保留在原始报告？
10. PostgreSQL 已满足目标时，为什么“停止演进”也是正确架构决策？

## 28. 完成门

- [ ] 容量模型、SLO 和停止条件由场景驱动，未使用空泛“高并发”。
- [ ] 每个结果包含 commit、硬件、版本、数据集、配置和原始文件。
- [ ] Agent、batch、Server ingest、query、mixed 和 recovery 均有独立实验。
- [ ] 性能实验仍通过 run ID + sequence 正确性验证。
- [ ] 至少一个优化形成“证据 -> 修改 -> 同条件复测 -> 代价”的闭环。
- [ ] PostgreSQL 优化使用真实 profile、指标和 query plan，不凭经验堆索引。
- [ ] pprof 默认关闭并只在受控独立监听器启用。
- [ ] README/简历中的数字可追溯，峰值与稳定值明确区分。
- [ ] ClickHouse、队列、分区或微服务没有在证据门槛前进入主架构。
- [ ] 若启动架构评估，已写清新的 ACK、幂等、查询可见性和运维成本。

通过本章后，你获得的是“在指定环境、版本和负载下可复现的性能结论”，而不是普遍的高性能声明。工程可信度来自边界清楚、证据完整和知道何时不增加复杂度。
