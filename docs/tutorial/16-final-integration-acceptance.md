# 16. 最终集成与验收

## 1. 本章目标

前面的章节分别实现 Agent、协议、Server、数据库和交付能力。本章负责回答最容易被忽略的问题：这些组件组合起来是否真的形成最终系统？

完成后，你应得到：

- 一个从干净环境可启动的完整系统；
- 一条从日志文件到 Query API 的真实链路；
- 一组能解释网络、崩溃、重复、轮转和容量边界的证据；
- 与当前 commit 对应的性能报告和 Release；
- 一份不夸大能力的 README、演示脚本和简历描述。

本章不是“最后再跑一次 `go test`”。它是跨模块合同的集中验收。

## 2. 前置完成门

开始前逐章确认，不允许用“代码大概有了”替代：

| 前置 | 必须已有的证据 |
| --- | --- |
| [基线](./01-baseline-and-workflow.md) | 无本机 replace；build/test/race/vet 通过 |
| [并发所有权](./02-go-concurrency-ownership.md) | Source、channel、spool、HTTP 和 shutdown owner 清楚 |
| [协议](./03-protocol-domain-contracts.md) | v1 DTO、错误码、兼容和真实 HTTP 合同测试 |
| [Agent runtime](./04-agent-runtime.md) | Pipeline 隔离、全局故障和关闭语义通过测试 |
| [Spool](./05-spool-checkpoint.md) | batch+checkpoint 原子提交，重启恢复 |
| [FileSource](./06-file-tail-rotation.md) | start position、半行、rename、truncate 测试 |
| [Dispatcher](./07-dispatch-retry-backpressure.md) | retry、Retry-After、auth pause、quarantine、spool full |
| [HTTP Server](./08-server-bootstrap-http.md) | timeout、limits、统一错误、优雅关闭 |
| [Auth](./09-auth-project-scopes.md) | Project、scope、禁用和轮换测试 |
| [PostgreSQL](./10-postgresql-repositories.md) | 迁移、事务、Repository 和查询计划基线 |
| [Ingest/Query](./11-ingest-query-retention.md) | commit 后 ACK、duplicate、cursor、retention |
| [可观测性](./12-observability.md) | logs/metrics/health/pprof 可用于诊断 |
| [故障测试](./13-testing-fault-injection.md) | 四个崩溃窗口和连续序号验收 |
| [交付](./14-compose-ci-release.md) | Compose、CI 和可追溯 artifact |
| [性能](./15-performance-evolution.md) | 带环境与原始结果的报告 |

若某项没有证据，返回对应章节补齐。不要在总验收中临时降低标准。

## 3. 最终仓库形态

具体目录可以随实现调整，但责任边界应接近：

```text
cmd/
  agent/                   # 薄 main：load/build/run/exit
  server/                  # 薄 main：load/build/run/exit
  admin/ or glinectl/      # 可选：创建 Project/Key 的管理命令
  loadgen/                 # 合成日志与 HTTP 负载
internal/
  protocol/ingestv1/       # v1 DTO、错误码、校验边界
  agent/
    runtime/
    source/
    parser/
    batch/
    spool/
    checkpoint/
    transport/
  server/
    bootstrap/
    httpapi/
    ingest/
    query/
    auth/
    health/
  storage/postgres/
  platform/logging/
  platform/metrics/
migrations/
deployments/compose/
scripts/
  demo/
  fault-inject/
  bench/
docs/
  adr/
  tutorial/
  benchmarks/
examples/
.github/workflows/
```

不要为了匹配教程目录而机械搬迁稳定代码。评审的是依赖方向和所有权，不是路径是否一字不差。

## 4. 从零环境启动

### 4.1 环境前置

记录而不是猜测：

- Go 版本；
- Docker Engine/Compose 版本；
- 可用端口；
- CPU、内存和磁盘；
- 操作系统；
- Release 或 commit hash。

Server 演示所需 secret 通过环境注入。示例：

```powershell
$env:GLINE_DATABASE_URL = '<local-demo-dsn>'
$env:GLINE_KEY_PEPPER = '<local-demo-secret>'
docker compose -f deployments/compose/docker-compose.yaml up -d --build
```

教程不提供真实 secret。Compose 配置应有本地开发默认生成/seed 流程，生产式运行则要求显式 secret。

### 4.2 就绪而不只是运行

```powershell
docker compose -f deployments/compose/docker-compose.yaml ps
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/livez'
Invoke-RestMethod -Uri 'http://127.0.0.1:8080/readyz'
```

断言：

- 容器 running；
- liveness 成功；
- readiness 成功；
- 当前迁移版本符合 Server；
- 日志无循环重启和 secret；
- 数据库命名卷已经挂载。

若端口占用，修改演示配置，不终止未知用户进程。

## 5. 初始化权限边界

通过管理命令或受控 seed 创建：

```text
Project A: demo
  Key A1: ingest only
  Key A2: query only

Project B: isolation-check
  Key B1: ingest + query (仅测试)
```

保存 secret 的安全要求：

- 创建响应只展示一次；
- 演示脚本从环境读取；
- stdout、shell history、CI 日志和截图不出现完整 secret；
- 数据库只保存 key prefix、HMAC 和 metadata；
- 测试结束无需用破坏性数据库清理证明隔离，使用唯一 run ID 即可。

## 6. 正常路径 E2E

### 6.1 生成输入

创建一次唯一运行 ID：

```text
run_id = UUIDv7/UUID
sequence = 1..N
```

生成器向 demo 日志文件追加：

```text
INFO run=<id> sequence=1 order created
WARN run=<id> sequence=2 slow dependency
ERROR run=<id> sequence=3 request timeout
this-line-has-no-known-level run=<id> sequence=4
```

启动 Agent 时显式传入配置路径和 ingest Key 环境变量。等待条件不是固定 sleep，而是：

- Agent sent/ack metric 达到预期；或
- Query API 最终返回对应 run ID 的全部数据，直到总体 deadline。

### 6.2 查询与断言

使用 query Key：

```text
GET entries by project context
  from/to include test window
  service = demo service
  q = run ID
```

断言：

- 正好得到 N 个唯一 sequence；
- `UNKNOWN` 行仍保留原消息；
- service、host、agent ID 和 pipeline ID 来自可信 Agent 配置；
- `observed_at` 与 `ingested_at` 都存在且语义不同；
- next cursor 在多页数据下无重复、无遗漏；
- Query API 不返回其他 Project 数据。

## 7. 幂等验收

保存或构造一个固定 v1 batch，两次提交：

1. 第一次返回 `accepted`。
2. 第二次返回 `duplicate`。
3. 两次都返回相同 batch ID 和 accepted entry count。
4. 数据库 entries 只存在一组。

然后使用相同 batch ID 修改一条 message：

- 返回 409 `idempotency_conflict`；
- 数据库原内容不变；
- Server 日志有 request ID、project ID 和 batch ID，但不含完整 payload；
- Agent 分类为永久冲突并进入 quarantine，不自动换 ID。

这是精确幂等证据，不能用 Bloom Filter 命中或“最终查询看起来一条”替代。

## 8. 权限与输入安全验收

| 场景 | 预期 |
| --- | --- |
| 无 Authorization | 401 稳定错误结构 |
| 错误 secret | 401，不泄露 Key 是否存在 |
| disabled Key | 403 或统一策略中的稳定结果 |
| query Key 调 ingest | 403 scope denied |
| ingest Key 调 query | 403 scope denied |
| Project A Key 指定 Project B | body/query 中的 Project 被拒绝或根本不存在该字段 |
| 超大 body | 413，Handler 不写数据库 |
| 超多 entries | 413/400 稳定 code，不部分写入 |
| unknown JSON field | 400，协议错误可诊断 |
| trailing JSON | 400 |
| 非法 level/time/string length | 400 validation error |
| 查询无时间范围/范围过大 | 400 或受控默认，不执行无界扫描 |
| limit 超上限 | 拒绝或按明确合同处理，不静默制造大响应 |

检查访问日志中没有 Authorization、payload、message、attributes 或原始 query keyword。

## 9. 故障恢复验收

### 9.1 Server 中断

1. 正常发送一段序号日志。
2. 只停止/暂停 Server 服务，不删除数据库卷。
3. 继续生成固定数量日志。
4. 观察 spool bytes、pending batches 和 oldest pending 增长。
5. 恢复 Server。
6. 等待 spool 归零。
7. 查询整个 run ID，检查 sequence 集合。

通过条件：

- 没有缺失 sequence；
- 没有重复 entry；
- Agent 内存没有随 backlog 无界增长；
- 恢复流量没有无限重试风暴；
- 429/5xx 时退避符合合同。

### 9.2 四个崩溃窗口

逐一执行：

```text
A. Source read 后、spool commit 前
B. spool commit 后、HTTP send 前
C. PostgreSQL commit 后、ACK 到达 Agent 前
D. ACK 到达后、spool delete 前
```

每次使用独立 run ID，重启 Agent/Server 后查询并检查连续序号。必要时只在测试构建中使用 failpoint 精确触发，不通过随机 kill 猜测是否命中窗口。

### 9.3 文件轮转

- rename + recreate：旧文件尾部与新 identity 的日志都可查；
- truncate：检测事件可观察，截断后数据继续采集；
- copytruncate：记录其固有竞态，不声称测试能消除窗口数据损失；
- 半行：多次 write 直到换行只形成一个 entry；
- 超长行：执行明确策略且内存有界。

## 10. 背压与容量验收

用很小的测试 spool 上限触发容量路径：

```text
Server unavailable
 -> spool reaches high watermark
 -> warning metric/log
 -> spool reaches max
 -> default policy blocks Source and readiness fails
```

恢复 Server 后：

- Dispatcher 优先处理已有 batch；
- 水位下降；
- Source 恢复读取；
- readiness 恢复；
- 没有 goroutine 泄漏。

如果测试 `drop_oldest`：

- 只能在显式配置下启用；
- dropped entries/bytes 指标精确增加；
- 高等级日志不包含原始正文；
- 最终缺失序号必须与 drop 记录一致。

## 11. Shutdown 验收

### Agent

发送关闭信号时：

1. 停止读取新记录；
2. 已接受的完整记录进入 spool；
3. deadline 内继续 dispatch；
4. 未 ACK batch 留盘；
5. checkpoint、文件、timer、HTTP transport、spool 和日志 writer 关闭；
6. 下次启动恢复。

### Server

1. readiness 变为失败；
2. 停止接受新连接；
3. 等待在途事务；
4. 停止 retention/background job；
5. flush telemetry；
6. 关闭数据库连接池；
7. deadline 超时产生可诊断非零退出。

使用子进程和真实 signal 验证。函数测试不能证明操作系统信号、监听器和 exit code 一致。

## 12. 可观测性验收

### 必须能从指标回答

- 哪个 Pipeline 停止了？
- Agent 是否正在积压？积压多大、最老多久？
- batch 为什么重试或进入 quarantine？
- Server 每秒接收多少 entries？
- 请求 p95/p99 和主要状态码是什么？
- 数据库操作变慢还是 HTTP 解码变慢？
- Query 扫描与返回多少行？
- retention 上次何时成功？

### 基数检查

metric label 中不得包含：

- batch ID；
- request ID；
- 完整 project ID；
- message、query、error text；
- 任意用户 attributes。

### 健康语义

- PostgreSQL 停止时，Server `/livez` 仍成功，`/readyz` 失败；
- 一个 Pipeline 失败时，Agent 进程仍 live，但 Pipeline gauge 失败；
- spool 满时 Agent readiness 失败；
- pprof 默认不可从公开业务地址访问。

## 13. 性能验收

不要把教程中的目标数字当成结果。对当前 commit 执行[性能章节](./15-performance-evolution.md)的矩阵并保存：

```text
commit
hardware / OS / Go / PostgreSQL
dataset and generator seed
Agent/Server/DB config
warmup and duration
p50/p95/p99
throughput and error rate
CPU / memory / disk / DB metrics
query plans
profiles
raw results
```

至少形成一个完整优化故事：

```text
基线 -> 观测到瓶颈 -> 提出假设 -> 单一修改 -> 同条件复测 -> 代价
```

没有观察到瓶颈时，不要为了故事强行替换 JSON 库、增加缓存或引入 ClickHouse。

## 14. CI 与 Release 验收

CI 至少覆盖：

- format check；
- build Agent/Server；
- unit test；
- race test；
- PostgreSQL integration；
- migration from empty database；
- protocol/OpenAPI validation；
- Docker image build。

Release 验收：

- tag 指向通过 CI 的 commit；
- Windows/Linux artifacts 可启动；
- `--version` 输出 tag、commit 和 build metadata；
- 镜像使用不可变 tag，不只提供 `latest`；
- checksum 可验证；
- 示例配置不含 secret；
- Release notes 说明迁移、兼容和已知限制。

## 15. README 最终结构

1. 一句话定位。
2. 目标架构图。
3. 当前已验证 Features，不包含 Roadmap。
4. 五分钟 Quick Start。
5. 正常查询示例。
6. 故障恢复演示。
7. 可靠性保证和明确例外。
8. API/OpenAPI 链接。
9. 当前 commit 的性能表与完整报告链接。
10. 开发、测试和发布命令。
11. 限制、非目标和演进信号。
12. 设计文档、ADR 与本教程入口。

不要用 Grafana 截图、徽章或架构图代替可执行 Quick Start。

## 16. 五分钟最终演示

### 0:00-0:40 定位

- 小型自托管日志平台；
- 两个自研二进制 + PostgreSQL；
- 至少一次传输 + 精确幂等写入。

### 0:40-1:20 启动与权限

- Compose 服务 ready；
- 展示 Project 与分 scope Key，不展示 secret。

### 1:20-2:10 正常链路

- 写四类日志；
- Query API 按条件返回；
- 解释 observed/ingested time 和 cursor。

### 2:10-3:30 故障恢复

- 暂停 Server；
- 写入更多序号日志；
- 展示 spool/oldest pending；
- 恢复后积压归零，无缺失/重复。

### 3:30-4:15 幂等与隔离

- 重放 batch 返回 duplicate；
- 另一 Project 查询为空；
- scope 错误被拒绝。

### 4:15-5:00 性能与边界

- 展示带环境信息的实测表和一个 query plan/profile；
- 说明 copytruncate、单机数据库和无完整 UI 等边界；
- 说明何种实测信号才会引入 ClickHouse/队列。

## 17. 可以写进简历的内容

只有在对应验收通过后，才使用：

> 设计并实现 Go 日志 Agent，通过多 Pipeline 隔离、事务 spool、文件 checkpoint 与可取消退避，在网络和进程故障后恢复未确认批次；以故障注入覆盖四个崩溃窗口。

> 设计版本化批次协议和 PostgreSQL 幂等事务，以 `(project_id, batch_id)` 唯一约束及 payload hash 处理不确定网络重试，并提供项目隔离、scope 鉴权和 keyset 日志查询。

> 建立 Prometheus、pprof、Compose、CI 和可复现负载报告，在【实测条件】下获得【实测结果】，并根据【查询计划/profile】完成【具体优化】。

方括号必须替换成真实证据。若 Agent 可靠章节未完成，只描述当前已验证的并发 Agent，不写“日志平台”。

## 18. 最终评审问题

你应能在白板上回答：

1. Source 读完一行后，哪一刻开始具备崩溃恢复保证？
2. 为什么 checkpoint 不等 Server ACK？
3. Server 在什么事件后允许 Agent 删除 batch？
4. ACK 丢失为何不会重复写入？
5. spool 满、DB 停止、401 和 400 分别发生什么？
6. rename/recreate 和 copytruncate 的保证有何不同？
7. Project 隔离在哪几层被强制？
8. 为什么 query 使用 keyset 而不是 offset？
9. 为什么先 PostgreSQL，何时考虑 ClickHouse？
10. 哪些指标证明背压正在发生？
11. build/test/race/Compose/E2E/benchmark 各自证明什么？
12. 当前系统明确不保证什么？

如果某个答案只能说“框架会处理”，回到对应章节，找到真正的状态和所有权。

## 19. 最终完成门

- [ ] 干净环境可构建、迁移、启动。
- [ ] 正常文件→Agent→Server→PostgreSQL→Query 闭环通过。
- [ ] batch accepted/duplicate/conflict 三条路径正确。
- [ ] Project 与 scope 隔离通过。
- [ ] 四个崩溃窗口通过连续序号验证。
- [ ] Server 中断、恢复、spool full 和 quarantine 可观察。
- [ ] 文件 rename、truncate、半行和超长行有明确结果。
- [ ] Agent/Server signal shutdown 有子进程证据。
- [ ] health、metrics、logs、pprof 满足安全和诊断要求。
- [ ] CI 与 Release 对应同一 commit。
- [ ] 性能报告可复现，没有未经验证的形容词。
- [ ] README、设计文档、ADR、教程和实现一致。
- [ ] 五分钟演示可由另一个人在没有作者口头帮助时完成。

到这里，Gline 才达到了本系列所说的最终形态。以后增加 UI、ClickHouse、消息队列或 fleet 管理，应作为新的产品阶段重新定义目标、ADR 和验收，而不是无限延长当前 MVP。

