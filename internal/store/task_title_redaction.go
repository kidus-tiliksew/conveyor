package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

type taskTitleRedactionsKey struct{}

// WithTaskTitleRedactions carries the removals made from the title of the one
// task about to be created, so TaskCreatedPayload records them by count and
// class and never by value (req-security-boundaries AC-3.1). A title with no
// removal leaves the payload unchanged.
func WithTaskTitleRedactions(ctx context.Context, stats core.RedactionStats) context.Context {
	if stats.Total() == 0 {
		return ctx
	}
	return context.WithValue(ctx, taskTitleRedactionsKey{}, stats)
}

// TaskTitleSecrets is the stored-secret read a backend takes before its
// transaction opens, because a command that holds the transaction's connection
// must not wait on a second one. The read's failure is kept rather than
// returned: only a command that goes on to write a title is refused, so an
// approval with no child and a start-over replay still answer.
type TaskTitleSecrets struct {
	values []string
	err    error
}

// ReadTaskTitleSecrets takes the snapshot. A nil source holds no stored secret,
// and the redactor still covers patterns and registered values.
func ReadTaskTitleSecrets(ctx context.Context, source redact.SecretSource) TaskTitleSecrets {
	if source == nil {
		return TaskTitleSecrets{}
	}
	values, err := source.ListGitHubAppKeysForRedaction(ctx)
	return TaskTitleSecrets{values: values, err: err}
}

// Redactor refuses when the secret read failed, so a title is never stored
// unredacted because of that failure (req-security-boundaries REQ-3; DEC-8).
func (s TaskTitleSecrets) Redactor() (*redact.Redactor, error) {
	if s.err != nil {
		return nil, fmt.Errorf("resolve redaction credentials: %w", s.err)
	}
	return redact.New(s.values), nil
}

// BlueprintChildTitle derives a child's title from its decomposition summary.
// Redaction runs on the whole summary before the cut, so the bound can't leave
// a partial secret that no pattern recognizes.
func BlueprintChildTitle(redactor *redact.Redactor, summary string) (string, core.RedactionStats) {
	title, stats := redactor.Redact(strings.TrimSpace(summary))
	return core.TruncateUTF8Bytes(title, 200), stats
}
