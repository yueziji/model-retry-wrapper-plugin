//go:build cgo

package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type pluginLifecycleState struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	closed            bool
	streamTasks       sync.WaitGroup
	activeHostStreams map[string]struct{}
	activeRequests    map[string]*requestCancellation
	pendingCancels    map[string]time.Time
}

type requestCancellation struct {
	cancel context.CancelFunc
}

const (
	pendingCancelTTL  = 30 * time.Second
	maxPendingCancels = 1024
)

var pluginLifecycle = newPluginLifecycleState()

func newPluginLifecycleState() *pluginLifecycleState {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginLifecycleState{
		ctx:               ctx,
		cancel:            cancel,
		activeHostStreams: make(map[string]struct{}),
		activeRequests:    make(map[string]*requestCancellation),
		pendingCancels:    make(map[string]time.Time),
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
	s.activeRequests = make(map[string]*requestCancellation)
	s.pendingCancels = make(map[string]time.Time)
}

// registerRequest creates a per-executor context and, when a RequestID is
// available, makes it cancellable by a matching request.complete event.
// The returned cleanup is safe to call more than once.
func (s *pluginLifecycleState) registerRequest(parent context.Context, requestID string) (context.Context, func(), error) {
	if s == nil {
		return nil, func() {}, fmt.Errorf("plugin lifecycle is unavailable")
	}
	requestID = normalizeRequestID(requestID)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, func() {}, fmt.Errorf("plugin is shutting down")
	}
	if parent == nil {
		parent = s.ctx
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	entry := &requestCancellation{cancel: cancel}
	var previous *requestCancellation
	pendingCancel := false
	if requestID != "" {
		if s.activeRequests == nil {
			s.activeRequests = make(map[string]*requestCancellation)
		}
		if s.pendingCancels == nil {
			s.pendingCancels = make(map[string]time.Time)
		}
		now := time.Now()
		s.prunePendingCancelsLocked(now)
		if expiresAt, ok := s.pendingCancels[requestID]; ok {
			pendingCancel = now.Before(expiresAt)
			delete(s.pendingCancels, requestID)
		}
		previous = s.activeRequests[requestID]
		s.activeRequests[requestID] = entry
	}
	s.mu.Unlock()

	// Request IDs should be unique. If a malformed host reuses one, stop the
	// older operation rather than allowing two retries to share one key.
	if previous != nil {
		previous.cancel()
	}
	if pendingCancel {
		cancel()
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			s.mu.Lock()
			if requestID != "" && s.activeRequests[requestID] == entry {
				delete(s.activeRequests, requestID)
			}
			s.mu.Unlock()
			cancel()
		})
	}
	return ctx, cleanup, nil
}

// cancelRequest cancels only the executor associated with requestID. When
// remember is true and registration has not happened yet, a short-lived
// tombstone closes the cancellation-before-registration race.
func (s *pluginLifecycleState) cancelRequest(requestID string, remember bool) bool {
	if s == nil {
		return false
	}
	requestID = normalizeRequestID(requestID)
	if requestID == "" {
		return false
	}
	s.mu.Lock()
	entry := s.activeRequests[requestID]
	if entry == nil && remember {
		if s.pendingCancels == nil {
			s.pendingCancels = make(map[string]time.Time)
		}
		now := time.Now()
		s.prunePendingCancelsLocked(now)
		if len(s.pendingCancels) >= maxPendingCancels {
			for pendingID := range s.pendingCancels {
				delete(s.pendingCancels, pendingID)
				break
			}
		}
		s.pendingCancels[requestID] = now.Add(pendingCancelTTL)
	}
	s.mu.Unlock()
	if entry == nil || entry.cancel == nil {
		return remember
	}
	entry.cancel()
	return true
}

func (s *pluginLifecycleState) prunePendingCancelsLocked(now time.Time) {
	for requestID, expiresAt := range s.pendingCancels {
		if !now.Before(expiresAt) {
			delete(s.pendingCancels, requestID)
		}
	}
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
	requestCancels := make([]context.CancelFunc, 0, len(s.activeRequests))
	for _, entry := range s.activeRequests {
		if entry != nil && entry.cancel != nil {
			requestCancels = append(requestCancels, entry.cancel)
		}
	}
	s.activeRequests = make(map[string]*requestCancellation)
	s.pendingCancels = make(map[string]time.Time)
	s.mu.Unlock()

	for _, cancel := range requestCancels {
		cancel()
	}
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
