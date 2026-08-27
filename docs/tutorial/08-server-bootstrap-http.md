# 08. Server 启动、HTTP 边界与生命周期

> 本章描述目标实现路径，不代表当前 Server 已经完成。它依赖[协议与领域合同](03-protocol-domain-contracts.md)，并落实[模块化单体 ADR](../adr/0001-modular-monolith.md)。

## 1. 本章目标

完成这一章后，Gline Server 应从“在 `main` 中临时启动一个 Gin router”演进为一个可配置、可测试、可观测、可优雅关闭的模块化单体进程。

你最终应获得：

- `main` 只负责加载配置、组装、运行和决定退出码。
- 一个拥有显式依赖和关闭顺序的 Server `App`。
- 带超时、body limit、严格 JSON、request ID 和安全恢复的 HTTP 层。
- `/livez` 与 `/readyz` 两种不同语义的健康接口。
- `/api/v1/batches` 与 `/api/v1/entries` 的路由骨架。
- 统一错误响应和领域错误到 HTTP 的映射。
- 可用 `httptest` 验证的 router，以及可用真实端口做冒烟验证的进程。

本章先搭骨架，不实现完整 SQL。数据库连接与 repository 在[PostgreSQL 与 Repository](10-postgresql-repositories.md)完成。

## 2. 当前实现差距

当前 `cmd/server/main.go` 做了所有事情：

1. 创建 Gin engine。
2. 安装 logger/recovery。
3. 定义健康接口。
4. 定义 API group 和 auth。
5. 构造打印型 Sink。
6. 硬编码监听 `:8080`。
7. 调用 `r.Run`。

这不是错误的原型，但不适合继续承载数据库和生产式行为：

- 无法在测试中复用完整路由组装。
- 监听地址、超时、限制和 shutdown deadline 不可配置。
- `gin.Engine.Run` 隐藏了 `http.Server` 所有权。
- 没有信号驱动的优雅关闭。
- `/healthz` 只证明 handler 线程能运行，不能区分存活与就绪。
- auth 只检查 header 非空。
- panic recovery 与错误响应格式未形成合同。
- 业务模块的依赖在 `main` 中临时创建，未来容易变成大文件。

## 3. 启动层的职责边界

推荐结构：

```text
cmd/server/main.go
    -> server/bootstrap.LoadConfig
    -> server/bootstrap.Build
    -> app.Run

internal/server/bootstrap/
    config.go
    build.go
    app.go

internal/server/httpapi/
    router.go
    middleware.go
    decode.go
    errors.go
```

### `main` 应负责

- 创建 signal context。
- 读取进程配置。
- 调用 `Build`。
- 运行 App。
- 将致命错误记录到 stderr，并返回非零退出码。

### `main` 不应负责

- 写路由业务逻辑。
- 拼 SQL。
- 解析 API Key。
- 决定 ingest 事务。
- 创建全局 singleton。
- 使用 `os.Exit` 跳过已建立资源的 defer。

一种便于测试退出码的模式：

```go
func main() {
    os.Exit(run())
}

func run() int {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    cfg, err := bootstrap.LoadConfig()
    if err != nil {
        fmt.Fprintln(os.Stderr, "invalid server configuration")
        return 2
    }

    app, err := bootstrap.Build(ctx, cfg)
    if err != nil {
        fmt.Fprintln(os.Stderr, "build server:", safeError(err))
        return 1
    }
    defer app.Close()

    if err := app.Run(ctx); err != nil {
        fmt.Fprintln(os.Stderr, "run server:", safeError(err))
        return 1
    }
    return 0
}
```

真实实现中，日志应使用项目统一的结构化 logger，并确保 DSN、API key、pepper 不进入错误文本。

## 4. 配置模型

### 4.1 推荐配置项

```go
type Config struct {
    ListenAddress string
    DatabaseURL   string
    AuthPepper    string

    ReadHeaderTimeout time.Duration
    ReadTimeout       time.Duration
    WriteTimeout      time.Duration
    IdleTimeout       time.Duration
    ShutdownTimeout   time.Duration

    DatabaseConnectTimeout time.Duration
    DatabaseQueryTimeout   time.Duration

    IngestLimits ingest.Limits
    QueryLimits  query.Limits

    LogLevel string
}
```

敏感配置：

- `DatabaseURL`
- `AuthPepper`
- 将来可能加入的 metrics admin token

这些值应来自环境变量或被忽略的本地配置，不能提交到仓库。配置摘要只记录“已设置/未设置”和非敏感数值，不打印原值。

### 4.2 加载顺序

推荐一个明确优先级：

```text
内置安全默认值 < 配置文件 < 环境变量 < 显式命令行参数
```

如果 MVP 只支持环境变量，也可以更简单；关键是不要让同一字段在不同地方以不可预测顺序覆盖。

### 4.3 校验规则

`LoadConfig` 完成解析，`Validate` 完成语义校验：

```go
func (c Config) Validate() error {
    var errs []error
    if c.ListenAddress == "" {
        errs = append(errs, errors.New("listen address is required"))
    }
    if c.DatabaseURL == "" {
        errs = append(errs, errors.New("database URL is required"))
    }
    if len(c.AuthPepper) < minimumPepperBytes {
        errs = append(errs, errors.New("auth pepper is missing or too short"))
    }
    if c.ReadHeaderTimeout <= 0 || c.ShutdownTimeout <= 0 {
        errs = append(errs, errors.New("HTTP timeouts must be positive"))
    }
    if err := c.IngestLimits.Validate(); err != nil {
        errs = append(errs, fmt.Errorf("ingest limits: %w", err))
    }
    return errors.Join(errs...)
}
```

不要在配置无效时“自动修复”为另一个不可见值。默认值可以在加载时填充；用户明确提供了非法值时应快速失败。

### 4.4 时间配置解析

优先使用 `time.ParseDuration` 形式，如 `5s`、`250ms`。错误信息可以指出配置键，但不要输出 secret 的值。

## 5. 依赖选择与包管理

本教程不手工猜第三方依赖版本。需要 PostgreSQL driver 或 migration 工具时：

1. 先检查现有 `go.mod` 与 Go 版本。
2. 阅读候选库官方文档，确认兼容性和维护状态。
3. 使用 Go 包管理器添加，例如：

```text
go get <driver-module>
go get <migration-module-or-command>
go mod tidy
```

4. 由 Go 工具更新 `go.mod` 和 `go.sum`，不要手工编辑 `go.sum`。
5. 运行 `go mod graph` 或 `go list -m all` 核对实际解析结果。
6. 在没有本机绝对 `replace` 的环境中验证。

对于 PostgreSQL，`pgx`/`pgxpool` 是合理候选；迁移可选择成熟的 `goose`、`golang-migrate` 等之一。最终只选一个迁移工具并记录 ADR/README，不要同时维持两套迁移状态。

## 6. `App`：资源所有权的中心

推荐把运行期资源集中到 `App`：

```go
type App struct {
    server          *http.Server
    pool            *pgxpool.Pool
    readiness       ReadinessState
    shutdownTimeout time.Duration
}

type ReadinessState interface {
    BeginDraining()
}

func (a *App) Run(ctx context.Context) error {
    errCh := make(chan error, 1)
    go func() {
        errCh <- a.server.ListenAndServe()
    }()

    select {
    case err := <-errCh:
        if errors.Is(err, http.ErrServerClosed) {
            return nil
        }
        return fmt.Errorf("http server: %w", err)
    case <-ctx.Done():
        a.readiness.BeginDraining()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
        defer cancel()
        if err := a.server.Shutdown(shutdownCtx); err != nil {
            return fmt.Errorf("graceful shutdown: %w", err)
        }
        err := <-errCh
        if err != nil && !errors.Is(err, http.ErrServerClosed) {
            return fmt.Errorf("http server after shutdown: %w", err)
        }
        return nil
    }
}

func (a *App) Close() error {
    a.pool.Close()
    return nil
}
```

真实实现要处理一个细节：如果 `Shutdown` 超时，是否调用 `Close` 强制终止连接。建议记录超时并调用 `Close`，但要把这视为非优雅关闭，并在指标或退出日志中暴露。

### 6.1 推荐关闭顺序

1. readiness 先切换为 false，阻止新流量。
2. 调用 `http.Server.Shutdown`，停止接收新连接并等待活动请求。
3. 等待后台 retention worker 停止。
4. 关闭数据库连接池。
5. flush logger（如果实现需要）。

资源必须由创建它的上层负责关闭。Repository 不应擅自关闭共享 pool。

## 7. `Build`：显式组装模块

```go
func Build(ctx context.Context, cfg Config) (*App, error) {
    if err := cfg.Validate(); err != nil {
        return nil, err
    }

    pool, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.Options{
        ConnectTimeout: cfg.DatabaseConnectTimeout,
    })
    if err != nil {
        return nil, fmt.Errorf("open database: %w", err)
    }

    // 从这一行开始，任何失败都必须关闭 pool。
    built := false
    defer func() {
        if !built {
            pool.Close()
        }
    }()

    authRepo := postgres.NewAPIKeyRepository(pool)
    batchRepo := postgres.NewBatchRepository(pool)
    entryRepo := postgres.NewEntryRepository(pool)

    authenticator := auth.NewAuthenticator(authRepo, cfg.AuthPepper)
    ingestService := ingest.NewService(batchRepo, cfg.IngestLimits)
    queryService := query.NewService(entryRepo, cfg.QueryLimits)

    readiness := postgres.NewReadiness(pool)
    router := httpapi.NewRouter(httpapi.Dependencies{
        Authenticator: authenticator,
        Ingest:        ingestService,
        Query:         queryService,
        Readiness:     readiness,
    })

    server := &http.Server{
        Addr:              cfg.ListenAddress,
        Handler:           router,
        ReadHeaderTimeout: cfg.ReadHeaderTimeout,
        ReadTimeout:       cfg.ReadTimeout,
        WriteTimeout:      cfg.WriteTimeout,
        IdleTimeout:       cfg.IdleTimeout,
    }

    built = true
    return &App{
        server:          server,
        pool:            pool,
        readiness:       readiness,
        shutdownTimeout: cfg.ShutdownTimeout,
    }, nil
}
```

Go 不允许在普通 defer closure 中修改未命名返回值来返回 Close 错误，因此 build 失败清理一般只记录 close 错误；对数据库 pool 而言 `Close` 无返回值。若某资源关闭会返回重要错误，应使用专门的 cleanup stack 或命名返回值，避免覆盖原始 build 错误。

## 8. Router 与模块化单体

### 8.1 路由树

```text
GET  /livez
GET  /readyz
GET  /metrics                 # 后续章节，可单独保护

POST /api/v1/batches          # ingest scope
GET  /api/v1/entries          # query scope
```

Router 只做组装：

```go
type Dependencies struct {
    Authenticator Authenticator
    Ingest        IngestService
    Query         QueryService
    Readiness     ReadinessChecker
    Logger        zerolog.Logger
}

func NewRouter(deps Dependencies) http.Handler {
    r := gin.New()
    r.Use(
        RequestID(),
        AccessLog(deps.Logger),
        Recovery(deps.Logger),
        SecurityHeaders(),
    )

    r.GET("/livez", LiveHandler())
    r.GET("/readyz", ReadyHandler(deps.Readiness))

    api := r.Group("/api/v1")
    api.Use(Authenticate(deps.Authenticator))
    api.POST("/batches", RequireScope(auth.ScopeIngest), IngestHandler(deps.Ingest))
    api.GET("/entries", RequireScope(auth.ScopeQuery), QueryHandler(deps.Query))
    return r
}
```

注意中间件顺序：

1. request ID 尽早建立，后续错误都有 ID。
2. access log 包住后续处理以记录状态与耗时。
3. recovery 捕获 Handler panic；具体相对顺序要用 Gin 的执行模型测试。
4. auth 在 API group，不阻挡 liveness。
5. scope 检查在认证之后、业务 Handler 之前。

## 9. HTTP Server 超时

### 9.1 每类超时解决什么问题

| 配置 | 保护对象 | 注意事项 |
| --- | --- | --- |
| `ReadHeaderTimeout` | 慢速 header 攻击 | 应始终设置 |
| `ReadTimeout` | 整个请求读取 | 上传大 batch 时不能过短 |
| `WriteTimeout` | 响应写入 | 必须覆盖允许的 DB 操作时长 |
| `IdleTimeout` | keep-alive 空闲连接 | 防止空闲连接长期占用 |
| handler/query timeout | 业务操作 | 应比上游代理 timeout 略短 |
| shutdown timeout | 优雅关闭总时间 | 到期后进入强制关闭 |

不能简单把所有值设成同一个短时间。若 `WriteTimeout` 比数据库事务 timeout 更短，Server 可能已经提交但来不及写 200；幂等仍保证正确，但会制造不必要重试。

### 9.2 Context 传播

Handler 必须把 `c.Request.Context()` 传入 service，再传入 repository。禁止在请求链路中换成 `context.Background()`。只有 shutdown cleanup 等必须脱离已取消请求的动作才创建独立有界 context。

## 10. 请求 ID

推荐行为：

- 接受格式合法且长度有限的 `X-Request-ID`。
- 缺失或非法时由 Server 生成。
- 放入 request context。
- 响应 header 回传。
- access log 和错误响应只记录该 ID，不记录认证 secret 或完整 body。

客户端 request ID 不能当安全身份，也不能当 batch 幂等 ID。它只用于一次 HTTP 请求的诊断；同一 batch 重试可以有不同 request ID。

## 11. Panic recovery 与错误响应

Gin 默认 recovery 可能输出栈和请求细节。目标行为应明确：

1. 记录 server-side stack、request ID、route、method。
2. 对 Authorization、Cookie、body 做脱敏或完全不记录。
3. 客户端只收到统一 `internal_error`。
4. 如果 header 已经写出，不能再假装返回结构化 500；记录该事实。

```go
func Recovery(logger zerolog.Logger) gin.HandlerFunc {
    return gin.CustomRecovery(func(c *gin.Context, recovered any) {
        requestID := RequestIDFromContext(c.Request.Context())
        logger.Error().
            Str("request_id", requestID).
            Str("method", c.Request.Method).
            Str("path", c.FullPath()).
            Interface("panic", safePanicValue(recovered)).
            Bytes("stack", debug.Stack()).
            Msg("request panic")

        WriteError(c, http.StatusInternalServerError, "internal_error", "internal server error", nil)
    })
}
```

`safePanicValue` 不应随意格式化大型或敏感对象。生产日志是否记录 stack 取决于部署信任边界，但本地简历项目至少应展示清晰策略。

## 12. 严格 Content-Type 与 body limit

上传 Handler 执行顺序：

1. 验证 `Content-Type` 主类型为 `application/json`，允许合法 charset 参数。
2. 使用 `http.MaxBytesReader` 包装 body。
3. 使用 `json.Decoder` 严格解码，拒绝 unknown fields。
4. 确认只有一个 JSON 值。
5. 映射并校验领域对象。
6. 调用 service。

```go
func IngestHandler(service IngestService, limits ingest.Limits) gin.HandlerFunc {
    return func(c *gin.Context) {
        if !isJSON(c.GetHeader("Content-Type")) {
            WriteError(c, 415, "unsupported_media_type", "content type must be application/json", nil)
            return
        }

        var req ingestv1.BatchRequest
        if err := DecodeJSON(c.Writer, c.Request, &req, limits.MaxBodyBytes); err != nil {
            WriteDecodeError(c, err)
            return
        }

        principal, ok := auth.PrincipalFromContext(c.Request.Context())
        if !ok {
            // 这是程序装配错误，而不是客户端未登录。
            WriteError(c, 500, "internal_error", "internal server error", nil)
            return
        }

        batch, err := mapper.ToDomain(principal.ProjectID, req)
        if err != nil {
            WriteDomainError(c, err)
            return
        }

        result, err := service.Accept(c.Request.Context(), batch)
        if err != nil {
            WriteDomainError(c, err)
            return
        }
        c.JSON(http.StatusOK, mapResult(result))
    }
}
```

Handler 不做 SQL，不解释 unique violation，不自己计算“duplicate”。

## 13. 统一错误映射

可以集中为：

```go
func WriteDomainError(c *gin.Context, err error) {
    switch {
    case errors.As(err, new(*ingest.ValidationError)):
        WriteError(c, 400, "invalid_request", "request validation failed", safeDetails(err))
    case errors.Is(err, ingest.ErrIdempotencyConflict):
        WriteError(c, 409, "idempotency_conflict", "batch ID conflicts with stored payload", nil)
    case errors.Is(err, context.DeadlineExceeded):
        WriteError(c, 503, "service_unavailable", "service temporarily unavailable", nil)
    case errors.Is(err, ingest.ErrUnavailable):
        WriteError(c, 503, "service_unavailable", "service temporarily unavailable", nil)
    default:
        logInternal(c, err)
        WriteError(c, 500, "internal_error", "internal server error", nil)
    }
}
```

不要把所有 context deadline 都映射成 503 而不理解来源：如果客户端主动断开，响应已经没有意义；日志/指标中应区分 client canceled 与 Server/DB deadline。

错误 response writer 应防止重复写 header，并保证 `Content-Type`、request ID 一致。

## 14. Liveness 与 Readiness

### 14.1 `/livez`

只回答“进程和 HTTP event loop 是否存活”。不要查询数据库，否则数据库故障会导致编排器重启健康进程，形成重启风暴。

```json
{"status":"alive"}
```

### 14.2 `/readyz`

回答“当前是否可以安全接收业务流量”。至少检查：

- 数据库可在短 timeout 内完成 ping 或轻量查询。
- schema migration 版本与程序兼容。
- Server 未进入 shutdown draining 状态。

```json
{
  "status": "not_ready",
  "checks": {
    "database": "failed",
    "schema": "unknown"
  }
}
```

公开环境不应返回 DSN、host、SQL 错误等内部信息。详细原因写到受控日志和指标。

### 14.3 readiness cache

每次 probe 都直连数据库可能在故障时放大压力。可以后台周期检查并缓存最近状态，但需要处理：

- 初始状态必须 not ready。
- 状态带更新时间和最大陈旧时间。
- shutdown 立即切 false。
- cache worker 有明确停止方法。

MVP 流量很小时也可直接做一个极短 timeout 的 `SELECT 1`，先测量再决定缓存。

## 15. 数据库连接池启动策略

`pgxpool.New` 一类构造可能只解析配置，并不证明数据库可达。Build 阶段应显式 `Ping`，否则进程会显示“已启动”但第一个请求才失败。

需要配置并验证：

- 最大连接数；
- 最小连接数是否真的需要；
- 连接最大寿命与 idle time；
- connect timeout；
- 每次 repository operation 的 query timeout；
- application name，便于 PostgreSQL 诊断。

连接池不是越大越好。多个 Server 实例的 pool 总和必须低于 PostgreSQL 可用连接，并为迁移、运维和保留任务留余量。

## 16. 优雅关闭的并发细节

### 16.1 正常信号路径

```text
SIGINT/SIGTERM
  -> root context canceled
  -> readiness false
  -> stop accepting new connections
  -> active request completes or reaches deadline
  -> background workers stop
  -> close DB pool
  -> process exits 0
```

### 16.2 监听失败路径

端口占用等 `ListenAndServe` 错误必须立即返回，不等待 signal。Build 成功但 Run 失败时，defer 仍应关闭 pool。

### 16.3 请求正在提交事务

如果 shutdown 取消 request context，事务应 rollback；如果数据库已 commit，再取消不会撤销提交。客户端可能收不到 200，Agent 会用同一 batch 重试并得到 duplicate。这正是幂等设计覆盖的窗口。

### 16.4 不要做的事

- 收到 signal 后立即 `os.Exit`。
- 在 goroutine 内调用 `log.Fatal` 或 `os.Exit`。
- `Shutdown(context.Background())` 无限等待。
- 关闭 pool 后才停止 HTTP。
- 忽略后台 goroutine 的退出。

## 17. 测试分层

### 17.1 配置单元测试

- 缺少数据库 URL。
- 缺少/过短 pepper。
- 非法 duration。
- limit 超过绝对上限。
- 环境变量覆盖普通默认值。
- 错误信息不包含 secret。

### 17.2 Router/Handler 测试

使用 `httptest.NewRecorder` 与 fake service，覆盖：

- `/livez` 不要求认证。
- API route 缺 key 返回 401。
- ingest key 可访问 batches，query-only key 返回 403。
- 错误 Content-Type 返回 415。
- body 超限返回 413。
- malformed/unknown/trailing JSON 返回稳定错误。
- project 来自 principal context。
- service 返回 accepted/duplicate/conflict 的映射。
- panic 返回安全 500，并带 request ID。

示例：

```go
func TestBatchRoutePassesAuthenticatedProject(t *testing.T) {
    ingestSvc := &recordingIngestService{result: ingest.AcceptedResult(1)}
    router := NewRouter(testDependencies(ingestSvc))

    req := httptest.NewRequest(http.MethodPost, "/api/v1/batches", strings.NewReader(validBody))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+testIngestKey)
    res := httptest.NewRecorder()

    router.ServeHTTP(res, req)

    if res.Code != http.StatusOK { t.Fatalf("status = %d", res.Code) }
    if diff := cmp.Diff(testProjectID, ingestSvc.got.ProjectID()); diff != "" {
        t.Fatalf("project mismatch (-want +got):\n%s", diff)
    }
}
```

### 17.3 生命周期测试

使用 `net.Listen("tcp", "127.0.0.1:0")` 让系统分配端口，避免硬编码导致并行测试冲突。验证：

- Server 可开始接受请求。
- cancel root context 后停止监听。
- 活动短请求能完成。
- 超过 shutdown deadline 的请求被终止。
- 端口已占用时 Run 返回错误。

不要依赖固定 sleep 判断启动完成。使用带 deadline 的重试探测或从 App 暴露 `Started` channel，且该 channel 只有监听真正建立后才关闭。

### 17.4 真实数据库集成测试

本章不要求所有 Handler 测试都启动 PostgreSQL。选少量高价值路径：真实 router + auth + service + repository + 临时 schema。更多细节见第 10、11 章。

## 18. 进程级冒烟验证

完成代码后，至少验证：

1. 配置缺失时快速失败且 exit code 非零。
2. 有效配置能监听动态或指定端口。
3. `/livez` 返回 200。
4. 数据库不可用时 `/readyz` 返回 503。
5. 数据库与迁移就绪后 `/readyz` 返回 200。
6. 发送终止信号后在 deadline 内退出。

Windows 与 Unix 信号支持不同。测试代码应避免假设所有平台都支持完全相同的 signal；平台差异可用 build tag 或独立脚本处理。不要为了冒烟验证去终止用户已启动的 Server。

### 18.1 验收证据记录

保留配置错误退出码、`/livez`/`/readyz` 响应、动态端口监听、信号关闭耗时和相关测试命令的实际输出。只有这些证据来自当前工作树和当前二进制时，才能把对应能力标为 verified；仅有代码骨架仍属于 implemented。

## 19. 可观测性最小要求

即使 Prometheus 尚未接入，结构化日志也应包含：

- 启动：版本、commit（若构建注入）、监听地址、非敏感限制摘要。
- readiness 状态变化。
- 每个请求：request ID、method、route template、status、duration、response bytes。
- 内部错误：request ID、错误链、安全上下文。
- shutdown：开始、drain 结果、超时、最终退出原因。

不要记录：

- Authorization header。
- 完整 DSN。
- pepper。
- 原始日志 body、message、attributes。
- 原始搜索关键词，除非部署信任模型明确允许且做了控制。

route label 应使用 `/api/v1/entries` 这样的模板，不使用包含动态值的原始 path，以免指标基数失控。

## 20. 常见坑

### 20.1 `Run` 看起来简单，所以继续使用

框架默认启动器隐藏了 `http.Server` 配置和 shutdown。原型可以，目标 Server 应显式拥有它。

### 20.2 Build 中途失败泄漏资源

连接池创建后，router 或其他依赖构造失败，若没有 cleanup 路径就泄漏。每引入一个资源，都要确定“谁创建、谁关闭、后续失败如何回滚”。

### 20.3 readiness 等于 liveness

数据库短暂故障时杀掉进程不能修数据库，只会增加波动。存活和接流量能力必须分开。

### 20.4 只限制 `Content-Length`

客户端可以省略或伪造该 header，chunked body 仍可能超大。必须由 `MaxBytesReader` 在读取时执行上限。

### 20.5 直接返回 decoder/SQL 错误

既泄露实现，又让客户端依赖不稳定文本。公开 code 稳定，内部 cause 保留在受控日志。

### 20.6 在中间件中使用字符串 key

Gin 的 `c.Set("project_id", ...)` 容易拼错和类型断言失败。认证主体最好放入标准 request context，使用未导出的强类型 key 和 helper。

### 20.7 给每个请求无条件做数据库 Ping

认证本身已经访问 key repository 时，额外 Ping 可能只是增加负载。readiness 和业务请求的数据库行为分别设计。

### 20.8 让 `Close` 与 `Run` 同时重复关闭资源

要定义幂等关闭或明确调用顺序。重复关闭 channel 会 panic；数据库 pool 通常可安全 Close，但不要依赖所有资源都如此。

## 21. 复盘题

1. 为什么 `main` 不应该包含路由和 SQL？
2. `Build` 成功前创建的资源如何在中途失败时释放？
3. `ReadHeaderTimeout` 与 handler timeout 分别保护什么？
4. 为什么 shutdown 时应先让 readiness 失败？
5. 数据库提交后响应丢失，为什么不会破坏最终写入唯一性？
6. `/livez` 为什么不应依赖数据库？
7. `Content-Length` 为什么不能替代读取时 body limit？
8. 哪些信息能进入 access log，哪些必须禁止？
9. router 单元测试与进程冒烟测试分别证明什么？
10. 如何证明 Server 没有依赖硬编码 `:8080`？

## 22. 完成门

- [ ] `main` 只做 load/build/run/exit code。
- [ ] Server 使用显式 `http.Server` 并配置所有关键 timeout。
- [ ] router 可在不监听真实端口时通过 `httptest` 测试。
- [ ] build 失败不会泄漏已创建资源。
- [ ] `/livez` 与 `/readyz` 语义和实现分开。
- [ ] `/api/v1/batches` 和 `/api/v1/entries` 使用统一认证上下文。
- [ ] body limit、严格 JSON、Content-Type 和统一错误响应生效。
- [ ] request ID 贯穿响应、日志和错误。
- [ ] SIGINT/SIGTERM 路径有有界优雅关闭证据。
- [ ] 依赖由 `go get`/迁移工具官方安装方式引入，`go.mod`/`go.sum` 由工具维护。
- [ ] 日志与错误未暴露 key、pepper、DSN 或日志内容。
- [ ] 目标能力均写成待实现，验证结果只有在命令实际通过后再更新。

完成后继续[Project、API Key 与 scope 授权](09-auth-project-scopes.md)。
