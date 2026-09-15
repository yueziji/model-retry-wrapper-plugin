package main

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelOverridesInheritAndIsolatePolicies(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
models: [retry-a, retry-b]
max_attempts: 5
initial_delay_ms: 500
max_delay_ms: 4000
max_elapsed_time_ms: 60000
status_codes: [429, 503]
retry_keywords: [global_error]
model_overrides:
  " Retry-A ":
    max_attempts: 3
    initial_delay_ms: 1000
  retry-b:
    max_attempts: 0
    max_elapsed_time_ms: 0
    status_codes: []
    retry_keywords: []
  unused-model:
    max_attempts: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	a := retryConfigForModel(cfg, " RETRY-A ")
	if a.MaxAttempts != 3 || a.InitialDelayMS != 1000 || a.MaxDelayMS != 4000 || a.MaxElapsedMS != 60000 {
		t.Fatalf("unexpected effective limits: attempts=%d initial=%d max=%d elapsed=%d", a.MaxAttempts, a.InitialDelayMS, a.MaxDelayMS, a.MaxElapsedMS)
	}
	if !reflect.DeepEqual(a.StatusCodes, cfg.StatusCodes) || !reflect.DeepEqual(a.RetryKeywords, cfg.RetryKeywords) {
		t.Fatal("omitted lists must inherit the global lists")
	}
	b := retryConfigForModel(cfg, "retry-b")
	if b.MaxAttempts != 0 || b.MaxElapsedMS != 0 || len(b.StatusCodes) != 0 || len(b.RetryKeywords) != 0 {
		t.Fatal("explicit zeros and empty lists must override globals")
	}
	if cfg.MaxAttempts != 5 || cfg.InitialDelayMS != 500 || len(cfg.StatusCodes) != 2 || len(cfg.RetryKeywords) != 1 {
		t.Fatal("resolving one model must not mutate global settings")
	}
	if got := retryConfigForModel(cfg, "other"); !reflect.DeepEqual(got, cfg) {
		t.Fatal("a model without overrides must use the global configuration")
	}
	if shouldRoute(cfg, "openai", "unused-model") {
		t.Fatal("an override must not opt an unlisted model into routing")
	}
}

func TestModelOverridesReplaceAndNormalizeLists(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
model_overrides:
  retry-a:
    status_codes: ["429", 429, "503"]
    retry_keywords: [" Overloaded ", OVERLOADED]
    max_delay_ms: 2000
`))
	if err != nil {
		t.Fatal(err)
	}
	effective := retryConfigForModel(cfg, "retry-a")
	if !reflect.DeepEqual([]int(effective.StatusCodes), []int{429, 503}) || !reflect.DeepEqual(effective.RetryKeywords, []string{"overloaded"}) {
		t.Fatal("model lists must replace, normalize and deduplicate rather than append to globals")
	}
	if retryDelay(effective, 8) != 2*time.Second || retryDelay(cfg, 8) != 10*time.Second {
		t.Fatal("each model must use its own backoff cap")
	}
}

func TestModelOverridesEmptyAndNullInherit(t *testing.T) {
	for _, raw := range []string{
		"model_overrides: {}",
		"model_overrides: null",
		"model_overrides:\n  retry-a: {}",
		"model_overrides:\n  retry-a: null",
		"model_overrides:\n  retry-a:\n    max_attempts: null\n    status_codes: null\n    retry_keywords: null",
	} {
		cfg, err := decodeConfig([]byte("max_attempts: 4\n" + raw))
		if err != nil {
			t.Fatalf("decodeConfig(%q): %v", raw, err)
		}
		effective := retryConfigForModel(cfg, "retry-a")
		if !reflect.DeepEqual(effective, cfg) {
			t.Fatalf("empty or null overrides must inherit: %q", raw)
		}
	}
}

func TestModelOverridesRejectInvalidEffectiveLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"attempts", "retry-a: {max_attempts: -1}", "max_attempts"},
		{"initial delay", "retry-a: {initial_delay_ms: 0}", "initial_delay_ms"},
		{"max delay", "retry-a: {max_delay_ms: 0}", "max_delay_ms"},
		{"elapsed", "retry-a: {max_elapsed_time_ms: -1}", "max_elapsed_time_ms"},
		{"overflow", "retry-a: {max_delay_ms: 9300000000000}", "maximum supported duration"},
		{"inherited max", "retry-a: {initial_delay_ms: 10001}", "must not exceed"},
		{"inherited initial", "retry-a: {max_delay_ms: 499}", "must not exceed"},
		{"empty name", "' ': {}", "must not be blank"},
		{"duplicate normalized name", "retry-a: {}\n  ' RETRY-A ': {}", "duplicate normalized model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeConfig([]byte("model_overrides:\n  " + tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "model_overrides") {
				t.Fatalf("error = %v, want model_overrides context and %q", err, tc.want)
			}
		})
	}
}

func TestModelOverridesControlFailureAndStreamStartupRules(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
max_attempts: 5
max_elapsed_time_ms: 60000
model_overrides:
  retry-a: {max_attempts: 1}
  retry-b: {max_attempts: 2, max_elapsed_time_ms: 20}
  retry-c: {status_codes: [], retry_keywords: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model   string
		attempt int
		want    bool
	}{
		{"retry-a", 1, false},
		{"retry-b", 1, true},
		{"retry-b", 2, false},
		{"retry-c", 1, false},
		{"other", 2, true},
	} {
		effective := retryConfigForModel(cfg, tc.model)
		for _, status := range []int{http.StatusTooManyRequests, 0} {
			retry, _ := shouldRetryFailure(effective, tc.attempt, status, errors.New("rate_limited"))
			if retry != tc.want {
				t.Fatalf("model=%s attempt=%d status=%d retry=%t, want %t", tc.model, tc.attempt, status, retry, tc.want)
			}
		}
		if retry := retryableStreamStartupError(effective, tc.attempt, "codex", []byte(startupFailedJSON)) != nil; retry != tc.want {
			t.Fatalf("model=%s stream startup retry=%t, want %t", tc.model, retry, tc.want)
		}
	}
	ctx, cancel := retryContext(nil, retryConfigForModel(cfg, "retry-b"))
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 20*time.Millisecond {
		t.Fatal("the model elapsed-time override must govern the retry context")
	}
}

func TestModelOverridesRegistrationUsesObjectField(t *testing.T) {
	for _, field := range pluginRegistration().Metadata.ConfigFields {
		if field.Name == "model_overrides" {
			if field.Type != pluginapi.ConfigFieldTypeObject {
				t.Fatalf("model_overrides type=%s, want object", field.Type)
			}
			return
		}
	}
	t.Fatal("model_overrides configuration field is missing")
}
