package gitx

import "testing"

func TestRepositoryGitHubSlug(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/Owner/Repo.git", "owner/repo"},
		{"git@github.com:Owner/Repo.git", "owner/repo"},
		{"ssh://git@github.com/Owner/Repo.git", "owner/repo"},
		{"https://git.example/Owner/Repo.git", ""},
		{"https://github.com.evil.test/Owner/Repo.git", ""},
		{"", ""},
	} {
		if got := GitHubSlug(tc.url); got != tc.want {
			t.Errorf("GitHubSlug(%q)=%q want %q", tc.url, got, tc.want)
		}
	}
}
