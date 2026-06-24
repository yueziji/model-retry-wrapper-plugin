package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
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

func TestDefaultConfigUsesUnboundedRetryAttempts(t *testing.T) {
	cfg := defaultPluginConfig()
	if cfg.MaxAttempts != 0 {
		t.Fatalf("MaxAttempts = %d, want 0", cfg.MaxAttempts)
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

func TestShouldRouteMatchesOpenAIResponseSourceFormatAlias(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
enabled: true
models:
  - gpt-5.5-ly
source_formats:
  - responses
`))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if !shouldRoute(cfg, "openai-response", "gpt-5.5-ly") {
		t.Fatal("expected responses alias to match openai-response requests")
	}
}

func TestPluginRegistrationSupportsOpenAIResponsesExecutorFormat(t *testing.T) {
	formats := supportedExecutorFormats()
	if !stringListContains(formats, "openai-response") {
		t.Fatalf("supportedExecutorFormats() = %#v, want openai-response", formats)
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

func TestRetryFailureUsesConfiguredKeywordWhenStatusMissing(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.MaxAttempts = 3
	err := errors.New(`host_call_failed: {"error":{"message":"rate_limited (request id: test)","type":"new_api_error","code":"rate_limited"}}`)

	shouldRetry, keyword := shouldRetryFailure(cfg, 1, 0, err)
	if !shouldRetry {
		t.Fatal("expected keyword fallback to retry")
	}
	if keyword != "rate_limited" {
		t.Fatalf("keyword = %q, want rate_limited", keyword)
	}

	shouldRetry, _ = shouldRetryFailure(cfg, 3, 0, err)
	if shouldRetry {
		t.Fatal("expected max attempts to stop keyword fallback retry")
	}

	shouldRetry, keyword = shouldRetryFailure(cfg, 1, http.StatusBadRequest, err)
	if shouldRetry || keyword != "" {
		t.Fatalf("status-coded failure retry = %v, keyword = %q; want false, empty", shouldRetry, keyword)
	}

	cfg.RetryKeywords = []string{"overloaded"}
	shouldRetry, keyword = shouldRetryFailure(cfg, 1, 0, err)
	if shouldRetry || keyword != "" {
		t.Fatalf("non-matching keyword fallback retry = %v, keyword = %q; want false, empty", shouldRetry, keyword)
	}
}

func TestDecodeConfigAcceptsStringStatusCodes(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
enabled: true
initial_delay_ms: 500
max_delay_ms: 4000
max_attempts: 0
models:
  - gpt-5.5-any
  - gpt-5.5-ly
status_codes:
  - "408"
  - "429"
  - "500"
  - "502"
  - "503"
  - "504"
`))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if want := []int{408, 429, 500, 502, 503, 504}; !reflect.DeepEqual([]int(cfg.StatusCodes), want) {
		t.Fatalf("StatusCodes = %#v, want %#v", []int(cfg.StatusCodes), want)
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

func TestWaitRetryDelayWithProbeStopsOnProbeError(t *testing.T) {
	want := errors.New("stream closed")
	calls := 0
	err := waitRetryDelayWithProbe(context.Background(), time.Millisecond, func() error {
		calls++
		if calls > 1 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("waitRetryDelayWithProbe() error = %v, want %v", err, want)
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want 2", calls)
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
