package reliable

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPipelineStateReconcilesDesiredStatusAndConfigVersion(t *testing.T) {
	state, err := NewPipelineState([]Pipeline{{ID: "pipe-1", ConfigVersion: 1}})
	if err != nil {
		t.Fatal(err)
	}
	state.Start("pipe-1")
	if report := state.Reports()[0]; report.Status != "running" {
		t.Fatalf("initial report = %+v", report)
	}

	if err := state.Apply(ControlSnapshot{Pipelines: []PipelineControl{{ID: "pipe-1", DesiredStatus: "paused", ConfigVersion: 1}}}); err != nil {
		t.Fatal(err)
	}
	if report := state.Reports()[0]; report.Status != "stopped" || report.LastError != nil {
		t.Fatalf("paused report = %+v", report)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := state.WaitUntilRunnable(waitCtx, "pipe-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paused wait error = %v", err)
	}

	if err := state.Apply(ControlSnapshot{Pipelines: []PipelineControl{{ID: "pipe-1", DesiredStatus: "enabled", ConfigVersion: 2}}}); err != nil {
		t.Fatal(err)
	}
	if report := state.Reports()[0]; report.Status != "error" || report.LastError == nil {
		t.Fatalf("version mismatch report = %+v", report)
	}

	if err := state.Apply(ControlSnapshot{Pipelines: []PipelineControl{{ID: "pipe-1", DesiredStatus: "enabled", ConfigVersion: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.WaitUntilRunnable(t.Context(), "pipe-1"); err != nil {
		t.Fatal(err)
	}
	if report := state.Reports()[0]; report.Status != "running" {
		t.Fatalf("resumed report = %+v", report)
	}
}

func TestPipelineStateReportsMissingServerRegistration(t *testing.T) {
	state, _ := NewPipelineState([]Pipeline{{ID: "pipe-1", ConfigVersion: 1}})
	state.Start("pipe-1")
	if err := state.Apply(ControlSnapshot{}); err != nil {
		t.Fatal(err)
	}
	report := state.Reports()[0]
	if report.Status != "error" || report.LastError == nil {
		t.Fatalf("missing registration report = %+v", report)
	}
}
