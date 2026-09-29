package main

import (
	"context"
	"errors"
	"fmt"
)

// Keep decision details in the message because CPA's text formatter omits most
// structured fields. Error bodies and configured keywords stay out of this summary.
func retryDecisionDiagnostic(message string, cfg pluginConfig, attempt, status int, err, stopErr error) string {
	statusMatch := shouldRetryStatus(cfg, status)
	keywordMatch := retryKeywordFromError(cfg, err) != ""
	retryAllowed, _ := shouldRetryFailure(cfg, attempt, status, err)
	stopReason := "none"
	switch {
	case errors.Is(stopErr, context.DeadlineExceeded):
		stopReason = "deadline_exceeded"
	case stopErr != nil:
		stopReason = "canceled"
	case !statusMatch && !keywordMatch:
		stopReason = "no_matching_rule"
	case !retryAllowed:
		stopReason = "max_attempts"
	}
	return fmt.Sprintf("%s attempt=%d status=%d status_match=%t keyword_match=%t retry_allowed=%t stop_reason=%s max_attempts=%d max_elapsed_time_ms=%d retry_keywords_count=%d",
		message, attempt, status, statusMatch, keywordMatch, retryAllowed && stopErr == nil,
		stopReason, cfg.MaxAttempts, cfg.MaxElapsedMS, len(cfg.RetryKeywords))
}
