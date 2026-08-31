# 03. 控制平面：Project、凭证、Agent 和 Pipeline 的完整生命周期

控制平面是让 Gline 从“日志上传接口”变成“可运营后端”的关键。它管理谁拥有数据、哪些 Agent 被允许接入、哪些采集管道正在运行以及谁执行过危险操作。接入平面处理日志数据流，控制平面处理配置和身份流；两者共享 Project 边界，但不混淆请求类型。

## 1. 实现前基线

> 本节的问题清单记录控制平面实现前的缺口。当前版本已经提供 Project、
> Key、Agent、Pipeline 的生命周期 API，并通过心跳返回期望控制状态。

在实现前，仓库已经出现 Project/API Key/Scope 的设计方向，但没有完整的管理生命周期。通常原型会把 Key 写死在环境变量，把 Agent ID 当作请求字段，把 Pipeline 配置放在本地文件里。这样可以跑 demo，却无法回答：

* Key 如何安全创建、展示、轮换和吊销？
* Project 禁用后，已有 Agent 和批次如何处理？
* Agent 重启后如何被识别为同一个实例？
* Pipeline 配置更新是否可追踪？
* 谁触发了 replay、retention 修改或 Project 禁用？

本章逐步补上这些后端能力，但不会一开始实现复杂的动态配置推送。第一版先完成可审计的注册、心跳和配置版本记录。

## 2. 前置知识

* API Key 哈希和最小权限；
* REST 资源建模、状态码和幂等操作；
* 乐观并发控制与版本号；
* PostgreSQL 事务和条件更新；
* 审计日志的不可变写入原则。

## 3. 控制平面边界

```text
Admin / CLI
      |
      v
Control API
  -> AuthN/AuthZ (admin scope or project owner role)
  -> Control Service
      -> ProjectRepository
      -> APIKeyRepository
      -> AgentRepository
      -> PipelineRepository
      -> AuditRepository

Agent
  -> Heartbeat API (agent scope, reports state and receives desired control)
  -> Control Service (只更新自己的 Agent/Pipeline 状态并返回控制快照)
```

控制平面 API 不能让 Agent 代替管理员创建 Project 或读取其他 Agent 的配置。Agent 只拥有注册后分配的 `agent` scope，以及按 Project 约束的心跳/状态上报权限。

## 4. Project 生命周期

### 4.1 创建

Project 创建请求至少需要 `slug` 和显示名称。slug 是 URL、审计和 CLI 中的稳定标识；名称可以修改。

```http
POST /api/v1/projects
Authorization: Bearer <admin-key>
Content-Type: application/json

{"slug":"demo","name":"Demo services"}
```

```json
{
  "id": "uuid",
  "slug": "demo",
  "name": "Demo services",
  "status": "active",
  "created_at": "..."
}
```

返回 201。slug 冲突返回稳定的 `project_slug_taken`，不要把数据库的完整 constraint 名暴露给客户端。

### 4.2 禁用和恢复

```http
POST /api/v1/projects/{projectID}/disable
POST /api/v1/projects/{projectID}/enable
```

操作在一个事务中完成：更新 Project 状态、必要时暂停 Pipeline、写 Audit Event。接入请求在认证后再次检查 Project 状态；不能仅依赖 API Key 状态，因为 Key 可能尚未逐个吊销。

禁用语义：

* 新 ingest 返回 `403 project_disabled`；
* Agent heartbeat 可以保留为 admin 可见的诊断数据，也可以返回 `403`，必须在 API 合同中固定；
* 普通 query 返回 `403`，管理员可以用明确的 admin scope 读取；
* 不删除既有日志，不自动清空 spool；恢复后 Agent 可以继续重试。

恢复必须是幂等的。禁用一个已经 disabled 的 Project，不应重复写大量审计；可以记录一次 `already_disabled` 结果，便于操作方知道当前状态。

## 5. API Key 生命周期

### 5.1 创建和一次性展示

```http
POST /api/v1/projects/{projectID}/keys
Authorization: Bearer <admin-key>
Content-Type: application/json

{"name":"agent-prod-1","agent_id":"optional-agent-uuid","scopes":["ingest"],"expires_at":"..."}
```

响应包含一次明文 secret：

```json
{
  "id":"uuid",
  "prefix":"glk_demo_7F2A",
  "secret":"glk_demo_7F2A.<random-secret>",
  "scopes":["ingest"],
  "warning":"store this value now; it will not be shown again"
}
```

服务端立刻计算 `secret_hash = H(secret || server_pepper)` 并丢弃明文。更成熟的实现可使用 HMAC-SHA-256，以固定长度结果便于索引和 constant-time 比较。Pepper 只来自运行环境秘密，不进入迁移、测试 fixture 或日志。

`agent_id` 为空时，Key 是 Project 级凭证，服务端仍需验证请求中的 Agent 属于该 Project；填写 `agent_id` 时，Key 只能为这个 Agent 写入，数据库通过 `(project_id, agent_id)` 复合外键阻止跨租户绑定。这样既支持早期的 Project 级接入，也为生产 Agent 做最小权限收紧留下明确路径。

### 5.2 认证查询

Key 格式可以是 `prefix.secret`。先解析 prefix 做候选查询，再计算 hash 比较：

```go
func (a *Authenticator) Authenticate(ctx context.Context, raw string) (AuthContext, error) {
    prefix, secret, ok := splitKey(raw)
    if !ok {
        return AuthContext{}, ErrInvalidCredential
    }
    candidates, err := a.keys.FindActiveByPrefix(ctx, prefix)
    if err != nil { return AuthContext{}, err }
    for _, key := range candidates {
        want := a.hasher.Sum(secret, a.pepper)
        if subtle.ConstantTimeCompare(want, key.SecretHash) == 1 {
            if err := key.ValidAt(a.clock.Now()); err != nil {
                return AuthContext{}, err
            }
            return AuthContext{
                KeyID: key.ID, ProjectID: key.ProjectID, Scopes: key.Scopes,
            }, nil
        }
    }
    return AuthContext{}, ErrInvalidCredential
}
```

认证成功后可以异步更新 `last_used_at`，但该更新失败不能让已经成功的 ingest 失败。不要把最后使用时间写进高基数 metrics label。

### 5.3 吊销、过期和轮换

```http
POST /api/v1/projects/{projectID}/keys/{keyID}/revoke
POST /api/v1/projects/{projectID}/keys/{keyID}/rotate
```

吊销使用条件更新：

```sql
UPDATE api_keys
SET status = 'revoked', revoked_at = now()
WHERE id = $1
  AND project_id = $2
  AND status = 'active';
```

如果影响行数为 0，再读取当前状态决定返回 `already_revoked`、`not_found` 还是 `project_disabled`。轮换是“创建新 Key + 审计 + 返回一次 secret”，是否自动吊销旧 Key 应作为显式参数或策略，不要隐式改变运行中的 Agent。

## 6. Agent 注册和心跳

### 6.1 注册

Agent 不直接创建 Project，但管理员可以为 Project 创建 registration token，或者用带 `agent:register` Scope 的 Key 调用：

```http
POST /api/v1/projects/{projectID}/agents
Authorization: Bearer <project-admin-key>

{"name":"host-a","hostname":"node-a","version":"0.1.0"}
```

创建使用 `(project_id, name)` 唯一约束。重试相同请求时可以返回已有 Agent，但如果 hostname/version 变化，应使用更新接口而不是静默覆盖身份。

### 6.2 心跳

```http
POST /api/v1/agents/{agentID}/heartbeat
Authorization: Bearer <agent-key>

{
  "version":"0.1.1",
  "pipelines":[
    {"id":"uuid","config_version":3,"status":"running","backlog_bytes":2048}
  ]
}
```

服务端从认证上下文得到 Agent 归属的 Project，并验证路径中的 `agentID` 属于该 Project。Agent 不能通过 body 传入另一个 Project 的 ID。心跳更新应是有界的：限制 pipeline 数量、错误摘要长度和 backlog 字段范围。

成功响应包含当前 Agent 所有 Pipeline 的控制快照：

```json
{
  "control": {
    "pipelines": [
      {"id":"uuid","desired_status":"paused","config_version":3}
    ]
  }
}
```

```sql
UPDATE agents
SET version = $3,
    last_heartbeat_at = $4,
    status = CASE WHEN status <> 'disabled' THEN 'active' ELSE status END,
    updated_at = $4
WHERE id = $1 AND project_id = $2;
```

随后按请求中的 pipeline 列表更新观测字段，不能改写管理员的控制状态：

```sql
UPDATE pipelines
SET reported_status = $4,
    reported_at = $5,
    last_error = $6,
    updated_at = $5
WHERE id = $1 AND project_id = $2 AND agent_id = $3;
```

后台 Agent State Worker 定期执行：

```sql
UPDATE agents
SET status = 'stale', updated_at = now()
WHERE status = 'active'
  AND last_heartbeat_at < now() - $1::interval;
```

`stale -> active` 只由有效心跳触发，`disabled` 不被心跳自动恢复。

## 7. Pipeline 配置和状态

### 7.1 配置版本

当前可靠 Agent 使用心跳返回的控制快照执行状态门控。它不会在线替换文件
source；如果 `config_version` 不一致，Agent 报告 `error` 并停止读取，部署
匹配版本后通过下一次心跳恢复。这是第一版可验证的配置发布边界。

Pipeline 的配置更新使用乐观版本：

```http
PUT /api/v1/projects/{projectID}/pipelines/{pipelineID}
If-Match: "config-3"

{"service":"api","paths":["C:/logs/api/*.log"],"parser":"json"}
```

```sql
UPDATE pipelines
SET config = $4,
    config_version = config_version + 1,
    service = $5,
    updated_at = now()
WHERE id = $1
  AND project_id = $2
  AND config_version = $3
  AND status <> 'disabled'
RETURNING config_version;
```

影响行数为 0 时返回 409 `config_version_conflict`，要求客户端重新读取后合并，而不是覆盖另一个管理员的修改。

### 7.2 状态上报和控制

Pipeline 的运行状态可由 Agent 心跳上报，但控制状态由 Server 管理。教程从第一版就分离两个字段：

```text
desired_status: enabled | paused | disabled   (server intent)
reported_status: running | stopped | error     (agent observation)
```

`pipelines.status` 保存 `desired_status` 的语义（这里沿用表字段名 `status`），`reported_status` 保存 Agent 观察到的运行状态。这样管理员暂停 Pipeline 但 Agent 尚未收到配置时，不会被错误显示成已经停止。

## 8. Admin API 和 Scope 设计

Scope 是最小权限单位，不用一个 `admin=true` 布尔值覆盖所有操作：

```text
project:read
project:write
key:manage
agent:read
agent:write
pipeline:read
pipeline:write
ingest
query
quarantine:read
quarantine:replay
retention:manage
audit:read
```

路由到 Scope 的映射应集中注册：

```go
var requiredScope = map[string]string{
    "POST /api/v1/batches": "ingest",
    "GET /api/v1/entries": "query",
    "POST /api/v1/projects": "project:write",
    "POST /api/v1/projects/{id}/keys": "key:manage",
    "POST /api/v1/quarantine/{id}/replay": "quarantine:replay",
}
```

实际 router 可能使用命名路由，不要依赖原始 URL 字符串做脆弱匹配；重点是让每条敏感路由有明确的最小 Scope。

Project owner/admin Key 可以跨资源执行操作，但每次 service 仍需要显式的 `projectID` 参数和资源归属检查。Scope 不是租户隔离的替代品。

## 9. 审计实现

审计写入和状态改变应在同一事务内：

```go
func (s *ControlService) RevokeKey(ctx context.Context, auth AuthContext, projectID, keyID APIKeyID) error {
    if err := auth.Require("key:manage"); err != nil { return err }
    return s.tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
        changed, err := s.keys.Revoke(ctx, tx, projectID, keyID)
        if err != nil { return err }
        outcome := "success"
        if !changed { outcome = "already_terminal" }
        return s.audit.Append(ctx, tx, AuditEvent{
            ProjectID: &projectID, ActorType: "api_key", ActorID: auth.KeyID.String(),
            Action: "key.revoke", Resource: "api_key", ResourceID: keyID.String(),
            Outcome: outcome,
        })
    })
}
```

审计失败时，控制操作整体失败；否则你无法证明一次 Key 吊销是否被记录。对于高频 Agent heartbeat，可以按分钟聚合或只记录状态变化，不能每秒写一条审计。

## 10. 配额与资源治理

控制平面负责配置配额，接入/查询平面负责执行：

```text
Project quota
├── max_batch_bytes
├── max_entries_per_batch
├── ingest_bytes_per_minute
├── query_concurrency
└── retention_days
```

第一版可以只做 body/batch 上限和每 Project 的并发信号量，使用量统计作为观测；阶段 B 再加入按分钟的令牌桶。无论实现哪种限流，都应返回 `429`、`Retry-After`（若适用）和稳定错误码，不能静默丢弃 Agent 批次。

配额拒绝的 Batch 是否进入 Agent 重试，取决于错误分类：

* 临时资源不足：429/503，可重试；
* Project 超过日配额：429/403，需要运维动作；
* Batch 超过单次最大大小：400，不应原样重试；
* 权限或 Project 禁用：401/403，不应盲目重试。

## 11. 分步实现

### Step 1：认证上下文

实现 `Authenticator`、`AuthContext`、Scope 检查和错误映射。先使用内存 fake repository 写行为测试，再接 PostgreSQL。

### Step 2：Project 管理

实现创建、读取、禁用、恢复。所有写操作附带 Audit。先不做删除，避免没有备份/级联策略时误删数据。

### Step 3：Key 管理

实现创建一次性 secret、active lookup、吊销、过期判定和轮换。写测试确保响应/日志/错误不含 secret。

### Step 4：Agent/Pipeline

实现 Agent 注册、心跳和 Pipeline 配置版本更新。加入 Project 归属和 `If-Match` 并发测试。

### Step 5：状态 worker

实现 stale 判定和有限重试。worker 接受 context，使用 ticker，并在 stop 后等待 goroutine 结束。不要由 ticker 直接写 HTTP。

### Step 6：Admin CLI 或 API

为演示和验收提供最小 CLI 命令：`project create`、`key create`、`key revoke`、`agent list`、`pipeline pause`。CLI 只调用 API，不绕过业务层直接改数据库。

## 12. 测试策略

* 认证：无效格式、错误 secret、revoked、expired、Project disabled、缺 Scope；
* Key：明文只返回一次、轮换不覆盖旧记录、吊销幂等；
* Project：slug 唯一、禁用后接入拒绝、恢复后可以继续；
* Agent：跨 Project agentID 被拒绝、稳定身份、心跳更新版本和状态；
* Pipeline：配置版本冲突返回 409、disabled 不可被普通更新恢复；
* Audit：每个状态改变在同一事务中留下事件；
* Worker：stale 判定、取消、超时和重复执行；
* Security integration：API Key 不能读取另一个 Project 的 Agent/Pipeline/Entry。

测试“禁止越权”和“状态转换”比测试每个管理字段的 JSON 拼写更有价值。

## 13. 验收证据

完成本章应保存一段可重复演示脚本或命令输出：

```text
1. 创建 project demo
2. 创建 ingest key，记录一次性 secret
3. 使用 key 注册 agent host-a
4. 创建 pipeline v1
5. 心跳显示 active/running
6. revoke key
7. 同一 key 的 ingest 得到 401/403
8. 轮换新 key，重新心跳/接入成功
9. 禁用 project，所有普通请求被拒绝
10. 查看 audit，包含上述动作但不包含 secret
```

另外提供数据库查询或 API 输出，证明 stale worker 能将超时 Agent 标记为 stale，心跳能恢复 active，disabled 不会被心跳自动恢复。

## 14. 常见坑

* 把注册、心跳、配置下发全部塞进 ingest 请求；
* Agent 自报 Project ID，服务端不查认证归属；
* API Key 表存明文以便“管理员查看”；
* `admin=true` 代替细粒度 Scope；
* 禁用 Project 只改一个布尔值，不在认证/接入/query 再检查；
* 直接覆盖 Pipeline 配置，没有版本和并发冲突；
* 每次心跳写 Audit，导致审计表和数据库写入压力失控；
* replay 通过新建随机 Batch ID，破坏原有幂等合同；
* CLI 直接连接数据库，绕过 API、权限和审计。

## 15. 复盘题

1. 为什么 Project disabled 不能只吊销当前看到的 API Key？
2. Key 轮换时自动撤销旧 Key 的优点和风险是什么？
3. `desired_status` 与 `reported_status` 分离解决了什么一致性问题？
4. 为什么心跳可以异步更新 `last_used_at`，而 Key 吊销的 Audit 不能异步？
5. replay 使用原 Batch ID 时，怎样处理“数据库里已经成功但 Quarantine 状态仍 pending”的窗口？

## 16. 本章完成门

* 可以创建/禁用/恢复 Project，并有审计；
* 可以安全创建、一次性展示、认证、吊销和轮换 API Key；
* 可以注册 Agent、发送心跳、观察 stale/active；
* 可以创建 Pipeline、用版本控制更新配置、拒绝过期版本；
* 每个操作都有 Project 归属和最小 Scope；
* 能用演示命令证明禁用、吊销、跨租户访问和审计行为；
* 能说明控制平面为什么让 Server 变成可运营后端，又不把动态配置、服务拆分和复杂 RBAC 提前扩张。
