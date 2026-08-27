package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
)

func decodeJSON(c *gin.Context, maxBytes int64, destination any) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return ingestv1.ErrBodyTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return errBadRequest("invalid_json", "request body is not valid JSON", nil)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errBadRequest("invalid_json", "request body must contain exactly one JSON object", nil)
	}
	return nil
}

func queryInt(c *gin.Context, name string, fallback, maximum int) (int, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > maximum {
		return 0, errBadRequest("invalid_"+name, name+" is outside the allowed range", nil)
	}
	return value, nil
}

func parseRFC3339(raw, field string) (time.Time, error) {
	value, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil || value.Year() < 1970 || value.Year() > 9999 {
		return time.Time{}, errBadRequest("invalid_"+field, field+" must be an RFC3339 timestamp", nil)
	}
	return value.UTC(), nil
}
