# 07. Retention、Quarantine、Replay、审计与 Usage

只有接入和查询还不够。一个可运营的后端必须能处理“坏数据怎么办、过期数据怎么办、失败是否可恢复、谁做过什么操作、每个 Project 消耗了多少资源”。本章将这些能力做成明确的后台任务和状态机。

## 1. 三种数据处理路径

```text
正常接入
  -> ingest_batches + log_entries
  -> 可查询
  -> retention 到期后删除

协议/业务永久错误
  -> Agent 本地 quarantine（保留原批次）
  -> 人工诊断/修复
  -> Agent 或 Admin replay

服务端已接受但异步处理失败（仅在明确设计后启用）
  -> server quarantine_batches
  -> replay worker
  -> 成功后进入正常幂等写入
```

第一版优先把 Agent 的永久错误隔离做好。Server 侧 quarantine 只有在你真的引入异步解析、外部索引或批量后处理时才启用。不要为了增加表而改变 ACK 语义。

## 2. 错误分类决定数据归宿

### 2.1 Agent 看到的错误

| 类型 | 例子 | 处理 |
| --- | --- | --- |
| 可重试 | 429、500、503、网络超时 | 原 batch 不变，按退避重试 |
| 永久协议错误 | 400、413、422 | 原 batch 写入本地 quarantine，不继续重试 |
| 身份/配置错误 | 401、403 | 暂停发送，告警，等待人工修复 |
| 幂等冲突 | 409 | 保留原 batch 并报警，不能自动改 batch ID |

`batch_id` 和 payload 必须在 quarantine 中保持原值，不能“修复时重新生成一个 batch”掩盖协议 bug。人工修复后可以明确创建新版本或 replay 原 batch，必须记录操作。

### 2.2 Server 的失败边界

同步 PostgreSQL 写入失败时，Server 返回 5xx/503，Agent 保留 spool。只有 Server 已经把完整 payload 写入 `quarantine_batches` 并提交成功，才可以返回一个明确的 `202 quarantined`，允许 Agent 删除本地副本。这个响应必须包含状态和 batch ID，且在 OpenAPI 中说明“不可查询，等待 replay”。

如果没有实现这个持久化 quarantine 事务，就不要返回 202；返回 500，让 Agent 重试才是安全行为。

## 3. Agent 本地 Quarantine

本地 quarantine 至少保存：

```text
batch_id
payload_hash
原始 batch 文件
最后 HTTP 状态和稳定 error code
首次失败时间
最近失败时间
尝试次数
request_id（若有）
```

文件命名不能只使用 error message；message 可能包含路径分隔符或敏感内容。使用受限的 `batch_id` 和日期目录，元数据使用独立 JSON。写入过程采用临时文件、fsync（按配置）和 rename，避免进程中断留下半文件。

Quarantine 目录也必须有容量上限。满时要暴露 `quarantine_write_failures`，并按产品策略暂停 pipeline；不能静默删除最旧批次。

## 4. Server Quarantine 表和状态机

当 Server 确实需要隔离时，使用显式状态：

```text
pending -> replaying -> resolved
                    \\-> discarded（永久失败）
replaying -> pending（租约过期或临时失败）
```

建议字段：

```sql
CREATE TABLE quarantine_batches (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    batch_id        uuid NOT NULL,
    payload         bytea NOT NULL,
    payload_hash    bytea NOT NULL CHECK (octet_length(payload_hash) = 32),
    error_code      text NOT NULL,
    error_detail    text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'replaying', 'resolved', 'discarded')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    claimed_at      timestamptz,
    resolved_at     timestamptz,
    UNIQUE (project_id, batch_id)
);
```

`payload` 仍受大小上限控制。敏感信息策略要明确：日志内容可能本身含隐私，访问 quarantine 需要更高的 admin scope，审计不能把 payload 写进日志。

## 5. Replay 的幂等设计

Replay 不应该复制一套“特殊插入逻辑”。它应读取已保存的原始协议 payload，重新走同一个 `IngestService.Accept`：

```text
quarantine row
  -> decode v1
  -> validate
  -> verify stored payload_hash
  -> IngestService.Accept(project, batch)
  -> accepted/duplicate: resolved
  -> validation/conflict: discarded + error_code
  -> transient DB error: 回到 pending
```

如果 replay 成功后 worker 在标记 `resolved` 前崩溃，再次 claim 同一行会再次调用 Accept；由于 batch ID + hash 幂等，结果是 duplicate，不会重复 entries。

### 5.1 Claim 并发

多个 Server 实例可能同时运行 replay worker。使用 PostgreSQL 行锁抢占：

```sql
WITH picked AS (
    SELECT id
    FROM quarantine_batches
    WHERE status = 'pending'
    ORDER BY created_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE quarantine_batches q
SET status = 'replaying',
    attempts = attempts + 1,
    claimed_at = now(),
    resolved_at = NULL
FROM picked
WHERE q.id = picked.id
RETURNING q.*;
```

claim 事务要短，不能在持有行锁时进行 HTTP、慢解析或长时间数据库写入。拿到 rows 后提交，再逐个处理。

### 5.2 租约与崩溃恢复

仅有 `replaying` 状态会留下“worker 崩溃后永远卡住”的问题。使用 `claimed_at` 记录租约起点，并按超时回收：

```sql
UPDATE quarantine_batches
SET status = 'pending', error_detail = 'replay lease expired'
WHERE status = 'replaying' AND claimed_at < now() - $1::interval;
```

回收操作必须有指标和审计事件，避免运维误以为数据正在处理。

## 6. Retention Worker

### 6.1 规则

默认依据 `ingested_at` 删除，而不是 `observed_at`。客户端时间可能错误，使用 observed time 会让未来时间戳的日志永久保留。

保留策略可以先做全局配置，后续再扩展到 Project：

```text
retention_days >= 1
retention_delete_batch_size 有上限
retention_interval
```

每次 job 只删除固定数量：

```sql
DELETE FROM log_entries
WHERE id IN (
    SELECT id
    FROM log_entries
    WHERE project_id = $1
      AND ingested_at < $2
    ORDER BY ingested_at, id
    LIMIT $3
)
RETURNING id;
```

具体 SQL 可根据 PostgreSQL 版本和索引调整；重要的是每轮事务短、可取消、可观测。删除 batch metadata 前要确认已经没有 entries 引用，或让外键级联承担明确责任并测试它。

### 6.2 Worker 代码形状

```go
func (w *RetentionWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.runOnce(ctx); err != nil {
			w.metrics.Failures.Inc()
			w.logger.Error("retention run failed", "err", sanitize(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
```

`runOnce` 内部循环直到本轮没有更多行或达到时间预算。不要每次启动无限删除，也不要忽略 context。

### 6.3 多实例执行

如果多个 Server 实例都跑 retention：

- 允许它们竞争小批量删除，因为 DELETE 是幂等的；或
- 使用数据库 advisory lock 保证同一 Project 只有一个 worker；或
- 把 retention worker 单独部署。

第一版可以接受重复扫描，但必须测量锁等待和数据库负担。不要在没有证据时声称“多实例一定安全”。

## 7. 审计事件

审计记录的是控制平面动作，不是每条日志：

```text
project.created
project.disabled
api_key.created
api_key.revoked
agent.registered
quarantine.replay_requested
quarantine.replay_completed
retention.policy_changed
```

每条审计事件至少包含：actor type/id、project、action、target、request ID、时间和经过脱敏的 details。Details 不应包含 secret、完整 batch payload 或 message。

审计写入的策略要明确：

- 安全敏感操作与主变更同事务写入，确保“操作成功但无审计”不会发生；
- 高频 usage/heartbeat 不写审计，使用 metrics 或 usage bucket；
- 审计失败是否阻止操作必须按动作分类。API Key 吊销等安全操作建议阻止成功返回，普通状态刷新可以降级并报警。

## 8. Usage 汇总与配额基础

接入路径可以在 batch 事务中原子增加 usage bucket：

```sql
INSERT INTO usage_buckets (project_id, bucket_start, entries, bytes)
VALUES ($1, date_trunc('minute', now()), $2, $3)
ON CONFLICT (project_id, bucket_start)
DO UPDATE SET entries = usage_buckets.entries + EXCLUDED.entries,
              bytes = usage_buckets.bytes + EXCLUDED.bytes;
```

这提供近似统计而不是账务系统。若 usage 更新和日志写入在同一个事务中，失败时一起回滚；若追求更高吞吐而异步汇总，必须承认短暂不一致并提供补算任务。

配额判断也要区分：

- 硬上限：拒绝当前 batch，返回 429/`quota_exceeded`；
- 软告警：接受但指标告警；
- 单批次上限：413/`batch_too_large`。

不要用内存计数作为跨 Server 实例的唯一配额来源。

## 9. 真实实现顺序

1. 先实现 Agent 本地 quarantine 和稳定错误码映射。
2. 实现 RetentionRepository 和单轮小批删除。
3. 加 retention worker 生命周期、取消和 metrics。
4. 建立 audit_events 表和控制平面动作审计。
5. 建立 usage_buckets 的事务汇总，提供查询接口。
6. 确认需要 Server quarantine 后，再添加表、claim SQL 和 lease 回收。
7. Replay 复用 IngestService，而不是复制写库逻辑。
8. 用 Compose 做“失败、隔离、修复、重放、查询可见”的 E2E。

## 10. 测试和故障注入

### Retention

- 过期行被删除，未过期行保留；
- 每轮最多删除配置的 batch size；
- worker 取消后不继续提交新事务；
- 数据库失败后记录失败指标并在下一轮恢复；
- 多实例同时运行不会删除未过期或其他 Project 数据。

### Quarantine

- 400/409/413 不会无限重试；
- quarantine 原始 payload hash 可验证；
- quarantine 容量满时有明确暂停/报警行为；
- payload 不出现在普通 access log。

### Replay

- 同一个 quarantine row 只能被一个 worker claim；
- worker 在 ingest commit 后崩溃，重复 replay 返回 duplicate；
- 业务校验仍失败时变为 discarded，保留 error_code、error_detail 和 attempts；
- lease 过期会回到 pending；
- replay 成功后查询能看到一次且仅一次 entries。

### 审计与 usage

- Key revoke、Project disable、replay 操作有审计事件；
- secret 不出现在 audit details；
- 同一 batch 重试不会重复增加 usage；
- usage 与日志事务一起回滚，或异步模式有补算证据。

### 完成门

- 有一条可演示的“永久失败 -> quarantine -> replay -> 查询”闭环；
- retention 在真实 PostgreSQL 上可重复运行且可取消；
- 多实例 claim、lease 恢复和幂等 replay 有集成测试；
- audit 和 usage 的一致性语义写入文档；
- 指标至少包含 retention duration/failures/deleted、quarantine pending/replay failures、usage entries/bytes；
- 不会把 quarantine 或 retention 描述成无限可靠备份。

## 11. 常见错误与复盘题

常见错误：

- 收到 400 仍无限重试原始 batch；
- 为 replay 编写第二套插入逻辑，导致幂等规则漂移；
- claim 时持有行锁执行慢操作；
- 没有 lease，worker 崩溃后 rows 永远是 replaying；
- retention 一次删除全表旧数据，形成长事务；
- 以 observed_at 作为唯一保留依据；
- 把每条日志写成审计事件；
- usage 在内存里计数，Server 重启或水平扩展后失真；
- quarantine 中存放没有大小上限的原始 payload。

复盘题：

1. 为什么 409 需要人工诊断而不是自动生成新 batch ID？
2. Server quarantine 返回 202 的前置条件是什么？
3. retention worker 如何在不阻塞接入事务的情况下工作？
4. replay worker 在“数据库已提交但自身崩溃”的窗口里为什么不会重复数据？
5. 哪些审计动作必须与主事务原子提交，哪些可以异步？
