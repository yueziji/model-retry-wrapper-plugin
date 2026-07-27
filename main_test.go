package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
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

func TestRetryDelaySaturatesInsteadOfOverflowing(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.InitialDelayMS = 500
	cfg.MaxDelayMS = 0

	if got := retryDelay(cfg, 100); got != time.Duration(math.MaxInt64) {
		t.Fatalf("attempt 100 delay = %v, want saturated duration", got)
	}
}

func TestDecodeConfigRejectsUnsafeRetryLimits(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{name: "negative attempts", config: "max_attempts: -1", wantErr: "max_attempts"},
		{name: "zero initial delay", config: "initial_delay_ms: 0", wantErr: "initial_delay_ms"},
		{name: "zero max delay", config: "max_delay_ms: 0", wantErr: "max_delay_ms"},
		{name: "negative elapsed time", config: "max_elapsed_time_ms: -1", wantErr: "max_elapsed_time_ms"},
		{name: "initial exceeds max", config: "initial_delay_ms: 2000\nmax_delay_ms: 1000", wantErr: "must not exceed"},
		{name: "max delay overflows duration", config: "max_delay_ms: 9300000000000", wantErr: "maximum supported duration"},
		{name: "initial delay overflows duration", config: "initial_delay_ms: 20000000000000", wantErr: "maximum supported duration"},
		{name: "elapsed time overflows duration", config: "max_elapsed_time_ms: 20000000000000", wantErr: "maximum supported duration"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeConfig([]byte(test.config))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("decodeConfig() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeConfigAllowsUnboundedElapsedTime(t *testing.T) {
	cfg, err := decodeConfig([]byte("max_attempts: 0"))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if cfg.MaxAttempts != 0 {
		t.Fatalf("MaxAttempts = %d, want 0", cfg.MaxAttempts)
	}
	if cfg.MaxElapsedMS != 0 {
		t.Fatalf("MaxElapsedMS = %d, want 0", cfg.MaxElapsedMS)
	}
}

func TestDecodeConfigAllowsExplicitlyDisablingKeywordRetry(t *testing.T) {
	cfg, err := decodeConfig([]byte("retry_keywords: []"))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if len(cfg.RetryKeywords) != 0 {
		t.Fatalf("RetryKeywords = %#v, want empty", cfg.RetryKeywords)
	}

	cfg, err = decodeConfig(nil)
	if err != nil {
		t.Fatalf("decodeConfig(defaults) error = %v", err)
	}
	if !reflect.DeepEqual(cfg.RetryKeywords, defaultPluginConfig().RetryKeywords) {
		t.Fatalf("default RetryKeywords = %#v, want %#v", cfg.RetryKeywords, defaultPluginConfig().RetryKeywords)
	}
}

func TestRetryContextUsesConfiguredElapsedLimit(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.MaxElapsedMS = 10
	ctx, cancel := retryContext(context.Background(), cfg)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("retry context did not reach its configured deadline")
	}
}

func TestRetryContextAllowsUnboundedElapsedTime(t *testing.T) {
	cfg := defaultPluginConfig()
	ctx, cancel := retryContext(context.Background(), cfg)

	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		t.Fatal("unbounded retry context unexpectedly has a deadline")
	}
	cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("unbounded retry context did not remain cancelable")
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

func TestStatusFromErrorRequiresExplicitStatusMarker(t *testing.T) {
	tests := []string{
		"model qwen3-235b-a22b: rate_limited",
		"model gpt-oss-120b: rate_limited",
		"request failed at 2026-07-27T10:00:00.503Z",
		"expected status 200 but the stream ended early",
		"redirect status 302 was not followed",
	}
	for _, message := range tests {
		if got := statusFromError(errors.New(message)); got != 0 {
			t.Fatalf("statusFromError(%q) = %d, want 0", message, got)
		}
	}

	for message, want := range map[string]int{
		"upstream HTTP 503":              http.StatusServiceUnavailable,
		"request failed with status 429": http.StatusTooManyRequests,
		"HTTP response status code=502":  http.StatusBadGateway,
	} {
		if got := statusFromError(errors.New(message)); got != want {
			t.Fatalf("statusFromError(%q) = %d, want %d", message, got, want)
		}
	}
}

func TestKeywordRetryIsNotMaskedByDigitsInModelName(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.MaxAttempts = 2
	err := errors.New("model qwen3-235b-a22b: rate_limited")
	status := statusFromError(err)
	shouldRetry, keyword := shouldRetryFailure(cfg, 1, status, err)
	if !shouldRetry || keyword != "rate_limited" {
		t.Fatalf("retry = %v, keyword = %q, want true, rate_limited", shouldRetry, keyword)
	}
}

func TestStatusFromErrorUsesWrappedStructuredStatus(t *testing.T) {
	err := fmt.Errorf("outer: %w", retryStatusError{status: http.StatusTooManyRequests, err: errors.New("limited")})
	if got := statusFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("statusFromError() = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestErrorEnvelopePreservesHTTPStatus(t *testing.T) {
	raw := errorEnvelopeForError("plugin_error", retryStatusError{status: http.StatusTooManyRequests, err: errors.New("limited")})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if env.Error == nil || env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("envelope error = %#v, want HTTP 429", env.Error)
	}
}

func TestExecutorHTTPRequestReturnsNotImplementedStatus(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, nil)
	if err != nil {
		t.Fatalf("handleMethod() error = %v", err)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v", errUnmarshal)
	}
	if env.Error == nil || env.Error.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("envelope error = %#v, want HTTP 501", env.Error)
	}
}

func TestPluginLifecycleShutdownCancelsAndWaitsForStreamTask(t *testing.T) {
	state := newPluginLifecycleState()
	ctx, err := state.beginStreamTask()
	if err != nil {
		t.Fatalf("beginStreamTask() error = %v", err)
	}
	if errTrack := state.trackHostStream("stream-1"); errTrack != nil {
		t.Fatalf("trackHostStream() error = %v", errTrack)
	}

	closedStream := make(chan string, 1)
	shutdownDone := make(chan struct{})
	go func() {
		state.shutdown(func(streamID string) error {
			closedStream <- streamID
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lifecycle context was not canceled")
	}
	select {
	case streamID := <-closedStream:
		if streamID != "stream-1" {
			t.Fatalf("closed stream = %q, want stream-1", streamID)
		}
	case <-time.After(time.Second):
		t.Fatal("active host stream was not closed")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the stream task ended")
	default:
	}

	state.endStreamTask()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after the stream task ended")
	}
	if _, errBegin := state.beginStreamTask(); errBegin == nil {
		t.Fatal("beginStreamTask() succeeded after shutdown")
	}
}
