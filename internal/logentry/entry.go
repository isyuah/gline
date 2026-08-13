package logentry

import "time"

type LogEntry struct {
	Timestamp time.Time      `json:"timestamp"`
	Level     LogLevel       `json:"level"`
	Host      string         `json:"host"`
	Message   string         `json:"message"`
	Service   string         `json:"service"`
	Data      map[string]any `json:"data,omitempty"`
}
