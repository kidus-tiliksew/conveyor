package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

type fakeGenesisStore struct {
	opts   postgresstore.GenesisOptions
	report postgresstore.GenesisReport
	err    error
	closed bool
}

func (f *fakeGenesisStore) ImportGenesis(_ context.Context, opts postgresstore.GenesisOptions) (postgresstore.GenesisReport, error) {
	f.opts = opts
	if opts.Progress != nil {
		opts.Progress("progress line")
	}
	return f.report, f.err
}

func (f *fakeGenesisStore) Close() { f.closed = true }

func TestMigrateLogRequiresDatabaseURL(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "")
	cmd := newMigrateLogCmd(func(context.Context, string) (genesisImporter, error) {
		t.Fatal("store opened without a database url")
		return nil, nil
	})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL") {
		t.Fatalf("err=%v", err)
	}
}

func TestMigrateLogPassesFlagsAndPrintsReport(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example/db")
	fake := &fakeGenesisStore{report: postgresstore.GenesisReport{
		Workspaces: []postgresstore.GenesisWorkspaceReport{{
			Workspace: "demo", HistoryImported: 12, HistoryAlreadyInLog: 3,
			SnapshotsWritten: map[string]int{"task": 4, "workspace": 1}, SnapshotsUnchanged: 2, MarkerWritten: true,
		}},
		Deployment: postgresstore.GenesisWorkspaceReport{Workspace: eventlog.DeploymentWorkspace, SnapshotsWritten: map[string]int{"user": 2}},
	}}
	var opened string
	cmd := newMigrateLogCmd(func(_ context.Context, databaseURL string) (genesisImporter, error) {
		opened = databaseURL
		return fake, nil
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--workspace-id", "demo", "--workspace-id", "ff-demo-2", "--batch", "500"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if opened != "postgres://example/db" || !fake.closed {
		t.Fatalf("opened=%q closed=%t", opened, fake.closed)
	}
	if len(fake.opts.Workspaces) != 2 || fake.opts.BatchSize != 500 {
		t.Fatalf("opts=%+v", fake.opts)
	}
	out := stdout.String()
	for _, want := range []string{"demo: history imported 12 (already in log 3)", "task=4 workspace=1", "unchanged 2, marker true", "deployment: snapshots written user=2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "progress line") {
		t.Fatalf("progress not routed to stderr: %q", stderr.String())
	}
}

func TestMigrateLogJSONSuppressesProgress(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example/db")
	fake := &fakeGenesisStore{report: postgresstore.GenesisReport{Workspaces: []postgresstore.GenesisWorkspaceReport{{Workspace: "demo", SnapshotsWritten: map[string]int{}}}}}
	cmd := newMigrateLogCmd(func(context.Context, string) (genesisImporter, error) { return fake, nil })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"workspace": "demo"`) {
		t.Fatalf("json stdout=%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("progress leaked into stderr under --json: %q", stderr.String())
	}
}

type fakeParityStore struct {
	reports map[string]postgresstore.ParityReport
	err     error
	closed  bool
	opts    postgresstore.ParityOptions
}

func (f *fakeParityStore) LogParity(_ context.Context, workspace string, opts postgresstore.ParityOptions) (postgresstore.ParityReport, error) {
	f.opts = opts
	if f.err != nil {
		return postgresstore.ParityReport{}, f.err
	}
	report, ok := f.reports[workspace]
	if !ok {
		return postgresstore.ParityReport{}, errors.New("unknown workspace " + workspace)
	}
	return report, nil
}

func (f *fakeParityStore) Close() { f.closed = true }

func TestLogParityRequiresWorkspace(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example/db")
	cmd := newLogParityCmd(func(context.Context, string) (parityChecker, error) {
		t.Fatal("store opened without a workspace")
		return nil, nil
	})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--workspace-id") {
		t.Fatalf("err=%v", err)
	}
}

func TestLogParityCleanExitsZeroAndDriftExitsNonZero(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example/db")
	fake := &fakeParityStore{reports: map[string]postgresstore.ParityReport{
		"clean": {Workspace: "clean", Streams: 3, Position: 9, Families: []postgresstore.ParityFamilyReport{{Family: "task", Match: 3}}},
		"drift": {Workspace: "drift", Streams: 2, Position: 5, Families: []postgresstore.ParityFamilyReport{{
			Family: "task", Match: 1, Drift: 1,
			UnfoldedKinds: map[string]int{"task.hold.set": 1},
			Drifts:        []postgresstore.ParityDrift{{Stream: eventlog.TaskStream("t2"), KindsSince: []string{"task.hold.set"}}},
		}}},
	}}
	open := func(context.Context, string) (parityChecker, error) { return fake, nil }

	cmd := newLogParityCmd(open)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--workspace-id", "clean", "--max-drifts", "7"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("clean workspace err=%v", err)
	}
	if fake.opts.MaxDrifts != 7 || !fake.closed {
		t.Fatalf("opts=%+v closed=%t", fake.opts, fake.closed)
	}
	if !strings.Contains(stdout.String(), "clean: 3 streams at position 9") || !strings.Contains(stdout.String(), "match 3") {
		t.Fatalf("stdout=%s", stdout.String())
	}

	cmd = newLogParityCmd(open)
	stdout.Reset()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--workspace-id", "clean", "--workspace-id", "drift"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "drift or missing") {
		t.Fatalf("drifted workspace err=%v", err)
	}
	for _, want := range []string{"drift: 2 streams", "unfolded task.hold.set", "drift task/t2 since: task.hold.set"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}
