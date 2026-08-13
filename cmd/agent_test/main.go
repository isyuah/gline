package main

import (
	"context"
	"fmt"

	"github.com/isyuah/gline/internal/agent"
	"github.com/isyuah/gline/internal/agent/destination"
	"github.com/isyuah/gline/internal/agent/parser"
	"github.com/isyuah/gline/internal/agent/sender"
	"github.com/isyuah/gline/internal/agent/source"
)

func main() {
	ctx := context.Background()

	src, err := source.NewFileSource("./test.log")
	if err != nil {
		panic(err)
	}
	defer src.Close()

	p := agent.SourcePipeline{
		Source:  src,
		Parser:  parser.NewStringLineLogParser(),
		Service: "test-service",
		Host:    "localhost",
	}

	ag := agent.Agent{
		Pipelines: []agent.SourcePipeline{p},
		Sender:    sender.NewTickOrBatchSender(destination.NewTerminalDestination(), sender.TickOrBatchSenderOptions{}),
	}

	if err := ag.Run(ctx); err != nil {
		fmt.Println(err)
	}
}
