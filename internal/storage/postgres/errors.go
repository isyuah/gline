package postgres

import (
	"database/sql"
	"errors"
)

type sqlStateError interface {
	SQLState() string
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var state sqlStateError
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505", "23503", "23514", "23P01":
			return errors.Join(ErrConflict, err)
		}
	}
	return err
}
