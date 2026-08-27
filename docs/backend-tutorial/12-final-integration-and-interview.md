# 12. 最终集成、演示和后端面试

本章不是新的功能章节，而是把前面的模块组合成可运行、可证明、可讲述的后端项目。完成本章后，你应该能从一个干净环境启动 Server，创建 Project 和 Key，注册 Agent，上传并查询日志，模拟失败和恢复，展示后台任务和指标，并解释为什么最终阶段才引入水平扩展与高可用。

## 1. 最终完成门

至少要具备：

- Project、API Key、Agent、Pipeline、Batch、Entry、Quarantine、Audit 和 Retention 的领域模型；
- `POST /api/v1/batches` 在 strict decode、认证、scope、校验和 PostgreSQL transaction 后才返回 ACK；
- 相同 `(project_id, batch_id)` 和相同规范化 hash 返回 duplicate；不同内容返回 conflict；
- `GET /api/v1/entries` 强制 Project、时间窗口、limit 和 keyset cursor；
- 控制平面能创建/吊销 Key，并查看 Agent/Pipeline 状态；
- Retention、Usage、Quarantine/Replay 是可取消、有指标、有失败语义的后台任务；
- Agent 的 spool/checkpoint 和 Server 的幂等边界通过真实故障测试；
- Compose、migration、CI、release artifact 和 README 能在新环境复现；
- 所有性能数字都绑定 commit、硬件、数据集、配置和原始结果；
- 第 11 章的水平扩展/高可用设计有实验或明确标为下一阶段，而不是把目标写成现状。

## 2. 推荐最终目录

```text
cmd/
  server/
  agent/
  glinectl/
internal/
  protocol/ingestv1/
  domain/
  server/
    auth/
    control/
    ingest/
    query/
    operations/
    httpapi/
    bootstrap/
  storage/postgres/
  platform/
    logging/
    metrics/
  agent/
    source/
    parser/
    spool/
    checkpoint/
    dispatcher/
    destination/
migrations/
deployments/compose/
tests/
docs/
```

目录只是建议。真正要评审的是：HTTP adapter 不依赖 SQL；领域服务不依赖 Gin；Repository 接口由使用方定义；控制平面、数据平面和后台任务的所有权清楚。

## 3. 五分钟演示

### 0:00-0:45：定位和拓扑

说明 Gline Server 是多租户日志管理后端，Agent 是边缘可靠客户端。展示四个逻辑平面和 PostgreSQL 边界，明确第一版是模块化单体。

### 0:45-1:30：初始化控制平面

```powershell
docker compose up -d
glinectl project create --slug demo --display-name "Demo"
glinectl key create --project demo --scope ingest
glinectl key create --project demo --scope query
```

只把成功创建后显示一次的 secret 放入被忽略的本地配置；不把真实 Key 放入终端录屏或日志。

### 1:30-2:20：正常接入和查询

启动 Agent，追加 INFO/WARN/ERROR 和一条解析失败记录。通过 Query API 展示 Project 隔离、时间过滤、服务过滤和 cursor 下一页。

### 2:20-3:20：故障恢复

暂停 Server，继续追加日志，展示 Agent spool bytes、最老批次年龄、重试状态和 Agent readiness。恢复 Server 后观察 backlog 下降，并用连续序号检查无缺失、无重复。

### 3:20-4:10：后端治理

展示一个被 Quarantine 的坏批次、审计事件、Project quota、Retention 运行结果和 Usage 汇总。解释坏批次为什么不是无限重试，也不是静默删除。

### 4:10-5:00：设计取舍和最终演进

展示 PostgreSQL query plan、故障测试和性能报告。说明为什么没有一开始拆微服务，什么证据会触发读副本、消息队列、ClickHouse 或 Server 拆分。

## 4. 后端面试主线

### 为什么不是普通 CRUD？

因为核心问题不是创建和查询日志，而是：跨 Agent/Server 网络边界的批次协议、数据库 commit 与 ACK 的一致性、重复请求幂等、Project 隔离、后台数据治理、文件采集恢复和受控扩展。

### 为什么使用模块化单体？

Ingest、Query、Control 和 Retention 共享 Project、Batch、Entry 和权限边界。第一阶段共享进程和 PostgreSQL transaction 能减少跨服务一致性问题；内部使用窄接口保留拆分空间。只有实测证明接入与查询需要独立扩缩容时才拆分。

### 如何保证重复上传不重复写？

Agent 首次进入 spool 时冻结 batch ID 和 payload。Server 在 `(project_id, batch_id)` 唯一约束下写 metadata，比较 Server 自己计算的 payload hash；相同内容返回 duplicate，不同内容返回 conflict。ACK 只在 PostgreSQL transaction commit 后返回。

### 如何保证查询不会拖垮系统？

强制 Project 和半开时间窗口，限制 limit、关键词长度和查询超时；使用 `(observed_at DESC, id DESC)` keyset pagination；根据真实 filter shape 和 `EXPLAIN (ANALYZE, BUFFERS)` 维护少量索引；慢查询和 pool wait 进入指标。

### 如何从模块化单体演进到高可用？

先让 Server 无本地业务状态，完成多副本兼容、migration 兼容、readiness、优雅关闭和重复请求测试；再通过负载均衡水平扩展。查询压力优先评估读副本，写入确认仍以主库 commit 为边界。只有在数据库、接入流量或恢复窗口持续不满足目标时，才引入队列、分区、ClickHouse 或独立服务。

## 5. 发布前检查

```powershell
git status --short --branch
git diff --check
go mod verify
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
docker compose config --quiet
```

然后运行真实 PostgreSQL migration、E2E、故障矩阵和固定数据集基准。不能用旧 commit 的结果覆盖当前 dirty 工作树。

## 6. 诚实的项目边界

即使全部阶段完成，也应明确：Gline 不是 Loki/Elastic 的替代品，不提供无限保留、任意全文分析、多区域复制或在没有实验依据时的生产 HA 保证。简历中只写已实现且有证据的能力；下一阶段设计放在 Roadmap 和 ADR 中。
