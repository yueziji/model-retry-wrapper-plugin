package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRetryDecisionDiagnosticExplainsRuleAndTermination(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		attempt     int
		maxAttempts int
		err         error
		stopErr     error
		want        []string
	}{
		{"keyword allows HTTP 400", 400, 1, 3, errors.New("private-keyword private-body"), nil,
			[]string{"status=400", "status_match=false", "keyword_match=true", "retry_allowed=true", "stop_reason=none"}},
		{"status allows retry", 503, 1, 3, errors.New("private-body"), nil,
			[]string{"status_match=true", "keyword_match=false", "retry_allowed=true", "stop_reason=none"}},
		{"both rules match", 503, 1, 3, errors.New("private-keyword"), nil,
			[]string{"status_match=true", "keyword_match=true", "retry_allowed=true"}},
		{"neither rule matches", 400, 1, 3, errors.New("private-body"), nil,
			[]string{"keyword_match=false", "retry_allowed=false", "stop_reason=no_matching_rule"}},
		{"keyword at attempt cap", 400, 3, 3, errors.New("private-keyword"), nil,
			[]string{"attempt=3", "keyword_match=true", "retry_allowed=false", "stop_reason=max_attempts", "max_attempts=3"}},
		{"status at attempt cap", 503, 3, 3, nil, nil,
			[]string{"status_match=true", "retry_allowed=false", "stop_reason=max_attempts"}},
		{"unlimited attempts", 400, 100, 0, errors.New("private-keyword"), nil,
			[]string{"retry_allowed=true", "stop_reason=none", "max_attempts=0"}},
		{"canceled while waiting", 400, 1, 3, errors.New("private-keyword"), context.Canceled,
			[]string{"keyword_match=true", "retry_allowed=false", "stop_reason=canceled"}},
		{"deadline while waiting", 503, 1, 3, nil, context.DeadlineExceeded,
			[]string{"retry_allowed=false", "stop_reason=deadline_exceeded"}},
		{"canceled before first attempt", 0, 0, 3, nil, context.Canceled,
			[]string{"attempt=0", "retry_allowed=false", "stop_reason=canceled"}},
		{"stream startup details", 400, 1, 3, &streamStartupError{status: 400, details: "private-keyword private-body"}, nil,
			[]string{"keyword_match=true", "retry_allowed=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.RetryKeywords = []string{"private-keyword"}
			cfg.MaxAttempts = tc.maxAttempts
			cfg.MaxElapsedMS = 10000
			message := retryDecisionDiagnostic("retry decision", cfg, tc.attempt, tc.status, tc.err, tc.stopErr)
			for _, want := range append(tc.want, "max_elapsed_time_ms=10000", "retry_keywords_count=1") {
				if !strings.Contains(message, want) {
					t.Fatalf("missing %q in %s", want, message)
				}
			}
			if strings.Contains(message, "private-") {
				t.Fatal("diagnostics exposed upstream details or configured keyword values")
			}
		})
	}
}

func TestRetryDecisionDiagnosticUsesEffectiveModelPolicy(t *testing.T) {
	cfg, err := decodeConfig([]byte("retry_keywords: [private-keyword]\nmodel_overrides:\n  retry-model:\n    status_codes: []\n    retry_keywords: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective := retryConfigForModel(cfg, "retry-model")
	message := retryDecisionDiagnostic("retry stopped", effective, 1, 503, errors.New("private-keyword"), nil)
	for _, want := range []string{"status_match=false", "keyword_match=false", "retry_keywords_count=0", "stop_reason=no_matching_rule"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %s", want, message)
		}
	}
	fields := retryLogFields(rpcExecutorRequest{}, effective, 1, 503, false)
	if _, exists := fields["retry_keywords"]; exists {
		t.Fatal("retry log fields exposed keyword values")
	}
}
