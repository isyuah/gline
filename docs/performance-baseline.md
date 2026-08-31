# 性能基线

这份基线用于让后续优化有可比较的起点，不代表所有机器、磁盘或部署拓扑
都能达到同样结果。基准覆盖可靠 Agent 在推进文件 checkpoint 前的本地
持久化边界：一次 WAL `Commit` 和一次 ACK，每一步都包含文件同步。

## 当前结果

环境：Windows amd64，AMD Ryzen 9 7940H，Go `go.mod` 声明的 Go 版本。

命令：

```powershell
go test ./internal/agent/spool -run '^$' -bench BenchmarkWALCommitAck -benchtime=10x -count=1
```

2026-08-31 本地结果：

```text
BenchmarkWALCommitAck-16    10    1612340 ns/op    2770 B/op    20 allocs/op
```

## 如何解释

- `1.61 ms/op` 是当前机器上每个批次完成本地 Commit + ACK 的观测值，不是
  网络吞吐或端到端 ingest 延迟。
- `-benchtime=10x` 只用于快速记录可审查的基线；正式容量评估应在目标磁盘
  上使用更大的样本，并分别测量批量大小、WAL 容量、故障重试和恢复时间。
- 正常 Dispatcher 队列读取使用 WAL 队头读取，不再在每一轮复制全部 pending
  payload；只有存在被资源状态阻塞的批次时才扫描队列以实现公平投递。
- PostgreSQL 查询仍应结合固定数据集、`EXPLAIN (ANALYZE, BUFFERS)` 和目标
  时间范围单独评估。当前仓库不把一次本地样本冒充成数据库容量结论。

基准入口位于 `internal/agent/spool/wal_benchmark_test.go`，CI 以相同命令
执行 Linux 基线，便于发现明显回归。
