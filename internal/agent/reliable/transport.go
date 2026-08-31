package reliable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ResultClass uint8

const (
	ResultAccepted ResultClass = iota + 1
	ResultDuplicate
	ResultRetryable
	ResultQuarantine
	ResultTerminal
	ResultBlocked
)

type SendResult struct {
	Class      ResultClass
	StatusCode int
	RetryAfter time.Duration
	Code       string
}

type Transport interface {
	Send(context.Context, []byte) (SendResult, error)
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type HTTPTransport struct {
	endpoint string
	token    string
	client   HTTPDoer
}

func NewHTTPTransport(endpoint, token string, client HTTPDoer) (*HTTPTransport, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("reliable transport endpoint is empty")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("reliable transport token is empty")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &HTTPTransport{endpoint: endpoint, token: token, client: client}, nil
}

func (t *HTTPTransport) Send(ctx context.Context, payload []byte) (SendResult, error) {
	var envelope struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || strings.TrimSpace(envelope.BatchID) == "" {
		return SendResult{Class: ResultTerminal, Code: "invalid_outbound_batch"}, errors.New("outbound payload has no valid batch_id")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(payload))
	if err != nil {
		return SendResult{}, fmt.Errorf("create ingest request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+t.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		return SendResult{Class: ResultRetryable}, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		return SendResult{Class: ResultRetryable, StatusCode: response.StatusCode}, readErr
	}
	result := SendResult{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now())}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		var acknowledgement struct {
			BatchID string `json:"batch_id"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal(body, &acknowledgement); err != nil {
			result.Class = ResultRetryable
			result.Code = "invalid_ack"
			return result, nil
		}
		if acknowledgement.BatchID != envelope.BatchID {
			result.Class = ResultRetryable
			result.Code = "invalid_ack_batch_id"
			return result, nil
		}
		switch acknowledgement.Status {
		case "accepted":
			result.Class = ResultAccepted
		case "duplicate":
			result.Class = ResultDuplicate
		default:
			result.Class = ResultRetryable
			result.Code = "invalid_ack_status"
		}
		return result, nil
	}

	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &failure)
	result.Code = failure.Error.Code
	if result.Code == "" {
		result.Code = failure.Code
	}
	switch {
	case response.StatusCode == http.StatusRequestTimeout,
		response.StatusCode == http.StatusTooManyRequests,
		response.StatusCode >= 500:
		result.Class = ResultRetryable
	case response.StatusCode == http.StatusConflict && result.Code == "resource_unavailable":
		// This is operator-controlled state, not a malformed immutable batch.
		// Keep the batch pending while allowing unrelated pipelines to drain.
		result.Class = ResultBlocked
	case response.StatusCode == http.StatusBadRequest,
		response.StatusCode == http.StatusConflict,
		response.StatusCode == http.StatusRequestEntityTooLarge,
		response.StatusCode == http.StatusUnprocessableEntity:
		// These responses describe this immutable batch. Persist it in the
		// local quarantine so one bad record cannot block unrelated pipelines.
		result.Class = ResultQuarantine
	default:
		// Authentication, authorization and endpoint failures are systemic.
		// Stop with the batch still pending so fixing configuration can resume it.
		result.Class = ResultTerminal
	}
	return result, nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
