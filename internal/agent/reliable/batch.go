package reliable

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/isyuah/gline/internal/logentry"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
)

func buildBatch(agentID, pipelineID string, sequence int64, entries []logentry.LogEntry, now time.Time) (string, []byte, error) {
	if len(entries) == 0 {
		return "", nil, errors.New("reliable batch is empty")
	}
	batchID, err := newUUID()
	if err != nil {
		return "", nil, err
	}
	wireEntries := make([]ingestv1.Entry, len(entries))
	for index, entry := range entries {
		level, attributes := wireLevel(entry.Level, entry.Data)
		wireEntries[index] = ingestv1.Entry{
			Sequence: index, ObservedAt: entry.Timestamp.UTC(), Level: level,
			Service: entry.Service, Host: entry.Host, Message: entry.Message,
			Attributes: attributes,
		}
	}
	request := ingestv1.BatchRequest{
		ProtocolVersion: ingestv1.Version, BatchID: batchID, AgentID: agentID,
		PipelineID: pipelineID, Sequence: sequence, SentAt: now.UTC(), Entries: wireEntries,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return "", nil, fmt.Errorf("encode reliable batch: %w", err)
	}
	return batchID, payload, nil
}

func wireLevel(level logentry.LogLevel, data map[string]any) (string, map[string]any) {
	value := strings.ToUpper(string(level))
	switch value {
	case "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL":
		return value, cloneAttributes(data)
	default:
		attributes := cloneAttributes(data)
		attributes["gline.original_level"] = value
		return "INFO", attributes
	}
}

func cloneAttributes(source map[string]any) map[string]any {
	if len(source) == 0 {
		return map[string]any{}
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate batch ID: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded), nil
}
