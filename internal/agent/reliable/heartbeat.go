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
	ID     string `json:"id"`
	Status string `json:"status"`
}

type HTTPHeartbeat struct {
	endpoint  string
	token     string
	version   string
	pipelines []HeartbeatPipeline
	client    HTTPDoer
}

func NewHTTPHeartbeat(endpoint, token, version string, pipelineIDs []string, client HTTPDoer) (*HTTPHeartbeat, error) {
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
	pipelines := make([]HeartbeatPipeline, len(pipelineIDs))
	for index, id := range pipelineIDs {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("heartbeat pipeline id is empty")
		}
		pipelines[index] = HeartbeatPipeline{ID: id, Status: "running"}
	}
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	return &HTTPHeartbeat{endpoint: parsed.String(), token: token, version: version, pipelines: pipelines, client: client}, nil
}

func (h *HTTPHeartbeat) Report(ctx context.Context) error {
	body, err := json.Marshal(struct {
		Version   string              `json:"version"`
		Pipelines []HeartbeatPipeline `json:"pipelines"`
	}{Version: h.version, Pipelines: h.pipelines})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create heartbeat request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		return fmt.Errorf("send heartbeat: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("heartbeat returned HTTP %d", response.StatusCode)
	}
	return nil
}
