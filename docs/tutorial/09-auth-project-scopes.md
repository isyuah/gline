# 09. Project、API Key 与 Scope 授权

> 本章是安全边界的实现教学，不表示当前 `AuthMiddleware` 已具备这些能力。当前代码只检查 `Authorization` 是否为空，不能验证身份，也不能提供项目隔离。

## 1. 本章目标

实现完成后，Server 应满足：

- 每个 API Key 只属于一个 Project。
- secret 只在创建时显示一次，数据库不保存明文。
- key 可以禁用、过期和轮换。
- `ingest` scope 只能写入，`query` scope 只能查询。
- `project_id` 由认证结果注入 context，body/query parameter 无权选择项目。
- Handler 和 repository 的调用路径不可能“忘记”项目隔离。
- 认证失败不泄露 key 是否存在、secret 是否错误或 Project 状态。
- 日志、metrics 和错误响应不包含 secret。

这不是完整企业 IAM。MVP 不需要用户密码、OAuth 登录、组织层级或 RBAC 管理后台。API Key + Project + 两个明确 scope 足够形成可信的后端安全边界。

### 1.1 当前代码差距

当前 `internal/server/auth.go` 只判断 `Authorization` header 是否为空：任意非空字符串都会通过，也不会产生 Project 或 scope。当前没有 Project/API Key 表、secret 摘要、禁用/过期/轮换流程和跨 Project 数据隔离测试；本章以下内容均为待实现目标。

## 2. 认证、授权与隔离是三个问题

### 2.1 认证 Authentication

回答：“这个请求携带的凭证是否有效，它代表哪个主体？”

输出一个可信 Principal：

```go
type Principal struct {
    KeyID     KeyID
    ProjectID ProjectID
    Scopes    ScopeSet
}
```

### 2.2 授权 Authorization

回答：“这个已认证主体能否执行当前操作？”

- 上传 batch 需要 `ingest`。
- 查询 entries 需要 `query`。
- 将来 Project 管理可引入独立 `admin`，但不要提前添加未使用 scope。

### 2.3 数据隔离 Data isolation

回答：“即使请求通过授权，SQL 是否仍然只能读写该 Project？”

鉴权中间件通过不等于 SQL 自动安全。每个 repository method 都必须显式接收 `ProjectID`；每条查询都必须有 `project_id` 条件；数据库唯一约束和外键也必须带 Project 维度。

## 3. 威胁模型

本章至少考虑：

- 外部调用者猜测或窃取 API Key。
- 数据库快照泄露，攻击者尝试离线恢复 secret。
- ingest key 被误用于读取日志。
- query key 尝试写入数据。
- 客户端伪造 `project_id` 访问其他项目。
- 已禁用 key 仍通过缓存继续使用。
- key 出现在 access log、错误文本、panic 或 metrics label。
- 两个并发轮换操作造成全部凭证同时失效。
- 比较 secret 时产生明显 timing side-channel。
- Project 禁用后，其 key 仍可访问。

不在 MVP 内承诺：

- 抵御 Server 主机完全失陷。
- 硬件安全模块。
- 用户身份和浏览器 session。
- 细粒度到 service/host 的访问控制。

## 4. Key 外形与生命周期

### 4.1 推荐外形

```text
glk_<public-id>_<secret>
```

- `glk`：便于 secret scanner 和人类识别。
- `public-id`：非秘密，用于数据库索引查找。
- `secret`：使用密码学安全随机数生成，只展示一次。

不要使用 Project ID 作为 public ID，也不要在 key 中编码 scope。scope 与禁用状态在 Server 数据库中管理，才能立即变更。

### 4.2 熵与编码

使用 `crypto/rand.Reader` 生成足够随机字节，再用无填充 base64url 或受控字符集编码。不要使用：

- 时间戳；
- UUID v1 的可预测部分；
- `math/rand`；
- Project 名称 + hash；
- 开发者手写密码。

具体字节数属于安全参数，应在实现时结合官方安全建议确定，而不是从本文复制一个未经验证的数字。为 public ID 和 secret 分别定义最小/最大编码长度，并严格解析。

### 4.3 生命周期状态

```text
active -> revoked
active -> expired (由 expires_at 判定)
```

MVP 可以用 `revoked_at IS NULL` 表示 active，而不存一个容易矛盾的 status 字符串。Project 也应有 `disabled_at`，禁用 Project 时所有 key 立即不可用。

删除 key 通常不如 revoke：保留元数据有助于审计，且不会让同一个 public ID 被意外重用。

## 5. 数据库模型

以下迁移是目标示意，最终迁移编号和 UUID 生成策略需与项目工具一致：

```sql
CREATE TABLE projects (
    id          uuid PRIMARY KEY,
    slug        text NOT NULL UNIQUE,
    display_name text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz NULL,
    CHECK (slug <> ''),
    CHECK (display_name <> '')
);

CREATE TABLE api_keys (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id),
    public_id           text NOT NULL UNIQUE,
    name                text NOT NULL,
    secret_hash         bytea NOT NULL,
    secret_hash_version smallint NOT NULL,
    scopes              text[] NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NULL,
    revoked_at          timestamptz NULL,
    last_used_at        timestamptz NULL,
    CHECK (public_id <> ''),
    CHECK (name <> ''),
    CHECK (cardinality(scopes) > 0),
    CHECK (scopes <@ ARRAY['ingest', 'query']::text[]),
    CHECK (expires_at IS NULL OR expires_at > created_at)
);

CREATE INDEX api_keys_project_id_idx ON api_keys (project_id);
CREATE INDEX api_keys_active_project_idx
    ON api_keys (project_id)
    WHERE revoked_at IS NULL;
```

说明：

- `public_id` 全局唯一，认证查询只需一次精确索引查找。
- `secret_hash` 建议固定长度 `bytea`，Repository 扫描后验证长度。
- `secret_hash_version` 为算法/格式演进留出空间。
- `scopes` 使用数组是 MVP 的简化选择；如果未来 scope 变多、有元数据或需要复杂查询，再拆关联表。
- CHECK 约束是最后防线，领域层仍应先校验。
- `last_used_at` 不应在每次请求同步更新，否则认证会变成写热点；可以先不更新、采样更新或异步聚合。

Project slug 适合人类命令和展示，但内部关联始终使用不可变 UUID。不要允许修改 Project ID。

## 6. 为什么使用带 Pepper 的 HMAC

API Key secret 是高熵机器凭证，不是低熵人类密码。推荐：

```text
stored_hash = HMAC-SHA-256(server_pepper, secret)
```

数据库泄露但 Server pepper 未泄露时，攻击者无法直接验证候选 secret。由于 secret 本身高熵，不一定需要使用为低熵密码设计的昂贵 password hashing；但这个判断建立在 secret 确实由 CSPRNG 生成、足够长且不可由用户自选。

禁止：

- 保存明文；
- 只做普通 SHA-256(secret)，不加服务器秘密；
- 使用可逆加密，然后把解密 key 和数据库放在一起；
- 日志中输出完整 key。

### 6.1 Hash 版本

```go
const SecretHashV1 uint16 = 1

func HashSecretV1(pepper, secret []byte) [32]byte {
    mac := hmac.New(sha256.New, pepper)
    _, _ = mac.Write(secret)
    var out [32]byte
    copy(out[:], mac.Sum(nil))
    return out
}
```

`hash.Hash.Write` 对当前实现不会返回错误，但使用返回值时保持代码清晰即可。比较时使用 `hmac.Equal` 或 `subtle.ConstantTimeCompare`，不要用字符串 `==`。

### 6.2 Pepper 管理

- 来自 secret manager 或环境变量。
- 启动时缺失/过短要失败。
- 不写入配置示例真实值。
- 日志只写 `pepper_configured=true`。
- 轮换策略需要版本或 pepper ID；MVP 可先通过“同时接受 old/new pepper 的短迁移窗口”设计，但不要未实现就声称已支持无停机轮换。

若 pepper 丢失，现有 key 无法验证，只能重新签发。因此部署文档必须把 pepper 纳入备份和恢复秘密清单。

## 7. Key 创建用例

### 7.1 输入与输出

```go
type CreateKeyCommand struct {
    ProjectID ProjectID
    Name      string
    Scopes    ScopeSet
    ExpiresAt *time.Time
}

type CreatedKey struct {
    ID        KeyID
    PublicID  string
    Plaintext string // 只由创建命令返回这一次
    CreatedAt time.Time
}
```

创建过程：

1. 校验 Project 存在且 active。
2. 校验 name、scope、expiry。
3. 生成 public ID 与 secret。
4. 计算 secret HMAC。
5. 只把 hash 和元数据写入数据库。
6. commit 成功后组装完整 plaintext 返回。
7. CLI 输出一次，并明确无法再次查看。

### 7.2 为什么 commit 后才输出

如果先向终端输出 key，随后数据库事务失败，用户拿到一个永远不可用的凭证。先 commit 再输出，保证展示的 key 已存在。

### 7.3 CLI，而不是公开管理 API

MVP 推荐提供本地管理命令或 seed 工具：

```text
gline-server project create --slug demo --display-name "Demo"
gline-server key create --project demo --name host-a --scope ingest
gline-server key revoke --public-id <id>
```

这只是目标命令形态。若项目当前 CLI 框架很轻，可以先用独立 `cmd/gline-admin`，也可以给 Server 增加明确 subcommand。不要为了三个命令引入大型框架；若引入依赖，仍通过 `go get` 选择兼容版本，让 Go 工具维护 `go.mod/go.sum`。

管理命令必须避免：

- secret 进入 shell history 的参数。
- 完整 key 被结构化 logger 再记录一次。
- CI 日志展示生成结果。
- 错误时打印包含 hash/pepper 的对象。

## 8. Scope 领域类型

不要在 Handler 中散落字符串比较：

```go
type Scope string

const (
    ScopeIngest Scope = "ingest"
    ScopeQuery  Scope = "query"
)

type ScopeSet struct {
    values map[Scope]struct{}
}

func NewScopeSet(scopes ...Scope) (ScopeSet, error) {
    // 拒绝未知值与空集合，自动消除重复。
}

func (s ScopeSet) Contains(scope Scope) bool {
    _, ok := s.values[scope]
    return ok
}
```

也可用位集合提高简单性，但数据库仍需稳定字符串映射。无论内部表示如何，公开 scope 名称是兼容合同。

建议让 `ScopeSet` 不暴露内部 map，防止调用方修改 Principal 的权限。

## 9. Token 解析

### 9.1 Authorization 规则

只接受：

```http
Authorization: Bearer <token>
```

解析应：

- 拒绝缺失 header。
- 拒绝多个 Authorization header 或含糊组合。
- scheme 大小写按 HTTP 规则处理，token 本身大小写敏感。
- 拒绝多余控制字符和超长 header。
- token 必须恰好符合 `glk_<public-id>_<secret>`。
- public ID/secret 字符集和编码长度必须有界。

不要用一个简单 `strings.Split(token, "_")` 后不检查段数；若编码字符集以后包含 `_`，会产生歧义。可以固定 public ID 编码长度，或使用 `SplitN` 并为 secret 选择不含分隔符的编码。

### 9.2 失败响应

- 缺失凭证：401 `authentication_required`。
- 格式错误、key 不存在、secret 错、key revoked/expired、Project disabled：统一 401 `invalid_api_key`。
- 已认证但缺 scope：403 `insufficient_scope`。

不要告诉外部调用者“public ID 存在但 secret 错误”，这会帮助枚举。

可返回标准 header：

```http
WWW-Authenticate: Bearer realm="gline"
```

不要把详细失败原因放进该 header。

## 10. Repository 接口

认证领域模块只依赖最窄接口：

```go
type CredentialRecord struct {
    KeyID             KeyID
    ProjectID         ProjectID
    SecretHash        [32]byte
    SecretHashVersion uint16
    Scopes            ScopeSet
    ExpiresAt         *time.Time
    RevokedAt         *time.Time
    ProjectDisabledAt *time.Time
}

type CredentialRepository interface {
    FindByPublicID(ctx context.Context, publicID string) (CredentialRecord, error)
}
```

`ErrNotFound` 属于 repository/domain 合同，不应泄漏 `pgx.ErrNoRows` 给中间件。SQL 查询可联结 projects：

```sql
SELECT
    k.id,
    k.project_id,
    k.secret_hash,
    k.secret_hash_version,
    k.scopes,
    k.expires_at,
    k.revoked_at,
    p.disabled_at
FROM api_keys AS k
JOIN projects AS p ON p.id = k.project_id
WHERE k.public_id = $1;
```

这样认证只需一次数据库 round trip，并同时判断 Project 状态。

## 11. Authenticator 流程

```go
type Authenticator struct {
    repo    CredentialRepository
    pepper []byte
    clock   Clock
}

func (a *Authenticator) Authenticate(ctx context.Context, rawToken string) (Principal, error) {
    parsed, err := ParseToken(rawToken)
    if err != nil {
        return Principal{}, ErrInvalidCredential
    }

    record, err := a.repo.FindByPublicID(ctx, parsed.PublicID)
    if err != nil {
        if errors.Is(err, ErrCredentialNotFound) {
            performDummyVerification(a.pepper, parsed.Secret)
            return Principal{}, ErrInvalidCredential
        }
        return Principal{}, fmt.Errorf("load credential: %w", err)
    }

    candidate, err := HashSecret(a.pepper, parsed.Secret, record.SecretHashVersion)
    if err != nil {
        // 数据库出现当前程序不认识的版本是部署/兼容问题，不是客户端输错 key。
        return Principal{}, fmt.Errorf("unsupported credential hash version: %w", err)
    }
    if !hmac.Equal(candidate[:], record.SecretHash[:]) {
        return Principal{}, ErrInvalidCredential
    }

    now := a.clock.Now()
    if record.RevokedAt != nil || record.ProjectDisabledAt != nil || expired(record.ExpiresAt, now) {
        return Principal{}, ErrInvalidCredential
    }

    return Principal{
        KeyID: record.KeyID,
        ProjectID: record.ProjectID,
        Scopes: record.Scopes,
    }, nil
}
```

### 11.1 Dummy verification

public ID 不存在时也做一次等价 HMAC，可以减少“快速不存在/慢速 secret 错”的时间差。网络环境中 timing 防护不是只靠这一点，但成本低且设计清晰。

### 11.2 Clock 注入

过期判断使用小型 Clock 接口或传入 `now`，让测试不依赖真实时间：

```go
type Clock interface { Now() time.Time }
```

不要在每个测试中 sleep 等待 key 过期。

## 12. Principal Context

使用标准 request context 和不可冲突的私有 key：

```go
type principalContextKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
    return context.WithValue(ctx, principalContextKey{}, p)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
    p, ok := ctx.Value(principalContextKey{}).(Principal)
    return p, ok
}
```

只有认证中间件能构造 Principal。Handler 不应接受 `X-Project-ID` 之类的 header 覆盖它。

如果使用 Gin `c.Set`，也应只存整个 Principal 并通过 helper 读取，不能分别写松散字符串。但标准 context 更容易贯穿 service/repository 和非 Gin 测试。

## 13. Middleware 分层

### 13.1 认证中间件

```go
func Authenticate(authenticator *Authenticator) gin.HandlerFunc {
    return func(c *gin.Context) {
        token, err := BearerToken(c.Request.Header)
        if err != nil {
            WriteAuthError(c, err)
            c.Abort()
            return
        }

        principal, err := authenticator.Authenticate(c.Request.Context(), token)
        if err != nil {
            WriteAuthError(c, err)
            c.Abort()
            return
        }

        c.Request = c.Request.WithContext(WithPrincipal(c.Request.Context(), principal))
        c.Next()
    }
}
```

### 13.2 Scope 中间件

```go
func RequireScope(required Scope) gin.HandlerFunc {
    return func(c *gin.Context) {
        principal, ok := PrincipalFromContext(c.Request.Context())
        if !ok {
            WriteError(c, 500, "internal_error", "internal server error", nil)
            c.Abort()
            return
        }
        if !principal.Scopes.Contains(required) {
            WriteError(c, 403, "insufficient_scope", "credential lacks required scope", nil)
            c.Abort()
            return
        }
        c.Next()
    }
}
```

Principal 缺失属于 router 装配错误，不能伪装成普通 401。测试应能发现 scope middleware 被错误地放在 auth 前面。

## 14. Project 隔离的纵深防御

### 14.1 Service 签名强制携带 Project

```go
func (s *Service) Accept(ctx context.Context, projectID ProjectID, batch Batch) (Result, error)
func (s *Service) List(ctx context.Context, projectID ProjectID, filter Filter) (Page, error)
```

若 Batch 已在构造时包含 project，也可只传 Batch；但不要同时传两个可能不一致的 project 值。

### 14.2 Repository 签名强制携带 Project

```go
AcceptBatch(ctx context.Context, projectID ProjectID, batch Batch, hash [32]byte)
ListEntries(ctx context.Context, projectID ProjectID, filter Filter)
```

### 14.3 SQL 强制过滤

```sql
WHERE project_id = $1
```

查询 batch 明细时，不要只按全局 entry ID 查找；使用 `(project_id, id)`。

### 14.4 数据库约束

- `ingest_batches` 唯一键是 `(project_id, batch_id)`。
- entries 的外键最好关联 `(project_id, batch_id)`，而不只是 batch 内部 surrogate ID。
- Project 删除策略默认 restrict 或软禁用，不能 cascade 意外清空日志。

### 14.5 PostgreSQL RLS 是否需要

Row Level Security 可作为额外防线，但 MVP 不应在应用层隔离尚未证明正确前，用 RLS 掩盖 SQL。若以后引入 RLS，要明确如何为每个事务设置 project context、连接池如何避免状态泄漏、migration/admin role 如何绕过。当前先通过强类型接口、SQL 和集成测试保证。

## 15. Key 轮换流程

安全轮换应允许重叠窗口：

1. 为同一 Project 创建新 key，scope 与用途按需设置。
2. 将新 key 配置到 Agent/客户端。
3. 验证新 key 已成功使用。
4. revoke 旧 key。
5. 观察旧 key 拒绝指标，确认没有遗漏实例。

不要“原地替换 secret”，因为：

- 无法区分新旧客户端。
- 回滚困难。
- 审计不清晰。
- 一次配置错误会造成全部中断。

可以同时存在多个 ingest key，例如每台主机一个 key。这样某台机器泄露时只 revoke 该 key，并从 key ID 定位来源。

## 16. 缓存策略

MVP 可以先不缓存认证查询：数据库索引查找很轻，正确性最简单。只有指标证明 auth 查询成为瓶颈后再加缓存。

若添加缓存，必须回答：

- revoke 最长多久生效？
- Project disable 最长多久生效？
- negative cache 是否让刚创建 key 暂时不可用？
- 缓存 key 是否包含 secret？禁止。
- 多实例如何失效？
- 缓存是否有界？

合理初始方案可能是只缓存 public ID 对应的 credential record，使用短 TTL，secret 每次仍做 HMAC 比较；但这仍延迟 revoke。简历中应诚实写出一致性窗口。

## 17. 速率限制与滥用保护

认证之前可按来源 IP 做粗粒度限流，认证之后可按 key ID/Project 做配额。注意：

- IP 可能来自反向代理，只有信任的 proxy 才能提供真实地址。
- key ID 是可控基数，但 Project slug/原始 token 不应成为 metrics label。
- 429 返回 `Retry-After`，Agent 进入受控退避。
- 限流状态如果只在单实例内存中，多实例下只是近似；MVP 可以接受，但要说明。
- 不要因为认证失败就把完整 token 写日志。

限流不是本章完成门的硬要求，但接口和错误码要留出稳定行为。

## 18. 审计与日志

应记录的安全事件：

- key 创建：key ID、Project ID、name、scopes、操作者来源。
- key revoke：同上，加 revoke 时间。
- Project disable/enable。
- 认证失败计数，必要时采样日志。
- scope 拒绝：key ID、Project ID、route，不含 secret。

不应记录：

- plaintext key。
- secret hash。
- pepper。
- Authorization header。
- 完整请求 body。

创建命令向用户展示 plaintext 是功能需求，不等于把它写入普通日志。可以直接写 stdout，并确保结构化 logger 不重复记录。

## 19. 测试策略

### 19.1 Token parser 单元测试

- 合法 token。
- 缺少 prefix。
- public ID/secret 缺失。
- 多余分隔符。
- 非法 base64url。
- 超长 header/token。
- 控制字符。
- token 大小写敏感。

### 19.2 Hash 单元测试

- 固定输入产生固定长度摘要。
- 相同 secret 和 pepper 相同摘要。
- secret 改变，摘要改变。
- pepper 改变，摘要改变。
- 比较使用 constant-time API。
- 未知 hash version 明确失败，不自动用当前版本猜测。

不要写“SHA-256 不会碰撞”之类测试。测试应保护项目的 hash 版本与输入规则，而不是密码学库本身。

### 19.3 Authenticator 单元测试

使用 fake repository 和 fake clock：

- 正常 key 返回正确 Principal。
- public ID 不存在。
- secret 错。
- revoked。
- expires_at 恰好到达边界。
- Project disabled。
- repository 临时失败映射为服务不可用，不映射为 invalid key。
- scopes 不被调用方修改。

```go
func TestAuthenticatorRejectsExpiredKey(t *testing.T) {
    now := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
    record := validCredentialRecord()
    expiredAt := now
    record.ExpiresAt = &expiredAt

    authenticator := newTestAuthenticator(record, fixedClock{now: now})
    _, err := authenticator.Authenticate(context.Background(), validToken(record))

    if !errors.Is(err, auth.ErrInvalidCredential) {
        t.Fatalf("error = %v, want invalid credential", err)
    }
}
```

明确边界：推荐 `now >= expires_at` 即失效。

### 19.4 Middleware/route 测试

| Key | `/batches` | `/entries` |
| --- | --- | --- |
| 无 key | 401 | 401 |
| 错 key | 401 | 401 |
| ingest | 200/业务结果 | 403 |
| query | 403 | 200/业务结果 |
| ingest+query | 两者允许 | 两者允许 |
| revoked | 401 | 401 |

测试应断言业务 fake 没有在拒绝路径被调用。

### 19.5 PostgreSQL 集成测试

插入两个 Project 和多个 key，覆盖：

- public ID 精确查找。
- Project disabled 联结结果。
- revoke 后下一次认证失败。
- key create 只存 hash，不存 plaintext。
- 不允许未知 scope。
- public ID 唯一约束在并发下生效。
- Project A 的 Principal 不能查到 Project B entries。

跨 Project 测试不能只 mock repository，因为真正风险在 SQL 是否漏掉 `project_id`。

### 19.6 日志泄露测试

可以给 logger 注入内存 buffer，触发成功、失败和 panic 路径，然后断言输出不包含：

- 完整 token；
- secret 部分；
- pepper；
- 数据库 URL 密码。

这是有价值的安全回归测试，因为泄露可能由未来“打印请求 header”重新引入。

## 20. 验收证据

建议保存以下证据：

```text
go test ./internal/server/auth/... ./internal/server/httpapi/... -count=1
go test -race ./internal/server/auth/... ./internal/server/httpapi/... -count=1
go test ./internal/storage/postgres/... -run 'Auth|Project|Isolation' -count=1
go vet ./internal/server/auth/... ./internal/server/httpapi/...
```

再执行一个真实流程：

1. 创建 Project A 和 B。
2. 为 A 创建 ingest key 和 query key。
3. ingest key 上传成功、查询被 403。
4. query key 查询成功、上传被 403。
5. A key 无法看见 B 数据。
6. 创建替代 key，验证后 revoke 旧 key。
7. 旧 key 立即或在文档声明的缓存窗口后失败。
8. 检查数据库、日志、shell 输出，确认 plaintext 只在创建结果出现一次。

## 21. 常见坑

### 21.1 用 `Authorization != ""` 当认证

这只证明请求发送了任意字符串。它不能识别 Project、权限、撤销或过期。

### 21.2 在数据库保存 plaintext key

数据库备份泄露就等于所有 Agent 凭证立即可用。只存 HMAC 摘要。

### 21.3 使用普通 SHA-256

虽然随机 secret 本身高熵，但数据库一旦泄露，普通 hash 仍能直接验证拿到的候选 token；HMAC pepper 增加独立秘密边界。

### 21.4 把 Project 放在 query/body

这让越权风险散落在每个 endpoint。Project 必须来自 Principal。

### 21.5 只在 Handler 检查 Project

未来后台任务或另一个 transport 可能绕过 Handler。Service/Repository 签名和 SQL 都要带 Project。

### 21.6 认证失败统一变 500

客户端无法判断凭证问题，会热重试。401/403 需要稳定；真正 repository 故障才是 503/500。

### 21.7 认证数据库错误伪装成 invalid key

这会在数据库故障时误导运维者去轮换所有 key。not found/invalid secret 与 repository unavailable 必须内部分类。

### 21.8 每次请求同步更新 `last_used_at`

会给高频 ingest 增加一次写和行锁竞争。先用 metrics 统计或采样更新。

### 21.9 永久认证缓存

revoke 永远不生效。任何缓存都要有明确一致性窗口和有界容量。

### 21.10 在指标 label 中放 public ID 或 Project slug

即使 public ID 不是 secret，大量 key 会制造高基数。安全事件可写结构化日志；指标用 status/reason 这类有限集合。

## 22. 复盘题

1. 认证、授权和数据隔离分别解决什么问题？
2. 为什么 Project ID 不能由客户端选择？
3. public ID 和 secret 为什么要分开？
4. 为什么随机 API key 可使用 HMAC，而用户密码通常需要慢 hash？
5. pepper 丢失会发生什么，备份策略应如何处理？
6. revoked、expired、Project disabled 对外为什么都可以返回同一 invalid key？
7. 为什么 repository 暂时不可用不能映射成 401？
8. key 轮换为什么要创建新 key 并保留重叠窗口，而非原地换 secret？
9. auth cache 引入了什么一致性代价？
10. 如何用真实数据库证明跨 Project 隔离，而不只证明 mock 被调用？

## 23. 完成门

- [ ] Project 与 API Key schema 有约束、索引和迁移。
- [ ] key secret 由 CSPRNG 生成，只在创建成功后显示一次。
- [ ] 数据库只存版本化 HMAC 摘要，不存 plaintext。
- [ ] pepper 缺失时 Server 快速失败，且日志不泄露其值。
- [ ] 认证输出强类型 Principal，Project 不可由请求覆盖。
- [ ] ingest/query scope 在路由层执行，拒绝路径不调用业务 service。
- [ ] revoked、expired、Project disabled 均有测试。
- [ ] repository 错误与 invalid credential 被正确区分。
- [ ] 每个日志 SQL 都显式带 `project_id`，并有双 Project 集成测试。
- [ ] 创建、轮换、revoke 有可执行流程和验证证据。
- [ ] access log、错误和 metrics 不含 token、hash、pepper。
- [ ] 未将缓存、限流或 pepper 无停机轮换等未实现能力写成已完成。

下一章进入[PostgreSQL、迁移与 Repository](10-postgresql-repositories.md)，把 Project、批次和日志的持久化边界落到事务与约束上。
