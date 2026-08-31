package reliable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type PipelineControl struct {
	ID            string `json:"id"`
	DesiredStatus string `json:"desired_status"`
	ConfigVersion int64  `json:"config_version"`
}

type ControlSnapshot struct {
	Pipelines []PipelineControl `json:"pipelines"`
}

// PipelineState reconciles local Pipeline configuration with the Server's
// desired state. Configuration changes are version-gated: the Agent stops
// reading until its local config_version matches the Server.
type PipelineState struct {
	mu sync.Mutex

	order        []string
	localVersion map[string]int64
	desired      map[string]PipelineControl
	controlKnown bool
	active       map[string]bool
	failures     map[string]string
	changed      chan struct{}
}

func NewPipelineState(pipelines []Pipeline) (*PipelineState, error) {
	state := &PipelineState{
		order: make([]string, 0, len(pipelines)), localVersion: make(map[string]int64, len(pipelines)),
		desired: make(map[string]PipelineControl), active: make(map[string]bool, len(pipelines)),
		failures: make(map[string]string, len(pipelines)), changed: make(chan struct{}),
	}
	for _, pipeline := range pipelines {
		id := strings.TrimSpace(pipeline.ID)
		if id == "" || pipeline.ConfigVersion <= 0 {
			return nil, errors.New("pipeline state requires an id and positive config version")
		}
		if _, exists := state.localVersion[id]; exists {
			return nil, fmt.Errorf("pipeline state contains duplicate id %q", id)
		}
		state.order = append(state.order, id)
		state.localVersion[id] = pipeline.ConfigVersion
	}
	return state, nil
}

func (s *PipelineState) Apply(snapshot ControlSnapshot) error {
	desired := make(map[string]PipelineControl, len(snapshot.Pipelines))
	for _, pipeline := range snapshot.Pipelines {
		pipeline.ID = strings.TrimSpace(pipeline.ID)
		if pipeline.ID == "" || pipeline.ConfigVersion <= 0 || !validDesiredStatus(pipeline.DesiredStatus) {
			return errors.New("heartbeat control contains an invalid pipeline")
		}
		if _, exists := desired[pipeline.ID]; exists {
			return fmt.Errorf("heartbeat control contains duplicate pipeline %q", pipeline.ID)
		}
		desired[pipeline.ID] = pipeline
	}

	s.mu.Lock()
	s.desired = desired
	s.controlKnown = true
	s.signalLocked()
	s.mu.Unlock()
	return nil
}

func (s *PipelineState) Start(id string) {
	s.mu.Lock()
	s.active[id] = true
	delete(s.failures, id)
	s.signalLocked()
	s.mu.Unlock()
}

func (s *PipelineState) Stop(id string) {
	s.mu.Lock()
	s.active[id] = false
	s.signalLocked()
	s.mu.Unlock()
}

func (s *PipelineState) Fail(id string, err error) {
	s.mu.Lock()
	s.active[id] = false
	s.failures[id] = boundedError(err)
	s.signalLocked()
	s.mu.Unlock()
}

func (s *PipelineState) WaitUntilRunnable(ctx context.Context, id string) error {
	for {
		s.mu.Lock()
		if _, exists := s.localVersion[id]; !exists {
			s.mu.Unlock()
			return fmt.Errorf("unknown local pipeline %q", id)
		}
		if s.runnableLocked(id) {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *PipelineState) Reports() []HeartbeatPipeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	reports := make([]HeartbeatPipeline, 0, len(s.order))
	for _, id := range s.order {
		status, message := s.reportLocked(id)
		report := HeartbeatPipeline{ID: id, ConfigVersion: s.localVersion[id], Status: status}
		if message != "" {
			report.LastError = &message
		}
		reports = append(reports, report)
	}
	return reports
}

func (s *PipelineState) runnableLocked(id string) bool {
	if s.failures[id] != "" {
		return false
	}
	if !s.controlKnown {
		return true
	}
	control, exists := s.desired[id]
	return exists && control.DesiredStatus == "enabled" && control.ConfigVersion == s.localVersion[id]
}

func (s *PipelineState) reportLocked(id string) (string, string) {
	if failure := s.failures[id]; failure != "" {
		return "error", failure
	}
	if s.controlKnown {
		control, exists := s.desired[id]
		if !exists {
			return "error", "pipeline is not registered for this Agent"
		}
		if control.ConfigVersion != s.localVersion[id] {
			return "error", fmt.Sprintf("config version mismatch: local=%d server=%d", s.localVersion[id], control.ConfigVersion)
		}
		if control.DesiredStatus == "error" {
			return "error", "server marked pipeline as errored"
		}
		if control.DesiredStatus != "enabled" {
			return "stopped", ""
		}
	}
	if s.active[id] {
		return "running", ""
	}
	return "stopped", ""
}

func (s *PipelineState) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func validDesiredStatus(status string) bool {
	switch status {
	case "enabled", "paused", "error", "disabled":
		return true
	default:
		return false
	}
}

func boundedError(err error) string {
	if err == nil {
		return "pipeline failed"
	}
	message := err.Error()
	if len(message) > 2_048 {
		message = message[:2_048]
	}
	return message
}
