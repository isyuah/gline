package source

import "time"

type RawRecord struct {
	ObservedAt time.Time
	Content    string

	// Durable position fields are populated by DurableFileSource. They stay
	// empty for legacy sources so the existing in-memory pipeline remains
	// compatible.
	SourceKey    string
	FileIdentity string
	EndOffset    int64
}
