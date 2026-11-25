package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runFeedback holds every backend to component-production-feedback invariants
// 1, 3 to 6, and 13 (feature-production-feedback-live FB-2;
// req-production-feedback REQ-1, REQ-2, and REQ-11; req-environments AC-2.4).
// Each case starts from a fresh workspace.
func runFeedback(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"FirstSignal", runFeedbackFirstSignal},
		{"NewAndDroppedVersions", runFeedbackNewAndDropped},
		{"OmittedServiceKeepsVersions", runFeedbackOmittedService},
		{"TwoVersionsOfOneService", runFeedbackTwoVersions},
		{"RepeatAndReappearance", runFeedbackReappearance},
		{"ExactVersions", runFeedbackExactVersions},
		{"LargestSignal", runFeedbackLargestSignal},
		{"Isolation", runFeedbackIsolation},
		{"Source", runFeedbackSource},
		{"NewestSignal", runFeedbackNewestSignal},
		{"EarlierArrival", runFeedbackEarlierArrival},
		{"ArrivalOrder", runFeedbackArrivalOrder},
		{"RecentWindow", runFeedbackRecentWindow},
		{"RepositoryCheck", runFeedbackRepositoryCheck},
		{"RefusalsRecordNothing", runFeedbackRefusals},
		{"KeyValidityAtTheWrite", runFeedbackKeyValidity},
		{"KeyCheckAndLastUse", runFeedbackLastUse},
		{"ObservesOnly", runFeedbackObservesOnly},
		{"ConcurrentSignals", runFeedbackConcurrent},
		{"ConcurrentRevocationAndArchive", runFeedbackConcurrentRefusal},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, factory.fresh(t, nil)) })
	}
}

// feedbackTime is a fixed arrival plus an offset. It holds microseconds,
// which every backend keeps.
func feedbackTime(offset time.Duration) time.Time {
	return time.Date(2026, 11, 25, 12, 0, 0, 123456000, time.UTC).Add(offset)
}

// feedbackEnvironment is an environment with one key.
type feedbackEnvironment struct {
	x   Fixture
	id  string
	key store.IssuedEnvironmentKey
}

func newFeedbackEnvironment(t *testing.T, x Fixture, name, label string) feedbackEnvironment {
	t.Helper()
	environment := createEnvironment(t, x, store.EnvironmentInput{Name: name})
	key, err := x.Backend.IssueEnvironmentKey(x.Context, environment.ID, label)
	requireOK(t, err)
	return feedbackEnvironment{x: x, id: environment.ID, key: key}
}

func (e feedbackEnvironment) signal(offset time.Duration, services ...store.FeedbackSignalService) store.FeedbackSignal {
	return store.FeedbackSignal{EnvironmentID: e.id, KeyID: e.key.ID, ArrivedAt: feedbackTime(offset), NextSignalSeconds: 60, Services: services}
}

func (e feedbackEnvironment) send(t *testing.T, offset time.Duration, services ...store.FeedbackSignalService) {
	t.Helper()
	requireOK(t, e.x.Backend.RecordFeedbackSignal(e.x.Context, e.signal(offset, services...)))
}

func (e feedbackEnvironment) running(t *testing.T) []store.FeedbackRunningVersion {
	t.Helper()
	running, err := e.x.Backend.ListFeedbackRunningVersions(e.x.Context, e.id)
	requireOK(t, err)
	return running
}

func (e feedbackEnvironment) deploys(t *testing.T) []store.FeedbackDeploy {
	t.Helper()
	deploys, err := e.x.Backend.ListFeedbackDeploys(e.x.Context, e.id)
	requireOK(t, err)
	return deploys
}

func (e feedbackEnvironment) newest(t *testing.T) (store.FeedbackSignalState, bool) {
	t.Helper()
	signals, err := e.x.Backend.ListFeedbackSignals(e.x.Context, []string{e.id})
	requireOK(t, err)
	state, ok := signals[e.id]
	return state, ok
}

// state renders everything a refused signal must leave unchanged, the last
// use of each key included.
func (e feedbackEnvironment) state(t *testing.T) string {
	t.Helper()
	newest, _ := e.newest(t)
	raw, err := json.Marshal([]any{environmentState(t, e.x), newest, e.running(t), e.deploys(t)})
	requireOK(t, err)
	return string(raw)
}

// service names one service with versions, each "commit", "@version", or
// "commit@version".
func feedbackService(name string, versions ...string) store.FeedbackSignalService {
	service := store.FeedbackSignalService{Name: name}
	for _, version := range versions {
		commit, text, _ := strings.Cut(version, "@")
		service.Versions = append(service.Versions, store.FeedbackSignalVersion{Commit: commit, Version: text})
	}
	return service
}

func renderFeedbackVersion(v store.FeedbackVersion) string {
	text := v.Commit
	if v.Version != "" {
		text += "@" + v.Version
	}
	return text
}

// requireRunning compares the running versions with "service=version+offset"
// lines, where the offset in seconds is the deploy time.
func (e feedbackEnvironment) requireRunning(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for _, v := range e.running(t) {
		if v.EnvironmentID != e.id || v.Resolved || v.DeployID == "" {
			t.Fatalf("running version=%+v, want an unresolved version of %s with a deploy", v, e.id)
		}
		got = append(got, fmt.Sprintf("%s=%s+%d", v.Service, renderFeedbackVersion(v.FeedbackVersion), int(v.DeployedAt.Sub(feedbackTime(0)).Seconds())))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("running=%v, want %v", got, want)
	}
}

// requireDeploys compares the deploys with "service=version+from" or
// "service=version+from-to" lines.
func (e feedbackEnvironment) requireDeploys(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	open := map[string]store.FeedbackDeploy{}
	seen := map[string]bool{}
	for _, d := range e.deploys(t) {
		if d.EnvironmentID != e.id || d.Resolved || d.ID == "" || seen[d.ID] {
			t.Fatalf("deploy=%+v, want an unresolved deploy of %s with its own ID", d, e.id)
		}
		seen[d.ID] = true
		if d.StoppedAt == nil {
			open[d.ID] = d
		}
		line := fmt.Sprintf("%s=%s+%d", d.Service, renderFeedbackVersion(d.FeedbackVersion), int(d.DeployedAt.Sub(feedbackTime(0)).Seconds()))
		if d.StoppedAt != nil {
			line += fmt.Sprintf("-%d", int(d.StoppedAt.Sub(feedbackTime(0)).Seconds()))
		}
		got = append(got, line)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("deploys=%v, want %v", got, want)
	}
	// The running versions are exactly the deploys that have not stopped.
	running := e.running(t)
	for _, v := range running {
		if d, ok := open[v.DeployID]; !ok || d.Service != v.Service || d.FeedbackVersion != v.FeedbackVersion || !d.DeployedAt.Equal(v.DeployedAt) {
			t.Fatalf("running version=%+v names deploy=%+v, want its own deploy that has not stopped", v, d)
		}
	}
	if len(running) != len(open) {
		t.Fatalf("running=%d versions, want one for each of %d deploys that have not stopped", len(running), len(open))
	}
}

func requireFeedbackField(t *testing.T, err error, field string) {
	t.Helper()
	var refused *store.FeedbackFieldError
	if !errors.As(err, &refused) || refused.Field != field || refused.Message == "" {
		t.Fatalf("err=%v, want a refusal of %s", err, field)
	}
}

func runFeedbackFirstSignal(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	if _, ok := e.newest(t); ok {
		t.Fatal("an environment with no signal has a newest signal")
	}
	e.requireRunning(t)
	e.requireDeploys(t)
	service := feedbackService("api", "abcdef0@1.0")
	service.Repository = "conveyor"
	e.send(t, 0, service, feedbackService("web", "@2.0"))
	e.requireRunning(t, "api=abcdef0@1.0+0", "web=@2.0+0")
	e.requireDeploys(t, "api=abcdef0@1.0+0", "web=@2.0+0")
	deploys := e.deploys(t)
	if deploys[0].FeedbackVersion != (store.FeedbackVersion{Repository: "conveyor", Commit: "abcdef0", Version: "1.0"}) || !deploys[0].DeployedAt.Equal(feedbackTime(0)) || deploys[1].Repository != "" {
		t.Fatalf("deploys=%+v", deploys)
	}
}

func runFeedbackNewAndDropped(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.send(t, 0, feedbackService("api", "@1"))
	// A new version beside the old one deploys and stops nothing.
	e.send(t, 10*time.Second, feedbackService("api", "@1", "@2"))
	e.requireRunning(t, "api=@1+0", "api=@2+10")
	e.requireDeploys(t, "api=@1+0", "api=@2+10")
	// A dropped version stops, and the kept one keeps its deploy time.
	e.send(t, 20*time.Second, feedbackService("api", "@2"))
	e.requireRunning(t, "api=@2+10")
	e.requireDeploys(t, "api=@1+0-20", "api=@2+10")
	// One signal can replace every version.
	e.send(t, 30*time.Second, feedbackService("api", "@3"))
	e.requireRunning(t, "api=@3+30")
	e.requireDeploys(t, "api=@1+0-20", "api=@2+10-30", "api=@3+30")
	for _, d := range e.deploys(t) {
		if (d.StoppedAt == nil) != (d.StopOrigin == nil) {
			t.Fatalf("deploy=%+v, want a stop time with its origin or neither", d)
		}
	}
}

func runFeedbackOmittedService(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.send(t, 0, feedbackService("api", "@1", "@2"), feedbackService("web", "@1"))
	e.send(t, 10*time.Second, feedbackService("web", "@2"))
	e.send(t, 20*time.Second, feedbackService("worker", "@1"))
	e.requireRunning(t, "api=@1+0", "api=@2+0", "web=@2+10", "worker=@1+20")
	e.requireDeploys(t, "api=@1+0", "api=@2+0", "web=@1+0-10", "web=@2+10", "worker=@1+20")
}

func runFeedbackTwoVersions(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.send(t, 0, feedbackService("api", "abcdef0@1", "abcdef1@2"))
	e.requireRunning(t, "api=abcdef0@1+0", "api=abcdef1@2+0")
	e.send(t, 10*time.Second, feedbackService("api", "abcdef1@2", "abcdef0@1"))
	e.requireRunning(t, "api=abcdef0@1+0", "api=abcdef1@2+0")
	e.requireDeploys(t, "api=abcdef0@1+0", "api=abcdef1@2+0")
	e.send(t, 20*time.Second, feedbackService("api", "abcdef1@2", "abcdef2@3"))
	e.requireRunning(t, "api=abcdef1@2+0", "api=abcdef2@3+20")
	e.requireDeploys(t, "api=abcdef0@1+0-20", "api=abcdef1@2+0", "api=abcdef2@3+20")
}

func runFeedbackReappearance(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	// A version named twice in one signal is one version.
	e.send(t, 0, feedbackService("api", "@1", "@1", "@1"))
	e.requireDeploys(t, "api=@1+0")
	first := e.running(t)[0].DeployID
	// A repeated signal records nothing but the newest signal.
	e.send(t, 10*time.Second, feedbackService("api", "@1"))
	e.requireRunning(t, "api=@1+0")
	e.requireDeploys(t, "api=@1+0")
	if newest, _ := e.newest(t); !newest.LastSignalAt.Equal(feedbackTime(10 * time.Second)) {
		t.Fatalf("newest=%+v, want the repeat", newest)
	}
	e.send(t, 20*time.Second, feedbackService("api", "@2"))
	e.send(t, 30*time.Second, feedbackService("api", "@1"))
	e.requireRunning(t, "api=@1+30")
	e.requireDeploys(t, "api=@1+0-20", "api=@2+20-30", "api=@1+30")
	if again := e.running(t)[0].DeployID; again == first {
		t.Fatal("a version that came back kept its stopped deploy")
	}
	// Another round leaves the first stop unchanged.
	e.send(t, 40*time.Second, feedbackService("api", "@2"))
	e.requireDeploys(t, "api=@1+0-20", "api=@2+20-30", "api=@1+30-40", "api=@2+40")
}

// runFeedbackExactVersions proves that names and versions are kept and
// compared byte for byte: case, padding, omitted members, and the split
// between members each make a different version or service.
func runFeedbackExactVersions(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	long := strings.Repeat("é", 199)
	versions := []string{"abcdef0", "ABCDEF0", "abcdef0@", "@abcdef0", "abcdef0@abcdef0", "@v1", "@V1", "@v1 ", "@ v1"}
	service := feedbackService("api", versions[0], versions[1], versions[3], versions[4], versions[5], versions[6], versions[7], versions[8], "@"+long+"a", "@"+long+"b")
	names := []string{"API", "api ", " api", "äpi", strings.Repeat("ü", 99) + "a", strings.Repeat("ü", 99) + "b"}
	services := []store.FeedbackSignalService{service}
	for _, name := range names {
		services = append(services, feedbackService(name, "@1"))
	}
	e.send(t, 0, services...)
	running := e.running(t)
	if len(running) != 10+len(names) || len(e.deploys(t)) != len(running) {
		t.Fatalf("running=%+v, want %d versions", running, 10+len(names))
	}
	stored := map[string]bool{}
	for _, v := range running {
		stored[v.Service+"|"+v.Commit+"|"+v.Version] = true
	}
	for _, want := range []string{"api|abcdef0|", "api|ABCDEF0|", "api||abcdef0", "api|abcdef0|abcdef0", "api||v1", "api||V1", "api||v1 ", "api|| v1", "api||" + long + "a", "api||" + long + "b", "api |" + "|1", " api||1", "API||1", "äpi||1"} {
		if !stored[want] {
			t.Fatalf("stored versions lack %q: %v", want, stored)
		}
	}
	e.send(t, 10*time.Second, services...)
	if len(e.deploys(t)) != len(running) {
		t.Fatal("an identical signal recorded a deploy")
	}
	// The stated repository is part of the version.
	e.send(t, 20*time.Second, feedbackService("repo", "abcdef0"))
	for i, repository := range []string{"conveyor", "app", ""} {
		named := feedbackService("repo", "abcdef0")
		named.Repository = repository
		e.send(t, time.Duration(30+i)*time.Second, named)
		var current []string
		for _, v := range e.running(t) {
			if v.Service == "repo" {
				current = append(current, v.Repository)
			}
		}
		if !slices.Equal(current, []string{repository}) {
			t.Fatalf("repositories=%v, want %q", current, repository)
		}
	}
}

func runFeedbackLargestSignal(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	build := func(generation int) []store.FeedbackSignalService {
		var services []store.FeedbackSignalService
		for i := range store.MaxFeedbackServices {
			service := feedbackService(fmt.Sprintf("service-%d", i))
			for j := range store.MaxFeedbackVersions {
				service.Versions = append(service.Versions, store.FeedbackSignalVersion{Version: fmt.Sprintf("%d.%d", generation, j)})
			}
			services = append(services, service)
		}
		return services
	}
	total := store.MaxFeedbackServices * store.MaxFeedbackVersions
	e.send(t, 0, build(1)...)
	e.send(t, 10*time.Second, build(2)...)
	deploys, stopped := e.deploys(t), 0
	for _, d := range deploys {
		if d.StoppedAt != nil {
			stopped++
		}
	}
	if running := e.running(t); len(running) != total || len(deploys) != 2*total || stopped != total {
		t.Fatalf("running=%d deploys=%d stopped=%d, want %d %d %d", len(running), len(deploys), stopped, total, 2*total, total)
	}
}

func runFeedbackIsolation(t *testing.T, x Fixture) {
	other := x.Workspace + "-feedback"
	y := x
	y.Workspace, y.Context = other, store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), other)
	_, err := x.Backend.BootstrapWorkspaceConfig(y.Context, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "elsewhere", URL: "https://example.test/elsewhere", Base: "main"}}})
	requireOK(t, err)
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	theirs := newFeedbackEnvironment(t, y, "Production", "watcher")
	production.send(t, 0, feedbackService("api", "@1"))
	staging.send(t, 10*time.Second, feedbackService("api", "@2"))
	theirs.send(t, 20*time.Second, feedbackService("api", "@3"))
	production.requireRunning(t, "api=@1+0")
	staging.requireRunning(t, "api=@2+10")
	theirs.requireRunning(t, "api=@3+20")
	production.send(t, 30*time.Second, feedbackService("api", "@4"))
	staging.requireDeploys(t, "api=@2+10")
	theirs.requireDeploys(t, "api=@3+20")
	// A read names only the asked environments of the bound workspace.
	signals, err := x.Backend.ListFeedbackSignals(x.Context, []string{production.id, theirs.id, "env-missing"})
	requireOK(t, err)
	if len(signals) != 1 || !signals[production.id].LastSignalAt.Equal(feedbackTime(30*time.Second)) {
		t.Fatalf("signals=%+v", signals)
	}
	if signals, err := x.Backend.ListFeedbackSignals(x.Context, nil); err != nil || len(signals) != 0 {
		t.Fatalf("signals of no environment=%+v err=%v", signals, err)
	}
	foreign := theirs
	foreign.x = x
	foreign.requireRunning(t)
	foreign.requireDeploys(t)
	// A key records only in its own workspace and environment.
	before, beforeTheirs := production.state(t), theirs.state(t)
	for name, signal := range map[string]store.FeedbackSignal{
		"another environment's key": {EnvironmentID: production.id, KeyID: staging.key.ID},
		"another workspace's key":   {EnvironmentID: production.id, KeyID: theirs.key.ID},
		"another workspace":         {EnvironmentID: theirs.id, KeyID: theirs.key.ID},
		"a missing environment":     {EnvironmentID: "env-missing", KeyID: production.key.ID},
		"no environment":            {KeyID: production.key.ID},
		"a missing key":             {EnvironmentID: production.id, KeyID: "envkey_missing"},
		"no key":                    {EnvironmentID: production.id},
	} {
		signal.ArrivedAt, signal.NextSignalSeconds, signal.Services = feedbackTime(time.Hour), 60, []store.FeedbackSignalService{feedbackService("api", "@9")}
		if err := x.Backend.RecordFeedbackSignal(x.Context, signal); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
			t.Fatalf("%s: err=%v, want the one refusal", name, err)
		}
	}
	// A repository of another workspace is refused.
	service := feedbackService("api", "@9")
	service.Repository = "elsewhere"
	requireFeedbackField(t, x.Backend.RecordFeedbackSignal(x.Context, production.signal(time.Hour, service)), "services[0].repository")
	if production.state(t) != before || theirs.state(t) != beforeTheirs {
		t.Fatal("a foreign signal changed a record")
	}
	service.Repository = "elsewhere"
	theirs.send(t, time.Hour, service)
}

func runFeedbackSource(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "first watcher")
	second, err := x.Backend.IssueEnvironmentKey(x.Context, e.id, "second watcher")
	requireOK(t, err)
	e.send(t, 0, feedbackService("api", "@1"))
	later := e
	later.key = second
	later.send(t, 10*time.Second, feedbackService("api", "@2"))
	origin := func(label string) store.FeedbackOrigin {
		return store.FeedbackOrigin{Source: store.FeedbackSourceSignal, Label: label, Actor: core.SystemActorID}
	}
	deploys := e.deploys(t)
	if len(deploys) != 2 || deploys[0].Origin != origin("first watcher") || deploys[0].StopOrigin == nil || *deploys[0].StopOrigin != origin("second watcher") || deploys[1].Origin != origin("second watcher") || deploys[1].StopOrigin != nil {
		t.Fatalf("deploys=%+v", deploys)
	}
	if newest, ok := e.newest(t); !ok || newest.Origin != origin("second watcher") || newest.EnvironmentID != e.id {
		t.Fatalf("newest=%+v", newest)
	}
	// Everything recorded under a revoked key stays (req-environments
	// AC-2.2).
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, e.id, e.key.ID))
	if again := e.deploys(t); len(again) != 2 || again[0].Origin != origin("first watcher") {
		t.Fatalf("deploys after the revocation=%+v", again)
	}
}

// runFeedbackNewestSignal covers the listening state at each edge of
// component-production-feedback invariant 13.
func runFeedbackNewestSignal(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	send := func(offset time.Duration, seconds int) store.FeedbackSignalState {
		t.Helper()
		signal := e.signal(offset, feedbackService("api", "@1"))
		signal.NextSignalSeconds = seconds
		requireOK(t, x.Backend.RecordFeedbackSignal(x.Context, signal))
		state, ok := e.newest(t)
		if !ok || !state.LastSignalAt.Equal(feedbackTime(offset)) || state.NextSignalSeconds != seconds {
			t.Fatalf("newest=%+v, want %d seconds at %v", state, seconds, feedbackTime(offset))
		}
		return state
	}
	require := func(state store.FeedbackSignalState, at time.Duration, want bool) {
		t.Helper()
		if got := state.Listening(feedbackTime(at)); got != want {
			t.Fatalf("listening at %v=%v, want %v", at, got, want)
		}
	}
	state := send(0, 60)
	require(state, 0, true)
	require(state, 2*time.Minute-time.Microsecond, true)
	require(state, 2*time.Minute, false)
	require(state, time.Hour, false)
	state = send(time.Minute, 10)
	require(state, 80*time.Second-time.Microsecond, true)
	require(state, 80*time.Second, false)
	state = send(2*time.Minute, 86400)
	require(state, 2*time.Minute+48*time.Hour-time.Microsecond, true)
	require(state, 2*time.Minute+48*time.Hour, false)
	// The state survives an archive.
	archiveEnvironment(t, x, e.id, true)
	if state, ok := e.newest(t); !ok || state.NextSignalSeconds != 86400 {
		t.Fatalf("newest after the archive=%+v", state)
	}
}

// runFeedbackEarlierArrival stores signals after a later arrival committed.
// Each one records its deploys and stops at its own arrival and ends at the
// next arrival that names the service. What runs now and the newest signal
// stay (component-production-feedback invariants 4 and 5;
// req-production-feedback AC-1.3).
func runFeedbackEarlierArrival(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "on time")
	second, err := x.Backend.IssueEnvironmentKey(x.Context, e.id, "overtaken")
	requireOK(t, err)
	late := e
	late.key = second
	origin := func(label string) store.FeedbackOrigin {
		return store.FeedbackOrigin{Source: store.FeedbackSourceSignal, Label: label, Actor: core.SystemActorID}
	}
	// requireOrigins names who started and who stopped the deploy of a version
	// at an offset. An empty stopped means that it still runs.
	requireOrigins := func(version string, offset time.Duration, started, stopped string) {
		t.Helper()
		for _, d := range e.deploys(t) {
			if d.Version != version || !d.DeployedAt.Equal(feedbackTime(offset)) {
				continue
			}
			if d.Origin != origin(started) || (stopped == "") != (d.StopOrigin == nil) || (d.StopOrigin != nil && *d.StopOrigin != origin(stopped)) {
				t.Fatalf("deploy=%+v stop=%+v, want started by %q and stopped by %q", d, d.StopOrigin, started, stopped)
			}
			return
		}
		t.Fatalf("no deploy of %s at %v", version, offset)
	}
	requireNewest := func(offset time.Duration, seconds int) {
		t.Helper()
		if newest, _ := e.newest(t); !newest.LastSignalAt.Equal(feedbackTime(offset)) || newest.NextSignalSeconds != seconds || newest.Origin != origin("on time") {
			t.Fatalf("newest=%+v, want the arrival at %v", newest, offset)
		}
	}
	e.send(t, 0, feedbackService("api", "@1"))
	later := e.signal(20*time.Second, feedbackService("api", "@2"))
	later.NextSignalSeconds = 120
	requireOK(t, x.Backend.RecordFeedbackSignal(x.Context, later))
	// A version only the earlier arrival names ran from 10 to 20 seconds. A
	// version it keeps is unchanged, and a service it alone names runs on.
	late.send(t, 10*time.Second, feedbackService("api", "@1", "@3"), feedbackService("web", "@1"))
	e.requireRunning(t, "api=@2+20", "web=@1+10")
	e.requireDeploys(t, "api=@1+0-20", "api=@3+10-20", "api=@2+20", "web=@1+10")
	requireOrigins("3", 10*time.Second, "overtaken", "on time")
	requireOrigins("1", 0, "on time", "on time")
	requireNewest(20*time.Second, 120)
	// A version the earlier arrival drops stops there. It runs now, so it came
	// back with the next arrival, which changed nothing when it was stored.
	e.send(t, 40*time.Second, feedbackService("api", "@2"))
	late.send(t, 30*time.Second, feedbackService("api", "@1"))
	e.requireRunning(t, "api=@2+40", "web=@1+10")
	e.requireDeploys(t, "api=@1+0-20", "api=@3+10-20", "api=@2+20-30", "api=@1+30-40", "api=@2+40", "web=@1+10")
	requireOrigins("2", 20*time.Second, "on time", "overtaken")
	requireOrigins("2", 40*time.Second, "on time", "")
	requireNewest(40*time.Second, 60)
	// The next arrival is the first one after it, not the newest one, and an
	// arrival that does not name the service is not one.
	e.send(t, 55*time.Second, feedbackService("web", "@1"))
	e.send(t, 60*time.Second, feedbackService("api", "@2"))
	e.send(t, 80*time.Second, feedbackService("api", "@2"))
	late.send(t, 50*time.Second, feedbackService("api", "@9"))
	e.requireRunning(t, "api=@2+60", "web=@1+10")
	e.requireDeploys(t, "api=@1+0-20", "api=@3+10-20", "api=@2+20-30", "api=@1+30-40", "api=@2+40-50", "api=@9+50-60", "api=@2+60", "web=@1+10")
	// An overtaken signal can be overtaken in turn, and it then ends a record
	// that an earlier pass wrote.
	late.send(t, 55*time.Second, feedbackService("api", "@8"))
	e.requireDeploys(t, "api=@1+0-20", "api=@3+10-20", "api=@2+20-30", "api=@1+30-40", "api=@2+40-50", "api=@9+50-55", "api=@8+55-60", "api=@2+60", "web=@1+10")
	// A version that the next arrival deploys was deployed here.
	e.send(t, 100*time.Second, feedbackService("api", "@5"))
	late.send(t, 90*time.Second, feedbackService("api", "@2", "@5"))
	e.requireRunning(t, "api=@5+90", "web=@1+10")
	e.requireDeploys(t, "api=@1+0-20", "api=@3+10-20", "api=@2+20-30", "api=@1+30-40", "api=@2+40-50", "api=@9+50-55", "api=@8+55-60", "api=@2+60-100", "api=@5+90", "web=@1+10")
	requireOrigins("5", 90*time.Second, "overtaken", "")
	requireNewest(100*time.Second, 60)
	// The same arrival applies, in the order the store takes it.
	e.send(t, 100*time.Second, feedbackService("api", "@4"))
	e.requireRunning(t, "api=@4+100", "web=@1+10")
	// An arrival before the first report of a service ends at that report.
	late.send(t, 5*time.Second, feedbackService("web", "@0"))
	e.requireRunning(t, "api=@4+100", "web=@1+10")
	for _, d := range e.deploys(t) {
		if d.Service == "web" && d.Version == "0" && (!d.DeployedAt.Equal(feedbackTime(5*time.Second)) || d.StoppedAt == nil || !d.StoppedAt.Equal(feedbackTime(10*time.Second))) {
			t.Fatalf("deploy=%+v, want 5 to 10 seconds", d)
		}
	}
	requireNewest(100*time.Second, 60)
}

// runFeedbackArrivalOrder stores the same signals in their arrival order and
// in a shuffled order. Both leave the same deploys, stops, origins, running
// versions, and newest signal.
func runFeedbackArrivalOrder(t *testing.T, x Fixture) {
	labels := []string{"first", "second", "third"}
	render := func(e feedbackEnvironment) string {
		t.Helper()
		var lines []string
		for _, d := range e.deploys(t) {
			line := fmt.Sprintf("deploy %s=%s %v %+v", d.Service, renderFeedbackVersion(d.FeedbackVersion), d.DeployedAt, d.Origin)
			if d.StoppedAt != nil {
				line += fmt.Sprintf(" to %v %+v", d.StoppedAt, *d.StopOrigin)
			}
			lines = append(lines, line)
		}
		for _, v := range e.running(t) {
			lines = append(lines, fmt.Sprintf("running %s=%s %v %v %s", v.Service, renderFeedbackVersion(v.FeedbackVersion), v.DeployedAt, v.ReportedAt, v.ReportedLabel))
		}
		newest, _ := e.newest(t)
		newest.EnvironmentID = ""
		slices.Sort(lines)
		return strings.Join(append(lines, fmt.Sprintf("newest %+v", newest)), "\n")
	}
	type target struct {
		e    feedbackEnvironment
		keys []string
	}
	newTarget := func(name string) target {
		t.Helper()
		made := target{e: newFeedbackEnvironment(t, x, name, labels[0])}
		made.keys = append(made.keys, made.e.key.ID)
		for _, label := range labels[1:] {
			key, err := x.Backend.IssueEnvironmentKey(x.Context, made.e.id, label)
			requireOK(t, err)
			made.keys = append(made.keys, key.ID)
		}
		return made
	}
	ordered, shuffled := newTarget("Ordered"), newTarget("Shuffled")
	for seed := range 24 {
		random := rand.New(rand.NewPCG(uint64(seed), 261125))
		type planned struct {
			key      int
			offset   time.Duration
			seconds  int
			services []store.FeedbackSignalService
		}
		var signals []planned
		for i := range 8 {
			p := planned{key: random.IntN(len(labels)), offset: time.Duration(100*seed+10*i) * time.Second, seconds: 10 + random.IntN(100)}
			// One signal in three names each service alone.
			omitted := random.IntN(3)
			for j, name := range []string{"api", "web"} {
				if j == omitted {
					continue
				}
				var versions []string
				for _, version := range []string{"@1", "@2", "@3"} {
					if random.IntN(2) == 0 {
						versions = append(versions, version)
					}
				}
				if len(versions) == 0 {
					versions = []string{"@1"}
				}
				p.services = append(p.services, feedbackService(fmt.Sprintf("%s-%d", name, seed), versions...))
			}
			signals = append(signals, p)
		}
		record := func(to target, order []int) string {
			t.Helper()
			for _, i := range order {
				signal := to.e.signal(signals[i].offset, signals[i].services...)
				signal.KeyID, signal.NextSignalSeconds = to.keys[signals[i].key], signals[i].seconds
				requireOK(t, x.Backend.RecordFeedbackSignal(x.Context, signal))
			}
			return render(to.e)
		}
		want := record(ordered, []int{0, 1, 2, 3, 4, 5, 6, 7})
		order := random.Perm(len(signals))
		if got := record(shuffled, order); got != want {
			t.Fatalf("seed %d order %v:\n%s\nwant the records of the arrival order:\n%s\nsignals %+v", seed, order, got, want, signals)
		}
	}
}

// runFeedbackRecentWindow proves which arrivals still end an earlier one: any
// arrival within FeedbackRecentWindow of the newest signal, and past it the
// ones that changed the service.
func runFeedbackRecentWindow(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	for _, offset := range []time.Duration{0, 300, 900, 1200} {
		e.send(t, offset*time.Second, feedbackService("api", "@1"))
	}
	e.send(t, 720*time.Second, feedbackService("api", "@3"))
	e.requireRunning(t, "api=@1+900")
	e.requireDeploys(t, "api=@1+0-720", "api=@3+720-900", "api=@1+900")
	// The arrival at 300 seconds changed nothing and left the window.
	e.send(t, 60*time.Second, feedbackService("api", "@2"))
	e.requireRunning(t, "api=@1+900")
	e.requireDeploys(t, "api=@1+0-60", "api=@2+60-720", "api=@3+720-900", "api=@1+900")
	if newest, _ := e.newest(t); !newest.LastSignalAt.Equal(feedbackTime(1200 * time.Second)) {
		t.Fatalf("newest=%+v", newest)
	}
}

// runFeedbackRepositoryCheck proves that the read-only check answers what the
// write answers, for the bound workspace's current configuration.
func runFeedbackRepositoryCheck(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	before := e.state(t)
	signal := func(repository string) store.FeedbackSignal {
		second := feedbackService("web", "@1")
		second.Repository = repository
		return e.signal(0, feedbackService("api", "@1"), second)
	}
	for _, repository := range []string{"", "app", "conveyor"} {
		requireOK(t, x.Backend.CheckFeedbackRepositories(x.Context, signal(repository)))
	}
	for _, repository := range []string{"ghost", "App", "app "} {
		requireFeedbackField(t, x.Backend.CheckFeedbackRepositories(x.Context, signal(repository)), "services[1].repository")
		requireFeedbackField(t, x.Backend.RecordFeedbackSignal(x.Context, signal(repository)), "services[1].repository")
	}
	missing := store.WithWorkspace(x.Context, x.Workspace+"-missing")
	if err := x.Backend.CheckFeedbackRepositories(missing, signal("app")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing workspace err=%v", err)
	}
	if after := e.state(t); after != before {
		t.Fatalf("a check changed the stored state:\nbefore %s\nafter  %s", before, after)
	}
}

func runFeedbackRefusals(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.send(t, 0, feedbackService("api", "@1"), feedbackService("web", "@1"))
	before := e.state(t)
	good := feedbackService("api", "@2")
	many := func(count int) []store.FeedbackSignalService {
		var services []store.FeedbackSignalService
		for i := range count {
			services = append(services, feedbackService(fmt.Sprintf("service-%d", i), "@1"))
		}
		return services
	}
	versions := feedbackService("web")
	for i := range store.MaxFeedbackVersions + 1 {
		versions.Versions = append(versions.Versions, store.FeedbackSignalVersion{Version: fmt.Sprint(i)})
	}
	ghost := feedbackService("web", "@2")
	ghost.Repository = "ghost"
	folded := feedbackService("web", "@2")
	folded.Repository = "Conveyor"
	for _, c := range []struct {
		field    string
		services []store.FeedbackSignalService
	}{
		{"services", nil},
		{"services", many(store.MaxFeedbackServices + 1)},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService("", "@1")}},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService(strings.Repeat("é", 101), "@1")}},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService("we\x00b", "@1")}},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService("we\xffb", "@1")}},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService("api", "@1")}},
		{"services[1].name", []store.FeedbackSignalService{good, feedbackService("ghp_0123456789abcdefghij0123456789abcdefghij", "@1")}},
		{"services[1].versions", []store.FeedbackSignalService{good, feedbackService("web")}},
		{"services[1].versions", []store.FeedbackSignalService{good, versions}},
		{"services[1].versions[0]", []store.FeedbackSignalService{good, feedbackService("web", "")}},
		{"services[1].versions[1].commit", []store.FeedbackSignalService{good, feedbackService("web", "@1", "abcdef")}},
		{"services[1].versions[0].commit", []store.FeedbackSignalService{good, feedbackService("web", strings.Repeat("a", 41))}},
		{"services[1].versions[0].commit", []store.FeedbackSignalService{good, feedbackService("web", "abcdefg")}},
		{"services[1].versions[0].version", []store.FeedbackSignalService{good, feedbackService("web", "@"+strings.Repeat("a", 201))}},
		{"services[1].versions[0].version", []store.FeedbackSignalService{good, feedbackService("web", "@1\x00")}},
		{"services[1].versions[0].version", []store.FeedbackSignalService{good, feedbackService("web", "@"+e.key.Value)}},
		{"services[1].repository", []store.FeedbackSignalService{good, ghost}},
		{"services[1].repository", []store.FeedbackSignalService{good, folded}},
	} {
		err := x.Backend.RecordFeedbackSignal(x.Context, e.signal(10*time.Second, c.services...))
		requireFeedbackField(t, err, c.field)
		if strings.Contains(err.Error(), e.key.Value) || strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "ghost") {
			t.Fatalf("the refusal of %s repeats a submitted value", c.field)
		}
	}
	for _, seconds := range []int{-1, 0, store.MinFeedbackSignalSeconds - 1, store.MaxFeedbackSignalSeconds + 1} {
		signal := e.signal(10*time.Second, good)
		signal.NextSignalSeconds = seconds
		requireFeedbackField(t, x.Backend.RecordFeedbackSignal(x.Context, signal), "next_signal_seconds")
	}
	noArrival := e.signal(0, good)
	noArrival.ArrivedAt = time.Time{}
	if err := x.Backend.RecordFeedbackSignal(x.Context, noArrival); err == nil {
		t.Fatal("a signal without an arrival was recorded")
	}
	if after := e.state(t); after != before {
		t.Fatalf("a refused signal changed the stored state:\nbefore %s\nafter  %s", before, after)
	}
}

// runFeedbackKeyValidity proves that a key checked earlier records nothing
// once it is revoked or its environment is archived.
func runFeedbackKeyValidity(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	if _, err := x.Backend.CheckEnvironmentKey(x.Context, e.key.Value); err != nil {
		t.Fatal(err)
	}
	e.send(t, 0, feedbackService("api", "@1"))
	refuse := func(what string) {
		t.Helper()
		before := e.state(t)
		if err := x.Backend.RecordFeedbackSignal(x.Context, e.signal(10*time.Second, feedbackService("api", "@2"))); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
			t.Fatalf("%s: err=%v, want the one refusal", what, err)
		}
		if after := e.state(t); after != before {
			t.Fatalf("%s changed the stored state:\nbefore %s\nafter  %s", what, before, after)
		}
	}
	archiveEnvironment(t, x, e.id, true)
	refuse("a signal to an archived environment")
	archiveEnvironment(t, x, e.id, false)
	e.send(t, 20*time.Second, feedbackService("api", "@3"))
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, e.id, e.key.ID))
	refuse("a signal under a revoked key")
	e.requireRunning(t, "api=@3+20")
}

// runFeedbackLastUse proves that the read-only check writes nothing and
// refuses exactly what verification refuses, and that an accepted signal
// records the last use at most once a minute (component-environments
// invariant 14).
func runFeedbackLastUse(t *testing.T, x Fixture) {
	if x.AgeEnvironmentKey == nil {
		t.Fatal("conformance fixture must provide AgeEnvironmentKey for the last-use case")
	}
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	for range 2 {
		verified, err := x.Backend.CheckEnvironmentKey(t.Context(), e.key.Value)
		if err != nil || verified != (store.VerifiedEnvironmentKey{Workspace: x.Workspace, EnvironmentID: e.id, KeyID: e.key.ID, Label: "watcher"}) {
			t.Fatalf("checked=%+v err=%v", verified, err)
		}
	}
	if at := environmentLastUse(t, x, e.key.ID); at != nil {
		t.Fatalf("a check recorded the last use %v", at)
	}
	e.send(t, 0, feedbackService("api", "@1"))
	first := environmentLastUse(t, x, e.key.ID)
	if first == nil || time.Since(*first).Abs() > 10*time.Minute {
		t.Fatalf("last use after a signal=%v", first)
	}
	e.send(t, 10*time.Second, feedbackService("api", "@1"))
	if again := environmentLastUse(t, x, e.key.ID); !again.Equal(*first) {
		t.Fatalf("a second signal within a minute moved the last use from %v to %v", first, again)
	}
	x.AgeEnvironmentKey(t, x.Context, e.key.ID, 2*time.Minute)
	aged := environmentLastUse(t, x, e.key.ID)
	requireFeedbackField(t, x.Backend.RecordFeedbackSignal(x.Context, e.signal(20*time.Second)), "services")
	if _, err := x.Backend.CheckEnvironmentKey(x.Context, e.key.Value); err != nil {
		t.Fatal(err)
	}
	if again := environmentLastUse(t, x, e.key.ID); !again.Equal(*aged) {
		t.Fatalf("a refused signal moved the last use from %v to %v", aged, again)
	}
	e.send(t, 20*time.Second, feedbackService("api", "@1"))
	if again := environmentLastUse(t, x, e.key.ID); !again.After(*aged) {
		t.Fatalf("a signal after a minute kept the last use at %v", again)
	}
	refuse := func(value, what string) {
		t.Helper()
		verified, err := x.Backend.CheckEnvironmentKey(x.Context, value)
		if !errors.Is(err, store.ErrInvalidEnvironmentKey) || verified != (store.VerifiedEnvironmentKey{}) {
			t.Fatalf("%s: checked=%+v err=%v, want the one refusal", what, verified, err)
		}
	}
	refuse("", "an empty value")
	refuse(e.key.ID, "a key ID")
	refuse(e.key.Value+"x", "an altered value")
	archiveEnvironment(t, x, e.id, true)
	refuse(e.key.Value, "a key of an archived environment")
	archiveEnvironment(t, x, e.id, false)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, e.id, e.key.ID))
	refuse(e.key.Value, "a revoked key")
}

// runFeedbackObservesOnly proves component-production-feedback invariant 1:
// a signal appends no event and changes no task or work order.
func runFeedbackObservesOnly(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	requireOK(t, x.Backend.CreateTask(x.Context, core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "app", Title: "Task", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}))
	observe := func() string {
		t.Helper()
		tasks, err := x.Backend.ListTasks(x.Context)
		requireOK(t, err)
		orders, err := x.Backend.ListWorkOrders(x.Context)
		requireOK(t, err)
		events, err := x.WorkspaceEvents(x.Context, "")
		requireOK(t, err)
		var taskEvents []core.Event
		for _, task := range tasks {
			listed, err := x.Backend.ListEvents(x.Context, task.ID)
			requireOK(t, err)
			taskEvents = append(taskEvents, listed...)
		}
		raw, err := json.Marshal([]any{tasks, orders, events, taskEvents})
		requireOK(t, err)
		return string(raw)
	}
	before := observe()
	service := feedbackService("api", "abcdef0")
	service.Repository = "app"
	e.send(t, 0, service)
	e.send(t, 10*time.Second, feedbackService("api", "abcdef1"))
	if after := observe(); after != before {
		t.Fatalf("a signal changed an observed record:\nbefore %s\nafter  %s", before, after)
	}
}

// runFeedbackConcurrent sends first and later signals at once. Each version
// keeps one deploy, the latest arrival's versions run, and the deploys are
// those of the arrival order.
func runFeedbackConcurrent(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	second, err := x.Backend.IssueEnvironmentKey(x.Context, e.id, "second")
	requireOK(t, err)
	send := func(key string, offset time.Duration, services ...store.FeedbackSignalService) func() error {
		signal := e.signal(offset, services...)
		signal.KeyID = key
		return func() error { return x.Backend.RecordFeedbackSignal(x.Context, signal) }
	}
	var acts []func() error
	for i := range 8 {
		key := e.key.ID
		if i%2 == 1 {
			key = second.ID
		}
		acts = append(acts, send(key, 0, feedbackService("api", "@1", "@2"), feedbackService(fmt.Sprintf("own-%d", i), "@1")))
	}
	if results := environmentRace(acts...); environmentWinners(results) != len(acts) {
		t.Fatalf("concurrent first signals=%v", results)
	}
	want := []string{"api=@1+0", "api=@2+0"}
	for i := range 8 {
		want = append(want, fmt.Sprintf("own-%d=@1+0", i))
	}
	e.requireRunning(t, want...)
	e.requireDeploys(t, want...)
	// The latest arrival decides, whatever order the store takes them in.
	acts = nil
	for i := range 8 {
		acts = append(acts, send(e.key.ID, time.Duration(10+i)*time.Second, feedbackService("api", fmt.Sprintf("@next-%d", i), fmt.Sprintf("@other-%d", i))))
	}
	if results := environmentRace(acts...); environmentWinners(results) != len(acts) {
		t.Fatalf("concurrent later signals=%v", results)
	}
	var running []string
	for _, v := range e.running(t) {
		if v.Service == "api" {
			running = append(running, v.Version)
		}
	}
	if !slices.Equal(running, []string{"next-7", "other-7"}) {
		t.Fatalf("running=%v, want the latest arrival's versions", running)
	}
	want = append(want[2:], "api=@1+0-10", "api=@2+0-10")
	for i := range 8 {
		stop := fmt.Sprintf("-%d", 11+i)
		if i == 7 {
			stop = ""
		}
		want = append(want, fmt.Sprintf("api=@next-%d+%d%s", i, 10+i, stop), fmt.Sprintf("api=@other-%d+%d%s", i, 10+i, stop))
	}
	e.requireDeploys(t, want...)
	if newest, _ := e.newest(t); !newest.LastSignalAt.Equal(feedbackTime(17 * time.Second)) {
		t.Fatalf("newest=%+v", newest)
	}
}

// runFeedbackConcurrentRefusal races signals with a revocation and an
// archive. Once either returns, no signal is recorded.
func runFeedbackConcurrentRefusal(t *testing.T, x Fixture) {
	for name, end := range map[string]func(feedbackEnvironment) error{
		"revocation": func(e feedbackEnvironment) error {
			return x.Backend.RevokeEnvironmentKey(x.Context, e.id, e.key.ID)
		},
		"archive": func(e feedbackEnvironment) error {
			_, err := x.Backend.UpdateEnvironment(x.Context, e.id, store.EnvironmentPatch{Archived: ptr(true)})
			return err
		},
	} {
		e := newFeedbackEnvironment(t, x, "Race "+name, "watcher")
		acts := []func() error{func() error { return end(e) }}
		for i := range 4 {
			acts = append(acts, func() error {
				return x.Backend.RecordFeedbackSignal(x.Context, e.signal(0, feedbackService(fmt.Sprintf("service-%d", i), "@1")))
			})
		}
		results := environmentRace(acts...)
		requireOK(t, results[0])
		accepted := 0
		for _, err := range results[1:] {
			if err == nil {
				accepted++
			} else if !errors.Is(err, store.ErrInvalidEnvironmentKey) {
				t.Fatalf("%s: signal err=%v", name, err)
			}
		}
		if deploys := e.deploys(t); len(deploys) != accepted {
			t.Fatalf("%s: deploys=%d, want one for each of %d accepted signals", name, len(deploys), accepted)
		}
		if err := x.Backend.RecordFeedbackSignal(x.Context, e.signal(time.Second, feedbackService("late", "@1"))); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
			t.Fatalf("%s: late signal err=%v", name, err)
		}
	}
}
