package feedback

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestApplyVersionRule covers req-environments AC-1.2 and
// component-production-feedback invariant 7: a commit pattern with and
// without *, a tag template with and without {service}, and each refused
// form.
func TestApplyVersionRule(t *testing.T) {
	for _, tc := range []struct {
		name, kind, text, version string
		want                      RuleTarget
		ok                        bool
	}{
		{"commit pattern alone", store.EnvironmentCommitPattern, "{commit}", "ABC1234", RuleTarget{Commit: "abc1234"}, true},
		{"commit pattern with literal text", store.EnvironmentCommitPattern, "build-{commit}", "build-0123456789abcdef", RuleTarget{Commit: "0123456789abcdef"}, true},
		{"commit pattern with *", store.EnvironmentCommitPattern, "v*-{commit}", "v1.2.3-abc1234", RuleTarget{Commit: "abc1234"}, true},
		{"commit pattern with * on both sides", store.EnvironmentCommitPattern, "*+{commit}.*", "2026.11+deadbeef.linux", RuleTarget{Commit: "deadbeef"}, true},
		{"a * right before the commit leaves the whole commit", store.EnvironmentCommitPattern, "*{commit}", "build-0123456789abcdef0123456789abcdef01234567", RuleTarget{Commit: "0123456789abcdef0123456789abcdef01234567"}, true},
		{"a * right after the commit leaves the whole commit", store.EnvironmentCommitPattern, "{commit}*", "0123456789abcdef0123456789abcdef01234567-linux", RuleTarget{Commit: "0123456789abcdef0123456789abcdef01234567"}, true},
		{"literal dot is not a wildcard", store.EnvironmentCommitPattern, "v1.{commit}", "v1x abc1234", RuleTarget{}, false},
		{"too short a commit", store.EnvironmentCommitPattern, "{commit}", "abc123", RuleTarget{}, false},
		{"too long a commit", store.EnvironmentCommitPattern, "{commit}", "0123456789abcdef0123456789abcdef012345678", RuleTarget{}, false},
		{"not hexadecimal", store.EnvironmentCommitPattern, "v-{commit}", "v-release", RuleTarget{}, false},
		{"tag template", store.EnvironmentTagTemplate, "v{version}", "1.2.3", RuleTarget{Tag: "v1.2.3"}, true},
		{"tag template with service", store.EnvironmentTagTemplate, "{service}/v{version}", "1.2.3", RuleTarget{Tag: "api/v1.2.3"}, true},
		{"placeholders expand once", store.EnvironmentTagTemplate, "{service}-v{version}", "{service}", RuleTarget{Tag: "api-v{service}"}, true},
		{"a version that names no tag", store.EnvironmentTagTemplate, "v{version}", "1 2", RuleTarget{}, false},
		{"a version that opens a path walk", store.EnvironmentTagTemplate, "{version}", "../main", RuleTarget{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := ApplyVersionRule(store.EnvironmentVersionRule{Kind: tc.kind, Text: tc.text}, tc.version, "api")
			if err != nil || ok != tc.ok || got != tc.want {
				t.Fatalf("got %+v ok=%v err=%v, want %+v ok=%v", got, ok, err, tc.want, tc.ok)
			}
		})
	}
	for _, refused := range []store.EnvironmentVersionRule{
		{Kind: store.EnvironmentCommitPattern, Text: "v1"},
		{Kind: store.EnvironmentCommitPattern, Text: "{commit}{commit}"},
		{Kind: store.EnvironmentCommitPattern, Text: "{Commit}"},
		{Kind: store.EnvironmentCommitPattern, Text: "{version}-{commit}"},
		{Kind: store.EnvironmentCommitPattern, Text: "{commit}}"},
		{Kind: store.EnvironmentCommitPattern, Text: "{{commit}"},
		{Kind: store.EnvironmentTagTemplate, Text: "{service}"},
		{Kind: store.EnvironmentTagTemplate, Text: "{commit}/{version}"},
		{Kind: store.EnvironmentTagTemplate, Text: "{version}{version}"},
		{Kind: store.EnvironmentTagTemplate, Text: "release {version}"},
		{Kind: "regex", Text: "{commit}"},
	} {
		if _, ok, err := ApplyVersionRule(refused, "1.2.3", "api"); err == nil || ok {
			t.Fatalf("rule %+v was applied", refused)
		}
		if _, err := store.NormalizeEnvironmentVersionRule(&refused); err == nil {
			t.Fatalf("rule %+v passed the environment write check", refused)
		}
	}
}
