package source

import "time"

type RawRecord struct {
	ObservedAt time.Time
	Content    string
}
