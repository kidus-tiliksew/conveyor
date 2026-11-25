package feedback

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

var resolverRepositories = []Repository{{Name: "conveyor", Slug: "acme/conveyor"}, {Name: "app", Slug: "acme/app"}, {Name: "local"}}

// TestResolve covers component-production-feedback invariant 7 and
// req-production-feedback AC-1.4: a commit, a pattern, and a tag; the named
// repository or the one repository that holds the commit; ambiguity; no
// rule; and every forge failure category.
func TestResolve(t *testing.T) {
	commit, otherCommit := sha("abc1234"), sha("def5678")
	tagRule := &store.EnvironmentVersionRule{Kind: store.EnvironmentTagTemplate, Text: "{service}/v{version}"}
	patternRule := &store.EnvironmentVersionRule{Kind: store.EnvironmentCommitPattern, Text: "v*-{commit}"}
	retryAt := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		setup   func(*stubForge)
		request ResolveRequest
		want    Resolution
	}{
		{"commit in the one repository that holds it", func(f *stubForge) { f.chain("acme/app", "", commit) },
			ResolveRequest{Version: store.FeedbackVersion{Commit: "ABC1234"}}, Resolution{Repository: "app", Commit: commit}},
		{"commit in the named repository", func(f *stubForge) { f.chain("acme/app", "", commit); f.chain("acme/conveyor", "", commit) },
			ResolveRequest{Version: store.FeedbackVersion{Repository: "conveyor", Commit: "abc1234"}}, Resolution{Repository: "conveyor", Commit: commit}},
		{"a commit two repositories hold is ambiguous", func(f *stubForge) { f.chain("acme/app", "", commit); f.chain("acme/conveyor", "", commit) },
			ResolveRequest{Version: store.FeedbackVersion{Commit: "abc1234"}}, Resolution{Reason: store.FeedbackReasonAmbiguous}},
		{"a stated commit comes before the version", func(f *stubForge) { f.chain("acme/app", "", commit); f.refs["acme/app|tags/api/v1.0"] = otherCommit },
			ResolveRequest{Version: store.FeedbackVersion{Commit: "abc1234", Version: "1.0"}, Service: "api", Rule: tagRule}, Resolution{Repository: "app", Commit: commit}},
		{"a commit pattern", func(f *stubForge) { f.chain("acme/conveyor", "", commit) },
			ResolveRequest{Version: store.FeedbackVersion{Version: "v1.2-abc1234"}, Rule: patternRule}, Resolution{Repository: "conveyor", Commit: commit}},
		{"a tag template with the service", func(f *stubForge) { f.refs["acme/app|tags/api/v1.0"] = otherCommit },
			ResolveRequest{Version: store.FeedbackVersion{Version: "1.0"}, Service: "api", Rule: tagRule}, Resolution{Repository: "app", Commit: otherCommit}},
		{"a tag in the named repository only", func(f *stubForge) { f.refs["acme/app|tags/api/v1.0"] = otherCommit },
			ResolveRequest{Version: store.FeedbackVersion{Repository: "conveyor", Version: "1.0"}, Service: "api", Rule: tagRule}, Resolution{Reason: store.FeedbackReasonNoMatch}},
		{"no rule", nil, ResolveRequest{Version: store.FeedbackVersion{Version: "1.0"}}, Resolution{Reason: store.FeedbackReasonNoVersionRule}},
		{"a version the rule does not fit", nil, ResolveRequest{Version: store.FeedbackVersion{Version: "release"}, Rule: patternRule}, Resolution{Reason: store.FeedbackReasonRuleMismatch}},
		{"no repository holds it", nil, ResolveRequest{Version: store.FeedbackVersion{Commit: "abc1234"}}, Resolution{Reason: store.FeedbackReasonNoMatch}},
		{"a named repository without a slug", nil, ResolveRequest{Version: store.FeedbackVersion{Repository: "local", Commit: "abc1234"}}, Resolution{Reason: store.FeedbackReasonNoRepository}},
		{"a read that answers another commit is no match", func(f *stubForge) { f.refs["acme/app|abc1234"] = otherCommit },
			ResolveRequest{Version: store.FeedbackVersion{Commit: "abc1234"}}, Resolution{Reason: store.FeedbackReasonNoMatch}},
		{"a failure in one candidate proves no uniqueness", func(f *stubForge) {
			f.chain("acme/app", "", commit)
			f.fail["acme/conveyor|abc1234"] = &github.Error{Category: github.ForgePermission, Err: errors.New("no access")}
		}, ResolveRequest{Version: store.FeedbackVersion{Commit: "abc1234"}}, Resolution{Reason: string(github.ForgePermission)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forge := newStubForge()
			if tc.setup != nil {
				tc.setup(forge)
			}
			tc.request.Repositories = resolverRepositories
			if got := Resolve(t.Context(), forge, tc.request); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	for category, failure := range map[github.ForgeErrorCategory]error{
		github.ForgeRequest:     &github.Error{Category: github.ForgeRequest, Err: errors.New("transport")},
		github.ForgeStatus:      github.StatusError(502, errors.New("bad gateway")),
		github.ForgeResponse:    &github.Error{Category: github.ForgeResponse, Err: errors.New("malformed")},
		github.ForgeRateLimited: github.RateLimitedError(retryAt, errors.New("rate limited")),
		github.ForgePermission:  &github.Error{Category: github.ForgePermission, Err: errors.New("revoked")},
	} {
		forge := newStubForge()
		forge.fail["acme/app|abc1234"] = failure
		got := Resolve(t.Context(), forge, ResolveRequest{Version: store.FeedbackVersion{Repository: "app", Commit: "abc1234"}, Repositories: resolverRepositories})
		if got.Reason != string(category) || got.Commit != "" {
			t.Fatalf("%s: got %+v", category, got)
		}
		if category == github.ForgeRateLimited && !got.RetryAt.Equal(retryAt) {
			t.Fatalf("rate limited retry=%v", got.RetryAt)
		}
	}
}
