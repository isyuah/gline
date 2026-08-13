package config

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/isyuah/testx"
)

func Test_Parse(t *testing.T) {
	data := []byte(`
version: 1

agent:
  log:
    level: info
    file: ./agent.log

pipelines:
  - id: app
    service: app
    host: localhost
    source:
      type: file
      params:
        path: ./app.log
    parser:
      type: string_line
      params: {}

sender:
  type: tick_or_batch
  params:
    batch_size: 100
    flush_interval: 5s
  destination:
    type: terminal
    params: {}
`)

	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}

	testx.Assert(t, cfg.Version).Equal(1)
	testx.Assert(t, len(cfg.Pipelines)).Equal(1)
	testx.Assert(t, cfg.Pipelines[0].Source.Type).Equal("file")

	var params FileSourceParams
	if err := yaml.Unmarshal(cfg.Pipelines[0].Source.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Path != "./app.log" {
		t.Fatalf("Path = %q, want %q", params.Path, "./app.log")
	}
}

func Test_ParseRejectsUnknownField(t *testing.T) {
	data := []byte(`
version: 1
agent:
  log:
    level: info
    file: ./agent.log
    typo: value
`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("ParseConfig() error = nil, want unknown field error")
	}
}
