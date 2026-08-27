# 10. 测试策略与故障注入

Gline 的价值不在于“有一个上传接口”，而在于系统在网络超时、数据库提交后 ACK 丢失、Agent 重启、文件轮转、配额耗尽和后台任务中断后仍然有可解释的结果。因此测试必须围绕稳定合同和故障窗口组织，而不是围绕函数数量组织。

## 10.1 当前差距与目标

仓库已有 Agent 生命周期、Pipeline 错误隔离和部分 Server 模块测试。开始本章时，仍应明确确认以下差距，而不是把测试数量当作覆盖证明：

- Agent 与 Server 的真实 HTTP 合同是否测试过；
- PostgreSQL 事务和唯一约束是否在真实数据库中测试过；
- ACK 丢失和 duplicate 是否有可重复的测试；
- schema、auth、quota、retention 和 query 边界是否有行为测试；
- race、cancel、shutdown、lease 和资源清理是否被验证；
- 故障注入是否在受控测试依赖中，而非生产开关；
- 测试是否能在干净环境重复运行。

目标是形成从纯函数到多进程故障实验的分层证据，并能在面试中说明“这个测试保护了哪个不变量”。

## 10.2 前置知识与测试金字塔

建议测试层次：

```text
          真实进程/Compose 故障实验
       Agent <-> Server <-> PostgreSQL
             HTTP 合同/集成测试
          Repository/Job/Quota 测试
      领域函数、编码、重试、分页测试
```

不是越上层越多越好。高层测试少而关键，低层测试快速、稳定、覆盖边界。每个新增测试先回答：

1. 它防止哪个现实缺陷？
2. 这个行为是 invariant、公开合同还是当前策略？
3. 内部重构后测试是否仍然有意义？
4. 是否有更便宜、更稳定的验证方式？

## 10.3 稳定不变量清单

把下面的陈述写进测试名称或测试说明：

### Ingest 不变量

- 合法 batch 在 PostgreSQL commit 后才得到成功 ACK；
- 相同 Project、相同 batch ID、相同 payload hash 的重试不会产生第二份有效写入；
- 相同 batch ID、不同 hash 会被拒绝为协议冲突；
- 未授权 Project、缺少 scope 或禁用 key 不能写入；
- 超过请求或 Project 配额的请求不推进 Agent 的安全 checkpoint。

### Query 不变量

- 查询结果只属于请求 Project；
- 游标只能继续向前，不使用 offset 导致深页漂移；
- 时间、service、level 等过滤器不会把其他 Project 的数据带出；
- 请求窗口和返回行数有上限；
- 无效游标以稳定错误码返回。

### Agent 不变量

- checkpoint 只在 batch durable commit 后推进；
- 未得到可接受 ACK 的 pending batch 不删除；
- timeout 使用同一 batch ID 重试；
- spool 满会产生背压或显式丢弃事件；
- 一个 Pipeline 的失败不隐式破坏其他 Pipeline。

### 生命周期不变量

- shutdown 有 bounded deadline；
- cancellation 后不会继续启动新的后台工作；
- `sending` lease 超时后可恢复；
- job 失败不会被静默吞掉；
- DB、文件、HTTP response body 和 goroutine 的所有权可追踪。

## 10.4 单元和领域测试

优先测试纯逻辑：

- canonical payload 与 hash 稳定性；
- batch size/entry count 上限；
- retry 分类与 exponential backoff 上限；
- `Retry-After` 解析和非法值；
- keyset cursor 编码/解码/签名；
- quota token 预留和释放；
- retention cutoff 计算；
- audit metadata allow-list 脱敏。

示例：

```go
func TestRetryTimeoutKeepsBatchIdentity(t *testing.T) {
    batch := mustBatch(t, 3)
    transport := &scriptedTransport{
        results: []SendResult{{Class: ResultTemporary}, {Class: ResultDuplicate}},
    }
    dispatcher := NewDispatcher(transport, fixedClock{}, noSleep())

    require.NoError(t, dispatcher.Send(context.Background(), batch))
    if got := transport.batchIDs; !slices.Equal(got, []string{batch.BatchID, batch.BatchID}) {
        t.Fatalf("retry changed batch identity: %v", got)
    }
}
```

这里测试的是稳定合同“重试不换 ID”，不是测试 dispatcher 内部调用了哪个 helper。

## 10.5 HTTP 合同测试

用 `httptest.Server` 验证 Agent transport 与 Server router 的真实编码、状态码和 header：

- 成功新写入返回 ACK；
- duplicate hash match 返回可接受的 duplicate 结果；
- duplicate hash mismatch 返回 conflict；
- 401/403、413、429、503 的 body 使用稳定 problem schema；
- request body 超限时 Server 在读取上限后拒绝，不能无限吃内存；
- Authorization 不会进入 response 或日志；
- request ID 在日志中可关联但不泄露 secret。

不要只用 mock handler 调用函数；至少让 HTTP 编码器、middleware 和路由一起运行。也不要只断言精确文案，除非文案是公开协议合同；更适合断言错误码、状态码和恢复动作。

## 10.6 PostgreSQL 集成测试

Repository 测试需要真实 PostgreSQL 行为，尤其是：

- migration 从空库按顺序执行；
- unique `(project_id,batch_id)` 在并发插入下只有一条有效记录；
- hash conflict 不覆盖原始 payload；
- Project 条件始终出现在 query 和 update/delete；
- keyset cursor 在同一时间戳下用 `id` 作为 tie-breaker；
- retention 分批删除并可在中途取消；
- job lease 的 owner 和过期条件阻止两个 worker 同时持有；
- transaction rollback 不留下半条 ingest 记录。

可以在本机 Compose 中启动专用测试数据库。测试结束只清理由测试创建的项目和容器；不要为了方便删除用户已有的数据库 volume。

## 10.7 并发、取消与 race

必须运行：

```powershell
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

针对并发合同增加确定性测试：

- 两个相同 batch 并发 ingest；
- 一个 query 与 retention 同时运行；
- quota 在并发请求下不会超过配置预算的允许误差；
- shutdown 与新请求竞态时，正在处理的请求遵守 deadline；
- cancellation 后 Source、Dispatcher 和 Job 都退出；
- Pipeline panic 不影响其他 Pipeline，但最终错误可诊断。

不要用固定 `time.Sleep` 等待 goroutine。使用 channel、WaitGroup、可控 clock 或测试 hook 表示事件发生；否则测试会在 CI 机器上随机失败。

## 10.8 故障注入设计

故障注入应位于依赖边界，使用明确的接口：

```go
type Store interface {
    InsertBatch(ctx context.Context, batch Batch) error
}

type Transport interface {
    Send(ctx context.Context, batch Batch) (SendResult, error)
}

type FaultPlan struct {
    FailAfterPayloadSync bool
    FailBeforeDBCommit   bool
    DropHTTPResponse     bool
    DelayDB              time.Duration
}
```

注入点与预期：

| 注入点 | 预期证据 |
| --- | --- |
| payload sync 后退出 | spool 可发现完整 payload，checkpoint 依合同处理 |
| DB commit 前失败 | Agent 得到临时失败，Server 不留半条数据 |
| DB commit 后丢 response | Agent 重试同 batch，收到 duplicate |
| PostgreSQL 连接池耗尽 | ready 或 quota 反映资源压力，不崩溃 |
| retention 删除中断 | 下次从稳定边界继续，不能跨 Project |
| query cancel | DB context 被取消，HTTP 响应不继续写入 |
| lease owner 进程退出 | 过期后另一个 worker 接管 |

测试实现不要从任意环境变量读取布尔值。生产二进制应默认无故障注入；测试构造函数显式接收 `FaultPlan`，并在每个测试结束后清理资源。

## 10.9 崩溃窗口实验

为四个窗口编写可重复实验：

```text
W1 Source read -> spool commit
W2 spool commit -> HTTP write
W3 HTTP write -> Server DB commit
W4 Server DB commit -> Agent reclaim
```

每次实验都保存：

- 初始文件内容和生成的 entry 序号；
- batch ID、payload hash；
- 进程退出点和退出原因；
- 重启扫描结果；
- Server 数据库中 `(project_id,batch_id)` 数量；
- 最终序号缺口、重复数和 quarantine 状态；
- 相关 metrics 和结构化日志。

验收表必须写清“最多重复”或“哪些场景未覆盖”，不能只写“无丢失”。机器断电、文件系统损坏和 copytruncate 的不可观测窗口要作为明确限制。

## 10.10 性能测试与结果表达

性能测试先定义变量，不先编造数字：

```text
payload size: 1 KiB / 10 KiB / 100 KiB
entries per batch: 1 / 50 / 500
concurrent agents: 1 / N
database: local Compose PostgreSQL
duration: warm-up + fixed measurement window
```

记录吞吐、p50/p95/p99、错误率、DB pool wait、CPU、RSS、spool lag 和 query rows。区分峰值、稳定持续值和恢复速度。机器、Go 版本、数据库参数、数据分布必须写入报告。

不要在简历写“支持十万条/秒”而没有可重复脚本和环境说明。写“在固定配置下完成了 X 场景基准，瓶颈由 DB pool wait/索引/编码占用主导”更可信。

## 10.11 实施顺序、验收证据和完成门

实施顺序：

1. 写不变量清单和失败矩阵；
2. 补齐领域和 HTTP 合同单测；
3. 用 Compose 跑 PostgreSQL 集成测试；
4. 加 race、cancel、shutdown 和 lease 测试；
5. 增加受控 fault hooks；
6. 编写四个崩溃窗口实验；
7. 固化性能脚本、报告和 CI 入口；
8. 在每次结构演进后重新跑关键故障矩阵。

验收证据：

- 一条命令能运行快速测试；
- 一条命令能启动依赖并运行集成测试；
- race/vet 结果被 CI 保存；
- 故障实验输出可关联 batch ID、request ID 和数据库结果；
- 测试不依赖开发者机器上残留的数据；
- 失败测试不会静默跳过关键数据库验证。

复盘题：

1. 为什么 ACK 丢失测试比“Server 返回 200”测试更重要？
2. 哪些行为是协议合同，哪些只是当前错误文案？
3. 一个测试如果依赖固定 sleep，可能在什么情况下误报？
4. 如何证明 query 没有跨 Project 泄露？
5. 性能结果怎样避免变成不可复现的简历数字？

完成门：

- [ ] 测试按稳定合同分层；
- [ ] HTTP、PostgreSQL、race 和故障窗口都有证据；
- [ ] timeout/duplicate、quota、lease、retention、cancel 都被覆盖；
- [ ] 所有测试可在干净环境重复；
- [ ] 性能报告包含环境、变量和限制；
- [ ] 生产构建不会误启用故障注入。

