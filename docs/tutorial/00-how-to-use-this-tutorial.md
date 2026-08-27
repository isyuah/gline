# 00. 怎样使用本教程

## 1. 本章目标

完成本章后，你应理解：

- 为什么教程不提供一个一次性大补丁；
- 如何区分当前事实、目标代码、伪代码和可执行示例；
- 如何判断一个开发切片是否真正完成；
- 什么时候应该停止改代码，转向日志、trace、故障注入或最小复现；
- 如何保护当前工作树中的用户改动。

本章没有业务代码，但它决定后面几十个实现决策是否可控。

## 2. 四类内容标记

教程中的内容按语义分为四类：

### 2.1 当前事实

来自当前源码、测试、Git 或本轮运行证据。例如：当前 `TickOrBatchSender` 在 destination 返回错误时退出。事实可能随实现演进而失效，所以开始章节前仍要重新检查。

### 2.2 稳定合同

项目有意长期保护的行为。例如：

- 相同 Project 内相同 batch ID 和相同 payload 重试不能写出重复 entry；
- query Key 不能执行 ingest；
- 一个 Pipeline panic 不应停止其他健康 Pipeline；
- Server 在数据库提交前不能返回成功 ACK。

稳定合同通常值得测试。

### 2.3 目标骨架

代码块表达推荐的类型、接口或依赖方向，但可能省略 import、错误包装、配置和测试辅助。它用于教学，不保证复制后立刻编译。

目标骨架会尽量采用：

```go
type BatchStore interface {
    Put(ctx context.Context, batch Batch, checkpoint Checkpoint) error
    Peek(ctx context.Context) (Batch, error)
    Acknowledge(ctx context.Context, id BatchID) error
}
```

而不是一次贴出几百行完整实现。你需要结合当前包结构完成细节。

### 2.4 可执行命令

命令块用于验证或正常开发。执行前检查：

- 当前目录是否正确；
- 命令是否会写文件、启动服务或改变数据库；
- 是否依赖已经完成的前置章节；
- 是否会影响用户正在运行的进程或容器；
- Windows PowerShell 和 CI/Linux 是否需要不同形式。

本教程不会要求你使用破坏性清理命令作为常规步骤。

## 3. 五级完成证据

| 等级 | 含义 | 示例 |
| --- | --- | --- |
| Implemented | 代码存在 | Repository 有 PostgreSQL 实现 |
| Verified | 对应行为被验证 | 幂等事务集成测试通过 |
| Integrated | 与相邻模块连通 | Handler 调用真实 Repository |
| Runnable | 完整程序可启动并完成场景 | Compose 中上传后可以查询 |
| Accepted | 用户或约定验收者确认体验 | 演示流程通过并被接受 |

不要用低一级证据代替高一级结论：

- 编译通过不代表 SQL 正确；
- Repository 单测通过不代表迁移可运行；
- Compose 容器处于 running 不代表 `/readyz` 就绪；
- 上传返回 200 不代表数据库可查询；
- 旧 benchmark 不代表当前 commit 性能。

## 4. 开始每章前的只读检查

在仓库根目录执行：

```powershell
git status --short --branch
rg --files -g 'AGENTS.md' -g '!vendor' -g '!node_modules'
git log --oneline --decorate -10
```

然后查看本章涉及文件的 diff：

```powershell
git diff -- <path-a> <path-b>
git diff --cached -- <path-a> <path-b>
```

如果同一核心文件里已有无法区分的用户修改，不要覆盖。先理解它与本章是否兼容；大规模变更且工作树混杂时，先建立安全 checkpoint 或使用独立 worktree，并等待用户选择。

## 5. 一个切片应该有多大

理想切片满足：

- 有一个外部可说明的结果；
- 失败时可以独立回退；
- 修改集中在一个模块和相邻合同；
- 验证成本在当前阶段可承受；
- 不需要同时保留多个互不兼容的半成品。

好的例子：

```text
让 GlineDest 产生的 v1 batch 被真实 Server router 解码并交给 recording sink
```

过大的例子：

```text
完成全部 Server、数据库、鉴权、Agent spool 和监控
```

过小且缺乏用户价值的例子：

```text
创建五个空接口和目录
```

## 6. 测试决策

写测试前回答：

1. 它防止什么真实回归？
2. 这是稳定合同、当前策略，还是实现细节？
3. 合理重构后，这个测试是否仍然有价值？
4. 编译器、类型系统或已有高层测试是否已经保证？
5. 有没有更便宜、更接近真实边界的验证？

例如：

- `SourceError.Unwrap` 若被调用者用于错误分类，测试自定义错误合同有价值；
- 不需要专门测试 Go 标准库的 `errors.As`；
- 不要断言 Handler 内部调用 helper 的次数，应该断言非法 payload 不写数据库；
- 不要断言 JSON 字段排列顺序，应该断言协议可解码且字段语义正确。

## 7. 两次失败后的调试切换

同一个错误若已经基于同一假设改了两次仍未解决，停止继续猜。改为收集：

- 最小输入和确定性复现；
- 完整错误链和分类；
- HTTP request ID 与状态码；
- SQLSTATE、事务边界和查询计划；
- goroutine、CPU、heap、block profile；
- spool/checkpoint 的状态摘要；
- 文件 identity、size 和 offset；
- Compose health 与容器日志。

有新证据指向不同根因后，再开启新的修改路径。

## 8. 依赖管理约定

- 使用 `go get <module>@<compatible-version>` 增加依赖，让 Go 工具更新 `go.mod` 和 `go.sum`。
- 使用 `go mod tidy` 清理依赖图。
- 不手工猜写 `go.sum`。
- 选择版本时先检查当前 Go 版本、现有依赖和官方兼容信息。
- 一个依赖只因“将来可能用到”而进入模块是不够的。
- 优先使用标准库和已经存在的库；只有真实复杂性或成熟实现值得复用时再增加。

教程提到 pgx、bbolt、Prometheus client 或 migration 工具时，表示推荐类别和候选，不代表要求锁定教程编写时的最新版本。

## 9. 数据与秘密

- 示例 Key、Project 和日志必须是合成数据。
- `.env`、本地 Agent 配置、数据库 DSN 和真实 token 保持忽略。
- 运行日志不输出 Authorization、用户日志正文或查询关键词。
- benchmark 数据集不得从真实生产日志复制。
- 截图和 CI artifact 发布前也要检查秘密。

## 10. 本章完成门

你应能回答：

- “测试通过”和“系统可运行”有什么区别？
- 什么情况下应停止继续写代码并增加可观测性？
- 为什么不能直接复制教程中的所有目标骨架？
- 一个切片何时值得提交？
- 当前工作树有未提交代码时，开始大改前要检查什么？

回答清楚后，进入[建立可复现基线](./01-baseline-and-workflow.md)。

