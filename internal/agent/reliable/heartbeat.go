package reliable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HeartbeatPipeline struct {
	ID            string  `json:"id"`
	ConfigVersion int64   `json:"config_version"`
	Status        string  `json:"status"`
	LastError     *string `json:"last_error,omitempty"`
}

type HTTPHeartbeat struct {
	endpoint string
	token    string
	version  string
	client   HTTPDoer
}

func NewHTTPHeartbeat(endpoint, token, version string, client HTTPDoer) (*HTTPHeartbeat, error) {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("heartbeat endpoint and token are required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("heartbeat endpoint must be an absolute URL")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	return &HTTPHeartbeat{endpoint: parsed.String(), token: token, version: version, client: client}, nil
}

func (h *HTTPHeartbeat) Report(ctx context.Context, pipelines []HeartbeatPipeline) (ControlSnapshot, error) {
	body, err := json.Marshal(struct {
		Version   string              `json:"version"`
		Pipelines []HeartbeatPipeline `json:"pipelines"`
	}{Version: h.version, Pipelines: pipelines})
	if err != nil {
		return ControlSnapshot{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return ControlSnapshot{}, fmt.Errorf("create heartbeat request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		return ControlSnapshot{}, fmt.Errorf("send heartbeat: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		return ControlSnapshot{}, fmt.Errorf("read heartbeat response: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ControlSnapshot{}, fmt.Errorf("heartbeat returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Control ControlSnapshot `json:"control"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return ControlSnapshot{}, fmt.Errorf("decode heartbeat response: %w", err)
	}
	return envelope.Control, nil
}
