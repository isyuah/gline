package domain

import "fmt"

type ProjectID string
type APIKeyID string
type AgentID string
type PipelineID string
type BatchID string
type QuarantineID string
type EntryID int64
type AuditID int64

func (id ProjectID) Valid() bool    { return validUUID(string(id)) }
func (id APIKeyID) Valid() bool     { return validUUID(string(id)) }
func (id AgentID) Valid() bool      { return validUUID(string(id)) }
func (id PipelineID) Valid() bool   { return validUUID(string(id)) }
func (id BatchID) Valid() bool      { return validUUID(string(id)) }
func (id QuarantineID) Valid() bool { return validUUID(string(id)) }

func ValidateProjectID(id ProjectID) error {
	if !id.Valid() {
		return fmt.Errorf("%w: project id", ErrInvalid)
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := range value {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			c := value[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
