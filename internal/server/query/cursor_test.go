package query

import (
	"errors"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

func TestCursorRejectsTamperingAndAnotherProject(t *testing.T) {
	codec, err := NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	hash := [32]byte{1}
	raw, err := codec.Encode(queryProjectID, hash, domain.EntryCursor{ObservedAt: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), ID: 10})
	if err != nil {
		t.Fatal(err)
	}
	tampered := raw[:len(raw)-1] + "A"
	if _, err := codec.Decode(tampered, queryProjectID, hash); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	if _, err := codec.Decode(raw, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", hash); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-project cursor error = %v", err)
	}
}
