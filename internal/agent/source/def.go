package source

import (
	"context"

	"github.com/isyuah/gline/internal/agent/agenterr"
)

type Source interface {
	NextRecord(ctx context.Context) (RawRecord, error)
}

type SourceError struct {
	Err  error
	Kind agenterr.ErrorKind
}

func FromError(err error, kind agenterr.ErrorKind) *SourceError {
	return &SourceError{err, kind}
}
func FromErrorTemp(err error) *SourceError {
	return &SourceError{err, agenterr.ErrorKindTemporary}
}
func FromErrorFatal(err error) *SourceError {
	return &SourceError{err, agenterr.ErrorKindFatal}
}

func (e *SourceError) Error() string {
	return e.Err.Error()
}
func (e *SourceError) Unwrap() error {
	return e.Err
}
