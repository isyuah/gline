package httpapi

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
)

type Config struct {
	BootstrapToken string
	AllowedOrigins []string
	MaxJSONBytes   int64
	IngestLimits   ingestv1.Limits
	Version        string
	ReadyTimeout   time.Duration
	Middleware     []gin.HandlerFunc
	Draining       func() bool
}

func DefaultConfig() Config {
	return Config{
		MaxJSONBytes: 1 << 20,
		IngestLimits: ingestv1.DefaultLimits(),
		Version:      "dev",
		ReadyTimeout: 2 * time.Second,
	}
}

type Handler struct {
	deps             Dependencies
	config           Config
	bootstrapToken   []byte
	allowedOriginSet map[string]struct{}
}

func New(config Config, deps Dependencies) (*Handler, error) {
	if deps.Authenticator == nil || deps.Control == nil || deps.Ingest == nil || deps.Query == nil ||
		deps.Operations == nil || deps.Projects == nil || deps.Keys == nil || deps.Agents == nil ||
		deps.Pipelines == nil || deps.Retention == nil || deps.Usage == nil || deps.Audit == nil ||
		deps.Quarantine == nil || deps.Ready == nil {
		return nil, errors.New("http api requires all dependencies")
	}
	if len(config.BootstrapToken) < 24 {
		return nil, errors.New("http api bootstrap token must contain at least 24 bytes")
	}
	if config.MaxJSONBytes <= 0 {
		config.MaxJSONBytes = 1 << 20
	}
	if config.IngestLimits.MaxEntries <= 0 {
		config.IngestLimits = ingestv1.DefaultLimits()
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 2 * time.Second
	}
	if strings.TrimSpace(config.Version) == "" {
		config.Version = "dev"
	}
	if config.Draining == nil {
		config.Draining = func() bool { return false }
	}
	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, origin := range config.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" || origin == "*" || strings.ContainsAny(origin, "\r\n") {
			return nil, errors.New("http api contains an invalid CORS origin")
		}
		origins[origin] = struct{}{}
	}
	return &Handler{deps: deps, config: config, bootstrapToken: []byte(config.BootstrapToken), allowedOriginSet: origins}, nil
}

func (h *Handler) Router() *gin.Engine {
	router := gin.New()
	_ = router.SetTrustedProxies(nil)
	router.Use(h.requestID(), h.recovery(), h.cors())
	if len(h.config.Middleware) > 0 {
		router.Use(h.config.Middleware...)
	}
	router.GET("/healthz", h.health)
	router.GET("/livez", h.health)
	router.GET("/readyz", h.ready)

	api := router.Group("/api/v1")
	api.Use(h.authenticate())
	api.GET("/projects", h.listProjects)
	api.POST("/projects", h.createProject)
	api.POST("/projects/:projectID/enable", h.setProjectStatus)
	api.POST("/projects/:projectID/disable", h.setProjectStatus)
	api.GET("/projects/:projectID/keys", h.listKeys)
	api.POST("/projects/:projectID/keys", h.createKey)
	api.POST("/projects/:projectID/keys/:keyID/revoke", h.revokeKey)
	api.GET("/agents", h.listAgents)
	api.POST("/projects/:projectID/agents", h.createAgent)
	api.POST("/agents/:agentID/heartbeat", h.heartbeat)
	api.GET("/pipelines", h.listPipelines)
	api.POST("/projects/:projectID/pipelines", h.createPipeline)
	api.PUT("/projects/:projectID/pipelines/:pipelineID", h.updatePipeline)
	api.POST("/projects/:projectID/pipelines/:pipelineID/:action", h.setPipelineStatus)
	api.POST("/batches", h.ingestBatch)
	api.GET("/entries", h.searchEntries)
	api.GET("/projects/:projectID/retention", h.getRetention)
	api.PUT("/projects/:projectID/retention", h.setRetention)
	api.GET("/projects/:projectID/usage", h.getUsage)
	api.GET("/audit", h.listAudit)
	api.GET("/quarantine", h.listQuarantine)
	api.POST("/quarantine/:id/replay", h.replayQuarantine)
	api.POST("/quarantine/:id/discard", h.discardQuarantine)
	return router
}
