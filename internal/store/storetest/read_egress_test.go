package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// readEgressFake serves reads through a real counting connection and ledger,
// so the runner is exercised against the production statistics, not a stub.
type readEgressFake struct{ ledger *dbmetrics.ReadLedger }

func newReadEgressFake() *readEgressFake { return &readEgressFake{ledger: dbmetrics.NewReadLedger()} }

func (f *readEgressFake) env() ReadEgressEnv {
	return ReadEgressEnv{Backend: "fake", CacheMode: "none", BaselineRevision: "base", CandidateRevision: "cand",
		ReadStats: f.ledger.Drain, Out: io.Discard}
}

func (f *readEgressFake) read(t *testing.T, label string, size int, rows uint64) {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	counted := f.ledger.WrapConn(client)
	go func() { _, _ = server.Write(make([]byte, size)); server.Close() }()
	conn := dbmetrics.ReadConnOf(counted)
	conn.Begin(label)
	if _, err := io.ReadFull(counted, make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	conn.End(1, rows)
}

func equalOutcome() (ReadEgressOutcome, error) {
	digest, err := ReadEgressDigest([]int{1, 2, 3})
	return ReadEgressOutcome{Digest: digest, Successes: 3}, err
}

func TestMeasureReadEgressRecordsInterleavedTrialsAndSpread(t *testing.T) {
	fake := newReadEgressFake()
	var order []string
	baselineCalls, candidateCalls := 0, 0
	c := ReadEgressCase{Name: "case", Fixture: map[string]int{"noise": 7},
		BaselineRevision: "candidate-revision:job-index-hidden-fold",
		Baseline: func(context.Context) (ReadEgressOutcome, error) {
			baselineCalls++
			order = append(order, "baseline")
			fake.read(t, "legacy", 1000+baselineCalls*100, 10)
			return equalOutcome()
		},
		Candidate: func(context.Context) (ReadEgressOutcome, error) {
			candidateCalls++
			order = append(order, "candidate")
			fake.read(t, "public", 100, 2)
			return equalOutcome()
		}}
	report, err := MeasureReadEgress(t.Context(), fake.env(), c)
	if err != nil {
		t.Fatal(err)
	}
	if report.BaselineRevision != c.BaselineRevision || report.CandidateRevision != "cand" {
		t.Fatalf("measurement provenance = %q / %q", report.BaselineRevision, report.CandidateRevision)
	}
	if baselineCalls != ReadEgressTrials+1 || candidateCalls != ReadEgressTrials+1 {
		t.Fatalf("calls baseline=%d candidate=%d, want one warmup plus %d trials each", baselineCalls, candidateCalls, ReadEgressTrials)
	}
	if got := strings.Join(order, ","); !strings.HasPrefix(got, "baseline,candidate,baseline,candidate,candidate,baseline,") {
		t.Fatalf("warmup then alternating trial order, got %s", got)
	}
	if len(report.Trials) != 2*ReadEgressTrials || !report.WorkEqual || report.Fixture["noise"] != 7 {
		t.Fatalf("report %+v", report)
	}
	// Warmup call 1 is excluded; recorded baseline calls were 2..6, so bytes are 1200..1600.
	got := report.Summary["baseline"].Bytes
	if got.Min < 1200 || got.Max < 1600 || got.Median < 1400 || got.Min > got.Median || got.Median > got.Max {
		t.Fatalf("baseline byte spread %+v", got)
	}
	if rows := report.Summary["baseline"].Rows; rows != (ReadEgressSpread{Min: 10, Median: 10, Max: 10}) {
		t.Fatalf("baseline rows %+v", rows)
	}
	if st := report.Summary["candidate"].Statements; st != (ReadEgressSpread{Min: 1, Median: 1, Max: 1}) {
		t.Fatalf("candidate statements %+v", st)
	}
	first := report.Trials[0]
	if first.Variant != "baseline" || len(first.Labels) != 1 || first.Labels[0].Label != "legacy" || first.Labels[0].Statements != 1 {
		t.Fatalf("first trial raw record %+v", first)
	}
	if report.Gains["returned_bytes"] == "" || report.Gains["rows"] != "5.000" {
		t.Fatalf("gains %+v", report.Gains)
	}
}

func TestMeasureReadEgressRefusesGainsWithoutEqualWork(t *testing.T) {
	fake := newReadEgressFake()
	for name, other := range map[string]ReadEgressOutcome{
		"digest":    {Digest: "different", Successes: 3},
		"successes": {Digest: mustDigest(t), Successes: 2},
	} {
		c := ReadEgressCase{Name: name,
			Baseline: func(context.Context) (ReadEgressOutcome, error) {
				fake.read(t, "legacy", 500, 5)
				return equalOutcome()
			},
			Candidate: func(context.Context) (ReadEgressOutcome, error) {
				fake.read(t, "public", 10, 1)
				return other, nil
			}}
		report, err := MeasureReadEgress(t.Context(), fake.env(), c)
		if err == nil || !strings.Contains(err.Error(), "different work") {
			t.Fatalf("%s: want different-work error, got %v", name, err)
		}
		if report.WorkEqual || report.Gains != nil || report.Summary != nil {
			t.Fatalf("%s: gains reported without equal work: %+v", name, report)
		}
	}
}

func TestMeasureReadEgressRejectsMissingStatisticsAndEmptyWork(t *testing.T) {
	fake := newReadEgressFake()
	serves := func(label string) ReadEgressWork {
		return func(context.Context) (ReadEgressOutcome, error) {
			fake.read(t, label, 50, 1)
			return equalOutcome()
		}
	}
	silent := func(context.Context) (ReadEgressOutcome, error) { return equalOutcome() }
	empty := func(context.Context) (ReadEgressOutcome, error) {
		fake.read(t, "x", 50, 1)
		return ReadEgressOutcome{Digest: "d", Successes: 0}, nil
	}
	cases := map[string]struct {
		env  ReadEgressEnv
		c    ReadEgressCase
		want string
	}{
		"variant records no statistics": {fake.env(), ReadEgressCase{Name: "c", Baseline: serves("a"), Candidate: silent}, "statistics are missing"},
		"no statistics source":          {ReadEgressEnv{Backend: "b", CacheMode: "none", BaselineRevision: "x", CandidateRevision: "y"}, ReadEgressCase{Name: "c", Baseline: serves("a"), Candidate: serves("b")}, "no driver read statistics"},
		"zero successes":                {fake.env(), ReadEgressCase{Name: "c", Baseline: empty, Candidate: serves("b")}, "served no work"},
		"missing revision":              {ReadEgressEnv{Backend: "b", CacheMode: "none", BaselineRevision: "x", ReadStats: fake.ledger.Drain}, ReadEgressCase{Name: "c", Baseline: serves("a"), Candidate: serves("b")}, "revisions are required"},
	}
	for name, tc := range cases {
		if _, err := MeasureReadEgress(t.Context(), tc.env, tc.c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestMeasureReadEgressRequireZeroCandidateReads(t *testing.T) {
	fake := newReadEgressFake()
	serves := func(label string) ReadEgressWork {
		return func(context.Context) (ReadEgressOutcome, error) {
			fake.read(t, label, 50, 1)
			return equalOutcome()
		}
	}
	silent := func(context.Context) (ReadEgressOutcome, error) { return equalOutcome() }
	other := func(context.Context) (ReadEgressOutcome, error) {
		return ReadEgressOutcome{Digest: "different", Successes: 3}, nil
	}
	cases := map[string]struct {
		c    ReadEgressCase
		want string
	}{
		"valid zero candidate":       {ReadEgressCase{Name: "c", RequireZeroCandidateReads: true, Baseline: serves("a"), Candidate: silent}, ""},
		"invalid positive candidate": {ReadEgressCase{Name: "c", RequireZeroCandidateReads: true, Baseline: serves("a"), Candidate: serves("b")}, "candidate driver reads must be zero"},
		"empty baseline":             {ReadEgressCase{Name: "c", RequireZeroCandidateReads: true, Baseline: silent, Candidate: silent}, "statistics are missing"},
		"outcome mismatch":           {ReadEgressCase{Name: "c", RequireZeroCandidateReads: true, Baseline: serves("a"), Candidate: other}, "different work"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := MeasureReadEgress(t.Context(), fake.env(), tc.c)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func mustDigest(t *testing.T) string {
	t.Helper()
	outcome, err := equalOutcome()
	if err != nil {
		t.Fatal(err)
	}
	return outcome.Digest
}

func TestReadEgressRegistryWritesMarkedJSONPerCase(t *testing.T) {
	fake := newReadEgressFake()
	registry := NewReadEgressRegistry[struct{}]()
	for _, name := range []string{"one", "two"} {
		registry.Register(name, func(t testing.TB, _ context.Context, _ struct{}) ReadEgressCase {
			return ReadEgressCase{Name: name,
				Baseline: func(context.Context) (ReadEgressOutcome, error) {
					fake.read(t.(*testing.T), "legacy", 64, 3)
					return equalOutcome()
				},
				Candidate: func(context.Context) (ReadEgressOutcome, error) {
					fake.read(t.(*testing.T), "public", 32, 3)
					return equalOutcome()
				}}
		})
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate registration was accepted")
			}
		}()
		registry.Register("one", func(testing.TB, context.Context, struct{}) ReadEgressCase { return ReadEgressCase{} })
	}()
	var out bytes.Buffer
	env := fake.env()
	env.Out = &out
	if reports := registry.Run(t, t.Context(), struct{}{}, env); len(reports) != 2 {
		t.Fatalf("reports %d", len(reports))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, ReadEgressMarker) {
			t.Fatalf("line %q lacks marker", line)
		}
		var decoded ReadEgressReport
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, ReadEgressMarker)), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Case != registry.Names()[i] || decoded.Backend != "fake" || decoded.CandidateRevision != "cand" || len(decoded.Trials) != 2*ReadEgressTrials {
			t.Fatalf("decoded %+v", decoded)
		}
	}
}
