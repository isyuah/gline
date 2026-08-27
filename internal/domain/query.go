package domain

import (
	"fmt"
	"strings"
	"time"
)

type EntryCursor struct {
	ObservedAt time.Time
	ID         EntryID
}

type EntryQuery struct {
	ProjectID ProjectID
	From      time.Time
	To        time.Time
	Services  []string
	Hosts     []string
	Levels    []string
	Message   string
	Cursor    *EntryCursor
	Limit     int
}

func (q EntryQuery) Validate(maxRange time.Duration, maxLimit int) error {
	if !q.ProjectID.Valid() {
		return fmt.Errorf("%w: query project", ErrInvalid)
	}
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) {
		return fmt.Errorf("%w: query time range", ErrInvalid)
	}
	if maxRange > 0 && q.To.Sub(q.From) > maxRange {
		return fmt.Errorf("%w: query range exceeds limit", ErrInvalid)
	}
	if q.Limit <= 0 || (maxLimit > 0 && q.Limit > maxLimit) {
		return fmt.Errorf("%w: query limit", ErrInvalid)
	}
	if q.Cursor != nil && (q.Cursor.ObservedAt.IsZero() || q.Cursor.ID <= 0) {
		return fmt.Errorf("%w: query cursor", ErrInvalid)
	}
	for _, values := range [][]string{q.Services, q.Hosts, q.Levels} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%w: empty query filter", ErrInvalid)
			}
		}
	}
	return nil
}

type EntryPage struct {
	Entries []Entry
	Next    *EntryCursor
}
