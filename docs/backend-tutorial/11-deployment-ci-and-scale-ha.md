# 11. 部署、CI、发布与最终水平扩展/高可用

本章是后端路线的最后一段。前面的单机模块化 Server、Agent spool、PostgreSQL 幂等接入、查询、后台任务和测试没有完成前，不要提前部署一组看似高级的微服务。最终阶段的目标不是增加名词，而是基于故障、容量和运维证据改变架构，并诚实说明一致性语义变化。

## 11.1 当前差距与最终分层

把交付状态分为四层：

```text
阶段 A：本地可复现
  Agent + Server + PostgreSQL，单机 Compose

阶段 B：可验证发布
  migration、CI、镜像、备份恢复、滚动升级演练

阶段 C：可扩展部署
  多个无状态 Server、负载均衡、共享 PostgreSQL、集中 metrics

阶段 D：高可用与数据演进
  PostgreSQL 故障域/读副本、队列或分析存储，基于证据拆分服务
```

当前代码是否已经达到某一层，必须由运行证据判断，不由目录或 Dockerfile 名称判断。本章中的 Compose、CI 和 HA 配置是目标骨架，不代表仓库已经拥有生产级部署。

## 11.2 前置知识

开始前应理解：

- 进程、容器、卷、网络和健康检查；
- migration 的向前兼容和回滚困难；
- PostgreSQL 备份、恢复、WAL/复制的基本概念；
- 负载均衡的连接复用、超时和 draining；
- stateless HTTP Server 与本地 Agent 状态的区别；
- 至少一次消息传递、消费者幂等和最终一致性；
- SLO、RPO、RTO 和故障域。

## 11.3 本地 Compose：先做可复现闭环

第一份 Compose 只提供主链路需要的服务：

```yaml
services:
  postgres:
    image: postgres:<pinned-version>
    environment:
      POSTGRES_DB: gline
      POSTGRES_USER: gline
      POSTGRES_PASSWORD: ${GLINE_DB_PASSWORD:?set in environment}
    volumes:
      - gline-postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gline -d gline"]
      interval: 5s
      timeout: 3s
      retries: 10

  server:
    build:
      context: .
    environment:
      GLINE_DATABASE_URL: ${GLINE_DATABASE_URL:?set in environment}
      GLINE_SERVER_API_KEY_PEPPER: ${GLINE_SERVER_API_KEY_PEPPER:?set in environment}
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "8080:8080"

volumes:
  gline-postgres:
```

不要在仓库提交真实密码，也不要使用 `latest` 作为演示可复现性的版本标签。具体镜像版本由项目维护者在实际环境中选择并锁定；教程不编造版本号。

本地启动步骤：

```powershell
$env:GLINE_DB_PASSWORD = "local-only-password"
$env:GLINE_SERVER_API_KEY_PEPPER = "local-only-pepper"
$env:GLINE_DATABASE_URL = "postgres://gline:local-only-password@postgres:5432/gline?sslmode=disable"
docker compose up -d postgres
docker compose run --rm server migrate up
docker compose up -d server
```

Compose 网络中使用服务名解析数据库；从宿主机运行迁移时，连接地址不同，必须在脚本中显式区分，不能复制一条 URL 到所有场景。

## 11.4 Migration、启动和滚动升级

推荐把 migration 当作独立发布步骤，而不是每个 Server 实例启动时抢锁执行：

```text
备份/确认恢复点
  -> 运行向前兼容 migration
  -> 部署能读旧 schema、写新字段的应用
  -> 迁移流量和指标
  -> 观察窗口后再清理旧字段/旧代码
```

扩展字段的安全顺序通常是：

1. add nullable column/index concurrently（若适用）；
2. 部署代码双写或兼容读取；
3. 回填小批次并记录进度；
4. 验证新读路径；
5. 再考虑约束收紧或删除旧字段。

回滚应用不等于回滚数据库。删除列、改变枚举或重写大量数据前，必须有明确备份和恢复方案。migration 失败应阻止发布，而不是让 Server 带着未知 schema 继续接入。

滚动升级要求：

- readiness 在新实例准备好前保持 false；
- draining 后不接收新请求；
- 已接收的 ingest 请求在 deadline 内完成或返回可重试结果；
- PostgreSQL connection pool 在退出前关闭；
- Agent 对 503、timeout 继续保留 batch；
- 新旧版本之间的 batch DTO 在声明的兼容窗口内可互通。

## 11.5 CI 设计

CI 应按成本分层，快速反馈和真实边界都要保留：

```text
pull request:
  gofmt check
  go test ./... -count=1
  go vet ./...
  go test -race ./...
  markdown/link/fence checks

integration:
  start PostgreSQL service
  apply migrations
  run HTTP + repository + Agent/Server contract tests

release:
  build binaries
  build image
  generate checksums/SBOM if toolchain is available
  run smoke test against clean Compose
```

示例 GitHub Actions 骨架：

```yaml
name: ci
on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:<pinned-version>
        env:
          POSTGRES_PASSWORD: test
          POSTGRES_DB: gline
          POSTGRES_USER: gline
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U gline -d gline"
          --health-interval 5s --health-timeout 3s --health-retries 10
    steps:
      - uses: actions/checkout@<pinned-sha>
      - uses: actions/setup-go@<pinned-sha>
        with:
          go-version-file: go.mod
      - run: gofmt -l .
      - run: go test ./... -count=1
      - run: go test -race ./... -count=1
      - run: go vet ./...
      - run: go test ./... -tags=integration -count=1
```

实际 action 版本、Go 版本和数据库镜像应由仓库锁定；尖括号只是提醒“不能漂移”。CI 不应把集成测试静默标记为 skipped。依赖不可用时要失败并说明原因。

## 11.6 备份、恢复与生产前演练

“有备份”不是“可恢复”。生产前至少演练：

1. 创建 Project、API Key、batch 和审计事件；
2. 执行数据库逻辑备份或平台选定的物理备份；
3. 在隔离的新数据库恢复；
4. 校验 schema、Project 隔离、batch 唯一约束和查询结果；
5. 用 Agent pending batch 重新发送，验证 duplicate 语义；
6. 记录恢复耗时和人工步骤；
7. 清理仅由演练产生的资源。

必须定义：

- RPO：最多允许丢失多长时间的数据；
- RTO：从故障开始到可接入的目标时间；
- 备份保留周期和加密位置；
- 恢复时 API Key pepper、配置密钥和审计数据如何一起恢复；
- Agent 本地 spool 不在 Server 数据库备份中，二者的恢复责任不同。

不要在没有演练的情况下宣称“灾备”。如果只验证了逻辑备份导入，应表述为“已验证逻辑备份恢复”，而不是“具备高可用灾备”。

## 11.7 从单机到水平扩展

只有满足以下前置条件，Server 才能安全增加副本：

- Server 不把 batch、Project 配置或 job lease 只存进程内存；
- API Key 验证材料和 Project 状态来自共享持久化存储；
- Agent 使用稳定的 HTTP 协议，重试不依赖粘滞会话；
- ingest 唯一约束在共享 PostgreSQL 中生效；
- background job 有数据库 lease，或明确只运行单实例；
- health/readiness 能让负载均衡器摘除 draining/不 ready 实例；
- metrics 带 instance 维度但业务告警能聚合全局。

拓扑：

```text
Agents
  -> Load Balancer
       -> Server A (stateless)
       -> Server B (stateless)
       -> Server C (stateless)
              |
              v
         PostgreSQL primary
```

负载均衡超时要大于服务端合理请求处理时间，但不能无限等待。连接复用、body size、idle timeout、draining 时间和 retry policy 必须在客户端、Server 和 LB 三处协调。

Agent 的本地 spool 仍是边缘状态，不要把它假设成共享存储。一个 Agent 只应由一个运行实例消费自己的 source；如果需要 active/standby，必须另行设计文件锁、租约和重复采集语义。

## 11.8 PostgreSQL 主从、读副本与故障域

读副本不是免费扩容。引入后会改变一致性：

- ingest commit 在 primary 成功后，replica 可能暂时不可见；
- query 若读 replica，用户会看到 read-after-write 延迟；
- retention 在 primary 执行，replica 可能更晚反映删除；
- replica lag 过大时必须摘除或降级，而不是继续承诺实时查询。

第一步可采用 primary 读写，只有在查询负载和 lag 指标证明需要时，才增加只读连接池：

```go
type DBRouter interface {
    Primary(ctx context.Context) *sql.DB
    Replica(ctx context.Context) (*sql.DB, error)
}
```

需要明确的策略：

- 写后短时间查询是否强制 primary；
- 游标在 primary/replica 切换时是否仍稳定；
- replica 失败是否回退 primary，以及回退预算；
- promotion 期间 ingest 返回什么，Agent 如何重试；
- 复制延迟指标和告警阈值如何从实测确定。

故障域至少区分：应用进程、容器节点、数据库主实例、存储卷、网络区域和凭证系统。把 Server 副本放在同一台机器上不等于跨故障域高可用。

## 11.9 消息队列、ClickHouse 与微服务的触发证据

### 消息队列

触发证据可以是：

- HTTP ingest 持续受 PostgreSQL 写入抖动影响；
- 需要把接入 ACK 与异步持久化解耦；
- 需要多个独立消费者处理索引、审计或分析；
- 本地 spool 和 Server 连接池已不能吸收目标峰值。

引入队列后，ACK 语义必须重新定义：

```text
ACK after DB commit      -> 客户端知道日志已写入查询库
ACK after queue append   -> 客户端只知道消息已进入队列
```

后者需要消费者幂等、死信、重放、积压告警和最终一致性说明。不能把“用了 Kafka”写成可靠性自动提高。

### ClickHouse 或其他分析存储

触发证据应来自查询工作负载：时间范围聚合、全文/高基数过滤已明显压垮 PostgreSQL，且数据保留和一致性要求允许分析库延迟。引入后保留 PostgreSQL 作为控制元数据/接入事实库，还是完全迁移，必须由数据模型和恢复策略决定。

### 微服务

只有当模块出现独立扩缩容、独立发布、团队边界或故障隔离需求，且模块化单体的 metrics/trace 已证明边界时才拆分。优先候选可能是 Query 或异步分析消费者，而不是把 Project、Key、Batch 随意拆成多个服务。

每次演进必须记录：

1. 现有瓶颈证据；
2. 新组件解决的具体问题；
3. 数据一致性和重试变化；
4. 新的运维成本和故障模式；
5. 回滚/降级路径；
6. 验收指标和停止条件。

## 11.10 HA 最终阶段实施顺序

建议顺序：

1. 完成单机模块化 Server 和端到端故障矩阵；
2. 固化镜像、migration、Compose、备份恢复和 CI；
3. 让 Server 无状态化，移除不必要的本地业务状态；
4. 用两实例 + 负载均衡做滚动升级演练；
5. 观察数据库连接、锁、p95/p99、错误率和 job lease；
6. 若查询负载成为证据，再引入读副本并处理 read-after-write；
7. 若接入与持久化解耦有证据，再评估消息队列；
8. 若查询模型需要，再评估 ClickHouse；
9. 只有边界稳定且部署/回滚清楚后，拆分独立服务。

每一次扩展都要先写 ADR，包含“不做什么”。例如：“本阶段不保证跨副本强一致查询；写后五秒内的查询路由 primary；队列未引入前 ACK 仍表示 PostgreSQL commit。”

## 11.11 发布验收、面试表达与完成门

发布验收：

- 干净机器或干净 Compose 可启动 PostgreSQL、migration、Server 和测试 Agent；
- CI 能阻止编译、race、vet、migration 和集成测试回归；
- rolling drain 期间已有请求有边界，新的请求可重试；
- 备份恢复演练有时间记录和数据校验；
- 两个 Server 实例并发 ingest 时 batch 幂等仍成立；
- replica/queue/ClickHouse 方案都有一致性和回滚说明；
- 未把理论容量写成已测事实。

面试中可以这样讲：

> 第一阶段我选择模块化单体和 PostgreSQL，因为核心问题是 Agent 到 Server 的可靠接入、批次幂等和查询隔离，而不是服务数量。完成故障注入和容量基线后，Server 可以水平扩展为无状态副本；如果查询或异步消费者出现独立瓶颈，再分别评估读副本、队列或分析库，并重新定义 ACK 和一致性语义。

这段话比“用了微服务、Kafka、Kubernetes”更有说服力，因为它说明了选择、证据和代价。

复盘题：

1. 为什么先做 Compose/CI/恢复演练，再谈 HA？
2. Server 水平扩展时，哪些状态必须离开进程内存？
3. 读副本会破坏什么用户体验？如何补偿？
4. 队列 ACK 与 PostgreSQL commit ACK 的区别是什么？
5. 什么指标能证明 Query 应该拆分，而不是继续优化 SQL？
6. 微服务拆分后，哪个事务边界会消失，如何替代？

完成门：

- [ ] 单机 Compose、migration、CI 和 smoke test 可重复运行；
- [ ] 备份恢复和滚动升级有实际证据；
- [ ] Server 在共享 PostgreSQL 下可运行多副本；
- [ ] draining、readiness、Agent retry 和 DB lease 协同；
- [ ] 读副本、队列、分析库和微服务都有触发证据与一致性说明；
- [ ] HA 阶段没有伪造容量、可用性或零丢失承诺；
- [ ] 最终架构能通过故障、恢复、发布和回滚演示。

