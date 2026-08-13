package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/isyuah/gline/internal/agent/build"
	"github.com/isyuah/gline/internal/agent/config"
)

func main() {
	file, err := os.Open(".glineconf")
	if err != nil {
		fmt.Printf("Error opening .glineconf: %s\n", err)
		os.Exit(1)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		fmt.Printf("Error reading .glineconf: %s\n", err)
		os.Exit(1)
	}

	cfg, err := config.ParseConfig(data)
	if err != nil {
		fmt.Printf("Error parsing .glineconf: %s\n", err)
		os.Exit(1)
	}
	ag, err := build.Agent(cfg)
	if err != nil {
		fmt.Printf("Error building agent: %s\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := ag.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Printf("Error running agent: %s\n", err)
		os.Exit(1)
	}
}
