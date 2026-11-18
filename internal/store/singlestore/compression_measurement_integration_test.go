package singlestore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// compressionMeasurementMarker prefixes the one JSON line the compression
// measurement writes to standard output for each measured read.
const compressionMeasurementMarker = "COMPRESSION_MEASUREMENT "

// compressionModes are the two connection settings the measurement pairs on
// one SingleStore service: protocol compression requested, the default, and
// disabled.
var compressionModes = []struct {
	name     string
	compress bool
}{{"requested", true}, {"disabled", false}}

// compressionReadCounts is what one measured read received in one mode.
type compressionReadCounts struct {
	Negotiation string `json:"negotiation"`
	Bytes       uint64 `json:"received_bytes"`
	Rows        uint64 `json:"rows"`
	Statements  uint64 `json:"statements"`
	// Successes is the item count an egress variant served; zero for a
	// suite label.
	Successes int `json:"successes,omitempty"`
}

// compressionMeasurement pairs one read's counts across the two modes.
type compressionMeasurement struct {
	Source  string                           `json:"source"`
	Read    string                           `json:"read"`
	Labels  []string                         `json:"labels"`
	Fixture map[string]int                   `json:"fixture,omitempty"`
	Modes   map[string]compressionReadCounts `json:"modes"`
	// RowsEqual, StatementsEqual, and SuccessesEqual compare the modes; an
	// egress read also proved that both of its variants served one digest
	// in each mode.
	RowsEqual       bool   `json:"rows_equal"`
	StatementsEqual bool   `json:"statements_equal"`
	SuccessesEqual  bool   `json:"successes_equal"`
	BytesRatio      string `json:"requested_over_disabled_bytes"`
}

// compressionSuiteLedger drains the store's read ledger for a read-volume
// suite, as the conformance ledger does, and also totals every label the
// suite set itself. Store methods set CamelCase labels; the suites set
// hyphenated ones (component-persistence invariant 31).
type compressionSuiteLedger struct {
	st      *Store
	mu      sync.Mutex
	pending map[string]map[string]uint64
	totals  map[string]compressionReadCounts
}

func newCompressionSuiteLedger(st *Store) *compressionSuiteLedger {
	st.ReadStats(0) // drop what the store read before the suite
	return &compressionSuiteLedger{st: st, totals: map[string]compressionReadCounts{},
		pending: map[string]map[string]uint64{"bytes": {}, "queries": {}, "rows": {}}}
}

func (l *compressionSuiteLedger) collect() {
	for _, total := range l.st.ReadStats(1 << 16).Top {
		l.pending["bytes"][total.Label] += total.Bytes
		l.pending["queries"][total.Label] += total.Queries
		l.pending["rows"][total.Label] += total.Rows
		if strings.Contains(total.Label, "-") {
			counts := l.totals[total.Label]
			counts.Bytes += total.Bytes
			counts.Rows += total.Rows
			counts.Statements += total.Queries
			l.totals[total.Label] = counts
		}
	}
}

func (l *compressionSuiteLedger) drain(metric string) func() map[string]uint64 {
	return func() map[string]uint64 {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.collect()
		drained := l.pending[metric]
		l.pending[metric] = map[string]uint64{}
		return drained
	}
}

func (l *compressionSuiteLedger) labels() map[string]compressionReadCounts {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.collect()
	return l.totals
}

// compressionWorkspace bootstraps a fresh workspace with the one repository
// every reused suite expects.
func compressionWorkspace(t *testing.T, st *Store, prefix string) (context.Context, string, *config.Config) {
	t.Helper()
	workspace := prefix + "-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return ctx, workspace, cfg
}

// TestHotReadCompressionMeasurementIntegration measures the bytes each
// existing measured read receives on one SingleStore service with protocol
// compression requested and with it disabled. It runs only when
// CONVEYOR_COMPRESSION_MEASUREMENT=1 (make measure-singlestore-compression)
// and then requires the database URL instead of skipping.
//
// Each mode opens its own store on the shared database and builds the same
// fixtures in its own workspaces through the public store API, with the
// sizes the suites already use. It runs every registered hot-read egress
// case through the egress harness, which warms both variants, drains the
// read ledger before each call, and requires both variants to serve one
// result digest; it records the medians of five trials. It then runs the
// claim session, renewal, and verification read-volume suites, which keep
// their own assertions, and records the totals of every label those suites
// set. Bytes are counted on the network connection below TLS, so they are
// wire bytes, after compression when it is negotiated (component-persistence
// invariant 43). The handshake of each store's own connections states
// whether compression was negotiated (component-runtime invariant 43).
func TestHotReadCompressionMeasurementIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_COMPRESSION_MEASUREMENT") != "1" {
		t.Skip("CONVEYOR_COMPRESSION_MEASUREMENT=1 selects the SingleStore compression measurement")
	}
	if strings.TrimSpace(os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")) == "" {
		t.Fatal("CONVEYOR_TEST_SINGLESTORE_URL is required for the compression measurement")
	}
	resetSharedDatabase(t)
	version, flags := serverGreeting(t, sharedDatabase.cfg.Addr)
	measurements := map[string]*compressionMeasurement{}
	record := func(source, read string, labels []string, fixture map[string]int, mode string, counts compressionReadCounts) {
		key := source + "\x00" + read
		m := measurements[key]
		if m == nil {
			m = &compressionMeasurement{Source: source, Read: read, Labels: labels, Fixture: fixture, Modes: map[string]compressionReadCounts{}}
			measurements[key] = m
		}
		m.Modes[mode] = counts
	}
	for _, mode := range compressionModes {
		st, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN(), mode.compress)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		requested, negotiation := st.Compression()
		if requested != mode.compress || negotiation == CompressionUnknown {
			t.Fatalf("%s store reports requested=%t negotiation=%s", mode.name, requested, negotiation)
		}
		var memsqlVersion string
		if err := st.db.QueryRowContext(t.Context(), "SELECT @@memsql_version").Scan(&memsqlVersion); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: server %s (memsql_version %s) capability flags 0x%08x; requested=%t negotiation=%s", mode.name, version, memsqlVersion, flags, requested, negotiation)
		t.Run(mode.name+"/egress", func(t *testing.T) {
			ctx, _, _ := compressionWorkspace(t, st, "compression-egress-"+mode.name)
			env, err := storetest.ReadEgressEnvFromProcess("singlestore", st.ReadStats)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			env.Out = &out
			reports := readEgressCases.Run(t, ctx, st, env)
			if len(reports) != len(readEgressCases.Names()) {
				t.Fatalf("measured %d of %d egress cases", len(reports), len(readEgressCases.Names()))
			}
			for _, report := range reports {
				if !report.WorkEqual {
					t.Fatalf("egress case %s served unequal work", report.Case)
				}
				for _, variant := range []string{"baseline", "candidate"} {
					labels := map[string]bool{}
					for _, trial := range report.Trials {
						if trial.Variant == variant {
							for _, label := range trial.Labels {
								labels[label.Label] = true
							}
						}
					}
					names := make([]string, 0, len(labels))
					for label := range labels {
						names = append(names, label)
					}
					sort.Strings(names)
					summary := report.Summary[variant]
					record("egress", report.Case+"/"+variant, names, report.Fixture, mode.name, compressionReadCounts{Negotiation: negotiation,
						Bytes: summary.Bytes.Median, Rows: summary.Rows.Median, Statements: summary.Statements.Median, Successes: report.Successes})
				}
			}
		})
		suite := func(name string, fixture map[string]int, run func(t *testing.T, ledger *compressionSuiteLedger, x storetest.Fixture)) {
			t.Run(mode.name+"/"+name, func(t *testing.T) {
				ctx, workspace, cfg := compressionWorkspace(t, st, "compression-"+name+"-"+mode.name)
				ledger := newCompressionSuiteLedger(st)
				x := storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg,
					DrainReadBytes: ledger.drain("bytes"), DrainReadQueries: ledger.drain("queries"), DrainReadRows: ledger.drain("rows")}
				run(t, ledger, x)
				for label, counts := range ledger.labels() {
					counts.Negotiation = negotiation
					record(name, label, []string{label}, fixture, mode.name, counts)
				}
			})
		}
		scaleFixture := map[string]int{"orders": storetest.ClaimSessionScaleOrders, "snapshot_bytes": storetest.ClaimSessionSnapshotBytes}
		suite("claim-session", scaleFixture, func(t *testing.T, ledger *compressionSuiteLedger, x storetest.Fixture) {
			scale := claimSessionScaleSingleStore(st, x.Workspace)
			scale.DrainReadBytes = ledger.drain("bytes")
			storetest.RunClaimSessionAndActivityReadsAtScale(t, x, scale)
		})
		suite("renewal", scaleFixture, func(t *testing.T, ledger *compressionSuiteLedger, x storetest.Fixture) {
			scale := claimSessionScaleSingleStore(st, x.Workspace)
			scale.DrainReadBytes = ledger.drain("bytes")
			storetest.RunRenewalReads(t, x, scale, func(st store.Store) func(context.Context, core.WorkOrderClaimIdentity, string, time.Duration) (core.WorkOrder, error) {
				return (&workorder.Service{Store: st}).RenewClaimWithLease
			})
		})
		suite("verification", nil, func(t *testing.T, _ *compressionSuiteLedger, x storetest.Fixture) {
			storetest.RunVerificationMeasuredReads(t, x)
		})
	}
	keys := make([]string, 0, len(measurements))
	for key := range measurements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var table strings.Builder
	fmt.Fprintf(&table, "%-14s %-62s %14s %14s %8s\n", "source", "read", "requested B", "disabled B", "ratio")
	for _, key := range keys {
		m := measurements[key]
		on, onOK := m.Modes["requested"]
		off, offOK := m.Modes["disabled"]
		if !onOK || !offOK {
			t.Errorf("%s %s was measured in one mode only", m.Source, m.Read)
			continue
		}
		m.RowsEqual, m.StatementsEqual, m.SuccessesEqual = on.Rows == off.Rows, on.Statements == off.Statements, on.Successes == off.Successes
		m.BytesRatio = "n/a"
		if off.Bytes > 0 {
			m.BytesRatio = fmt.Sprintf("%.3f", float64(on.Bytes)/float64(off.Bytes))
		}
		if m.Source == "egress" && !m.SuccessesEqual {
			t.Errorf("egress %s served %d items with compression requested and %d disabled", m.Read, on.Successes, off.Successes)
		}
		line, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("%s%s\n", compressionMeasurementMarker, line)
		fmt.Fprintf(&table, "%-14s %-62s %14d %14d %8s\n", m.Source, m.Read, on.Bytes, off.Bytes, m.BytesRatio)
	}
	t.Logf("received bytes per measured read, compression requested versus disabled:\n%s", table.String())
}
