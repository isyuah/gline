# 01. 建立可复现基线

## 1. 本章目标

本章不增加产品功能。目标是把当前仓库变成别人可以克隆、构建、测试和继续开发的可靠起点。

完成后应具备：

- 当前 Agent/Server 原型变更有清楚边界；
- `go.mod` 不再依赖 `E:/Proj/testx`；
- 基础构建、测试、竞态和 vet 在独立环境可运行；
- Server 上传原型的当前状态不会与后续 v1 协议混淆；
- 每个后续切片都有统一验证与提交方法。

对应路线图：[阶段 0](../06-development-roadmap.md)。

## 2. 当前起点

编写教程时，仓库的关键事实是：

- `master` 只有一个已提交基线 `dce24fc`；
- Agent 配置、生命周期和批量 Sender 已经有测试；
- Server 上传 Handler、占位 auth 和 `TestSink` 位于未提交工作中；
- `go.mod` 通过绝对路径 replace 引入个人测试库；
- Server 地址硬编码为 `:8080`；
- Agent→Server 的真实 HTTP 合同测试尚未完成；
- 当前工作树通过 build/test/race/vet，但依赖本机环境。

这些事实会变化。开始实现时重新运行本章命令，不要把教程快照当成永远正确。

## 3. 先梳理工作树

```powershell
git status --short --branch
git diff --stat
git diff --cached --stat
git diff -- cmd internal go.mod go.sum
git diff --cached -- cmd internal go.mod go.sum
```

逐个回答：

| 问题 | 当前需要确认的内容 |
| --- | --- |
| 删除的 `cmd/agent_test/main.go` 是否有意？ | 它是废弃 smoke 程序还是仍有价值？ |
| Server 文件为什么同时 AM？ | 暂存的是空骨架，未暂存的是实际实现，提交时必须取完整工作树版本 |
| `go.mod/go.sum` 修改属于什么？ | Gin/Server 依赖与可能的 Mongo 间接依赖，不能混入无关未来能力 |
| `.gitignore` 与 examples 是否应进入同一切片？ | 它们服务于可运行配置，可一并评审但要说明 |

在用户确认前，不要重置、stash 或覆盖这些修改。若当前 Server 原型可以形成一个独立、验证过的切片，应先完成它；若协议马上会重写，也可以明确保留为工作中状态，但不能假装工作树干净。

## 4. 消除本机测试依赖

### 4.1 为什么这是第一优先级

```go
replace github.com/isyuah/testx => E:/Proj/testx
```

这会让 CI、面试官和其他开发者在没有该目录时无法解析模块。即使业务代码很好，仓库不可复现也会直接削弱可信度。

### 4.2 盘点用法

```powershell
rg -n 'github.com/isyuah/testx|testx\.' -g '*.go' .
```

把断言按用途分类：

- 普通相等：标准 `if got != want`；
- slice/struct 深比较：现有 `go-cmp`；
- error chain：项目定义的错误合同可以用 `errors.Is/As`；
- 带排序比较：`cmpopts.SortSlices`；
- 只验证接口实现：若编译时断言足够，不写运行测试。

### 4.3 迁移策略

不要在仓库里复制一套新的通用断言框架。优先把测试改为清楚的标准测试：

```go
if diff := cmp.Diff(want, got); diff != "" {
    t.Fatalf("entries mismatch (-want +got):\n%s", diff)
}
```

对单个标量：

```go
if got != want {
    t.Fatalf("level = %q, want %q", got, want)
}
```

错误比较：

```go
if !errors.Is(err, wantErr) {
    t.Fatalf("Run() error = %v, want %v", err, wantErr)
}
```

测试消息应说明行为差异，而不是输出一个没有上下文的 false。

### 4.4 让 Go 工具更新模块

所有引用移除后：

```powershell
go mod edit -dropreplace github.com/isyuah/testx
go mod tidy
go test ./... -count=1
```

若 `go mod tidy` 删除了 Mongo driver 或其他未使用依赖，这是依赖图的正常结果；检查 diff，确认没有计划中的实际 import 被误删。不要为了“以后会用”保留未使用依赖。

### 4.5 独立环境验证

最低成本方法是在 CI 或临时 worktree/clone 中运行：

```powershell
go mod download
go test ./... -count=1
```

关键证据是构建过程没有访问 `E:/Proj/testx`。仅在作者机器上通过不够，因为该目录仍然存在。

## 5. 固定基础命令

### 5.1 快速循环

```powershell
gofmt -w <modified-go-files>
go test ./internal/<changed-package> -count=1
go vet ./internal/<changed-package>
```

### 5.2 切片完成

```powershell
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

### 5.3 为什么分层

- 快速循环给即时反馈；
- 全量测试发现跨包合同回归；
- race 成本较高，适合并发切片完成门；
- build 验证两个二进制入口；
- vet 不能替代测试，但能发现另一类静态问题。

命令成功仍不证明 Server 能启动或数据库链路可用。运行证据会在后续章节增加。

## 6. 建立协议前的最小 HTTP 集成

在正式 v1 DTO 重构前，先保护当前发送端与接收端确实兼容：

```text
GlineDest.SendEntries
  -> httptest.Server
  -> 实际 router
  -> EntriesUploadHandler
  -> recording EntrySink
```

测试至少断言：

- method 与 path 正确；
- Content-Type 正确；
- Authorization 格式符合当前合同；
- `entries` JSON 被真实 Handler 解码；
- Sink 接收的内容与发送内容一致；
- 非成功状态会被 destination 作为错误返回。

这个测试的价值不是永久固定旧 URL，而是先揭示两个半链路是否已经漂移。进入[协议章节](./03-protocol-domain-contracts.md)后，可以把它升级为稳定 v1 合同测试。

## 7. 把 Router 从 main 中分离

目标边界：

```go
type ServerDependencies struct {
    EntrySink sink.EntrySink
    // 后续加入 Authenticator、Logger、Metrics，不要一次预建空字段。
}

func NewRouter(deps ServerDependencies) (http.Handler, error) {
    // 注册 middleware、health 和 v1 route。
}
```

`main` 负责进程事项：加载配置、构建依赖、监听 signal、运行和关闭。Router 构造负责 HTTP 拓扑。这样集成测试不需要启动独立 exe，也不会占用固定 `8080`。

不要为了测试把所有对象做成全局变量。显式 dependency struct 已足够当前规模。

## 8. 记录基线证据

建议形成如下记录：

```text
commit: <hash>
go version: <output>
platform: windows/amd64 or CI target
commands:
  go build ./cmd/...              PASS
  go test ./... -count=1          PASS
  go test -race ./... -count=1    PASS
  go vet ./...                    PASS
known gaps:
  no persistent server storage
  provisional auth
  no durable agent spool
```

不要写覆盖率数字作为质量结论。当前最重要的是并发生命周期和 HTTP 边界得到行为验证。

## 9. 推荐提交边界

可形成两个独立切片：

1. `test: remove machine-local test dependency`
2. `test(protocol): cover agent-to-server upload contract`

若 Router 提取是第二个测试的必要实现，可以与其放在同一切片。不要顺便加入数据库、重命名所有包或升级无关依赖。

提交前：

```powershell
git diff --check
git status --short
git diff --cached --stat
git diff --cached
```

只暂存本切片文件。不要把用户其他未提交工作一起捕获。

## 10. 常见错误

### 为了去掉 replace，把 testx 源码复制进仓库

这只是把本地耦合变成自维护测试框架。当前断言规模不值得。

### 先创建完整 CI，再修可复现性

CI 会立即失败且没有新增诊断价值。先让模块独立，再在交付章节把同一命令放进 CI。

### 把当前 upload JSON 当作最终协议

当前测试只证明已有两端兼容。v1 仍需要 protocol version、batch ID、agent ID、sequence 和稳定错误模型。

### 看到工作树混乱就 reset

现有修改属于用户。先理解、分离和建立可恢复边界，不能丢弃。

### 一次提交所有“工程卫生”

依赖修复、Router 提取、CI、lint 和 README 是不同的行为切片。按可验证结果组织，而不是按“都是杂事”组织。

## 11. 本章完成门

- [ ] `go.mod` 没有个人绝对路径 replace。
- [ ] 新环境或 CI 能下载依赖并运行测试。
- [ ] 当前 Server 原型改动边界已经明确。
- [ ] Agent→Server 真实 HTTP 测试通过。
- [ ] Router 可以不监听真实端口而被测试。
- [ ] build/test/race/vet 有当前 commit 的结果。
- [ ] Git 暂存区只包含有意切片。

完成后，先学习[Go 并发与资源所有权](./02-go-concurrency-ownership.md)，再进入协议与功能开发。

