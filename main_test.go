package main

import (
	"net/http"
	"testing"
	"time"
)

func TestShouldRouteMatchesConfiguredModelAlias(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Models = []string{"retry-codex-gpt-5.5"}

	if !shouldRoute(cfg, "openai", "retry-codex-gpt-5.5") {
		t.Fatal("expected configured alias to route")
	}
	if shouldRoute(cfg, "openai", "gpt-5.5") {
		t.Fatal("expected unconfigured model to bypass")
	}
}

func TestShouldRouteRespectsSourceFormatFilter(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Models = []string{"retry-claude-sonnet"}
	cfg.SourceFormats = []string{"claude"}

	if !shouldRoute(cfg, "claude", "retry-claude-sonnet") {
		t.Fatal("expected claude request to route")
	}
	if shouldRoute(cfg, "openai", "retry-claude-sonnet") {
		t.Fatal("expected openai request to bypass")
	}
}

func TestRetryAttemptHonorsConfiguredStatusAndMaxAttempts(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.StatusCodes = []int{http.StatusBadGateway}
	cfg.MaxAttempts = 3

	if !shouldRetryAttempt(cfg, 1, http.StatusBadGateway) {
		t.Fatal("expected first failed attempt to retry")
	}
	if !shouldRetryAttempt(cfg, 2, http.StatusBadGateway) {
		t.Fatal("expected second failed attempt to retry")
	}
	if shouldRetryAttempt(cfg, 3, http.StatusBadGateway) {
		t.Fatal("expected max attempts to stop retry")
	}
	if shouldRetryAttempt(cfg, 1, http.StatusBadRequest) {
		t.Fatal("expected non-configured status to bypass retry")
	}
}

func TestRetryDelayCapsExponentialBackoff(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.InitialDelayMS = 500
	cfg.MaxDelayMS = 1200

	if got := retryDelay(cfg, 1); got != 500*time.Millisecond {
		t.Fatalf("attempt 1 delay = %v, want 500ms", got)
	}
	if got := retryDelay(cfg, 2); got != time.Second {
		t.Fatalf("attempt 2 delay = %v, want 1s", got)
	}
	if got := retryDelay(cfg, 3); got != 1200*time.Millisecond {
		t.Fatalf("attempt 3 delay = %v, want capped 1.2s", got)
	}
}

func TestStatusFromErrorParsesStatusText(t *testing.T) {
	err := retryStatusError{status: http.StatusTooManyRequests}
	if got := statusFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("statusFromError(retryStatusError) = %d, want 429", got)
	}

	wrapped := retryStatusError{status: 0, err: err}
	if got := statusFromError(wrapped); got != http.StatusTooManyRequests {
		t.Fatalf("statusFromError(text wrapper) = %d, want 429", got)
	}
}
