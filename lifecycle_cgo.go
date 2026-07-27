//go:build cgo

package main

import (
	"context"
	"fmt"
	"sync"
)

type pluginLifecycleState struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	closed            bool
	streamTasks       sync.WaitGroup
	activeHostStreams map[string]struct{}
}

var pluginLifecycle = newPluginLifecycleState()

func newPluginLifecycleState() *pluginLifecycleState {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginLifecycleState{
		ctx:               ctx,
		cancel:            cancel,
		activeHostStreams: make(map[string]struct{}),
	}
}

func (s *pluginLifecycleState) reopen() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.closed = false
	s.streamTasks = sync.WaitGroup{}
	s.activeHostStreams = make(map[string]struct{})
}

func (s *pluginLifecycleState) context() (context.Context, error) {
	if s == nil {
		return nil, fmt.Errorf("plugin lifecycle is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("plugin is shutting down")
	}
	return s.ctx, nil
}

func (s *pluginLifecycleState) beginStreamTask() (context.Context, error) {
	if s == nil {
		return nil, fmt.Errorf("plugin lifecycle is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("plugin is shutting down")
	}
	s.streamTasks.Add(1)
	return s.ctx, nil
}

func (s *pluginLifecycleState) endStreamTask() {
	if s != nil {
		s.streamTasks.Done()
	}
}

func (s *pluginLifecycleState) trackHostStream(streamID string) error {
	if s == nil || streamID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("plugin is shutting down")
	}
	s.activeHostStreams[streamID] = struct{}{}
	return nil
}

func (s *pluginLifecycleState) untrackHostStream(streamID string) {
	if s == nil || streamID == "" {
		return
	}
	s.mu.Lock()
	delete(s.activeHostStreams, streamID)
	s.mu.Unlock()
}

func (s *pluginLifecycleState) shutdown(closeHostStream func(string) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.streamTasks.Wait()
		return
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	streamIDs := make([]string, 0, len(s.activeHostStreams))
	for streamID := range s.activeHostStreams {
		streamIDs = append(streamIDs, streamID)
	}
	s.mu.Unlock()

	for _, streamID := range streamIDs {
		if closeHostStream != nil {
			_ = closeHostStream(streamID)
		}
	}
	s.streamTasks.Wait()
}

func retryContext(parent context.Context, cfg pluginConfig) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if cfg.MaxElapsedMS == 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, durationFromMillis(cfg.MaxElapsedMS))
}
