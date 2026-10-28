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
	mu          sync.Mutex
	authID      string
	retries     int
	failures    int
	dispatched  bool
	exhausted   map[string]struct{}
	unavailable bool
	deferred    bool
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
	state := &credentialRetry{retries: cfg.SameAuthRetries}
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
	s.unavailable = false
	s.deferred = false
	req.pinnedAuthID = s.authID
}

func (s *credentialRetry) failed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authID == "" {
		return
	}
	// An unselectable credential has not used an extra attempt. Unless the host
	// supplied a cooldown deadline, move on rather than retrying a disabled auth.
	if !s.dispatched {
		s.retireLocked()
		return
	}
	s.failures++
	if s.failures > s.retries {
		s.retireLocked()
	}
}

// Each credential receives a fresh retry budget. Completed credentials stay
// excluded for the rest of this request, even after their cooldown expires.
func (s *credentialRetry) retireLocked() {
	if s.authID != "" {
		if s.exhausted == nil {
			s.exhausted = make(map[string]struct{})
		}
		s.exhausted[s.authID] = struct{}{}
	}
	s.authID = ""
	s.failures = 0
}

func (s *credentialRetry) noAlternative() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailable
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
	s.retireLocked()
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
	var selected *pluginapi.SchedulerAuthCandidate
	for i := range req.Candidates {
		candidate := &req.Candidates[i]
		_, exhausted := s.exhausted[candidate.ID]
		if candidate.ID == "" || exhausted || (s.authID != "" && candidate.ID != s.authID) {
			continue
		}
		if selected == nil || candidate.Priority > selected.Priority ||
			(candidate.Priority == selected.Priority && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if selected == nil {
		s.unavailable = s.authID == ""
		return okEnvelope(reject)
	}
	if s.authID == "" {
		s.authID = selected.ID
	}
	s.dispatched = true
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected.ID})
}

// The RPC error preserves model_cooldown as JSON text, but not its Go type or
// RetryAfter method. Read only the host's recovery fields; never log that text.
func (s *credentialRetry) cooldownDelay(req rpcExecutorRequest, err error, cfg pluginConfig, attempt int) (time.Duration, bool) {
	if s == nil || err == nil {
		return 0, false
	}
	s.mu.Lock()
	if s.authID == "" || s.dispatched {
		s.mu.Unlock()
		return 0, false
	}
	authID := s.authID
	s.mu.Unlock()
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
			if auth.ID == authID && !auth.Disabled {
				if remaining := time.Until(auth.NextRetryAfter); remaining > 0 {
					seconds = int64(remaining/time.Second) + 1
					knownCooldown = true
				}
				break
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
	pluginLog(req.HostCallbackID, "debug", fmt.Sprintf("model-retry-wrapper: waiting for pinned credential cooldown attempt=%d delay_ms=%d", attempt, durationMillis(delay)), map[string]any{
		"attempt": attempt, "delay_ms": durationMillis(delay), "credential_retry": true,
	})
	return waitRetryDelay(ctx, delay)
}
