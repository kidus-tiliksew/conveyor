package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Source is the narrow GitHub observation boundary. The implementation may
// poll or adapt webhook redelivery, but it must return stable occurrence IDs.
type Source interface {
	Observations(context.Context, time.Time) ([]Observation, error)
}

type Poller struct {
	Service       *Service
	Source        Source
	StartupWindow time.Duration
	RetryInitial  time.Duration
	RetryMaximum  time.Duration
	Attempts      int
	Sleep         func(context.Context, time.Duration) error
	Now           func() time.Time
}

// Poll performs startup reconciliation and bounded forge retries. Persisted
// observation identity, rather than the polling cursor, is authoritative for
// deduplication across restart and redelivery (design-monitor-drift).
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
		backoffUntil := now.Add(delay)
		detail, _, redactErr := redact.Text(ctx, p.Service.RedactionSecrets, err.Error())
		if redactErr != nil {
			return fmt.Errorf("resolve monitor redaction credentials: %w", redactErr)
		}
		_ = p.Service.Store.RecordMonitorFailure(ctx, category, detail, backoffUntil)
		kind := "monitor.retry"
		if attempt == attempts {
			kind = "monitor.terminal_failure"
		}
		_ = p.Service.Store.AuditMonitor(ctx, kind, map[string]any{
			"attempt": attempt, "max_attempts": attempts,
			"forge_error_category": category, "error": detail,
			"backoff_until": backoffUntil,
		})
		if attempt == attempts {
			return fmt.Errorf("observe GitHub after %d attempt(s): %s", attempt, detail)
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
	for _, observation := range observations {
		if _, err = p.Service.Process(ctx, observation); err != nil {
			return err
		}
	}
	return p.Service.Store.RecordMonitorSuccess(ctx, now)
}
