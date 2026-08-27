package domain

import "errors"

var (
	ErrInvalid             = errors.New("invalid domain value")
	ErrInvalidTransition   = errors.New("invalid state transition")
	ErrProjectDisabled     = errors.New("project is disabled")
	ErrScopeDenied         = errors.New("scope denied")
	ErrIdempotencyConflict = errors.New("batch idempotency conflict")
)
