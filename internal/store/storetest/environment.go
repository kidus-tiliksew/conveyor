package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// EnvironmentStoredKey is one stored environment key as a test binary reads
// it: Digest is the hash column and Text renders every other column.
type EnvironmentStoredKey struct {
	Digest []byte
	Text   string
}

// runEnvironments holds every backend to component-environments invariants 2
// to 14 and 16 (feature-production-feedback-live FB-1; req-environments
// REQ-1 and REQ-2). Each case starts from a fresh workspace.
func runEnvironments(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"CreateRecordsOneEvent", runEnvironmentCreate},
		{"NameRules", runEnvironmentNames},
		{"VersionRules", runEnvironmentVersionRules},
		{"TimingWindow", runEnvironmentTimingWindow},
		{"TrackedBranches", runEnvironmentTrackedBranches},
		{"GitHubNames", runEnvironmentGitHubNames},
		{"EditsRecordOldAndNew", runEnvironmentEdits},
		{"RefusalsRecordNothing", runEnvironmentRefusals},
		{"ArchiveAndRestore", runEnvironmentArchive},
		{"ListOrderAndFilter", runEnvironmentList},
		{"WorkspaceIsolation", runEnvironmentIsolation},
		{"KeyIssue", runEnvironmentKeyIssue},
		{"KeyLimit", runEnvironmentKeyLimit},
		{"KeyRevocation", runEnvironmentKeyRevocation},
		{"KeyVerification", runEnvironmentKeyVerification},
		{"KeyLastUse", runEnvironmentKeyLastUse},
		{"KeySecrets", runEnvironmentKeySecrets},
		{"Suggestions", runEnvironmentSuggestions},
		{"IgnoredSuggestions", runEnvironmentIgnored},
		{"ActorRequired", runEnvironmentActor},
		{"ConcurrentNames", runEnvironmentConcurrentNames},
		{"ConcurrentRestores", runEnvironmentConcurrentRestores},
		{"ConcurrentKeys", runEnvironmentConcurrentKeys},
		{"ConcurrentSuggestions", runEnvironmentConcurrentSuggestions},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, factory.fresh(t, nil)) })
	}
}

// sameEnvironmentJSON compares the JSON of two values without regard to key
// order, because a backend can reorder the keys of a stored payload.
func sameEnvironmentJSON(t *testing.T, a, b any) bool {
	t.Helper()
	var decoded [2]any
	for i, value := range []any{a, b} {
		raw, err := json.Marshal(value)
		requireOK(t, err)
		requireOK(t, jsonUnmarshal(raw, &decoded[i]))
	}
	return reflect.DeepEqual(decoded[0], decoded[1])
}

func ptr[T any](value T) *T { return &value }

func createEnvironment(t *testing.T, x Fixture, input store.EnvironmentInput) store.EnvironmentRecord {
	t.Helper()
	record, err := x.Backend.CreateEnvironment(x.Context, input)
	requireOK(t, err)
	return record
}

func updateEnvironment(t *testing.T, x Fixture, id string, patch store.EnvironmentPatch) store.EnvironmentRecord {
	t.Helper()
	record, err := x.Backend.UpdateEnvironment(x.Context, id, patch)
	requireOK(t, err)
	return record
}

func archiveEnvironment(t *testing.T, x Fixture, id string, archived bool) store.EnvironmentRecord {
	t.Helper()
	return updateEnvironment(t, x, id, store.EnvironmentPatch{Archived: &archived})
}

func environmentEvents(t *testing.T, x Fixture) []core.Event {
	t.Helper()
	events, err := x.WorkspaceEvents(x.Context, "environment.")
	requireOK(t, err)
	return events
}

// environmentState renders everything a refused request must leave unchanged:
// each environment with its keys, the suggestions, and the events.
func environmentState(t *testing.T, x Fixture) string {
	t.Helper()
	records, err := x.Backend.ListEnvironmentRecords(x.Context, store.EnvironmentListQuery{})
	requireOK(t, err)
	suggestions, err := x.Backend.ListEnvironmentSuggestions(x.Context)
	requireOK(t, err)
	var events []string
	for _, event := range environmentEvents(t, x) {
		events = append(events, fmt.Sprintf("%d %s", event.ID, event.Kind))
	}
	raw, err := json.Marshal([]any{records, suggestions, events})
	requireOK(t, err)
	return string(raw)
}

func requireEnvironmentUnchanged(t *testing.T, x Fixture, before, what string) {
	t.Helper()
	if after := environmentState(t, x); after != before {
		t.Fatalf("%s changed the stored state:\nbefore %s\nafter  %s", what, before, after)
	}
}

func requireEnvironmentField(t *testing.T, err error, field string, mentions ...string) {
	t.Helper()
	var refused *store.EnvironmentFieldError
	if !errors.As(err, &refused) || refused.Field != field {
		t.Fatalf("err=%v, want a refusal of %s", err, field)
	}
	for _, text := range mentions {
		if !strings.Contains(refused.Message, text) {
			t.Fatalf("refusal %q does not name %q", refused.Message, text)
		}
	}
}

func requireEnvironmentNotFound(t *testing.T, err error) {
	t.Helper()
	var missing *store.EnvironmentNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("err=%v, want environment not found", err)
	}
}

func requireEnvironmentArchived(t *testing.T, err error) {
	t.Helper()
	var archived *store.EnvironmentArchivedError
	if !errors.As(err, &archived) {
		t.Fatalf("err=%v, want environment archived", err)
	}
}

func requireInvalidEnvironmentKey(t *testing.T, x Fixture, value, what string) {
	t.Helper()
	verified, err := x.Backend.VerifyEnvironmentKey(x.Context, value)
	if !errors.Is(err, store.ErrInvalidEnvironmentKey) || err.Error() != store.ErrInvalidEnvironmentKey.Error() || verified != (store.VerifiedEnvironmentKey{}) {
		t.Fatalf("%s: verified=%+v err=%v, want the one refusal", what, verified, err)
	}
}

func environmentChanges(t *testing.T, event core.Event) map[string]store.EnvironmentChange {
	t.Helper()
	var payload store.EnvironmentEventPayload
	requireOK(t, jsonUnmarshal(event.Payload, &payload))
	return payload.Changes
}

// environmentRace runs every act at once and returns their results. An act
// that answers ErrRetryable runs again, as a caller does.
func environmentRace(acts ...func() error) []error {
	results := make([]error, len(acts))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, act := range acts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				if results[i] = act(); !errors.Is(results[i], store.ErrRetryable) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	close(start)
	wg.Wait()
	return results
}

func environmentWinners(results []error) int {
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		}
	}
	return won
}

func runEnvironmentCreate(t *testing.T, x Fixture) {
	record := createEnvironment(t, x, store.EnvironmentInput{
		Name: "  Production  ", TrackedBranches: map[string]string{"conveyor": "main", "app": "release/1"}, GitHubEnvironments: []string{" prod ", "PROD", "production-eu"},
		VersionRule: &store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "v*-{commit}"}, TimingWindowMinutes: ptr(90),
	})
	if !regexp.MustCompile(`^env-\d{6}-[0-9a-f]{6}$`).MatchString(record.ID) {
		t.Fatalf("environment ID %q is not env-<yymmdd>-<6 hex>", record.ID)
	}
	if record.Name != "Production" || !maps.Equal(record.TrackedBranches, map[string]string{"conveyor": "main", "app": "release/1"}) || !slices.Equal(record.GitHubEnvironments, []string{"prod", "production-eu"}) ||
		record.VersionRule == nil || *record.VersionRule != (store.EnvironmentVersionRule{Kind: "commit_pattern", Text: "v*-{commit}"}) || record.TimingWindowMinutes != 90 || record.Archived || record.CreatedBy == "" || record.CreatedAt.IsZero() || len(record.Keys) != 0 {
		t.Fatalf("created environment = %+v", record)
	}
	records, err := x.Backend.ListEnvironmentRecords(x.Context, store.EnvironmentListQuery{})
	requireOK(t, err)
	if len(records) != 1 || !sameEnvironmentJSON(t, records[0], record) {
		t.Fatalf("read records = %+v, created %+v", records, record)
	}
	events := environmentEvents(t, x)
	if len(events) != 1 || events[0].Kind != store.EnvironmentCreatedEvent || events[0].ActorID == "" || events[0].TaskID != "" {
		t.Fatalf("events = %+v, want one environment.created with an actor", events)
	}
	changes := environmentChanges(t, events[0])
	if len(changes) != 6 {
		t.Fatalf("environment.created changes = %+v, want every field", changes)
	}
	for field, change := range changes {
		if change.Old != nil {
			t.Fatalf("environment.created %s old = %v, want null", field, change.Old)
		}
	}
	if changes["name"].New != "Production" || changes["timing_window_minutes"].New != float64(90) || changes["archived"].New != false {
		t.Fatalf("environment.created changes = %+v", changes)
	}
	archived := createEnvironment(t, x, store.EnvironmentInput{Name: "Old", Archived: true})
	if !archived.Archived {
		t.Fatalf("environment created archived = %+v", archived)
	}
}

func runEnvironmentNames(t *testing.T, x Fixture) {
	longest := strings.Repeat("é", 40)
	for _, name := range []string{"Production", "École", "Kilo", longest} {
		createEnvironment(t, x, store.EnvironmentInput{Name: name})
	}
	before := environmentState(t, x)
	for _, name := range []string{"", "   ", strings.Repeat("a", 41), strings.Repeat("é", 41), "bad\x00name", "bad\xffname"} {
		_, err := x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: name})
		requireEnvironmentField(t, err, "name")
	}
	// The last name holds the Kelvin sign, which equals k without regard to
	// case.
	for taken, name := range map[string]string{"Production": " pRODUCTION ", "École": "éCOLE", "Kilo": "Kilo", longest: strings.Repeat("É", 40)} {
		_, err := x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: name})
		requireEnvironmentField(t, err, "name", fmt.Sprintf("%q", taken))
	}
	requireEnvironmentUnchanged(t, x, before, "a refused name")
	// Names that differ in more than case are different on every backend.
	for _, name := range []string{"Ecole", "Production 2", "Pröduction"} {
		createEnvironment(t, x, store.EnvironmentInput{Name: name})
	}
	staging := createEnvironment(t, x, store.EnvironmentInput{Name: "Staging"})
	_, err := x.Backend.UpdateEnvironment(x.Context, staging.ID, store.EnvironmentPatch{Name: ptr("PRODUCTION")})
	requireEnvironmentField(t, err, "name", `"Production"`)
	// An environment can change the case of its own name.
	if renamed := updateEnvironment(t, x, staging.ID, store.EnvironmentPatch{Name: ptr("STAGING")}); renamed.Name != "STAGING" {
		t.Fatalf("renamed = %+v", renamed)
	}
}

func runEnvironmentVersionRules(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	if environment.VersionRule != nil {
		t.Fatalf("version rule = %+v, want none", environment.VersionRule)
	}
	rule := func(kind, text string) store.EnvironmentPatch {
		return store.EnvironmentPatch{VersionRuleSet: true, VersionRule: &store.EnvironmentVersionRule{Kind: kind, Text: text}}
	}
	for _, accepted := range [][2]string{
		{"commit_pattern", "{commit}"}, {"commit_pattern", "*-{commit}-*"}, {"commit_pattern", strings.Repeat("é", 192) + "{commit}"},
		{"tag_template", "{version}"}, {"tag_template", "{service}/v{version}"}, {"tag_template", "@{service}/app@{version}"}, {"tag_template", "refs/heads/{version}"}, {"tag_template", strings.Repeat("a", 191) + "{version}"},
	} {
		updated := updateEnvironment(t, x, environment.ID, rule(accepted[0], accepted[1]))
		if updated.VersionRule == nil || updated.VersionRule.Kind != accepted[0] || updated.VersionRule.Text != accepted[1] {
			t.Fatalf("stored rule = %+v, want %v", updated.VersionRule, accepted)
		}
	}
	before := environmentState(t, x)
	for _, refused := range [][2]string{
		{"commit_pattern", ""}, {"commit_pattern", "*"}, {"commit_pattern", "{commit}{commit}"}, {"commit_pattern", "{Commit}"}, {"commit_pattern", strings.Repeat("a", 193) + "{commit}"},
		{"tag_template", ""}, {"tag_template", "{service}"}, {"tag_template", "{version}{version}"}, {"tag_template", "{service}{service}{version}"}, {"tag_template", strings.Repeat("a", 192) + "{version}"},
		// Each of these holds the right placeholders and names no valid tag.
		{"tag_template", "release {version}"}, {"tag_template", " {version}"}, {"tag_template", "{version} "}, {"tag_template", "{service} {version}"}, {"tag_template", "v{version}\t"}, {"tag_template", "v{version}\x7f"},
		{"tag_template", "v..{version}"}, {"tag_template", "{version}."}, {"tag_template", ".{version}"}, {"tag_template", "{service}/.{version}"}, {"tag_template", "{version}.lock"}, {"tag_template", "{service}.lock/{version}"},
		{"tag_template", "/{version}"}, {"tag_template", "{version}/"}, {"tag_template", "{service}//{version}"}, {"tag_template", "-{version}"}, {"tag_template", "v@{{version}"},
		{"tag_template", "v~{version}"}, {"tag_template", "v^{version}"}, {"tag_template", "v:{version}"}, {"tag_template", "v?{version}"}, {"tag_template", "v*{version}"}, {"tag_template", "v[{version}"}, {"tag_template", `v\{version}`},
		{"", "{commit}"}, {"regex", "{commit}"}, {"COMMIT_PATTERN", "{commit}"}, {"commit_pattern", "{commit}\x00"},
	} {
		_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, rule(refused[0], refused[1]))
		requireEnvironmentField(t, err, "version_rule")
		_, err = x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: "Other", VersionRule: rule(refused[0], refused[1]).VersionRule})
		requireEnvironmentField(t, err, "version_rule")
	}
	requireEnvironmentUnchanged(t, x, before, "a refused version rule")
	// An omitted rule stays, and an explicit null clears it.
	if kept := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{TimingWindowMinutes: ptr(5)}); kept.VersionRule == nil {
		t.Fatal("an omitted version rule was cleared")
	}
	if cleared := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{VersionRuleSet: true}); cleared.VersionRule != nil {
		t.Fatalf("cleared rule = %+v", cleared.VersionRule)
	}
	events := environmentEvents(t, x)
	if change := environmentChanges(t, events[len(events)-1])["version_rule"]; change.Old == nil || change.New != nil {
		t.Fatalf("version rule change = %+v, want old rule and null", change)
	}
}

func runEnvironmentTimingWindow(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Default"})
	if environment.TimingWindowMinutes != 60 {
		t.Fatalf("default window = %d, want 60", environment.TimingWindowMinutes)
	}
	for _, minutes := range []int{1, 1440} {
		if updated := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{TimingWindowMinutes: &minutes}); updated.TimingWindowMinutes != minutes {
			t.Fatalf("window = %d, want %d", updated.TimingWindowMinutes, minutes)
		}
	}
	before := environmentState(t, x)
	for _, minutes := range []int{-1, 0, 1441} {
		_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, store.EnvironmentPatch{TimingWindowMinutes: &minutes})
		requireEnvironmentField(t, err, "timing_window_minutes")
		_, err = x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: "Other", TimingWindowMinutes: &minutes})
		requireEnvironmentField(t, err, "timing_window_minutes")
	}
	requireEnvironmentUnchanged(t, x, before, "a refused window")
}

func runEnvironmentTrackedBranches(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", TrackedBranches: map[string]string{"conveyor": "main"}})
	before := environmentState(t, x)
	for _, branches := range []map[string]string{{"missing-repo": "main"}, {"conveyor": "main", "missing-repo": "main"}, {"CONVEYOR": "main"}, {"": "main"}} {
		named := ""
		for repository := range branches {
			if repository != "conveyor" {
				named = fmt.Sprintf("%q", repository)
			}
		}
		_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, store.EnvironmentPatch{TrackedBranches: &branches})
		requireEnvironmentField(t, err, "tracked_branches", named)
		_, err = x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: "Other", TrackedBranches: branches})
		requireEnvironmentField(t, err, "tracked_branches", named)
	}
	for _, branch := range []string{"", " ", "two words", "a..b", "refs/heads/main", "-x", "x.lock", "bad\x00", strings.Repeat("a", 256)} {
		_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, store.EnvironmentPatch{TrackedBranches: &map[string]string{"app": branch}})
		requireEnvironmentField(t, err, "tracked_branches")
	}
	requireEnvironmentUnchanged(t, x, before, "refused tracked branches")
	replaced := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{TrackedBranches: &map[string]string{"app": "release/2"}})
	if !maps.Equal(replaced.TrackedBranches, map[string]string{"app": "release/2"}) {
		t.Fatalf("replaced branches = %v", replaced.TrackedBranches)
	}
	if emptied := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{TrackedBranches: &map[string]string{}}); len(emptied.TrackedBranches) != 0 {
		t.Fatalf("emptied branches = %v", emptied.TrackedBranches)
	}
}

func runEnvironmentGitHubNames(t *testing.T, x Fixture) {
	longest := strings.Repeat("é", 255)
	ten := []string{longest}
	for i := range 9 {
		ten = append(ten, fmt.Sprintf("prod-%d", i))
	}
	production := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", GitHubEnvironments: ten})
	if !slices.Equal(production.GitHubEnvironments, ten) {
		t.Fatalf("names = %v", production.GitHubEnvironments)
	}
	staging := createEnvironment(t, x, store.EnvironmentInput{Name: "Staging"})
	before := environmentState(t, x)
	for _, names := range [][]string{{""}, {"  "}, {strings.Repeat("a", 256)}, {"bad\x00"}, append(slices.Clone(ten), "eleven")} {
		_, err := x.Backend.UpdateEnvironment(x.Context, staging.ID, store.EnvironmentPatch{GitHubEnvironments: &names})
		requireEnvironmentField(t, err, "github_environments")
	}
	for _, name := range []string{"PROD-0", strings.Repeat("É", 255)} {
		_, err := x.Backend.UpdateEnvironment(x.Context, staging.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"mine", name}})
		requireEnvironmentField(t, err, "github_environments", `"Production"`)
		_, err = x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: "Other", GitHubEnvironments: []string{name}})
		requireEnvironmentField(t, err, "github_environments", `"Production"`)
	}
	requireEnvironmentUnchanged(t, x, before, "refused GitHub names")
	// An environment can keep its own names, and an archived environment
	// holds none against another.
	updateEnvironment(t, x, production.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"Prod-0"}})
	archiveEnvironment(t, x, production.ID, true)
	updateEnvironment(t, x, staging.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"prod-0"}})
}

func runEnvironmentEdits(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"prod"}})
	before := environmentState(t, x)
	// An edit to the stored values changes nothing.
	same := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{Name: ptr("Production"), TrackedBranches: &map[string]string{}, GitHubEnvironments: &[]string{"prod"}, VersionRuleSet: true, TimingWindowMinutes: ptr(60), Archived: ptr(false)})
	if !sameEnvironmentJSON(t, same, environment) {
		t.Fatalf("unchanged edit = %+v, want %+v", same, environment)
	}
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{})
	requireEnvironmentUnchanged(t, x, before, "an edit to the stored values")
	updated := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{Name: ptr("Prod"), TrackedBranches: &map[string]string{"app": "main"}, GitHubEnvironments: &[]string{"prod", "prod-eu"}, VersionRuleSet: true, VersionRule: &store.EnvironmentVersionRule{Kind: "tag_template", Text: "v{version}"}, TimingWindowMinutes: ptr(15)})
	if updated.ID != environment.ID || updated.CreatedBy != environment.CreatedBy || !updated.CreatedAt.Equal(environment.CreatedAt) || updated.Name != "Prod" {
		t.Fatalf("updated = %+v", updated)
	}
	events := environmentEvents(t, x)
	if len(events) != 2 || events[1].Kind != store.EnvironmentUpdatedEvent || events[1].ActorID == "" {
		t.Fatalf("events = %+v, want one environment.updated", events)
	}
	changes := environmentChanges(t, events[1])
	want := map[string]store.EnvironmentChange{
		"name":                  {Old: "Production", New: "Prod"},
		"tracked_branches":      {Old: map[string]any{}, New: map[string]any{"app": "main"}},
		"github_environments":   {Old: []any{"prod"}, New: []any{"prod", "prod-eu"}},
		"version_rule":          {Old: nil, New: map[string]any{"kind": "tag_template", "text": "v{version}"}},
		"timing_window_minutes": {Old: 60, New: 15},
	}
	if !sameEnvironmentJSON(t, changes, want) {
		t.Fatalf("changes = %+v, want %+v", changes, want)
	}
	// An edit and an archive in one request record one event.
	archived := updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{Name: ptr("Retired"), Archived: ptr(true)})
	events = environmentEvents(t, x)
	if !archived.Archived || len(events) != 3 || events[2].Kind != store.EnvironmentArchivedEvent || len(environmentChanges(t, events[2])) != 2 {
		t.Fatalf("archived = %+v events = %+v", archived, events)
	}
}

func runEnvironmentRefusals(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	before := environmentState(t, x)
	// One bad field refuses the whole edit (component-environments invariant
	// 8).
	for field, patch := range map[string]store.EnvironmentPatch{
		"tracked_branches":      {Name: ptr("Renamed"), TrackedBranches: &map[string]string{"missing": "main"}},
		"version_rule":          {Name: ptr("Renamed"), VersionRuleSet: true, VersionRule: &store.EnvironmentVersionRule{Kind: "tag_template"}},
		"timing_window_minutes": {Name: ptr("Renamed"), TimingWindowMinutes: ptr(0)},
		"github_environments":   {Name: ptr("Renamed"), GitHubEnvironments: &[]string{""}},
	} {
		_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, patch)
		requireEnvironmentField(t, err, field)
	}
	_, err := x.Backend.UpdateEnvironment(x.Context, "env-000000-000000", store.EnvironmentPatch{Name: ptr("Renamed")})
	requireEnvironmentNotFound(t, err)
	_, err = x.Backend.IssueEnvironmentKey(x.Context, "env-000000-000000", "deploy")
	requireEnvironmentNotFound(t, err)
	requireEnvironmentNotFound(t, x.Backend.RevokeEnvironmentKey(x.Context, "env-000000-000000", "envkey_missing"))
	requireEnvironmentNotFound(t, x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, "envkey_missing"))
	for _, label := range []string{"", "  ", strings.Repeat("é", 61), "bad\x00"} {
		_, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, label)
		requireEnvironmentField(t, err, "label")
	}
	requireEnvironmentField(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, " "), "github_environment")
	requireEnvironmentField(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, strings.Repeat("a", 256)), "github_environment")
	for _, report := range []store.EnvironmentReport{{Repository: "app", ReportedAt: time.Now()}, {GitHubEnvironment: strings.Repeat("a", 256), Repository: "app", ReportedAt: time.Now()}} {
		_, err = x.Backend.RecordEnvironmentReport(x.Context, report)
		requireEnvironmentField(t, err, "github_environment")
	}
	_, err = x.Backend.RecordEnvironmentReport(x.Context, store.EnvironmentReport{GitHubEnvironment: "prod", Repository: "app"})
	requireEnvironmentField(t, err, "reported_at")
	requireEnvironmentUnchanged(t, x, before, "a refused request")
}

func runEnvironmentArchive(t *testing.T, x Fixture) {
	production := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"prod"}})
	kept, err := x.Backend.IssueEnvironmentKey(x.Context, production.ID, "kept")
	requireOK(t, err)
	revoked, err := x.Backend.IssueEnvironmentKey(x.Context, production.ID, "revoked")
	requireOK(t, err)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, production.ID, revoked.ID))
	archived := archiveEnvironment(t, x, production.ID, true)
	if !archived.Archived || len(archived.Keys) != 1 || archived.Keys[0].ID != kept.ID {
		t.Fatalf("archived = %+v", archived)
	}
	requireInvalidEnvironmentKey(t, x, kept.Value, "a key of an archived environment")
	before := environmentState(t, x)
	for _, patch := range []store.EnvironmentPatch{
		{Name: ptr("Renamed")}, {Name: ptr("Production")}, {TrackedBranches: &map[string]string{}}, {GitHubEnvironments: &[]string{}}, {VersionRuleSet: true}, {TimingWindowMinutes: ptr(60)},
		{Name: ptr("Renamed"), Archived: ptr(false)}, {Name: ptr("Renamed"), Archived: ptr(true)},
	} {
		_, err := x.Backend.UpdateEnvironment(x.Context, production.ID, patch)
		requireEnvironmentArchived(t, err)
	}
	_, err = x.Backend.IssueEnvironmentKey(x.Context, production.ID, "another")
	requireEnvironmentArchived(t, err)
	archiveEnvironment(t, x, production.ID, true)
	requireEnvironmentUnchanged(t, x, before, "an act an archived environment refuses")

	// An archived environment holds no name against another, and a restore
	// passes both uniqueness rules again.
	successor := createEnvironment(t, x, store.EnvironmentInput{Name: "production"})
	_, err = x.Backend.UpdateEnvironment(x.Context, production.ID, store.EnvironmentPatch{Archived: ptr(false)})
	requireEnvironmentField(t, err, "name", `"production"`)
	updateEnvironment(t, x, successor.ID, store.EnvironmentPatch{Name: ptr("Successor"), GitHubEnvironments: &[]string{"PROD"}})
	_, err = x.Backend.UpdateEnvironment(x.Context, production.ID, store.EnvironmentPatch{Archived: ptr(false)})
	requireEnvironmentField(t, err, "github_environments", `"Successor"`)
	requireInvalidEnvironmentKey(t, x, kept.Value, "a key after a refused restore")
	updateEnvironment(t, x, successor.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{}})
	restored := archiveEnvironment(t, x, production.ID, false)
	if restored.Archived || !slices.Equal(restored.GitHubEnvironments, []string{"prod"}) {
		t.Fatalf("restored = %+v", restored)
	}
	if verified, err := x.Backend.VerifyEnvironmentKey(x.Context, kept.Value); err != nil || verified.KeyID != kept.ID {
		t.Fatalf("restored key = %+v %v", verified, err)
	}
	requireInvalidEnvironmentKey(t, x, revoked.Value, "a revoked key after a restore")
	events := environmentEvents(t, x)
	last := events[len(events)-1]
	if last.Kind != store.EnvironmentRestoredEvent || !sameEnvironmentJSON(t, environmentChanges(t, last), map[string]any{"archived": map[string]any{"old": true, "new": false}}) {
		t.Fatalf("restore event = %s %s", last.Kind, last.Payload)
	}
	// A key can be revoked while its environment is archived.
	archiveEnvironment(t, x, production.ID, true)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, production.ID, kept.ID))
	archiveEnvironment(t, x, production.ID, false)
	requireInvalidEnvironmentKey(t, x, kept.Value, "a key revoked while archived")
}

func runEnvironmentList(t *testing.T, x Fixture) {
	names := []string{"beta", "Alpha", "gamma", "Old"}
	for _, name := range names {
		createEnvironment(t, x, store.EnvironmentInput{Name: name, Archived: name == "Old"})
	}
	for _, c := range []struct {
		archived *bool
		want     []string
	}{{ptr(false), []string{"Alpha", "beta", "gamma"}}, {ptr(true), []string{"Old"}}, {nil, []string{"Alpha", "beta", "gamma", "Old"}}} {
		query := store.EnvironmentListQuery{Archived: c.archived}
		environments, err := x.Backend.ListEnvironments(x.Context, query)
		requireOK(t, err)
		records, err := x.Backend.ListEnvironmentRecords(x.Context, query)
		requireOK(t, err)
		var plain, withKeys []string
		for i := range environments {
			plain = append(plain, environments[i].Name)
			withKeys = append(withKeys, records[i].Name)
			if environments[i].TrackedBranches == nil || environments[i].GitHubEnvironments == nil || records[i].Keys == nil {
				t.Fatalf("row %+v holds a null list", records[i])
			}
		}
		if !slices.Equal(plain, c.want) || !slices.Equal(withKeys, c.want) || len(records) != len(environments) {
			t.Fatalf("listed %v and %v, want %v", plain, withKeys, c.want)
		}
	}
	raw, err := json.Marshal(mustEnvironments(t, x))
	requireOK(t, err)
	if bytes.Contains(raw, []byte(`"keys"`)) {
		t.Fatalf("the key-free list carries keys: %s", raw)
	}
}

func mustEnvironments(t *testing.T, x Fixture) []store.Environment {
	t.Helper()
	environments, err := x.Backend.ListEnvironments(x.Context, store.EnvironmentListQuery{})
	requireOK(t, err)
	return environments
}

func runEnvironmentIsolation(t *testing.T, x Fixture) {
	other := x.Workspace + "-other"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), other)
	if _, err := x.Backend.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "elsewhere", URL: "https://example.test/elsewhere", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	y := x
	y.Workspace, y.Context = other, ctx
	mine := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"prod"}})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, mine.ID, "mine")
	requireOK(t, err)
	// Names are unique per workspace, and repositories come from the bound
	// workspace's own configuration.
	theirs := createEnvironment(t, y, store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"prod"}, TrackedBranches: map[string]string{"elsewhere": "main"}})
	_, err = x.Backend.UpdateEnvironment(x.Context, mine.ID, store.EnvironmentPatch{TrackedBranches: &map[string]string{"elsewhere": "main"}})
	requireEnvironmentField(t, err, "tracked_branches", `"elsewhere"`)
	_, err = y.Backend.UpdateEnvironment(y.Context, theirs.ID, store.EnvironmentPatch{TrackedBranches: &map[string]string{"app": "main"}})
	requireEnvironmentField(t, err, "tracked_branches", `"app"`)
	before, beforeOther := environmentState(t, x), environmentState(t, y)
	_, err = y.Backend.UpdateEnvironment(y.Context, mine.ID, store.EnvironmentPatch{Name: ptr("Taken")})
	requireEnvironmentNotFound(t, err)
	_, err = y.Backend.IssueEnvironmentKey(y.Context, mine.ID, "theirs")
	requireEnvironmentNotFound(t, err)
	requireEnvironmentNotFound(t, y.Backend.RevokeEnvironmentKey(y.Context, mine.ID, key.ID))
	requireEnvironmentNotFound(t, y.Backend.RevokeEnvironmentKey(y.Context, theirs.ID, key.ID))
	if recorded, err := x.Backend.RecordEnvironmentReport(x.Context, store.EnvironmentReport{GitHubEnvironment: "preview", Repository: "elsewhere", ReportedAt: time.Now()}); err != nil || recorded {
		t.Fatalf("report of another workspace's repository recorded=%v err=%v", recorded, err)
	}
	requireEnvironmentUnchanged(t, x, before, "a foreign act")
	requireEnvironmentUnchanged(t, y, beforeOther, "a foreign act")
	if environments := mustEnvironments(t, y); len(environments) != 1 || environments[0].ID != theirs.ID {
		t.Fatalf("other workspace lists %+v", environments)
	}
	recorded, err := y.Backend.RecordEnvironmentReport(y.Context, store.EnvironmentReport{GitHubEnvironment: "preview", Repository: "elsewhere", ReportedAt: time.Now()})
	if err != nil || !recorded {
		t.Fatalf("report recorded=%v err=%v", recorded, err)
	}
	if suggestions, err := x.Backend.ListEnvironmentSuggestions(x.Context); err != nil || len(suggestions) != 0 {
		t.Fatalf("another workspace's suggestion is listed: %+v %v", suggestions, err)
	}
	// The key alone names its workspace, whatever the context binds.
	for name, verifyCtx := range map[string]context.Context{"own": x.Context, "other": y.Context, "none": t.Context()} {
		verified, err := x.Backend.VerifyEnvironmentKey(verifyCtx, key.Value)
		if err != nil || verified.Workspace != x.Workspace || verified.EnvironmentID != mine.ID {
			t.Fatalf("%s workspace context: verified=%+v err=%v", name, verified, err)
		}
	}
}

func runEnvironmentKeyIssue(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	issued, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "  deploy script ")
	requireOK(t, err)
	if !regexp.MustCompile(`^envkey_[A-Za-z0-9_-]{16}$`).MatchString(issued.ID) {
		t.Fatalf("key ID %q is not envkey_<12 bytes in URL-safe base64>", issued.ID)
	}
	secret, ok := strings.CutPrefix(issued.Value, "cv_env_"+issued.ID+"_")
	if raw, err := base64.RawURLEncoding.DecodeString(secret); !ok || err != nil || len(raw) != 32 {
		t.Fatalf("key value does not have the cv_env_<key id>_<32-byte secret> shape")
	}
	if issued.Label != "deploy script" || issued.CreatedBy == "" || issued.CreatedAt.IsZero() || issued.LastUsedAt != nil {
		t.Fatalf("issued = %+v", issued.EnvironmentKey)
	}
	second, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, strings.Repeat("é", 60))
	requireOK(t, err)
	if second.ID == issued.ID || second.Value == issued.Value {
		t.Fatal("two keys share an ID or a value")
	}
	records, err := x.Backend.ListEnvironmentRecords(x.Context, store.EnvironmentListQuery{})
	requireOK(t, err)
	if len(records) != 1 || len(records[0].Keys) != 2 || !sameEnvironmentJSON(t, records[0].Keys[0], issued.EnvironmentKey) || records[0].Keys[1].ID != second.ID {
		t.Fatalf("records = %+v", records)
	}
	events := environmentEvents(t, x)
	if len(events) != 3 || events[1].Kind != store.EnvironmentKeyIssuedEvent || events[1].ActorID == "" {
		t.Fatalf("events = %+v", events)
	}
	want := map[string]any{"workspace_id": x.Workspace, "environment_id": environment.ID, "key_id": issued.ID, "label": "deploy script"}
	var payload map[string]any
	requireOK(t, jsonUnmarshal(events[1].Payload, &payload))
	if !sameEnvironmentJSON(t, payload, want) {
		t.Fatalf("key event payload = %s", events[1].Payload)
	}
}

func runEnvironmentKeyLimit(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	var keys []store.IssuedEnvironmentKey
	for i := range store.MaxEnvironmentKeys {
		key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, fmt.Sprintf("key %d", i))
		requireOK(t, err)
		keys = append(keys, key)
	}
	before := environmentState(t, x)
	_, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "sixth")
	var limit *store.EnvironmentKeyLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("sixth key err=%v, want the key limit", err)
	}
	requireEnvironmentUnchanged(t, x, before, "a key over the limit")
	// The limit is per environment, and a revoked key frees its slot.
	other := createEnvironment(t, x, store.EnvironmentInput{Name: "Staging"})
	_, err = x.Backend.IssueEnvironmentKey(x.Context, other.ID, "other")
	requireOK(t, err)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, keys[0].ID))
	_, err = x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "sixth")
	requireOK(t, err)
}

func runEnvironmentKeyRevocation(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	other := createEnvironment(t, x, store.EnvironmentInput{Name: "Staging"})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "deploy")
	requireOK(t, err)
	kept, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "kept")
	requireOK(t, err)
	if _, err := x.Backend.VerifyEnvironmentKey(x.Context, key.Value); err != nil {
		t.Fatal(err)
	}
	// Another environment's key answers as a missing one.
	requireEnvironmentNotFound(t, x.Backend.RevokeEnvironmentKey(x.Context, other.ID, key.ID))
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, key.ID))
	requireInvalidEnvironmentKey(t, x, key.Value, "a revoked key")
	events := environmentEvents(t, x)
	last := events[len(events)-1]
	if last.Kind != store.EnvironmentKeyRevokedEvent || last.ActorID == "" || !sameEnvironmentJSON(t, last.Payload, map[string]any{"workspace_id": x.Workspace, "environment_id": environment.ID, "key_id": key.ID, "label": "deploy"}) {
		t.Fatalf("revocation event = %s %s", last.Kind, last.Payload)
	}
	before := environmentState(t, x)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, key.ID))
	requireEnvironmentUnchanged(t, x, before, "revoking a revoked key")
	if strings.Contains(before[:strings.Index(before, "environment.created")], key.ID) {
		t.Fatalf("a read lists the revoked key: %s", before)
	}
	if _, err := x.Backend.VerifyEnvironmentKey(x.Context, kept.Value); err != nil {
		t.Fatalf("the other key: %v", err)
	}
}

func runEnvironmentKeyVerification(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "deploy")
	requireOK(t, err)
	verified, err := x.Backend.VerifyEnvironmentKey(x.Context, key.Value)
	if err != nil || verified != (store.VerifiedEnvironmentKey{Workspace: x.Workspace, EnvironmentID: environment.ID, KeyID: key.ID, Label: "deploy"}) {
		t.Fatalf("verified = %+v %v", verified, err)
	}
	// A rename of the environment changes no identity the key names.
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{Name: ptr("Renamed")})
	if again, err := x.Backend.VerifyEnvironmentKey(x.Context, key.Value); err != nil || again != verified {
		t.Fatalf("verified again = %+v %v", again, err)
	}
	digest := sha256.Sum256([]byte(key.Value))
	for _, value := range []string{
		"", "cv_env_", key.ID, key.Value + "x", key.Value[:len(key.Value)-1], " " + key.Value, strings.ToUpper(key.Value), strings.TrimPrefix(key.Value, "cv_env_"),
		"cv_env_" + key.ID + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32)), string(digest[:]), hex.EncodeToString(digest[:]), "bad\x00value\xff",
	} {
		requireInvalidEnvironmentKey(t, x, value, "a wrong value")
	}
	if _, err := x.Backend.VerifyPersonalAccessToken(x.Context, key.Value); err == nil {
		t.Fatal("a key verified as a personal access token")
	}
	if _, err := x.Backend.VerifyCredential(x.Context, key.Value); err == nil {
		t.Fatal("a key verified as a user credential")
	}
}

func environmentLastUse(t *testing.T, x Fixture, keyID string) *time.Time {
	t.Helper()
	records, err := x.Backend.ListEnvironmentRecords(x.Context, store.EnvironmentListQuery{})
	requireOK(t, err)
	for _, record := range records {
		for _, key := range record.Keys {
			if key.ID == keyID {
				return key.LastUsedAt
			}
		}
	}
	t.Fatalf("key %s is not listed", keyID)
	return nil
}

func runEnvironmentKeyLastUse(t *testing.T, x Fixture) {
	if x.AgeEnvironmentKey == nil {
		t.Fatal("conformance fixture must provide AgeEnvironmentKey for the last-use case")
	}
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "deploy")
	requireOK(t, err)
	verify := func() {
		t.Helper()
		if _, err := x.Backend.VerifyEnvironmentKey(x.Context, key.Value); err != nil {
			t.Fatal(err)
		}
	}
	if at := environmentLastUse(t, x, key.ID); at != nil {
		t.Fatalf("unused key has last use %v", at)
	}
	verify()
	first := environmentLastUse(t, x, key.ID)
	if first == nil || time.Since(*first).Abs() > 10*time.Minute {
		t.Fatalf("first last use = %v", first)
	}
	verify()
	if again := environmentLastUse(t, x, key.ID); again == nil || !again.Equal(*first) {
		t.Fatalf("a second use within a minute moved the last use from %v to %v", first, again)
	}
	x.AgeEnvironmentKey(t, x.Context, key.ID, 30*time.Second)
	aged := environmentLastUse(t, x, key.ID)
	verify()
	if again := environmentLastUse(t, x, key.ID); !again.Equal(*aged) {
		t.Fatalf("a use 30 seconds later moved the last use from %v to %v", aged, again)
	}
	x.AgeEnvironmentKey(t, x.Context, key.ID, time.Minute)
	aged = environmentLastUse(t, x, key.ID)
	verify()
	stamped := environmentLastUse(t, x, key.ID)
	if !stamped.After(*aged) {
		t.Fatalf("a use after a minute kept the last use at %v", stamped)
	}
	// A refused key records no use, and last use appends no event.
	x.AgeEnvironmentKey(t, x.Context, key.ID, time.Hour)
	aged = environmentLastUse(t, x, key.ID)
	archiveEnvironment(t, x, environment.ID, true)
	requireInvalidEnvironmentKey(t, x, key.Value, "a key of an archived environment")
	if again := environmentLastUse(t, x, key.ID); !again.Equal(*aged) {
		t.Fatalf("a refused use moved the last use from %v to %v", aged, again)
	}
	if events := environmentEvents(t, x); len(events) != 3 {
		t.Fatalf("events = %+v, want created, key_issued, and archived", events)
	}
}

// runEnvironmentKeySecrets proves that the server keeps the SHA-256 of a key
// value and nothing else of it, and that no read and no event holds the value
// or the hash, also when a person sends the live value, or another
// credential, back in any field (component-environments invariants 9 and 11;
// req-security-boundaries REQ-2, REQ-3, AC-2.8).
func runEnvironmentKeySecrets(t *testing.T, x Fixture) {
	if x.StoredEnvironmentKey == nil {
		t.Fatal("conformance fixture must provide StoredEnvironmentKey for the secrets case")
	}
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "deploy")
	requireOK(t, err)
	digest := sha256.Sum256([]byte(key.Value))
	secret := strings.TrimPrefix(key.Value, "cv_env_"+key.ID+"_")
	stored := x.StoredEnvironmentKey(t, x.Context, key.ID)
	if !bytes.Equal(stored.Digest, digest[:]) {
		t.Fatal("the stored hash is not the SHA-256 of the value")
	}
	forbidden := []string{key.Value, secret, hex.EncodeToString(digest[:]), strings.ToUpper(hex.EncodeToString(digest[:])), base64.StdEncoding.EncodeToString(digest[:]), base64.RawURLEncoding.EncodeToString(digest[:])}
	if strings.Contains(stored.Text, secret) {
		t.Fatal("a stored column beside the hash holds the secret")
	}
	if _, err := x.Backend.VerifyEnvironmentKey(x.Context, key.Value); err != nil {
		t.Fatal(err)
	}
	// Forty characters, so it fits a name and a label.
	const token = "ghp_Conformance0123456789abcdefABCDEF012"
	forbidden = append(forbidden, token, percentEncoded(key.Value))
	before := environmentState(t, x)
	requireRefused := func(err error, field, what string) {
		t.Helper()
		requireEnvironmentField(t, err, field, "credential")
		for _, value := range forbidden {
			if strings.Contains(err.Error(), value) {
				t.Fatalf("%s: the refusal repeats the secret", what)
			}
		}
	}
	for _, secret := range []string{key.Value, "x" + key.Value, token, percentEncoded(key.Value), "token=conformanceassigned0123456789"} {
		create := func(input store.EnvironmentInput) error {
			if input.Name == "" {
				input.Name = "Other"
			}
			_, err := x.Backend.CreateEnvironment(x.Context, input)
			return err
		}
		update := func(patch store.EnvironmentPatch) error {
			_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, patch)
			return err
		}
		rules := []*store.EnvironmentVersionRule{{Kind: store.EnvironmentCommitPattern, Text: secret + "{commit}"}, {Kind: store.EnvironmentTagTemplate, Text: "{service}/" + secret + "/{version}"}}
		requireRefused(create(store.EnvironmentInput{Name: secret}), "name", "create name")
		requireRefused(update(store.EnvironmentPatch{Name: &secret}), "name", "update name")
		for _, branches := range []map[string]string{{"app": secret}, {secret: "main"}} {
			requireRefused(create(store.EnvironmentInput{TrackedBranches: branches}), "tracked_branches", "create branches")
			requireRefused(update(store.EnvironmentPatch{TrackedBranches: &branches}), "tracked_branches", "update branches")
		}
		requireRefused(create(store.EnvironmentInput{GitHubEnvironments: []string{"prod", secret}}), "github_environments", "create GitHub names")
		requireRefused(update(store.EnvironmentPatch{GitHubEnvironments: &[]string{"prod", secret}}), "github_environments", "update GitHub names")
		for _, rule := range rules {
			requireRefused(create(store.EnvironmentInput{VersionRule: rule}), "version_rule", "create rule")
			requireRefused(update(store.EnvironmentPatch{VersionRuleSet: true, VersionRule: rule}), "version_rule", "update rule")
		}
		_, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, secret)
		requireRefused(err, "label", "key label")
		_, err = x.Backend.RecordEnvironmentReport(x.Context, store.EnvironmentReport{GitHubEnvironment: secret, Repository: "app", ReportedAt: time.Now()})
		requireRefused(err, "github_environment", "report name")
		requireRefused(x.Backend.IgnoreEnvironmentSuggestion(x.Context, secret), "github_environment", "ignored name")
	}
	requireEnvironmentUnchanged(t, x, before, "secret-shaped text")
	// A name that only mentions the prefix is ordinary text.
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"cv_env_ keys"}})
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, key.ID))
	requireInvalidEnvironmentKey(t, x, key.Value, "a revoked key")
	if kept := x.StoredEnvironmentKey(t, x.Context, key.ID); !bytes.Equal(kept.Digest, digest[:]) {
		t.Fatal("revocation lost the key record")
	}
	text := environmentState(t, x)
	for _, event := range environmentEvents(t, x) {
		text += string(event.Payload)
	}
	for _, value := range forbidden {
		if strings.Contains(text, value) {
			t.Fatal("a read or an event holds the key value or its hash")
		}
	}
}

// percentEncoded encodes every byte of value as %XX.
func percentEncoded(value string) string {
	var out strings.Builder
	for i := range len(value) {
		fmt.Fprintf(&out, "%%%02X", value[i])
	}
	return out.String()
}

func recordReport(t *testing.T, x Fixture, name, repository string, at time.Time, want bool) {
	t.Helper()
	recorded, err := x.Backend.RecordEnvironmentReport(x.Context, store.EnvironmentReport{GitHubEnvironment: name, Repository: repository, ReportedAt: at})
	if err != nil || recorded != want {
		t.Fatalf("report %s of %s recorded=%v err=%v, want %v", name, repository, recorded, err, want)
	}
}

func requireSuggestions(t *testing.T, x Fixture, want ...store.EnvironmentSuggestion) {
	t.Helper()
	suggestions, err := x.Backend.ListEnvironmentSuggestions(x.Context)
	requireOK(t, err)
	if !sameEnvironmentJSON(t, suggestions, append([]store.EnvironmentSuggestion{}, want...)) {
		t.Fatalf("suggestions = %+v, want %+v", suggestions, want)
	}
}

func runEnvironmentSuggestions(t *testing.T, x Fixture) {
	base := store.EnvironmentTime(time.Now().Add(-time.Hour))
	requireSuggestions(t, x)
	recordReport(t, x, " Preview ", "app", base, true)
	recordReport(t, x, "canary", "conveyor", base, true)
	requireSuggestions(t, x, store.EnvironmentSuggestion{GitHubEnvironment: "canary", Repository: "conveyor", LastReportedAt: base}, store.EnvironmentSuggestion{GitHubEnvironment: "Preview", Repository: "app", LastReportedAt: base})
	// The newest report wins, and an earlier or repeated one records nothing.
	newest := base.Add(time.Minute)
	recordReport(t, x, "preview", "conveyor", newest, true)
	recordReport(t, x, "PREVIEW", "app", base.Add(time.Second), false)
	recordReport(t, x, "preview", "app", newest, false)
	recordReport(t, x, "preview", "unknown", newest.Add(time.Hour), false)
	canary := store.EnvironmentSuggestion{GitHubEnvironment: "canary", Repository: "conveyor", LastReportedAt: base}
	preview := store.EnvironmentSuggestion{GitHubEnvironment: "preview", Repository: "conveyor", LastReportedAt: newest}
	requireSuggestions(t, x, canary, preview)
	if environments := mustEnvironments(t, x); len(environments) != 0 {
		t.Fatalf("a suggestion created %+v", environments)
	}
	if events := environmentEvents(t, x); len(events) != 0 {
		t.Fatalf("a report appended %+v", events)
	}
	// Listing a name drops its suggestion, and a listed name makes none.
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Preview", GitHubEnvironments: []string{"PREVIEW"}})
	requireSuggestions(t, x, canary)
	recordReport(t, x, "preview", "app", newest.Add(time.Minute), false)
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"preview", "Canary"}})
	requireSuggestions(t, x)
	// An archive and an edit that drops a name bring back no earlier report.
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{"preview"}})
	archiveEnvironment(t, x, environment.ID, true)
	requireSuggestions(t, x)
	// A later report of a name that only an archived environment lists is a
	// suggestion, and a restore lists the name again.
	later := newest.Add(2 * time.Minute)
	recordReport(t, x, "preview", "app", later, true)
	requireSuggestions(t, x, store.EnvironmentSuggestion{GitHubEnvironment: "preview", Repository: "app", LastReportedAt: later})
	archiveEnvironment(t, x, environment.ID, false)
	requireSuggestions(t, x)
	// An environment created archived lists nothing yet.
	recordReport(t, x, "edge", "app", later, true)
	createEnvironment(t, x, store.EnvironmentInput{Name: "Edge", GitHubEnvironments: []string{"edge"}, Archived: true})
	requireSuggestions(t, x, store.EnvironmentSuggestion{GitHubEnvironment: "edge", Repository: "app", LastReportedAt: later})
}

func runEnvironmentIgnored(t *testing.T, x Fixture) {
	at := store.EnvironmentTime(time.Now().Add(-time.Hour))
	recordReport(t, x, "Preview", "app", at, true)
	requireOK(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, " preview "))
	requireSuggestions(t, x)
	events := environmentEvents(t, x)
	if len(events) != 1 || events[0].Kind != store.EnvironmentSuggestionIgnoredEvent || events[0].ActorID == "" ||
		!sameEnvironmentJSON(t, events[0].Payload, map[string]any{"workspace_id": x.Workspace, "github_environment": "Preview", "changes": map[string]any{"ignored": map[string]any{"old": false, "new": true}}}) {
		t.Fatalf("events = %+v", events)
	}
	// The name stays ignored, whatever is reported, until an environment
	// lists it.
	recordReport(t, x, "preview", "app", at.Add(time.Minute), false)
	requireOK(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, "PREVIEW"))
	if events := environmentEvents(t, x); len(events) != 1 {
		t.Fatalf("ignoring an ignored name appended %+v", events)
	}
	// A name can be ignored before any report.
	requireOK(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, "unreported"))
	recordReport(t, x, "unreported", "app", at, false)
	requireSuggestions(t, x)
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Preview", GitHubEnvironments: []string{"preview"}})
	requireEnvironmentField(t, x.Backend.IgnoreEnvironmentSuggestion(x.Context, "Preview"), "github_environment", `"Preview"`)
	updateEnvironment(t, x, environment.ID, store.EnvironmentPatch{GitHubEnvironments: &[]string{}})
	requireSuggestions(t, x)
	recordReport(t, x, "preview", "app", at.Add(2*time.Minute), true)
	requireSuggestions(t, x, store.EnvironmentSuggestion{GitHubEnvironment: "preview", Repository: "app", LastReportedAt: at.Add(2 * time.Minute)})
}

func runEnvironmentActor(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, "deploy")
	requireOK(t, err)
	before := environmentState(t, x)
	bare := store.WithWorkspace(t.Context(), x.Workspace)
	for name, act := range map[string]func() error{
		"create": func() error {
			_, err := x.Backend.CreateEnvironment(bare, store.EnvironmentInput{Name: "Other"})
			return err
		},
		"update": func() error {
			_, err := x.Backend.UpdateEnvironment(bare, environment.ID, store.EnvironmentPatch{Name: ptr("Other")})
			return err
		},
		"issue": func() error {
			_, err := x.Backend.IssueEnvironmentKey(bare, environment.ID, "other")
			return err
		},
		"revoke": func() error { return x.Backend.RevokeEnvironmentKey(bare, environment.ID, key.ID) },
		"ignore": func() error { return x.Backend.IgnoreEnvironmentSuggestion(bare, "preview") },
	} {
		if err := act(); !errors.Is(err, store.ErrMissingActor) {
			t.Fatalf("%s without an actor err=%v", name, err)
		}
	}
	requireEnvironmentUnchanged(t, x, before, "an act without an actor")
	// Events carry the actor of the context.
	user := store.WithActor(x.Context, store.Actor{ID: store.UserActorID("usr-environments"), Role: core.ActorUser})
	record, err := x.Backend.UpdateEnvironment(user, environment.ID, store.EnvironmentPatch{TimingWindowMinutes: ptr(5)})
	requireOK(t, err)
	events := environmentEvents(t, x)
	if last := events[len(events)-1]; last.ActorID != "user:usr-environments" || last.ActorRole != core.ActorUser || record.CreatedBy == last.ActorID {
		t.Fatalf("event actor = %s %s", last.ActorID, last.ActorRole)
	}
}

func runEnvironmentConcurrentNames(t *testing.T, x Fixture) {
	names := []string{"production", "Production", "PRODUCTION", "pRoduction", "prodUction", "productioN"}
	var acts []func() error
	for _, name := range names {
		acts = append(acts, func() error {
			_, err := x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: name})
			return err
		})
	}
	results := environmentRace(acts...)
	if won := environmentWinners(results); won != 1 {
		t.Fatalf("%d creates won one name: %v", won, results)
	}
	for _, err := range results {
		if err != nil {
			requireEnvironmentField(t, err, "name")
		}
	}
	acts = nil
	for i := range 6 {
		acts = append(acts, func() error {
			_, err := x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: fmt.Sprintf("lister %d", i), GitHubEnvironments: []string{strings.ToUpper("prod")[:i%4] + "prod"[i%4:]}})
			return err
		})
	}
	results = environmentRace(acts...)
	if won := environmentWinners(results); won != 1 {
		t.Fatalf("%d creates won one GitHub name: %v", won, results)
	}
	if environments, events := mustEnvironments(t, x), environmentEvents(t, x); len(environments) != 2 || len(events) != 2 {
		t.Fatalf("environments=%+v events=%+v, want two each", environments, events)
	}
}

func runEnvironmentConcurrentRestores(t *testing.T, x Fixture) {
	var acts []func() error
	for range 4 {
		environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"prod"}})
		archiveEnvironment(t, x, environment.ID, true)
		acts = append(acts, func() error {
			_, err := x.Backend.UpdateEnvironment(x.Context, environment.ID, store.EnvironmentPatch{Archived: ptr(false)})
			return err
		})
	}
	results := environmentRace(acts...)
	if won := environmentWinners(results); won != 1 {
		t.Fatalf("%d restores won one name: %v", won, results)
	}
	unarchived, err := x.Backend.ListEnvironments(x.Context, store.EnvironmentListQuery{Archived: ptr(false)})
	requireOK(t, err)
	if len(unarchived) != 1 {
		t.Fatalf("unarchived = %+v", unarchived)
	}
}

func runEnvironmentConcurrentKeys(t *testing.T, x Fixture) {
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: "Production"})
	var mu sync.Mutex
	var keys []store.IssuedEnvironmentKey
	var acts []func() error
	for i := range 8 {
		acts = append(acts, func() error {
			key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, fmt.Sprintf("key %d", i))
			if err == nil {
				mu.Lock()
				defer mu.Unlock()
				keys = append(keys, key)
			}
			return err
		})
	}
	results := environmentRace(acts...)
	if won := environmentWinners(results); won != store.MaxEnvironmentKeys {
		t.Fatalf("%d issues won, want %d: %v", won, store.MaxEnvironmentKeys, results)
	}
	for _, err := range results {
		var limit *store.EnvironmentKeyLimitError
		if err != nil && !errors.As(err, &limit) {
			t.Fatalf("losing issue err=%v", err)
		}
	}
	// Concurrent first uses all verify, and concurrent revocations append one
	// event.
	verify := func() error {
		_, err := x.Backend.VerifyEnvironmentKey(x.Context, keys[0].Value)
		return err
	}
	if results := environmentRace(verify, verify, verify, verify); environmentWinners(results) != 4 {
		t.Fatalf("concurrent verifications = %v", results)
	}
	before := len(environmentEvents(t, x))
	revoke := func() error { return x.Backend.RevokeEnvironmentKey(x.Context, environment.ID, keys[0].ID) }
	if results := environmentRace(revoke, revoke, revoke, revoke); environmentWinners(results) != 4 {
		t.Fatalf("concurrent revocations = %v", results)
	}
	if after := len(environmentEvents(t, x)); after != before+1 {
		t.Fatalf("concurrent revocations appended %d events, want one", after-before)
	}
	requireInvalidEnvironmentKey(t, x, keys[0].Value, "a revoked key")
}

func runEnvironmentConcurrentSuggestions(t *testing.T, x Fixture) {
	at := store.EnvironmentTime(time.Now().Add(-time.Hour))
	report := func(name string, offset time.Duration) func() error {
		return func() error {
			_, err := x.Backend.RecordEnvironmentReport(x.Context, store.EnvironmentReport{GitHubEnvironment: name, Repository: "app", ReportedAt: at.Add(offset)})
			return err
		}
	}
	requireRace := func(acts ...func() error) {
		t.Helper()
		if results := environmentRace(acts...); environmentWinners(results) != len(acts) {
			t.Fatalf("race results = %v", results)
		}
	}
	// Out-of-order reports keep the newest.
	requireRace(report("newest", 3*time.Minute), report("newest", time.Minute), report("newest", 5*time.Minute), report("newest", 2*time.Minute))
	requireSuggestions(t, x, store.EnvironmentSuggestion{GitHubEnvironment: "newest", Repository: "app", LastReportedAt: at.Add(5 * time.Minute)})
	// A report never revives a name that is ignored or listed at the same
	// time.
	ignore := func() error { return x.Backend.IgnoreEnvironmentSuggestion(x.Context, "newest") }
	requireRace(report("newest", 6*time.Minute), ignore, report("newest", 7*time.Minute), ignore)
	requireSuggestions(t, x)
	if events := environmentEvents(t, x); len(events) != 1 {
		t.Fatalf("concurrent ignores appended %+v, want one event", events)
	}
	requireRace(report("listed", time.Minute), func() error {
		_, err := x.Backend.CreateEnvironment(x.Context, store.EnvironmentInput{Name: "Lister", GitHubEnvironments: []string{"LISTED"}})
		return err
	}, report("listed", 2*time.Minute))
	requireSuggestions(t, x)
}
