package localgit

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"path/filepath"
	"sort"
	"strings"
)

// RepositoryOriginIdentity returns one unambiguous normalized origin identity.
func RepositoryOriginIdentity(ctx context.Context, checkout string) (string, error) {
	output, err := commandOutput(ctx, checkout, "git", "remote", "get-url", "--all", "origin")
	if err != nil {
		return "", fmt.Errorf("read current origin: %w", err)
	}
	identities := map[string]struct{}{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		identity, normalizeErr := gitx.NormalizeRepositoryIdentity(line)
		if normalizeErr != nil {
			return "", fmt.Errorf("normalize current origin %q: %w", strings.TrimSpace(line), normalizeErr)
		}
		identities[identity] = struct{}{}
	}
	if len(identities) == 0 {
		return "", fmt.Errorf("current repository has no readable origin")
	}
	if len(identities) != 1 {
		values := make([]string, 0, len(identities))
		for identity := range identities {
			values = append(values, identity)
		}
		sort.Strings(values)
		return "", fmt.Errorf("current repository has ambiguous origin identities %q", strings.Join(values, ", "))
	}
	for identity := range identities {
		return identity, nil
	}
	panic("unreachable")
}

// VerifyRepositoryIdentity fails closed unless checkout's origin is the
// canonical configured workspace repository.
func VerifyRepositoryIdentity(ctx context.Context, checkout, assignedName, assignedURL string) error {
	assigned, err := gitx.NormalizeRepositoryIdentity(assignedURL)
	if err != nil {
		return fmt.Errorf("assigned repository %q identity %q is invalid: %w", assignedName, assignedURL, err)
	}
	current, err := RepositoryOriginIdentity(ctx, checkout)
	if err != nil {
		return fmt.Errorf("repository identity mismatch: assigned %q (%s), current unavailable: %w", assignedName, assigned, err)
	}
	if current != assigned {
		return fmt.Errorf("repository identity mismatch: assigned %q (%s), current repository is %s", assignedName, assigned, current)
	}
	return nil
}

func repositoryRootAt(ctx context.Context, path string) (string, error) {
	output, err := commandOutput(ctx, path, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(output)), nil
}

func primaryCheckoutRoot(ctx context.Context, checkout string) (string, error) {
	output, err := commandOutput(ctx, checkout, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Dir(strings.TrimSpace(output))), nil
}
