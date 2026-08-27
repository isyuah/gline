package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

const (
	principalKey = "gline.principal"
	bootstrapKey = "gline.bootstrap"
	requestIDKey = "gline.request_id"
)

var bootstrapKeyID = domain.APIKeyID("00000000-0000-4000-8000-000000000001")
var bootstrapProjectID = domain.ProjectID("00000000-0000-4000-8000-000000000002")

func (h *Handler) requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !validRequestID(id) {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err == nil {
				id = hex.EncodeToString(raw[:])
			} else {
				id = "request-id-unavailable"
			}
		}
		c.Set(requestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func (h *Handler) recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				writeError(c, errors.New("panic while serving request"))
			}
		}()
		c.Next()
	}
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (h *Handler) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if _, allowed := h.allowedOriginSet[origin]; !allowed {
				if c.Request.Method == http.MethodOptions {
					writeError(c, errForbidden("origin is not allowed"))
					c.Abort()
					return
				}
			} else {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
				c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
				c.Header("Access-Control-Expose-Headers", "X-Request-ID")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.Status(http.StatusNoContent)
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *Handler) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			writeError(c, errUnauthorized())
			c.Abort()
			return
		}
		if secureEqual(raw, h.bootstrapToken) {
			c.Set(principalKey, bootstrapPrincipal())
			c.Set(bootstrapKey, true)
			c.Next()
			return
		}
		principal, err := h.deps.Authenticator.Authenticate(c.Request.Context(), raw)
		if err != nil {
			if errors.Is(err, serverauth.ErrInvalidCredential) {
				writeError(c, errUnauthorized())
			} else {
				writeError(c, err)
			}
			c.Abort()
			return
		}
		if !principal.Valid() {
			writeError(c, errUnauthorized())
			c.Abort()
			return
		}
		c.Set(principalKey, principal)
		c.Set(bootstrapKey, false)
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") {
		return "", false
	}
	raw := strings.TrimSpace(header[7:])
	return raw, raw != "" && !strings.ContainsAny(raw, " \t\r\n") && len(raw) <= 768
}

func secureEqual(raw string, expected []byte) bool {
	provided := []byte(raw)
	if len(provided) != len(expected) {
		var dummy [32]byte
		_ = subtle.ConstantTimeCompare(dummy[:], dummy[:])
		return false
	}
	return subtle.ConstantTimeCompare(provided, expected) == 1
}

func bootstrapPrincipal() serverauth.Principal {
	return serverauth.Principal{
		KeyID: bootstrapKeyID, ProjectID: bootstrapProjectID,
		Scopes: map[domain.Scope]struct{}{
			domain.ScopeIngest: {}, domain.ScopeQuery: {}, domain.ScopeProjectRead: {},
			domain.ScopeProjectWrite: {}, domain.ScopeKeyManage: {}, domain.ScopeAgentRead: {},
			domain.ScopeAgentWrite: {}, domain.ScopePipelineRead: {}, domain.ScopePipelineWrite: {},
			domain.ScopeQuarantineRead: {}, domain.ScopeQuarantineReplay: {},
			domain.ScopeRetentionManage: {}, domain.ScopeAuditRead: {},
		},
	}
}

func principal(c *gin.Context) serverauth.Principal {
	value, _ := c.Get(principalKey)
	result, _ := value.(serverauth.Principal)
	return result
}

func isBootstrap(c *gin.Context) bool {
	value, _ := c.Get(bootstrapKey)
	result, _ := value.(bool)
	return result
}

func (h *Handler) projectPrincipal(c *gin.Context, raw string, scope domain.Scope) (serverauth.Principal, bool) {
	projectID := domain.ProjectID(strings.TrimSpace(raw))
	if !projectID.Valid() {
		writeError(c, errBadRequest("invalid_project_id", "project_id must be a UUID", nil))
		return serverauth.Principal{}, false
	}
	result := principal(c)
	if err := result.Require(scope); err != nil {
		writeError(c, err)
		return serverauth.Principal{}, false
	}
	if isBootstrap(c) {
		result.ProjectID = projectID
		return result, true
	}
	if err := result.RequireProject(projectID); err != nil {
		writeError(c, err)
		return serverauth.Principal{}, false
	}
	return result, true
}
