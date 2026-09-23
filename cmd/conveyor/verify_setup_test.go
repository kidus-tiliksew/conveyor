package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestVerifyLocalSetupFieldsAndListing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.yaml")
	for _, field := range []struct{ key, value string }{{"harness", "codex"}, {"model", "test-model"}, {"effort", "medium"}, {"timeout", "45m"}} {
		if err := setLocalExecutionField(path, "demo", "execution.verify."+field.key, field.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := setLocalExecutionField(path, "demo", "execution.verify_concurrency", "2"); err != nil {
		t.Fatal(err)
	}
	if err := setLocalExecutionField(path, "demo", "execution.verify.timeout", "45m"); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Execution.VerifyConcurrency != 2 {
		t.Fatal("verify concurrency lost")
	}
	item, err := selectLocalExecutionDispatch(workerservice.DispatchOrder{Order: core.WorkOrder{Stage: core.StageVerify}}, loaded)
	if err != nil || item.Harness.Name != "codex" || item.Model != "test-model" || item.Effort != "medium" {
		t.Fatalf("verify dispatch: %+v %v", item, err)
	}
	var output bytes.Buffer
	if err := printLocalExecutionConfig(&output, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "execution.verify.timeout") || !strings.Contains(output.String(), "45m") || !strings.Contains(output.String(), "execution.verify_concurrency") {
		t.Fatalf("verify missing from listing: %s", output.String())
	}
}

func TestMissingVerifySetupRefusesBeforeClaim(t *testing.T) {
	_, err := selectLocalExecutionDispatch(workerservice.DispatchOrder{Order: core.WorkOrder{Stage: core.StageVerify}}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("missing verify route: %v", err)
	}
	remedy := localExecutionSetupRemedy("/local/setup.yaml", err)
	if !strings.Contains(remedy.Error(), localExecutionSetupCommand) {
		t.Fatalf("remedy: %v", remedy)
	}
}

func TestRunVerifyMissingRouteLeavesQueued(t *testing.T) {
	calls, _, err := runTaskSelectionErrorScenario(t, core.StageVerify, 0, func(value string) string { return value })
	if calls != 0 || err == nil || !strings.Contains(err.Error(), localExecutionSetupCommand) {
		t.Fatalf("claims=%d err=%v", calls, err)
	}
}
