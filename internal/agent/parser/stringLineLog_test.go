package parser

import (
	"testing"

	"github.com/isyuah/gline/internal/agent/source"
	"github.com/isyuah/gline/internal/logentry"
)

func TestStringLineLogParser_Parse(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantLevel   logentry.LogLevel
		wantMessage string
	}{
		{"info1", "INFO user login", logentry.LevelInfo, "user login"},
		{"info2", "INFO INFO something", logentry.LevelInfo, "INFO something"},
		{"debug", "DEBUG http access", logentry.LevelDebug, "http access"},
		{"unknown", "without message", logentry.LevelUnknown, "without message"},
	}
	p := NewStringLineLogParser()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := p.Parse(source.RawRecord{Content: c.input})
			if err != nil {
				t.Fatal(err)
			}
			if got.Level != c.wantLevel || got.Message != c.wantMessage {
				t.Errorf("want %s, got %s, for %s", c.wantMessage, got.Message, c.input)
			}
		})
	}
}
