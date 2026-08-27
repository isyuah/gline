package ingestv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testProjectID = "11111111-1111-1111-1111-111111111111"

func TestDecodeRejectsUnknownAndTrailingJSON(t *testing.T) {
	valid := `{"protocol_version":1,"batch_id":"44444444-4444-4444-4444-444444444444","agent_id":"22222222-2222-2222-2222-222222222222","pipeline_id":"33333333-3333-3333-3333-333333333333","sequence":0,"sent_at":"2026-08-24T00:00:00Z","entries":[{"sequence":0,"observed_at":"2026-08-24T00:00:00Z","level":"info","service":"api","host":"node","message":"ok","attributes":{}}]}`
	if _, _, err := Decode(strings.NewReader(strings.Replace(valid, `"sequence":0`, `"unknown":true,"sequence":0`, 1)), 1<<20); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, _, err := Decode(strings.NewReader(valid+` {}`), 1<<20); !errors.Is(err, ErrTrailingJSON) {
		t.Fatalf("trailing value error = %v", err)
	}
	if _, _, err := Decode(strings.NewReader(valid), 8); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("body limit error = %v", err)
	}
}

func TestNormalizeCanonicalizesObjectKeysAndNumbers(t *testing.T) {
	first := decodeForTest(t, `{"b":1.0,"nested":{"z":2,"a":0.0010}}`)
	second := decodeForTest(t, `{"nested":{"a":1e-3,"z":2.0},"b":1}`)
	batchOne, err := Normalize(first, testProjectID, 100, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	batchTwo, err := Normalize(second, testProjectID, 100, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if batchOne.PayloadHash != batchTwo.PayloadHash {
		t.Fatalf("semantically equal payload hashes differ: %x != %x", batchOne.PayloadHash, batchTwo.PayloadHash)
	}
	if batchOne.Entries[0].Level != "INFO" || batchOne.Entries[0].ProjectID != testProjectID {
		t.Fatalf("normalization did not bind server-owned fields: %+v", batchOne.Entries[0])
	}
}

func TestNormalizeReportsBoundedFieldErrors(t *testing.T) {
	request := decodeForTest(t, `{}`)
	request.BatchID = "not-a-uuid"
	request.Entries[0].Sequence = 7
	limits := DefaultLimits()
	limits.MaxFieldErrors = 2
	_, err := Normalize(request, testProjectID, 100, limits)
	var validation *ValidationError
	if !errors.As(err, &validation) || len(validation.Fields) != 2 {
		t.Fatalf("validation error = %#v", err)
	}
}

func decodeForTest(t *testing.T, attributes string) BatchRequest {
	t.Helper()
	if attributes == `{}` {
		attributes = `{}`
	}
	raw := `{"protocol_version":1,"batch_id":"44444444-4444-4444-4444-444444444444","agent_id":"22222222-2222-2222-2222-222222222222","pipeline_id":"33333333-3333-3333-3333-333333333333","sequence":0,"sent_at":"2026-08-24T00:00:00Z","entries":[{"sequence":0,"observed_at":"2026-08-24T00:00:00Z","level":"info","service":"api","host":"node","message":"ok","attributes":` + attributes + `}]}`
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	var request BatchRequest
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	return request
}
