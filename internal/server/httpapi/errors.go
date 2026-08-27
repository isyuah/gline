package httpapi

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	"github.com/isyuah/gline/internal/server/admission"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/ingest"
	"github.com/isyuah/gline/internal/server/query"
	"github.com/isyuah/gline/internal/storage/postgres"
)

type apiError struct {
	Status  int
	Code    string
	Message string
	Details any
	Cause   error
}

func (e *apiError) Error() string { return e.Message }
func (e *apiError) Unwrap() error { return e.Cause }

func errUnauthorized() error {
	return &apiError{Status: http.StatusUnauthorized, Code: "invalid_credential", Message: "API token is missing, invalid, or expired"}
}

func errForbidden(message string) error {
	return &apiError{Status: http.StatusForbidden, Code: "forbidden", Message: message}
}

func errBadRequest(code, message string, details any) error {
	return &apiError{Status: http.StatusBadRequest, Code: code, Message: message, Details: details}
}

func writeError(c *gin.Context, err error) {
	mapped := mapError(err)
	var limited *admission.LimitError
	if errors.As(err, &limited) {
		seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.Itoa(seconds))
	} else if errors.Is(err, query.ErrCapacityLimited) {
		c.Header("Retry-After", "1")
	}
	errorBody := gin.H{"code": mapped.Code, "message": mapped.Message, "request_id": requestID(c)}
	if mapped.Details != nil {
		errorBody["details"] = mapped.Details
	}
	c.AbortWithStatusJSON(mapped.Status, gin.H{"error": errorBody})
}

func mapError(err error) *apiError {
	var explicit *apiError
	if errors.As(err, &explicit) {
		return explicit
	}
	var validation *ingestv1.ValidationError
	if errors.As(err, &validation) {
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_batch", Message: "batch validation failed", Details: validation.Fields, Cause: err}
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return &apiError{Status: http.StatusBadRequest, Code: "invalid_json", Message: "request body is not valid JSON", Cause: err}
	}
	switch {
	case errors.Is(err, serverauth.ErrInvalidCredential):
		return &apiError{Status: http.StatusUnauthorized, Code: "invalid_credential", Message: "API token is missing, invalid, or expired", Cause: err}
	case errors.Is(err, domain.ErrScopeDenied):
		return &apiError{Status: http.StatusForbidden, Code: "scope_denied", Message: "API token does not grant the required scope", Cause: err}
	case errors.Is(err, serverauth.ErrProjectMismatch), errors.Is(err, serverauth.ErrAgentMismatch):
		return &apiError{Status: http.StatusForbidden, Code: "tenant_mismatch", Message: "resource belongs to another authentication boundary", Cause: err}
	case errors.Is(err, postgres.ErrNotFound):
		return &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "resource was not found", Cause: err}
	case errors.Is(err, postgres.ErrConflict), errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, control.ErrVersionConflict):
		return &apiError{Status: http.StatusConflict, Code: "conflict", Message: "resource state conflicts with the request", Cause: err}
	case errors.Is(err, control.ErrResourceBinding):
		return &apiError{Status: http.StatusBadRequest, Code: "invalid_resource_binding", Message: "resource does not belong to the requested parent", Cause: err}
	case errors.Is(err, domain.ErrProjectDisabled), errors.Is(err, control.ErrDisabled), errors.Is(err, ingest.ErrAgentDisabled), errors.Is(err, ingest.ErrPipelineUnavailable):
		return &apiError{Status: http.StatusConflict, Code: "resource_unavailable", Message: "resource is disabled or unavailable", Cause: err}
	case errors.Is(err, ingestv1.ErrBodyTooLarge):
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "request body exceeds the configured limit", Cause: err}
	case errors.Is(err, admission.ErrBatchExceedsCapacity):
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "admission_capacity_exceeded", Message: "batch cost exceeds the configured ingest capacity", Cause: err}
	case errors.Is(err, admission.ErrLimited):
		return &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "ingest capacity is temporarily exhausted", Cause: err}
	case errors.Is(err, query.ErrCapacityLimited):
		return &apiError{Status: http.StatusTooManyRequests, Code: "query_capacity_limited", Message: "query capacity is temporarily exhausted", Cause: err}
	case errors.Is(err, query.ErrExecutionTimeout):
		return &apiError{Status: http.StatusGatewayTimeout, Code: "query_timeout", Message: "query execution exceeded its time budget", Cause: err}
	case errors.Is(err, ingestv1.ErrUnsupportedVersion):
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "unsupported_protocol", Message: "ingest protocol version is not supported", Cause: err}
	case errors.Is(err, ingestv1.ErrInvalidJSON), errors.Is(err, ingestv1.ErrTrailingJSON):
		return &apiError{Status: http.StatusBadRequest, Code: "invalid_json", Message: "request body is not valid JSON", Cause: err}
	case errors.Is(err, query.ErrInvalidTimeRange):
		return &apiError{Status: http.StatusBadRequest, Code: "invalid_time_range", Message: "from and to must define a bounded RFC3339 range", Cause: err}
	case errors.Is(err, query.ErrInvalidLimit), errors.Is(err, query.ErrInvalidFilter), errors.Is(err, query.ErrInvalidCursor), errors.Is(err, domain.ErrInvalid):
		return &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "request parameters are invalid", Cause: err}
	default:
		return &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "the server could not complete the request", Cause: err}
	}
}

func requestID(c *gin.Context) string {
	value, _ := c.Get(requestIDKey)
	id, _ := value.(string)
	return id
}
