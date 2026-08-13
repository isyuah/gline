package logentry

type LogLevel string

const (
	LevelTrace   = "TRACE"
	LevelDebug   = "DEBUG"
	LevelInfo    = "INFO"
	LevelWarn    = "WARN"
	LevelError   = "ERROR"
	LevelFatal   = "FATAL"
	LevelUnknown = "UNKNOWN"
)
