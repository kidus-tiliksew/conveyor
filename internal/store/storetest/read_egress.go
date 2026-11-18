package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// ReadEgressMarker prefixes the one JSON line each measured case writes to
// standard output, so a Make target can select measurements from test logs.
const ReadEgressMarker = "READ_EGRESS_MEASUREMENT "

// ReadEgressTrials is the number of interleaved baseline/candidate trials
// recorded after the warmup pair.
const ReadEgressTrials = 5

// readEgressTopLabels bounds the labels drained per call; one call uses a
// handful.
const readEgressTopLabels = 1000

// ReadEgressOutcome is what one run of a variant served: a digest of the
// served result and the count of successful items it carries.
type ReadEgressOutcome struct {
	Digest    string
	Successes int
}

// ReadEgressDigest hashes the JSON form of a served result.
func ReadEgressDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ReadEgressWork performs one variant's read through a real backend. Each
// variant sets its own read label on the context it passes to the database
// (dbmetrics.WithReadLabel) so the statistics attribute bytes to it.
type ReadEgressWork func(ctx context.Context) (ReadEgressOutcome, error)

// ReadEgressCase is one named baseline/candidate pair on one fixture.
type ReadEgressCase struct {
	Name      string
	Baseline  ReadEgressWork
	Candidate ReadEgressWork
	// BaselineRevision overrides the run default for a non-inline baseline.
	BaselineRevision string
	// Fixture reports the owned fixture's counts (documents, matching
	// events, noise events, payload bytes, and so on).
	Fixture map[string]int
	// RequireZeroCandidateReads keeps the default nonzero driver-stat
	// check on the baseline and, when true, requires the candidate call
	// to record Bytes=0, Rows=0, Queries=0, and no Top labels. Warm HTTP
	// memo cases set this so a cache hit cannot hide a store round trip.
	RequireZeroCandidateReads bool
}

// ReadEgressEnv describes the run recorded with every measurement.
type ReadEgressEnv struct {
	Backend           string
	CacheMode         string
	BaselineRevision  string
	CandidateRevision string
	// ReadStats drains the opened store's driver read statistics
	// (the store's ReadStats method).
	ReadStats func(top int) dbmetrics.ReadInterval
	// Out receives one marker line per case; nil means standard output.
	Out io.Writer
}

// ReadEgressLabel is one label's counts in one call.
type ReadEgressLabel struct {
	Label      string `json:"label"`
	Bytes      uint64 `json:"returned_bytes"`
	Rows       uint64 `json:"rows"`
	Statements uint64 `json:"statements"`
}

// ReadEgressTrial is the raw record of one measured call.
type ReadEgressTrial struct {
	Trial      int               `json:"trial"`
	Variant    string            `json:"variant"`
	Bytes      uint64            `json:"returned_bytes"`
	Rows       uint64            `json:"rows"`
	Statements uint64            `json:"statements"`
	Digest     string            `json:"digest"`
	Successes  int               `json:"successes"`
	Labels     []ReadEgressLabel `json:"labels"`
}

// ReadEgressSpread is min/median/max of one counter over the trials.
type ReadEgressSpread struct {
	Min    uint64 `json:"min"`
	Median uint64 `json:"median"`
	Max    uint64 `json:"max"`
}

// ReadEgressSummary aggregates one variant's trials.
type ReadEgressSummary struct {
	Bytes      ReadEgressSpread `json:"returned_bytes"`
	Rows       ReadEgressSpread `json:"rows"`
	Statements ReadEgressSpread `json:"statements"`
}

// ReadEgressReport is the machine-readable measurement of one case.
type ReadEgressReport struct {
	Case              string                       `json:"case"`
	Backend           string                       `json:"backend"`
	CacheMode         string                       `json:"cache_mode"`
	BaselineRevision  string                       `json:"baseline_revision"`
	CandidateRevision string                       `json:"candidate_revision"`
	Fixture           map[string]int               `json:"fixture"`
	Trials            []ReadEgressTrial            `json:"trials"`
	Summary           map[string]ReadEgressSummary `json:"summary"`
	// WorkEqual is true only when every call of both variants served the
	// same digest and success count; Gains is absent otherwise.
	WorkEqual bool              `json:"work_equal"`
	Digest    string            `json:"digest"`
	Successes int               `json:"successes"`
	Gains     map[string]string `json:"gains,omitempty"`
}

// MeasureReadEgress runs a warmup pair and ReadEgressTrials interleaved
// trials of the case, alternating which variant runs first. It returns an
// error, with the raw trials gathered so far, when a statistic is missing,
// a variant fails or serves nothing, or the served work differs. Gains are
// computed only after every call served identical work.
func MeasureReadEgress(ctx context.Context, env ReadEgressEnv, c ReadEgressCase) (ReadEgressReport, error) {
	report := ReadEgressReport{Case: c.Name, Backend: env.Backend, CacheMode: env.CacheMode,
		BaselineRevision: env.BaselineRevision, CandidateRevision: env.CandidateRevision,
		Fixture: c.Fixture, Trials: []ReadEgressTrial{}}
	if c.BaselineRevision != "" {
		report.BaselineRevision = c.BaselineRevision
	}
	switch {
	case env.ReadStats == nil:
		return report, errors.New("read egress: no driver read statistics source")
	case c.Name == "" || c.Baseline == nil || c.Candidate == nil:
		return report, errors.New("read egress: case needs a name and both variants")
	case env.Backend == "" || env.CacheMode == "" || report.BaselineRevision == "" || env.CandidateRevision == "":
		return report, errors.New("read egress: backend, cache mode, and both revisions are required")
	}
	works := map[string]ReadEgressWork{"baseline": c.Baseline, "candidate": c.Candidate}
	var want *ReadEgressOutcome
	call := func(trial int, variant string) error {
		env.ReadStats(readEgressTopLabels) // start from an empty interval
		outcome, err := works[variant](ctx)
		interval := env.ReadStats(readEgressTopLabels)
		if err != nil {
			return fmt.Errorf("read egress %s %s: %w", c.Name, variant, err)
		}
		if c.RequireZeroCandidateReads && variant == "candidate" {
			if interval.Bytes != 0 || interval.Rows != 0 || interval.Queries != 0 || len(interval.Top) != 0 {
				return fmt.Errorf("read egress %s %s: candidate driver reads must be zero (bytes=%d rows=%d statements=%d labels=%d)", c.Name, variant, interval.Bytes, interval.Rows, interval.Queries, len(interval.Top))
			}
		} else if interval.Bytes == 0 || interval.Queries == 0 || len(interval.Top) == 0 {
			return fmt.Errorf("read egress %s %s: driver read statistics are missing (bytes=%d statements=%d)", c.Name, variant, interval.Bytes, interval.Queries)
		}
		if outcome.Successes <= 0 || outcome.Digest == "" {
			return fmt.Errorf("read egress %s %s served no work (successes=%d)", c.Name, variant, outcome.Successes)
		}
		if want == nil {
			want = &outcome
		} else if outcome != *want {
			return fmt.Errorf("read egress %s %s served different work: digest %s successes %d, want digest %s successes %d",
				c.Name, variant, outcome.Digest, outcome.Successes, want.Digest, want.Successes)
		}
		if trial < 0 {
			return nil // warmup is checked for equality but not recorded
		}
		row := ReadEgressTrial{Trial: trial, Variant: variant, Bytes: interval.Bytes, Rows: interval.Rows, Statements: interval.Queries,
			Digest: outcome.Digest, Successes: outcome.Successes}
		for _, total := range interval.Top {
			row.Labels = append(row.Labels, ReadEgressLabel{Label: total.Label, Bytes: total.Bytes, Rows: total.Rows, Statements: total.Queries})
		}
		report.Trials = append(report.Trials, row)
		return nil
	}
	if err := call(-1, "baseline"); err != nil {
		return report, err
	}
	if err := call(-1, "candidate"); err != nil {
		return report, err
	}
	for trial := range ReadEgressTrials {
		order := [2]string{"baseline", "candidate"}
		if trial%2 == 1 {
			order = [2]string{"candidate", "baseline"}
		}
		for _, variant := range order {
			if err := call(trial, variant); err != nil {
				return report, err
			}
		}
	}
	report.WorkEqual = true
	report.Digest, report.Successes = want.Digest, want.Successes
	report.Summary = map[string]ReadEgressSummary{}
	for _, variant := range []string{"baseline", "candidate"} {
		var bytes, rows, statements []uint64
		for _, row := range report.Trials {
			if row.Variant == variant {
				bytes, rows, statements = append(bytes, row.Bytes), append(rows, row.Rows), append(statements, row.Statements)
			}
		}
		report.Summary[variant] = ReadEgressSummary{Bytes: spread(bytes), Rows: spread(rows), Statements: spread(statements)}
	}
	b, k := report.Summary["baseline"], report.Summary["candidate"]
	report.Gains = map[string]string{
		"returned_bytes": ratio(b.Bytes.Median, k.Bytes.Median),
		"rows":           ratio(b.Rows.Median, k.Rows.Median),
		"statements":     ratio(b.Statements.Median, k.Statements.Median),
	}
	return report, nil
}

func spread(values []uint64) ReadEgressSpread {
	sorted := append([]uint64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n == 0 {
		return ReadEgressSpread{}
	}
	median := sorted[n/2]
	if n%2 == 0 {
		median = sorted[n/2-1] + (sorted[n/2]-sorted[n/2-1])/2
	}
	return ReadEgressSpread{Min: sorted[0], Median: median, Max: sorted[n-1]}
}

// ratio states baseline over candidate medians as a decimal string.
func ratio(baseline, candidate uint64) string {
	if candidate == 0 {
		return "n/a (candidate median 0)"
	}
	return fmt.Sprintf("%.3f", float64(baseline)/float64(candidate))
}

// ReadEgressBuilder prepares a case on the caller's owned fixture. It fails
// the test on fixture errors.
type ReadEgressBuilder[S any] func(t testing.TB, ctx context.Context, st S) ReadEgressCase

// ReadEgressRegistry holds the named cases one backend package measures.
// A package declares one registry; each case registers a builder, and the
// package's measurement test runs them all.
type ReadEgressRegistry[S any] struct {
	names    []string
	builders map[string]ReadEgressBuilder[S]
}

// NewReadEgressRegistry returns an empty registry.
func NewReadEgressRegistry[S any]() *ReadEgressRegistry[S] {
	return &ReadEgressRegistry[S]{builders: map[string]ReadEgressBuilder[S]{}}
}

// Register adds a named case. It panics on an empty or repeated name, which
// is a programming error found when the test binary starts.
func (r *ReadEgressRegistry[S]) Register(name string, build ReadEgressBuilder[S]) {
	if name == "" || build == nil {
		panic("storetest: read egress case needs a name and a builder")
	}
	if _, dup := r.builders[name]; dup {
		panic("storetest: read egress case registered twice: " + name)
	}
	r.names = append(r.names, name)
	r.builders[name] = build
}

// Names lists registered cases in registration order.
func (r *ReadEgressRegistry[S]) Names() []string { return append([]string(nil), r.names...) }

// Run builds and measures every registered case, writing one marker line per
// case to env.Out (standard output by default). It fails the test when the
// registry is empty or any case fails; a failed case still writes its raw
// trials so far.
func (r *ReadEgressRegistry[S]) Run(t *testing.T, ctx context.Context, st S, env ReadEgressEnv) []ReadEgressReport {
	t.Helper()
	if len(r.names) == 0 {
		t.Fatal("no read egress case is registered")
	}
	out := env.Out
	if out == nil {
		out = os.Stdout
	}
	var reports []ReadEgressReport
	for _, name := range r.names {
		c := r.builders[name](t, ctx, st)
		if c.Name != name {
			t.Fatalf("read egress builder %q returned case %q", name, c.Name)
		}
		report, err := MeasureReadEgress(ctx, env, c)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		line, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(out, "%s%s\n", ReadEgressMarker, line)
		reports = append(reports, report)
	}
	return reports
}

// ReadEgressEnvFromProcess builds the run description for one backend from
// the process environment. CONVEYOR_READ_EGRESS_BASELINE_REVISION and
// CONVEYOR_READ_EGRESS_CANDIDATE_REVISION name the compared revisions; the
// candidate defaults to the checked-out HEAD (with +dirty for uncommitted
// tracked changes), and the baseline defaults to the inline statements of
// ReadEgressInlineBaselineRevision, because the baseline variant is the
// statement embedded in the case, not code at the candidate revision.
// CONVEYOR_READ_EGRESS_CACHE_MODE defaults to
// "none" (no result cache between the store and the database).
func ReadEgressEnvFromProcess(backend string, stats func(top int) dbmetrics.ReadInterval) (ReadEgressEnv, error) {
	env := ReadEgressEnv{Backend: backend, ReadStats: stats,
		CacheMode:         envOr("CONVEYOR_READ_EGRESS_CACHE_MODE", "none"),
		BaselineRevision:  envOr("CONVEYOR_READ_EGRESS_BASELINE_REVISION", "inline-queries@"+ReadEgressInlineBaselineRevision),
		CandidateRevision: os.Getenv("CONVEYOR_READ_EGRESS_CANDIDATE_REVISION")}
	if env.CandidateRevision == "" {
		out, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			return env, fmt.Errorf("candidate revision: set CONVEYOR_READ_EGRESS_CANDIDATE_REVISION or run inside the checkout: %w", err)
		}
		env.CandidateRevision = strings.TrimSpace(string(out))
		if dirty, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(strings.TrimSpace(string(dirty))) > 0 {
			env.CandidateRevision += "+dirty"
		}
	}
	return env, nil
}

// ReadEgressCount reads a positive fixture size from the named environment
// variable, falling back to def when it is unset.
func ReadEgressCount(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
