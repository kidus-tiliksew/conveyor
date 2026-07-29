package gitx

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NormalizeRepositoryIdentity canonicalizes the configured and local origin
// forms used by checkout safety checks. Transport and Git user names are not
// repository identity; GitHub owner/repository case and a trailing .git are
// likewise normalized (spec §8.2).
func NormalizeRepositoryIdentity(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("repository identity is empty")
	}
	if !strings.Contains(value, "://") {
		if at := strings.LastIndex(value, "@"); at >= 0 {
			if colon := strings.Index(value[at+1:], ":"); colon >= 0 {
				hostStart := at + 1
				hostEnd := hostStart + colon
				return normalizeRemoteIdentity(value[hostStart:hostEnd], value[hostEnd+1:])
			}
		}
		if filepath.IsAbs(value) || strings.HasPrefix(value, ".") {
			return normalizeLocalIdentity(value)
		}
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse repository identity %q: %w", value, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "file":
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", fmt.Errorf("file repository identity %q has unsupported host %q", value, parsed.Host)
		}
		path, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return "", fmt.Errorf("decode repository identity %q: %w", value, err)
		}
		return normalizeLocalIdentity(path)
	case "http", "https", "ssh", "git":
		if parsed.Hostname() == "" {
			return "", fmt.Errorf("repository identity %q has no host", value)
		}
		host := parsed.Hostname()
		if port := parsed.Port(); port != "" && !defaultRepositoryPort(parsed.Scheme, port) {
			host += ":" + port
		}
		return normalizeRemoteIdentity(host, parsed.Path)
	default:
		return "", fmt.Errorf("repository identity %q uses unsupported scheme %q", value, parsed.Scheme)
	}
}

func defaultRepositoryPort(scheme, port string) bool {
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "ssh":
		return port == "22"
	case "git":
		return port == "9418"
	default:
		return false
	}
}

func normalizeRemoteIdentity(host, path string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" || strings.Contains(path, "\\") {
		return "", fmt.Errorf("remote repository identity %q/%q is incomplete", host, path)
	}
	if host == "github.com" {
		path = strings.ToLower(path)
	}
	return host + "/" + path, nil
}

func normalizeLocalIdentity(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve local repository identity %q: %w", path, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	} else if !os.IsNotExist(resolveErr) {
		return "", fmt.Errorf("resolve local repository identity %q: %w", path, resolveErr)
	}
	return "file:" + filepath.Clean(absolute), nil
}

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
		identity, normalizeErr := NormalizeRepositoryIdentity(line)
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
	assigned, err := NormalizeRepositoryIdentity(assignedURL)
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

// ResolvePrimaryCheckout finds a configured repository only at deterministic,
// operator-owned locations: the daemon's current checkout, its same-parent
// directory named for the configured repository, or an explicitly local
// repository URL. Every candidate must match origin before it is returned.
func ResolvePrimaryCheckout(ctx context.Context, startDir, repoName, repoURL string) (string, error) {
	assigned, err := NormalizeRepositoryIdentity(repoURL)
	if err != nil {
		return "", fmt.Errorf("resolve repository %q: %w", repoName, err)
	}
	var candidates []string
	if root, rootErr := repositoryRootAt(ctx, startDir); rootErr == nil {
		primary, primaryErr := primaryCheckoutRoot(ctx, root)
		if primaryErr == nil {
			candidates = append(candidates, primary)
			if safeRepositoryDirectoryName(repoName) {
				candidates = append(candidates, filepath.Join(filepath.Dir(primary), repoName))
			}
		}
	}
	if strings.HasPrefix(assigned, "file:") {
		candidates = append(candidates, strings.TrimPrefix(assigned, "file:"))
	}

	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		root, rootErr := repositoryRootAt(ctx, candidate)
		if rootErr != nil {
			continue
		}
		primary, primaryErr := primaryCheckoutRoot(ctx, root)
		if primaryErr != nil {
			continue
		}
		if _, duplicate := seen[primary]; duplicate {
			continue
		}
		seen[primary] = struct{}{}
		current, identityErr := RepositoryOriginIdentity(ctx, primary)
		if identityErr == nil && current == assigned {
			return primary, nil
		}
	}
	return "", fmt.Errorf("configured repository %q (%s) has no available matching primary checkout", repoName, assigned)
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

func safeRepositoryDirectoryName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
