package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"gopkg.in/yaml.v3"
)

func namedSetupFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	second := local.Setups[0]
	second.Name = "secondary"
	second.ExecutionSettings.Spec.Model = "secondary-spec"
	second.Review.Seats = append([]config.ReviewSeat(nil), second.Review.Seats...)
	local.Setups = append(local.Setups, second)
	if err = writeValidatedLocalExecutionConfig(path, local); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNamedSetupMutationsPreserveDefaultProjection(t *testing.T) {
	path := namedSetupFixture(t)
	if err := setNamedLocalExecutionField(path, "secondary", "execution.spec.model", "updated-secondary"); err != nil {
		t.Fatal(err)
	}
	if err := setNamedLocalExecutionField(path, "secondary", "review.seat.2.model", "updated-seat-two"); err != nil {
		t.Fatal(err)
	}
	local, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secondary, _, err := namedSetup(local, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if local.DefaultSetup != "default" || local.ExecutionSettings.Spec.Model == "updated-secondary" {
		t.Fatalf("named update changed default projection: %+v", local)
	}
	if secondary.ExecutionSettings.Spec.Model != "updated-secondary" || secondary.Review.Seats[1].Model != "updated-seat-two" {
		t.Fatalf("secondary=%+v", secondary)
	}
	if err = setDefaultExecutionSetup(path, "secondary"); err != nil {
		t.Fatal(err)
	}
	local, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if local.DefaultSetup != "secondary" || local.ExecutionSettings.Spec.Model != "updated-secondary" {
		t.Fatalf("default projection=%+v", local.ExecutionSettings)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted config.WorkspaceDocument
	if err = yaml.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.DefaultSetup != "secondary" || persisted.ExecutionSettings == nil || persisted.ExecutionSettings.Spec.Model != "updated-secondary" || persisted.Review.Seats[1].Model != "updated-seat-two" {
		t.Fatalf("persisted default projection=%+v", persisted)
	}
}

func TestNamedSetupDeleteRulesAndCompleteListing(t *testing.T) {
	path := namedSetupFixture(t)
	if err := deleteNamedExecutionSetup(path, "default"); err == nil || !strings.Contains(err.Error(), "cannot delete default") {
		t.Fatalf("default delete error=%v", err)
	}
	if err := setDefaultExecutionSetup(path, "secondary"); err != nil {
		t.Fatal(err)
	}
	if err := deleteNamedExecutionSetup(path, "default"); err != nil {
		t.Fatal(err)
	}
	if err := deleteNamedExecutionSetup(path, "secondary"); err == nil || !strings.Contains(err.Error(), "last remaining") {
		t.Fatalf("last delete error=%v", err)
	}
	var output bytes.Buffer
	if err := printNamedExecutionSetups(&output, path); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"secondary (default)", "spec", "implement", "review.seat.1", "review.seat.2", "priority order"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("listing missing %q:\n%s", want, output.String())
		}
	}
}

func TestSelectTemplateHarnessIncludesCursor(t *testing.T) {
	var destination string
	var harnesses []config.Harness
	if err := selectTemplateHarness("cursor", &destination, &harnesses); err != nil {
		t.Fatal(err)
	}
	if destination != "cursor" || len(harnesses) != 1 || harnesses[0].Name != "cursor" || harnesses[0].Command[0] != "cursor-agent" {
		t.Fatalf("destination=%q harnesses=%+v", destination, harnesses)
	}
	if err := selectTemplateHarness("other", &destination, &harnesses); err == nil || !strings.Contains(err.Error(), "codex, claude, grok, or cursor") {
		t.Fatalf("error=%v", err)
	}
}

func TestNamedCursorSelectionClearsUnsupportedEffort(t *testing.T) {
	harnesses := []config.Harness{config.HarnessTemplates()[0].Harness}
	implementation := config.ImplementationSettings{Harness: "codex", Effort: "high"}
	if err := setImplementationField(&implementation, "harness", "cursor", &harnesses); err != nil {
		t.Fatal(err)
	}
	seat := config.ReviewSeat{Harness: "codex", Effort: "high"}
	if err := setReviewSeatField(&seat, "harness", "cursor", &harnesses); err != nil {
		t.Fatal(err)
	}
	if implementation.Effort != "" || seat.Effort != "" {
		t.Fatalf("implementation=%+v seat=%+v", implementation, seat)
	}
}

func TestNamedReviewSeatsAddRemoveAndMovePreservePriorityOrder(t *testing.T) {
	path := namedSetupFixture(t)
	third := config.ReviewSeat{Harness: "local-agent", Model: "third", Effort: "high"}
	if err := mutateNamedReviewSeats(path, "secondary", func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
		return append(seats, third), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mutateNamedReviewSeats(path, "secondary", func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
		seat := seats[2]
		return append([]config.ReviewSeat{seat}, seats[:2]...), nil
	}); err != nil {
		t.Fatal(err)
	}
	local, _ := config.Load(path)
	setup, _, _ := namedSetup(local, "secondary")
	if setup.Review.Seats[0].Model != "third" || len(setup.Review.Seats) != 3 {
		t.Fatalf("seats=%+v", setup.Review.Seats)
	}
	if err := mutateNamedReviewSeats(path, "secondary", func(seats []config.ReviewSeat) ([]config.ReviewSeat, error) {
		return append(seats[:1], seats[2:]...), nil
	}); err != nil {
		t.Fatal(err)
	}
	local, _ = config.Load(path)
	setup, _, _ = namedSetup(local, "secondary")
	if len(setup.Review.Seats) != 2 || setup.Review.Seats[0].Model != "third" {
		t.Fatalf("seats=%+v", setup.Review.Seats)
	}
}

func TestNamedSetupResolutionAndOverflowArePreClaimDecisions(t *testing.T) {
	path := namedSetupFixture(t)
	selected, err := loadNamedLocalExecutionSetup(path, "secondary")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Config.DefaultSetup != "secondary" || selected.Config.ExecutionSettings.Spec.Model != "secondary-spec" {
		t.Fatalf("selected=%+v", selected.Config)
	}
	if _, err = loadNamedLocalExecutionSetup(path, "missing"); err == nil || !strings.Contains(err.Error(), "configured setups: default, secondary") {
		t.Fatalf("unknown error=%v", err)
	}
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "review-overflow", Stage: core.StageReview, ReviewSeat: 3}}
	_, err = selectLocalWorkerDispatch(item, selected.Config, &bytes.Buffer{})
	var capacity *reviewSeatCapacityError
	if !errors.As(err, &capacity) || capacity.Required != 3 || capacity.Configured != 2 {
		t.Fatalf("overflow error=%v", err)
	}
	var log bytes.Buffer
	if !logWorkerReviewSeatOverflow(&log, item, selected.Config, err) || !strings.Contains(log.String(), "leaving order queued without claiming") {
		t.Fatalf("worker log=%q", log.String())
	}
}

func TestRunCommandExposesPerInvocationSetupSelection(t *testing.T) {
	flag := runCmd().Flags().Lookup("setup")
	if flag == nil || flag.DefValue != "" || !strings.Contains(flag.Usage, "does not change") {
		t.Fatalf("setup flag=%+v", flag)
	}
	command := setupCmd()
	for _, args := range [][]string{{"create"}, {"list"}, {"edit"}, {"delete"}, {"default"}, {"seat", "add"}, {"seat", "remove"}, {"seat", "move"}} {
		if _, _, err := command.Find(args); err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
	}
}
