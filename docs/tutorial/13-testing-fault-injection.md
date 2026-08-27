# 13. 测试体系、故障注入与可靠性证明

> 本章描述目标测试体系，不表示这些测试当前已经存在。测试的任务是保护 Gline 的稳定合同，尤其是确认边界、项目隔离、崩溃恢复和资源所有权，而不是固定 Gin、SQL driver 或内部 goroutine 的实现形态。

相关设计：[协议与存储设计](../04-domain-api-and-storage.md)、[可靠性设计与故障矩阵](../05-reliability-security-observability.md)、[开发路线图](../06-development-roadmap.md)。

## 1. 本章目标

完成后你应能：

1. 区分单元、组件、集成、端到端和故障测试各自保护的合同。
2. 使用真实 HTTP、真实 PostgreSQL、真实 spool reopen 和真实文件系统验证边界。
3. 对四个关键崩溃窗口进行可重复故障注入。
4. 使用 `run_id + 连续 sequence` 检测缺失与重复，而不是只比较总行数。
5. 在 Windows PowerShell 和 CI/Linux 中运行同一套核心验证。
6. 识别偶发测试、环境污染和“测试实现细节”的问题，并能系统排查。

## 2. 前置条件

- 上传协议已有版本化 DTO 和稳定错误 code。
- Server ACK 只在 PostgreSQL 事务提交后返回。
- `(project_id, batch_id)` 唯一约束与 payload hash 冲突语义已经明确。
- Agent batch 与 checkpoint 在同一个 spool 事务中提交。
- Dispatcher 对 temporary、rate-limited、auth、permanent、conflict 有稳定分类。
- 进程可以通过 context/信号有界关闭。
- 测试数据库和 spool 路径与开发数据隔离。

如果某个合同尚未实现，应先将测试写成对应开发切片的验收，不要用大量 mock 假装链路已经闭环。

## 3. 测试原则

### 3.1 保护稳定合同

值得保护：

- spool batch 与 checkpoint 的原子关系；
- Server commit 后才 ACK；
- 同 batch 重试不重复写，冲突内容得到 409；
- API Key 的 Project 与 scope 隔离；
- keyset cursor 的无遗漏分页合同；
- shutdown、取消、重试和资源释放；
- 文件 rename/recreate、truncate、半行边界；
- `/livez` 与 `/readyz` 语义；
- 对外错误 code 与重试分类。

通常不值得单独测试：

- Gin 是否会调用一个已注册 Handler；
- `errors.Is`、JSON 标准库、SQL driver 自身行为；
- 简单 getter、字段赋值构造函数；
- 私有 helper 的调用次数；
- 精确 goroutine 数、内部 channel 布局；
- 普通日志文案、空格或 JSON 字段顺序。

### 3.2 先问回归场景

每加一个测试先回答：

1. 哪个真实缺陷可能再次出现？
2. 如果内部重构但外部行为不变，这个测试还应通过吗？
3. 语言、类型系统或已有高层测试是否已经覆盖？
4. 能否用更低成本、更稳定的验证替代？

答不出时，先不要增加测试。

## 4. 测试分层

| 层级 | 依赖 | 典型合同 | 运行频率 |
| --- | --- | --- | --- |
| 单元 | 内存、窄 fake | 校验、错误分类、batch 边界、cursor | 每次提交 |
| 组件 | 真实组件 + 临时目录/HTTP | spool reopen、transport、file rotation | 每次提交或 PR |
| PostgreSQL 集成 | 真实数据库 | migration、事务幂等、隔离、查询计划基本形状 | PR |
| 端到端 | Agent + Server + PostgreSQL | 文件到 Query API 的闭环 | PR/主分支 |
| 故障注入 | 子进程、网络/进程中断 | 四个崩溃窗口、恢复、不缺不重 | 主分支/定时 |
| 性能 | 固定环境与数据集 | 吞吐、延迟、资源、恢复速率 | 手工/定时 |

不要把全部验证塞进一个超大 E2E。失败时必须能快速定位到协议、spool、Server 事务或部署层。

## 5. 建议的测试目录与标签

目标布局可以逐步形成：

```text
internal/...                 # 包级单元与组件测试
internal/storage/postgres/   # 真实 PostgreSQL 集成测试
tests/
  contract/                  # Agent transport -> Server router
  e2e/                       # 文件 -> Query API
  fault/                     # 子进程和故障场景
  testdata/                  # 非敏感、固定小样本
```

集成测试可以使用 build tag，例如 `integration`；故障测试可使用 `fault`。标签只分隔环境成本，不能让关键测试长期无人运行。

PowerShell：

```powershell
go test ./... -count=1
go test -race ./... -count=1
go test -tags=integration ./... -count=1
go test -tags='integration,fault' ./tests/... -count=1 -timeout=15m
```

CI/Linux：

```bash
go test ./... -count=1
go test -race ./... -count=1
go test -tags=integration ./... -count=1
go test -tags='integration fault' ./tests/... -count=1 -timeout=15m
```

实际 package 路径和 timeout 以实现为准。不要用无限 timeout，也不要用 `-count=1` 掩盖偶发失败；排查时应主动重复运行。

## 6. 单元测试：快而有意义

### 6.1 优先测试

- batch 在条数、字节数、flush interval 边界形成；
- 单条超限时得到永久错误而非无限拆分；
- HTTP status/error code 映射到稳定重试类别；
- `Retry-After` 合法与非法输入；
- full jitter 上限与 context 取消；
- cursor 版本、签名、损坏和边界；
- 协议字段、时间范围、attributes 深度/大小校验；
- payload hash 的规范化输入保持稳定；
- spool full policy 的状态转换。

### 6.2 避免真实 sleep

将等待抽象为可注入时钟或 `Wait(ctx, duration)`：

```go
type Waiter interface {
	Wait(context.Context, time.Duration) error
}

type TimerWaiter struct{}

func (TimerWaiter) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

单元测试注入可控 waiter，断言“计划的 delay”和取消行为；不要 sleep 数秒等待。

### 6.3 使用窄 fake

Fake 应记录领域结果，如 `CommitBatch` 的参数和结果，而不是复刻完整 PostgreSQL。接口过大导致 fake 难写，往往说明业务层依赖边界过宽。

## 7. Agent 到 Server 的 HTTP 合同测试

这是持久化之前就应完成的第一条跨模块测试：

1. 使用真实 Server router 启动 `httptest.Server`。
2. Server 注入 recording ingest service 或 sink。
3. 构造真实 Agent HTTP destination。
4. 发送一个包含固定字段的 batch。
5. 验证 method、path、Content-Type、Authorization 和 DTO 解码。
6. Server 返回 accepted/duplicate/error，验证 Agent 分类。

测试保护的是 HTTP 合同，不应断言 Gin middleware 的内部调用顺序。

至少覆盖：

- accepted 200；
- duplicate 200；
- invalid JSON/validation 400；
- invalid/disabled key 401/403；
- conflict 409；
- too large 413；
- 429 + `Retry-After`；
- 503；
- response body 超限或非法 JSON；
- context deadline。

## 8. PostgreSQL 集成测试

必须连接真实 PostgreSQL，验证数据库承担的合同：

### 8.1 migration

- 空数据库执行全部 up migration 成功；
- schema 版本可被 readiness 读取；
- dirty/不兼容版本使 readiness 失败；
- migration 重复执行策略明确；
- 回滚能力按迁移工具和发布策略验证，不盲目承诺所有 down 都无损。

### 8.2 幂等事务

并发发送同一 `(project_id, batch_id)`：

- 只有一份 entries；
- 其余请求得到 duplicate；
- accepted entry count 一致；
- 相同 ID 不同 payload hash 得到 conflict；
- 事务失败不会留下 batch metadata 或半批 entries。

不要先 `SELECT` 再决定 `INSERT`，测试应故意并发以证明唯一约束处理竞态。

### 8.3 项目隔离

创建 Project A、B：

- 同一个 batch ID 可以分别存在于两个 Project；
- A 的 query key 看不到 B；
- ingest-only key 不能查询；
- query-only key 不能上传；
- request body/query 中伪造 project ID 不生效；
- Repository 每个查询都包含 project 条件。

### 8.4 查询分页

构造相同 `observed_at` 的多行数据，使用 `id` 打破平局。分页遍历后检查：

- 没有缺失、重复；
- 排序稳定；
- cursor 损坏被拒绝；
- cursor 不可跨 Project 使用；
- 时间范围和 limit 上限生效。

## 9. Spool 与文件系统组件测试

### 9.1 spool reopen

使用测试临时目录：

1. 打开 spool。
2. 在同一事务写 batch + checkpoint。
3. 关闭并重新打开。
4. 验证 batch 内容、顺序、ID、payload hash、checkpoint 不变。
5. ACK 后删除 batch，再 reopen 确认清理完成。

不要直接操作开发机器的真实 spool，更不能在测试清理中使用宽泛递归删除。

### 9.2 容量与损坏

- 小容量触发 high watermark 与 full policy；
- 默认 block 时 Source 停止推进；
- 显式 drop policy 才允许丢弃，并产生计数；
- schema version 不兼容时拒绝启动并给出行动提示；
- 截断/损坏文件不会被当作空 spool 静默启动。

### 9.3 真实轮转

在 `t.TempDir()` 中实际创建文件，覆盖：

- 正常 append；
- rename + recreate；
- truncate 后继续；
- 文件暂时消失后恢复；
- 半行分多次写；
- 超长无换行内容；
- 关闭时最后半行策略。

Windows 与 Linux 对 rename、打开 handle 和 file identity 的语义不同，因此至少在两套 CI OS 上跑文件测试。测试应围绕“记录是否被采集和 checkpoint 是否正确”，不要固定平台内部 identity 结构。

## 10. 确定性数据生成器

故障实验每条合成日志必须包含：

```text
run_id=<本次实验唯一ID> sequence=<从0连续递增> payload=<固定或可生成内容>
```

概念模型：

```go
type VerificationRecord struct {
	RunID    string `json:"run_id"`
	Sequence uint64 `json:"sequence"`
	Payload  string `json:"payload"`
}
```

生成器要求：

- `run_id` 每次实验唯一，但由测试记录下来；
- sequence 从明确起点连续递增；
- flush 到文件，必要时显式同步以区分“应用尚未写盘”和“Agent 丢失”；
- 记录计划条数、开始/结束时间、写入错误；
- 不含真实业务数据或凭证。

### 10.1 验证算法

Query API 拉取完整目标 `run_id`，按 sequence 统计：

```text
expected = [0, N)
seen[sequence] += 1

missing    = expected 中 seen == 0
duplicates = seen > 1
unexpected = sequence 不在 expected
```

还应记录：

- 第一个/最后一个可查询 sequence；
- 故障恢复完成时间；
- 页数与 cursor；
- 每个重复 sequence 的 entry ID/batch ID；
- 查询期间是否仍在写入。

只比较数据库总行数是错误的：一条缺失加一条重复仍可能得到相同总数。

## 11. 安全的故障注入机制

不要在生产 HTTP API 中加入“点击即崩溃”端点。优先使用以下顺序：

1. 测试进程中的窄 hook；
2. `go test` helper subprocess；
3. 测试专用 build tag；
4. 外部停止 Compose service；
5. 受控网络代理，用于断连/延迟/429。

一个窄 hook 的概念：

```go
type Failpoint interface {
	Reach(name string)
}

type NoopFailpoint struct{}

func (NoopFailpoint) Reach(string) {}
```

生产 bootstrap 永远注入 `NoopFailpoint`。测试 helper 注入在指定点调用 `os.Exit` 的实现。不要用可远程修改的全局 map，不要让任意名称从用户请求触发。

故障测试应在独立临时 spool、测试数据库/schema 和独立端口运行。开始前把解析后的绝对测试路径写入日志，确认它位于测试临时目录；清理只交给测试框架或精确 `docker compose down`，不删除命名开发卷。

## 12. 四个关键崩溃窗口

这是 Gline 可靠性声明的核心证据。

### 12.1 Source 已读，spool 事务提交前崩溃

注入点：`before_spool_commit`。

步骤：

1. 写入带 run ID 的连续记录。
2. Agent 读到目标 batch，但在 spool commit 前退出。
3. 检查磁盘：batch 和新 checkpoint 都不应可见。
4. 用同一配置重启 Agent。
5. Agent 从旧 checkpoint 重读并提交。
6. 等待查询可见，执行 sequence validator。

正确结果：无缺失；可能发生重新读取，但最终数据库有效单份。

如果 checkpoint 已推进而 batch 不存在，说明原子边界破坏，会永久丢失，必须停止扩大开发范围。

### 12.2 spool 已提交，HTTP 发送前崩溃

注入点：`after_spool_commit`。

步骤：

1. 提交 batch + checkpoint 后立即退出。
2. 验证 spool 中 batch 存在且 checkpoint 已推进。
3. 重启 Agent，先恢复旧 batch，再启动新 Source 采集。
4. Server 接收原始 batch ID 和完全相同 payload。
5. sequence validator 检查无缺失、无重复。

正确结果：batch 从 spool 恢复，不依赖原日志文件仍然存在。

### 12.3 PostgreSQL commit 后 ACK 丢失

注入点：`after_db_commit_before_ack`，或通过测试 transport 在响应返回客户端前断开连接。

步骤：

1. Server 提交 batch 与 entries。
2. 在 HTTP 成功响应到达 Agent 前中断。
3. Agent 将结果分类为不确定/可重试，保留 spool batch。
4. Agent 使用同一 batch ID 和 payload 重试。
5. Server 读取已有 payload hash，返回 `duplicate`。
6. Agent 收到合法 duplicate 后才允许清理本地 batch。

正确结果：数据库只有一份 entries；Agent 最终清理；accepted/duplicate 指标符合真实结果。

如果 Server 在 commit 前就增加 accepted 指标或 Agent 为重试生成新 ID，测试必须失败。

### 12.4 Agent 收到 ACK，本地 batch 删除前崩溃

注入点：`after_ack_before_spool_delete`。

步骤：

1. Server 返回 accepted/duplicate。
2. Agent 在本地删除事务前退出。
3. 重启后 batch 仍在 spool。
4. 再次上传同一 batch。
5. Server 返回 duplicate。
6. Agent 完成本地删除。

正确结果：最终数据库有效单份，spool 归零，checkpoint 不回退。

“已经 ACK 所以重启时直接跳过”不能只依靠内存状态；进程已丢失该信息。

## 13. 网络与协议故障矩阵

| 场景 | 操作 | 稳定断言 |
| --- | --- | --- |
| Server 不可达 | 停止 Server，继续写文件 | spool 增长、内存有界、恢复后清空 |
| 请求超时 | 响应延迟超过 deadline | 同 batch 重试，等待可取消 |
| 429 | 返回合法 `Retry-After` | 不早于允许窗口重试 |
| 400 | 返回稳定 protocol error | batch 进入 quarantine，不热循环 |
| 401/403 | 错误/禁用 key | Dispatcher 暂停或低频探测，readiness 失败 |
| 409 | 相同 ID 不同内容 | quarantine + 高等级诊断，不改 ID 重试 |
| 413 | batch 超限 | 按稳定规则拆分；不可拆单条进入 quarantine |
| 500/503 | 临时 Server/DB 故障 | full jitter 退避，同 batch 重试 |
| 响应非法/过大 | 恶意或错误 Server | body 有界读取，连接与错误正确处理 |
| TLS/DNS 临时失败 | 受控代理或错误地址 | 分类为临时错误，不泄露凭证 |

每个场景至少断言状态转换、资源有界和最终恢复，不能只断言“返回了 error”。

## 14. 文件故障矩阵

| 场景 | 操作 | 断言 |
| --- | --- | --- |
| rename + recreate | 重命名旧文件并创建同名新文件 | 旧 handle 尾部与新 identity 都被读取 |
| truncate | size 变为小于 offset | 检测事件、后续从配置位置继续 |
| copytruncate 竞态 | 复制期间写入再 truncate | 报告保证边界，不声称绝对零丢失 |
| 半行 | 分段写入，最后补换行 | 只产生一条完整 entry |
| 轮转时半行 | 旧文件以半行结束 | 按显式 partial 策略处理 |
| 超长行 | 超过 `max_line_bytes` | 内存有界，按配置隔离/拒绝/截断 |
| 暂时不存在 | 删除/延迟创建目标文件 | 可取消退避，恢复后继续 |
| 权限失败 | 使用平台可控权限场景 | Pipeline 致命失败且其他 Pipeline 不受影响 |

copytruncate 的固有竞态无法仅靠 tail 消除。测试的目标是验证“检测并继续”和“暴露潜在缺口”，不是伪造零丢失证明。

## 15. 并发、关闭与资源测试

需要覆盖：

- 一个 Pipeline 临时错误，其他 Pipeline 继续；
- 一个 Pipeline fatal/panic，被隔离并记录 stack，进程不盲目退出；
- 共享 Sender 永久失败后，生产者停止；
- shutdown 先停 Source，再提交形成中的 batch；
- deadline 内尽力上传，未 ACK batch 留在 spool；
- HTTP response body 总是关闭；
- timer 在取消时停止；
- 文件 handle、spool、DB pool、ops/pprof Server 按所有权关闭。

`go test -race` 是必要证据，但只覆盖实际执行路径。还应以组件生命周期测试触发错误、取消、panic 和重复启动/停止路径。

不要把“goroutine 数必须精确等于 N”写成脆弱合同。可以在稳定静默期比较基线范围，结合 goroutine profile 检查持续增长和未退出栈。

## 16. 端到端主场景

### 16.1 正常闭环

1. 启动测试 PostgreSQL 和迁移。
2. 启动 Server，等待 ready。
3. 创建测试 Project 和最小 scope Key。
4. 启动 Agent，写入 N 条确定性记录。
5. 通过 Query API 游标遍历。
6. 验证 sequence 完整且每个只出现一次。

### 16.2 中断恢复

1. 写入一部分记录并确认可查。
2. 只停止 Server service，不删除数据库/volume。
3. 继续写入剩余记录。
4. 观察 Agent spool 与 pending age 增长。
5. 恢复 Server。
6. 等待 spool 清空。
7. 查询整个 run ID，检查 missing/duplicate/unexpected。

### 16.3 项目隔离

使用相同 service/host 和相似内容分别写入 Project A、B，再分别查询。断言结果仅属于当前 Key 的 Project；服务端日志和指标不泄露另一项目内容。

## 17. 让测试可重复而非偶然通过

- 使用端口 `:0` 让操作系统分配端口，避免硬编码冲突。
- readiness 用轮询 + 总 deadline，不用固定长 sleep。
- 用事件、channel 或数据库状态协调崩溃点，不猜测执行时机。
- 每个测试使用唯一 run ID、Project 和临时目录。
- 失败时打印安全的状态摘要：run ID、sequence 范围、batch 数、状态分类。
- 固定随机种子，或在失败时打印 seed 以便复现。
- 不依赖测试执行顺序。
- 并行测试不能共享数据库 schema、端口或全局 registry。
- 清理失败不得覆盖原始断言失败。

排查偶发测试时：

PowerShell：

```powershell
go test ./path/to/package -run 'TestName' -count=50 -race -timeout=10m
```

CI/Linux：

```bash
go test ./path/to/package -run 'TestName' -count=50 -race -timeout=10m
```

如果重复失败，先收集时序与状态证据，不要连续增加 sleep。

## 18. Windows 与 Linux 差异

- Windows 打开的文件可能阻止某些 rename/delete 行为；测试应按系统能力建立等价场景。
- Windows 没有与 Unix 完全相同的 signal 集合。进程测试可以使用可控 stdin/控制事件或 helper subprocess 协议；不要假装 `SIGTERM` 脚本跨平台相同。
- 路径身份与大小写规则不同，不应把内部路径字符串作为跨平台合同。
- race detector 和 CGO/依赖要求需要在 CI runner 上明确验证。
- PowerShell 参数引用与 Bash 不同，文档应分别给命令，而不是混用反斜杠续行。

跨平台测试保护行为等价，不要求底层实现完全一致。

## 19. 失败处理

### sequence 缺失

先定位缺失范围对应的 batch 与 checkpoint：

1. 生成器是否成功写入并 flush？
2. Source 是否读到？
3. batch 是否提交 spool？
4. checkpoint 是否越过未提交数据？
5. Server 是否提交？
6. Query 是否因分页/时间范围漏读？

不要立即归咎于网络，也不要用补写掩盖证据。

### sequence 重复

检查同 sequence 是否来自相同 batch。相同 batch 重复说明数据库唯一约束/事务失效；不同 batch 说明 Agent 重读时生成了新幂等身份，或 checkpoint/spool 边界错误。

### spool 无法 reopen

保留失败目录作为测试 artifact，记录 schema version 和错误，不把损坏文件当空队列重建。确认测试清理范围后再进行隔离处理。

### PostgreSQL 测试偶发冲突

检查是否多个测试共享 Project/batch ID 或 schema，是否 migration 与测试并行。使用独立 schema/database 或序列化基础设施初始化，不要简单加重试让真实隔离错误消失。

### CI 通过、本机失败

记录 Go/PostgreSQL 版本、OS、文件系统语义、timezone 和实际命令。不要把本机绝对 `replace` 恢复回来“修复”依赖。

## 20. 常见错误

- 为每个修改函数机械增加测试。
- 单元测试 mock 掉 HTTP、数据库和 spool，却声称验证了可靠性。
- 只比较最终总行数。
- 每次重试生成新 batch ID。
- 用 sleep 猜测 commit 或 ACK 已发生。
- 生产二进制保留可远程触发的崩溃端点。
- 故障测试删除开发数据库 volume。
- 把框架调用顺序和精确日志文案固定成合同。
- 只在 Linux 测文件轮转，却宣称 Windows 可用。
- 运行一次 `go test -race` 就宣称没有并发问题。
- 测试失败时丢弃 spool/数据库证据，导致无法定位。

## 21. 验收证据

每次完整可靠性运行至少记录：

- Git commit、OS、Go/PostgreSQL 版本；
- 测试命令与退出码；
- run ID、预期 sequence 范围、seed；
- missing、duplicates、unexpected 的完整结果；
- 四个崩溃点各自的进程退出和恢复记录；
- spool 提交状态、Server accepted/duplicate 结果；
- 故障前、中、后的关键指标；
- race 结果；
- 失败 artifact 的位置和脱敏说明。

“全部通过”不是充分报告。需要让另一个人知道测试了什么、在哪个 commit 和环境下测试。

## 22. 复盘题

1. 为什么数据库总行数相等不能证明无缺失、无重复？
2. spool 提交前崩溃与提交后崩溃，checkpoint 分别应是什么状态？
3. PostgreSQL commit 后 ACK 丢失，Agent 为什么必须重试同一个 batch？
4. ACK 后本地删除前崩溃，为什么不能依赖内存中的“已成功”状态？
5. 哪些测试必须使用真实 PostgreSQL？
6. 为什么文件轮转不能只 mock `os.File`？
7. 如何避免 failpoint 成为生产安全漏洞？
8. race detector 能证明什么，不能证明什么？
9. 一个测试连续失败两次且证据不足时，你下一步应收集什么？
10. 哪个 Gline 合同值得长期保护，哪个当前策略应该允许演进？

## 23. 完成门

- [ ] 单元测试集中保护校验、分类、状态转换和协议合同。
- [ ] Agent transport 到 Server router 有真实 HTTP 合同测试。
- [ ] 真实 PostgreSQL 测试覆盖 migration、幂等事务、Project/scope 隔离和分页。
- [ ] spool reopen、容量和文件轮转使用真实临时文件系统验证。
- [ ] 四个崩溃窗口均有确定性、可重复的自动化证据。
- [ ] 每次故障实验使用唯一 run ID 和连续 sequence，报告缺失与重复。
- [ ] Server 中断恢复测试证明内存有界、spool 可恢复、最终有效单份。
- [ ] 关键生命周期路径通过 race 测试，并检查资源释放。
- [ ] Windows 与 Linux 的平台差异有对应验证或明确限制。
- [ ] 故障注入只存在于测试组装，不暴露生产远程开关。

通过本章后，可以准确表达“这些故障窗口已经被验证”。仍不能把有限场景扩写成绝对零丢失或生产可用；保证边界和未覆盖故障必须继续公开说明。
