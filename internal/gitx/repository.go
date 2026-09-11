package gitx

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// GitHubSlug derives the canonical github.com owner/repository identity.
// req-repository-onboarding REQ-1; component-git-delivery.
func GitHubSlug(repositoryURL string) string {
	identity, err := NormalizeRepositoryIdentity(repositoryURL)
	if err != nil || !strings.HasPrefix(identity, "github.com/") {
		return ""
	}
	return strings.TrimPrefix(identity, "github.com/")
}

// NormalizeRepositoryIdentity canonicalizes the configured and local origin
// forms used by checkout safety checks. Transport and Git user names are not
// repository identity; GitHub owner/repository case and a trailing .git are
// likewise normalized (design-git-delivery).
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
