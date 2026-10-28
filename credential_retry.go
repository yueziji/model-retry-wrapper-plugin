//go:build cgo

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const credentialSequenceHeader = "X-Model-Retry-Wrapper-Credential-Sequence"

// Only live wrapper requests participate in scheduling. The opaque token keeps
// concurrent requests independent, including callbacks through other wrappers.
var credentialSequences sync.Map

type credentialRetry struct {
	mu         sync.Mutex
	authID     string
	retries    int
	failures   int
	dispatched bool
	exhausted  map[string]struct{}
	excluded   map[string]struct{}
	candidates map[string]struct{}
	round      int
	newRound   bool
	deferred   bool
}

func credentialRetryEnabled(cfg pluginConfig) bool {
	if !cfg.Enabled {
		return false
	}
	for _, model := range cfg.Models {
		if retryConfigForModel(cfg, model).SameAuthRetries > 0 {
			return true
		}
	}
	return false
}

func beginCredentialRetry(req *rpcExecutorRequest, cfg pluginConfig) (*credentialRetry, func()) {
	if cfg.SameAuthRetries == 0 {
		return nil, func() {}
	}
	req.credentialSequence = rand.Text()
	state := &credentialRetry{retries: cfg.SameAuthRetries, round: 1}
	credentialSequences.Store(req.credentialSequence, state)
	return state, func() { credentialSequences.Delete(req.credentialSequence) }
}

func credentialRequestHeaders(req rpcExecutorRequest) http.Header {
	headers := nestedRequestHeaders(req.Headers)
	for key := range headers {
		if strings.EqualFold(key, credentialSequenceHeader) {
			delete(headers, key)
		}
	}
	if req.credentialSequence != "" {
		headers.Set(credentialSequenceHeader, req.credentialSequence)
	}
	return headers
}

func (s *credentialRetry) prepare(req *rpcExecutorRequest) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatched = false
	s.newRound = false
	s.deferred = false
	req.pinnedAuthID = s.authID
}

func (s *credentialRetry) failed(retryable bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authID == "" {
		return
	}
	if !s.dispatched || !retryable {
		s.retireLocked(false)
		return
	}
	s.failures++
	if s.failures > s.retries {
		s.retireLocked(true)
	}
}

// Only matching failures exhaust a round's budget. Other retirements remain
// excluded throughout this request, including after a round change.
func (s *credentialRetry) retireLocked(exhausted bool) {
	if s.authID != "" {
		if exhausted {
			if s.exhausted == nil {
				s.exhausted = make(map[string]struct{})
			}
			s.exhausted[s.authID] = struct{}{}
		} else {
			if s.excluded == nil {
				s.excluded = make(map[string]struct{})
			}
			s.excluded[s.authID] = struct{}{}
		}
	}
	s.authID = ""
	s.failures = 0
}

// Empty selections and cooldown checks never count as dispatched attempts.
func (s *credentialRetry) selectionFailure(ctx context.Context, req rpcExecutorRequest, err error, cfg pluginConfig, attempt int) (bool, error) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	dispatched, newRound, round := s.dispatched, s.newRound, s.round
	s.mu.Unlock()
	if newRound {
		pluginLog(req.HostCallbackID, "debug", fmt.Sprintf("model-retry-wrapper: starting credential retry round round=%d attempt=%d", round, attempt), map[string]any{
			"round": round, "attempt": attempt, "credential_retry": true,
		})
	}
	if dispatched || err == nil {
		return false, nil
	}
	if delay, cooling := s.cooldownDelay(req, err, cfg, attempt); cooling {
		return true, waitCredentialCooldown(ctx, req, delay, attempt)
	}
	s.mu.Lock()
	pinned := s.authID != ""
	if pinned {
		s.retireLocked(false)
	}
	s.mu.Unlock()
	if pinned {
		pluginLog(req.HostCallbackID, "debug", "model-retry-wrapper: releasing unavailable credential without cooldown", map[string]any{"attempt": attempt, "credential_retry": true})
		return true, nil
	}
	pluginLog(req.HostCallbackID, "warn", "model-retry-wrapper: credential selection stopped stop_reason=no_available_credentials", map[string]any{"attempt": attempt - 1, "credential_retry": true})
	return true, err
}

// Keep CPA's native failover for errors outside the wrapper's retry rules.
// A rejected second pick tells us CPA wanted to try another credential.
func (s *credentialRetry) releaseForFailover(cfg pluginConfig, attempt int) bool {
	if s == nil || (cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deferred || s.authID == "" {
		return false
	}
	s.retireLocked(false)
	return true
}

func pickRetryCredential(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var token string
	for key, values := range req.Options.Headers {
		if strings.EqualFold(key, credentialSequenceHeader) && len(values) > 0 {
			token = values[0]
			break
		}
	}
	value, exists := credentialSequences.Load(token)
	if !exists || token == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{})
	}
	s := value.(*credentialRetry)
	s.mu.Lock()
	defer s.mu.Unlock()
	reject := pluginapi.SchedulerPickResponse{
		Handled: true, Reject: true, RejectCode: "auth_unavailable",
		RejectReason: "credential retry wrapper deferred credential failover",
	}
	if s.dispatched {
		// CPA may ask again inside this host call or in another native retry
		// round. Return control to the outer loop before it can switch auths.
		s.deferred = true
		return okEnvelope(reject)
	}
	if s.candidates == nil {
		s.candidates = make(map[string]struct{})
	}
	for _, candidate := range req.Candidates {
		if candidate.ID != "" {
			s.candidates[candidate.ID] = struct{}{}
		}
	}
	selected := s.selectLocked(req.Candidates, false)
	if selected == nil && s.authID == "" && len(s.exhausted) > 0 {
		// Re-evaluate only candidates CPA currently offers. A cooling unused
		// credential must not hold up an available credential's next round.
		selected = s.selectLocked(req.Candidates, true)
		if selected != nil {
			s.exhausted = nil
			s.round++
			s.newRound = true
		}
	}
	if selected == nil {
		return okEnvelope(reject)
	}
	if s.authID == "" {
		s.authID = selected.ID
	}
	s.dispatched = true
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected.ID})
}

func (s *credentialRetry) selectLocked(candidates []pluginapi.SchedulerAuthCandidate, nextRound bool) *pluginapi.SchedulerAuthCandidate {
	var selected *pluginapi.SchedulerAuthCandidate
	for i := range candidates {
		candidate := &candidates[i]
		_, exhausted := s.exhausted[candidate.ID]
		_, excluded := s.excluded[candidate.ID]
		if candidate.ID == "" || excluded || (exhausted && !nextRound) || (s.authID != "" && candidate.ID != s.authID) {
			continue
		}
		if selected == nil || candidate.Priority > selected.Priority ||
			(candidate.Priority == selected.Priority && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	return selected
}

// The RPC error preserves model_cooldown as JSON text, but not its Go type or
// RetryAfter method. Read only the host's recovery fields; never log that text.
func (s *credentialRetry) cooldownDelay(req rpcExecutorRequest, err error, cfg pluginConfig, attempt int) (time.Duration, bool) {
	if s == nil || err == nil {
		return 0, false
	}
	s.mu.Lock()
	if s.dispatched {
		s.mu.Unlock()
		return 0, false
	}
	authID := s.authID
	eligible := make(map[string]struct{})
	for id := range s.candidates {
		if _, excluded := s.excluded[id]; !excluded {
			eligible[id] = struct{}{}
		}
	}
	allExcluded := len(s.candidates) > 0 && len(eligible) == 0
	s.mu.Unlock()
	if allExcluded {
		return 0, false
	}
	message := err.Error()
	start := strings.IndexByte(message, '{')
	var detail struct {
		Error struct {
			Code         string `json:"code"`
			ResetSeconds int64  `json:"reset_seconds"`
		} `json:"error"`
	}
	knownCooldown := start >= 0 && json.Unmarshal([]byte(message[start:]), &detail) == nil && detail.Error.Code == "model_cooldown"
	seconds := detail.Error.ResetSeconds
	if !knownCooldown {
		// Credential-level cooldowns may be returned as auth_unavailable,
		// whose RPC text has no recovery hint. Read only runtime metadata.
		if !strings.Contains(message, "auth_unavailable") && !strings.Contains(message, "auth_not_found") {
			return 0, false
		}
		raw, errList := callHost(pluginabi.MethodHostAuthList, struct {
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}{req.HostCallbackID})
		var list struct {
			Files []struct {
				ID             string    `json:"id"`
				Disabled       bool      `json:"disabled"`
				NextRetryAfter time.Time `json:"next_retry_after"`
			} `json:"files"`
		}
		if errList != nil || json.Unmarshal(raw, &list) != nil {
			return 0, false
		}
		for _, auth := range list.Files {
			_, related := eligible[auth.ID]
			if !auth.Disabled && ((authID != "" && auth.ID == authID) || (authID == "" && related)) {
				if remaining := time.Until(auth.NextRetryAfter); remaining > 0 {
					until := int64(remaining/time.Second) + 1
					if !knownCooldown || until < seconds {
						seconds = until
					}
					knownCooldown = true
				}
			}
		}
		if !knownCooldown {
			return 0, false
		}
	}
	if seconds < 1 {
		seconds = 1
	}
	// Poll at most once per max_delay_ms while retaining the same attempt
	// number. A model cooldown must not consume the credential retry budget.
	delay := time.Duration(cfg.MaxDelayMS) * time.Millisecond
	if seconds <= int64(delay/time.Second) {
		delay = time.Duration(seconds) * time.Second
	}
	if backoff := retryDelay(cfg, attempt); backoff > delay {
		delay = backoff
	}
	return delay, true
}

func waitCredentialCooldown(ctx context.Context, req rpcExecutorRequest, delay time.Duration, attempt int) error {
	pluginLog(req.HostCallbackID, "debug", fmt.Sprintf("model-retry-wrapper: waiting for credential cooldown attempt=%d delay_ms=%d stop_reason=model_cooldown", attempt, durationMillis(delay)), map[string]any{
		"attempt": attempt, "delay_ms": durationMillis(delay), "credential_retry": true,
	})
	return waitRetryDelay(ctx, delay)
}
