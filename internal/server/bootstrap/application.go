package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/isyuah/gline/internal/protocol/ingestv1"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/config"
	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/httpapi"
	"github.com/isyuah/gline/internal/server/ingest"
	"github.com/isyuah/gline/internal/server/maintenance"
	"github.com/isyuah/gline/internal/server/observability"
	"github.com/isyuah/gline/internal/server/operations"
	"github.com/isyuah/gline/internal/server/query"
	"github.com/isyuah/gline/internal/storage/postgres"
	"github.com/isyuah/gline/migrations"
)

type Application struct {
	server      *http.Server
	store       *postgres.Store
	maintenance *maintenance.Worker
	logger      *slog.Logger
	shutdown    time.Duration
}

func New(ctx context.Context, cfg config.Config, version string, logger *slog.Logger) (*Application, error) {
	if logger == nil {
		logger = slog.Default()
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	failed := true
	defer func() {
		if failed {
			_ = db.Close()
		}
	}()

	databaseCtx, cancel := context.WithTimeout(ctx, cfg.DatabaseTimeout)
	defer cancel()
	if err := db.PingContext(databaseCtx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := postgres.Migrate(databaseCtx, db, migrations.FS); err != nil {
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	store := postgres.New(db)

	authenticator, err := serverauth.NewAuthenticator(store.APIKeys(), []byte(cfg.APIKeyPepper), nil, serverauth.WithLogger(logger))
	if err != nil {
		return nil, err
	}
	controlService, err := control.NewService(controlTransactions(store), nil, nil, nil, []byte(cfg.APIKeyPepper))
	if err != nil {
		return nil, err
	}
	ingestService, err := ingest.NewService(ingestTransactions(store), nil)
	if err != nil {
		return nil, err
	}
	cursorSecret := sha256.Sum256(append([]byte("gline-query-cursor-v1\x00"), []byte(cfg.APIKeyPepper)...))
	queryConfig := query.DefaultConfig()
	queryConfig.MaxRange = cfg.QueryMaxRange
	queryConfig.ExecutionTimeout = cfg.QueryTimeout
	queryConfig.MaxLimit = cfg.QueryMaxPageSize
	if queryConfig.DefaultLimit > queryConfig.MaxLimit {
		queryConfig.DefaultLimit = queryConfig.MaxLimit
	}
	queryService, err := query.NewService(store.Projects(), store.Entries(), newProjectLimiter(cfg.QueryConcurrency), cursorSecret[:], queryConfig)
	if err != nil {
		return nil, err
	}
	ingestLimits := ingestv1.DefaultLimits()
	if cfg.MaxRequestBytes < ingestLimits.MaxBodyBytes {
		ingestLimits.MaxBodyBytes = cfg.MaxRequestBytes
	}
	operationsService, err := operations.New(operationTransactions(store), ingestService, ingestLimits)
	if err != nil {
		return nil, err
	}

	handler, err := httpapi.New(httpapi.Config{
		BootstrapToken: cfg.BootstrapToken, AllowedOrigins: cfg.AllowedOrigins,
		MaxJSONBytes: cfg.MaxRequestBytes, IngestLimits: ingestLimits,
		Version: version, ReadyTimeout: cfg.DatabaseTimeout,
	}, httpapi.Dependencies{
		Authenticator: authenticator, Control: controlService, Ingest: ingestService,
		Query: queryService, Operations: operationsService, Projects: store.Projects(),
		Keys: store.APIKeys(), Agents: store.Agents(), Pipelines: store.Pipelines(),
		Retention: store.Retention(), Usage: store.Usage(), Audit: store.Audit(),
		Quarantine: store.Quarantine(), Ready: store,
	})
	if err != nil {
		return nil, err
	}
	router := handler.Router()
	registerer := prometheus.NewRegistry()
	registerer.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(registerer, promhttp.HandlerOpts{})))
	metrics := observability.NewHTTPMetrics(registerer)

	maintenanceConfig := maintenance.DefaultConfig()
	maintenanceConfig.Interval = cfg.MaintenanceEvery
	maintenanceConfig.AgentStaleAfter = cfg.AgentStaleAfter
	maintenanceConfig.BatchSize = cfg.RetentionBatch
	worker, err := maintenance.New(store.Agents(), store.Retention(), store.Quarantine(), maintenanceConfig, logger)
	if err != nil {
		return nil, err
	}
	failed = false
	return &Application{
		server: &http.Server{
			Addr: cfg.HTTPAddr, Handler: metrics.Wrap(router),
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		},
		store: store, maintenance: worker, logger: logger, shutdown: cfg.ShutdownTimeout,
	}, nil
}

func (a *Application) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverErrors := make(chan error, 1)
	workerErrors := make(chan error, 1)
	go func() { serverErrors <- a.server.ListenAndServe() }()
	go func() { workerErrors <- a.maintenance.Run(runCtx) }()
	a.logger.Info("gline server started", "address", a.server.Addr)

	var runErr error
	workerStopped := false
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve HTTP: %w", err)
		}
	case err := <-workerErrors:
		workerStopped = true
		if !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("maintenance worker: %w", err)
		}
	}
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), a.shutdown)
	defer shutdownCancel()
	shutdownErr := a.server.Shutdown(shutdownCtx)
	if !workerStopped {
		select {
		case err := <-workerErrors:
			if !errors.Is(err, context.Canceled) {
				runErr = errors.Join(runErr, fmt.Errorf("maintenance worker: %w", err))
			}
		case <-shutdownCtx.Done():
			runErr = errors.Join(runErr, fmt.Errorf("stop maintenance worker: %w", shutdownCtx.Err()))
		}
	}
	closeErr := a.store.Close()
	if runErr != nil || shutdownErr != nil || closeErr != nil {
		return errors.Join(runErr, shutdownErr, closeErr)
	}
	a.logger.Info("gline server stopped")
	return nil
}
