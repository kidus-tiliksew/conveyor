package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Source is the narrow GitHub observation boundary. The implementation may
// poll or adapt webhook redelivery, but it must return stable occurrence IDs.
type Source interface {
	Observations(context.Context, time.Time) ([]Observation, error)
}

// passFailureRecordTimeout bounds the status write for a failed pass, which
// runs even when the poll's own context was canceled.
const passFailureRecordTimeout = 5 * time.Second

type Poller struct {
	Service       *Service
	Source        Source
	StartupWindow time.Duration
	RetryInitial  time.Duration
	RetryMaximum  time.Duration
	Attempts      int
	Sleep         func(context.Context, time.Duration) error
	Now           func() time.Time
	// StopOnRateLimit ends the poll at a rate-limited GitHub answer instead
	// of retrying it, because a retry before GitHub's reset fails too. The
	// timed GitHub fallback sets it; push deliveries keep the bounded
	// retries (component-runtime, Timed GitHub fallback).
	StopOnRateLimit bool
}

// Poll performs startup reconciliation and bounded forge retries. Persisted
// observation identity, rather than the polling cursor, is authoritative for
// deduplication across restart and redelivery (component-monitor-drift).
func (p *Poller) Poll(ctx context.Context) error {
	if p.Service == nil || p.Source == nil || p.Service.Store == nil {
		return fmt.Errorf("monitor poller is not configured")
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	delay := p.RetryInitial
	if delay <= 0 {
		delay = time.Second
	}
	maximum := p.RetryMaximum
	if maximum < delay {
		maximum = 30 * time.Second
	}
	since := now.Add(-p.StartupWindow)
	var observations []Observation
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		observations, err = p.Source.Observations(ctx, since)
		if err == nil {
			break
		}
		category := string(githubtrigger.ErrorCategory(err))
		if category == "" {
			category = string(githubtrigger.ForgeRequest)
		}
		retryAt, limited := githubtrigger.RateLimitRetryAt(err)
		final := attempt == attempts || limited && p.StopOnRateLimit
		backoffUntil := now.Add(delay)
		if limited && retryAt.After(backoffUntil) {
			backoffUntil = retryAt
		}
		detail, _, redactErr := redact.Text(ctx, p.Service.RedactionSecrets, err.Error())
		if redactErr != nil {
			return fmt.Errorf("resolve monitor redaction credentials: %w", redactErr)
		}
		_ = p.Service.Store.RecordMonitorFailure(ctx, category, detail, backoffUntil)
		kind := "monitor.retry"
		if final {
			kind = "monitor.terminal_failure"
		}
		_ = p.Service.Store.AuditMonitor(ctx, kind, map[string]any{
			"attempt": attempt, "max_attempts": attempts,
			"forge_error_category": category, "error": detail,
			"backoff_until": backoffUntil,
		})
		if final {
			failure := fmt.Errorf("observe GitHub after %d attempt(s): %s", attempt, detail)
			if limited {
				// Keep the category and GitHub's reset time for the caller;
				// the message stays the redacted detail.
				return githubtrigger.RateLimitedError(retryAt, failure)
			}
			return failure
		}
		if p.Sleep != nil {
			if sleepErr := p.Sleep(ctx, delay); sleepErr != nil {
				return sleepErr
			}
		}
		delay *= 2
		if delay > maximum {
			delay = maximum
		}
	}
	// One failed observation never strands a later one. Identity makes the
	// next pass's re-read of the window retry the failure and deduplicate the
	// rest (component-monitor-drift, State and failure).
	var failures []error
	for _, observation := range observations {
		if ctxErr := ctx.Err(); ctxErr != nil {
			failures = append(failures, ctxErr)
			break
		}
		if _, processErr := p.Service.Process(ctx, observation); processErr != nil {
			failures = append(failures, fmt.Errorf("process observation %s: %w", observation.Identity(), processErr))
		}
	}
	if len(failures) > 0 {
		return p.recordPassFailure(ctx, errors.Join(failures...), now.Add(delay))
	}
	return p.Service.Store.RecordMonitorSuccess(ctx, now)
}

// passFailure carries the redacted, single-line text recorded in status while
// keeping the joined causes for errors.Is and errors.As.
type passFailure struct {
	detail string
	cause  error
}

func (f *passFailure) Error() string { return f.detail }
func (f *passFailure) Unwrap() error { return f.cause }

// recordPassFailure states a failed pass in status and returns it to the
// caller, which owns any retry. The record outlives a canceled poll, and a
// failure to write it joins the returned error rather than vanishing.
func (p *Poller) recordPassFailure(ctx context.Context, failure error, backoffUntil time.Time) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), passFailureRecordTimeout)
	defer cancel()
	detail, _, redactErr := redact.Text(recordCtx, p.Service.RedactionSecrets, strings.ReplaceAll(failure.Error(), "\n", "; "))
	if redactErr != nil {
		detail = "monitor pass failed; its failure text could not be redacted"
		failure = errors.Join(failure, fmt.Errorf("resolve monitor redaction credentials: %w", redactErr))
	}
	// A store or intake failure has no forge category, and it must not keep
	// the category of an earlier failed read.
	category := string(githubtrigger.ErrorCategory(failure))
	if recordErr := p.Service.Store.RecordMonitorFailure(recordCtx, category, detail, backoffUntil); recordErr != nil {
		failure = errors.Join(failure, fmt.Errorf("record monitor failure: %w", recordErr))
		detail = detail + "; the failure could not be recorded in status"
	}
	return &passFailure{detail: detail, cause: failure}
}
