package ingestv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/isyuah/gline/internal/domain"
)

const Version = 1

var (
	ErrInvalidJSON        = errors.New("ingest v1: invalid json")
	ErrTrailingJSON       = errors.New("ingest v1: trailing json value")
	ErrBodyTooLarge       = errors.New("ingest v1: body too large")
	ErrValidation         = errors.New("ingest v1: validation failed")
	ErrUnsupportedVersion = errors.New("ingest v1: unsupported protocol version")
)

type BatchRequest struct {
	ProtocolVersion int       `json:"protocol_version"`
	BatchID         string    `json:"batch_id"`
	AgentID         string    `json:"agent_id"`
	PipelineID      string    `json:"pipeline_id"`
	Sequence        int64     `json:"sequence"`
	SentAt          time.Time `json:"sent_at"`
	Entries         []Entry   `json:"entries"`
}

type Entry struct {
	Sequence   int            `json:"sequence"`
	ObservedAt time.Time      `json:"observed_at"`
	Level      string         `json:"level"`
	Service    string         `json:"service"`
	Host       string         `json:"host"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"attributes"`
}

type Limits struct {
	MaxBodyBytes       int64
	MaxEntries         int
	MaxServiceBytes    int
	MaxHostBytes       int
	MaxLevelBytes      int
	MaxMessageBytes    int
	MaxAttributesBytes int
	MaxAttributesDepth int
	MaxFieldErrors     int
}

func DefaultLimits() Limits {
	return Limits{
		MaxBodyBytes: 4 << 20, MaxEntries: 2_000,
		MaxServiceBytes: 128, MaxHostBytes: 255, MaxLevelBytes: 16,
		MaxMessageBytes: 256 << 10, MaxAttributesBytes: 64 << 10,
		MaxAttributesDepth: 8, MaxFieldErrors: 16,
	}
}

type FieldError struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string { return ErrValidation.Error() }
func (e *ValidationError) Unwrap() error { return ErrValidation }

// Decode accepts exactly one JSON object, preserves JSON numbers, and rejects
// unknown fields. The caller should still impose the HTTP server's body limit;
// this limit also protects non-HTTP callers.
func Decode(r io.Reader, maxBytes int64) (BatchRequest, int, error) {
	if r == nil {
		return BatchRequest{}, 0, fmt.Errorf("%w: empty body", ErrInvalidJSON)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLimits().MaxBodyBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return BatchRequest{}, 0, fmt.Errorf("read ingest body: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return BatchRequest{}, len(raw), ErrBodyTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var request BatchRequest
	if err := decoder.Decode(&request); err != nil {
		return BatchRequest{}, len(raw), fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return BatchRequest{}, len(raw), ErrTrailingJSON
		}
		return BatchRequest{}, len(raw), fmt.Errorf("%w: %v", ErrTrailingJSON, err)
	}
	return request, len(raw), nil
}

// Normalize validates the wire contract, injects the authenticated Project,
// and computes the only payload representation used for idempotency.
func Normalize(request BatchRequest, projectID domain.ProjectID, payloadBytes int, limits Limits) (domain.Batch, error) {
	if request.ProtocolVersion != Version {
		return domain.Batch{}, ErrUnsupportedVersion
	}
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	validation := validator{limit: limits.MaxFieldErrors}
	batchID := domain.BatchID(request.BatchID)
	agentID := domain.AgentID(request.AgentID)
	pipelineID := domain.PipelineID(request.PipelineID)
	validation.require(projectID.Valid(), "project_id", "authenticated project is invalid")
	validation.require(batchID.Valid(), "batch_id", "must be a UUID")
	validation.require(agentID.Valid(), "agent_id", "must be a UUID")
	validation.require(pipelineID.Valid(), "pipeline_id", "must be a UUID")
	validation.require(request.Sequence >= 0, "sequence", "must be non-negative")
	validation.require(!request.SentAt.IsZero(), "sent_at", "must be RFC3339")
	validation.require(len(request.Entries) > 0, "entries", "must not be empty")
	validation.require(len(request.Entries) <= limits.MaxEntries, "entries", "exceeds batch limit")

	entries := make([]domain.Entry, len(request.Entries))
	for index, item := range request.Entries {
		path := fmt.Sprintf("entries[%d]", index)
		validation.require(item.Sequence == index, path+".sequence", fmt.Sprintf("expected %d", index))
		validation.require(!item.ObservedAt.IsZero(), path+".observed_at", "must be RFC3339")
		level := strings.ToUpper(strings.TrimSpace(item.Level))
		validation.require(validLevel(level), path+".level", "unsupported level")
		validation.text(path+".level", level, limits.MaxLevelBytes, false)
		service := strings.TrimSpace(item.Service)
		host := strings.TrimSpace(item.Host)
		validation.text(path+".service", service, limits.MaxServiceBytes, false)
		validation.text(path+".host", host, limits.MaxHostBytes, false)
		validation.text(path+".message", item.Message, limits.MaxMessageBytes, true)
		attributes := item.Attributes
		if attributes == nil {
			attributes = map[string]any{}
		}
		canonicalAttributes, err := canonicalJSON(attributes, limits.MaxAttributesDepth)
		if err != nil {
			validation.add(path+".attributes", "must contain supported JSON values")
		} else if len(canonicalAttributes) > limits.MaxAttributesBytes {
			validation.add(path+".attributes", "exceeds byte limit")
		}
		entries[index] = domain.Entry{
			BatchSequence: index, Service: service, Host: host, Level: level,
			Message: item.Message, ObservedAt: item.ObservedAt.UTC(), Attributes: attributes,
		}
	}
	if len(validation.fields) > 0 {
		return domain.Batch{}, &ValidationError{Fields: validation.fields}
	}
	canonical, err := CanonicalPayload(request, entries)
	if err != nil {
		return domain.Batch{}, err
	}
	if payloadBytes <= 0 {
		payloadBytes = len(canonical)
	}
	batch := domain.Batch{
		ID: batchID, ProjectID: projectID, AgentID: agentID, PipelineID: pipelineID,
		Sequence: request.Sequence, PayloadHash: sha256.Sum256(canonical),
		PayloadBytes: payloadBytes, Entries: entries, CreatedAt: request.SentAt.UTC(),
	}
	for index := range batch.Entries {
		batch.Entries[index].ProjectID = projectID
		batch.Entries[index].BatchID = batchID
		batch.Entries[index].AgentID = agentID
		batch.Entries[index].PipelineID = pipelineID
	}
	if err := batch.Validate(); err != nil {
		return domain.Batch{}, err
	}
	return batch, nil
}

func CanonicalPayload(request BatchRequest, entries []domain.Entry) ([]byte, error) {
	if len(request.Entries) != len(entries) {
		return nil, fmt.Errorf("%w: normalized entry count", ErrValidation)
	}
	var out bytes.Buffer
	out.WriteString(`{"protocol_version":1,"batch_id":`)
	writeQuoted(&out, strings.ToLower(request.BatchID))
	out.WriteString(`,"agent_id":`)
	writeQuoted(&out, strings.ToLower(request.AgentID))
	out.WriteString(`,"pipeline_id":`)
	writeQuoted(&out, strings.ToLower(request.PipelineID))
	fmt.Fprintf(&out, `,"sequence":%d,"sent_at":`, request.Sequence)
	writeQuoted(&out, request.SentAt.UTC().Format(time.RFC3339Nano))
	out.WriteString(`,"entries":[`)
	for index, entry := range entries {
		if index > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, `{"sequence":%d,"observed_at":`, index)
		writeQuoted(&out, entry.ObservedAt.UTC().Format(time.RFC3339Nano))
		out.WriteString(`,"level":`)
		writeQuoted(&out, entry.Level)
		out.WriteString(`,"service":`)
		writeQuoted(&out, entry.Service)
		out.WriteString(`,"host":`)
		writeQuoted(&out, entry.Host)
		out.WriteString(`,"message":`)
		writeQuoted(&out, entry.Message)
		out.WriteString(`,"attributes":`)
		encoded, err := canonicalJSON(entry.Attributes, DefaultLimits().MaxAttributesDepth)
		if err != nil {
			return nil, fmt.Errorf("canonicalize entry %d attributes: %w", index, err)
		}
		out.Write(encoded)
		out.WriteByte('}')
	}
	out.WriteString(`]}`)
	return out.Bytes(), nil
}

func PayloadHash(payload []byte) [32]byte { return sha256.Sum256(payload) }

type validator struct {
	fields []FieldError
	limit  int
}

func (v *validator) require(ok bool, path, reason string) {
	if !ok {
		v.add(path, reason)
	}
}

func (v *validator) add(path, reason string) {
	if v.limit <= 0 || len(v.fields) < v.limit {
		v.fields = append(v.fields, FieldError{Path: path, Reason: reason})
	}
}

func (v *validator) text(path, value string, maxBytes int, allowEmpty bool) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		v.add(path, "must be valid UTF-8 without NUL")
		return
	}
	if !allowEmpty && value == "" {
		v.add(path, "must not be empty")
	}
	if maxBytes > 0 && len(value) > maxBytes {
		v.add(path, "exceeds byte limit")
	}
}

func validLevel(level string) bool {
	switch level {
	case "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL":
		return true
	default:
		return false
	}
}

func canonicalJSON(value any, maxDepth int) ([]byte, error) {
	var out bytes.Buffer
	if err := appendCanonical(&out, value, 0, maxDepth); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonical(out *bytes.Buffer, value any, depth, maxDepth int) error {
	if depth > maxDepth {
		return errors.New("maximum JSON depth exceeded")
	}
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case string:
		writeQuoted(out, typed)
	case json.Number:
		number, err := canonicalNumber(typed.String())
		if err != nil {
			return err
		}
		out.WriteString(number)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return errors.New("non-finite JSON number")
		}
		out.WriteString(strconv.FormatFloat(typed, 'g', -1, 64))
	case float32:
		return appendCanonical(out, float64(typed), depth, maxDepth)
	case int:
		out.WriteString(strconv.Itoa(typed))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case int32:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case uint:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		out.WriteString(strconv.FormatUint(typed, 10))
	case uint32:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonical(out, item, depth+1, maxDepth); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeQuoted(out, key)
			out.WriteByte(':')
			if err := appendCanonical(out, typed[key], depth+1, maxDepth); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

func writeQuoted(out *bytes.Buffer, value string) {
	encoded, _ := json.Marshal(value)
	out.Write(encoded)
}

// canonicalNumber normalizes a JSON decimal without converting through
// float64, so large integer attributes cannot collide through precision loss.
func canonicalNumber(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty JSON number")
	}
	negative := raw[0] == '-'
	if negative {
		raw = raw[1:]
	}
	parts := strings.SplitN(raw, "e", 2)
	if len(parts) == 1 {
		parts = strings.SplitN(raw, "E", 2)
	}
	exponent := 0
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil || parsed < -100000 || parsed > 100000 {
			return "", errors.New("invalid JSON exponent")
		}
		exponent = parsed
	}
	mantissa := parts[0]
	dot := strings.IndexByte(mantissa, '.')
	if dot < 0 {
		dot = len(mantissa)
	} else if strings.Count(mantissa, ".") != 1 {
		return "", errors.New("invalid JSON number")
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", errors.New("invalid JSON number")
	}
	leadingZeroes := len(digits) - len(strings.TrimLeft(digits, "0"))
	decimalPosition := dot + exponent - leadingZeroes
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	for strings.HasSuffix(digits, "0") {
		digits = strings.TrimSuffix(digits, "0")
	}
	sign := ""
	if negative {
		sign = "-"
	}
	if decimalPosition > -6 && decimalPosition <= 21 {
		switch {
		case decimalPosition <= 0:
			return sign + "0." + strings.Repeat("0", -decimalPosition) + digits, nil
		case decimalPosition >= len(digits):
			return sign + digits + strings.Repeat("0", decimalPosition-len(digits)), nil
		default:
			return sign + digits[:decimalPosition] + "." + digits[decimalPosition:], nil
		}
	}
	coefficient := digits[:1]
	if len(digits) > 1 {
		coefficient += "." + digits[1:]
	}
	return sign + coefficient + "e" + strconv.Itoa(decimalPosition-1), nil
}
