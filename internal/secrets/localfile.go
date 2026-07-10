package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// LocalFileResolver is the zero-infrastructure Phase 1 backend (spec §10.1,
// §17.2). SOPS mode stores <set>.sops.env and decrypts only into process
// memory. Plain mode stores <set>.env and must be selected explicitly for
// development fixtures.
type LocalFileResolver struct {
	Root       string
	Backend    string
	SOPSBinary string
	SOPSConfig string
}

func (l *LocalFileResolver) Resolve(ctx context.Context, ref Ref) (string, error) {
	values, path, err := l.load(ctx, ref)
	if err != nil {
		return "", err
	}
	value, ok := values[ref.Name]
	if !ok {
		return "", fmt.Errorf("secret %s not found in %s", ref.Name, path)
	}
	return value, nil
}

func (l *LocalFileResolver) Set(ctx context.Context, ref Ref, value string) error {
	if !ValidEnvName(ref.Name) {
		return fmt.Errorf("secret name %q is not a valid environment variable", ref.Name)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("Phase 1 environment secrets must be single-line and NUL-free")
	}
	values := map[string]string{}
	path := l.path(ref)
	if _, err := os.Stat(path); err == nil {
		var loadErr error
		values, _, loadErr = l.load(ctx, ref)
		if loadErr != nil {
			return loadErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	values[ref.Name] = value
	plaintext := encodeDotenv(values)
	if l.backend() == BackendPlain {
		return writeAtomic(path, plaintext, 0o600)
	}

	binary := l.sopsBinary()
	args := l.sopsArgs("encrypt", "--input-type", "dotenv", "--output-type", "dotenv", "--filename-override", path)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = bytes.NewReader(plaintext)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	encrypted, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("encrypt secret set %s/%s with sops: %w: %s", ref.Workspace, ref.Set, err, strings.TrimSpace(stderr.String()))
	}
	return writeAtomic(path, encrypted, 0o600)
}

func (l *LocalFileResolver) load(ctx context.Context, ref Ref) (map[string]string, string, error) {
	path := l.path(ref)
	// ParseRef already rejects traversal segments; keep containment as
	// defense in depth for Refs constructed directly.
	if !strings.HasPrefix(path, filepath.Clean(l.Root)+string(filepath.Separator)) {
		return nil, path, fmt.Errorf("secret ref %s/%s escapes secret root", ref.Workspace, ref.Set)
	}
	var data []byte
	var err error
	if l.backend() == BackendSOPS {
		cmd := exec.CommandContext(ctx, l.sopsBinary(), l.sopsArgs("decrypt", "--input-type", "dotenv", "--output-type", "dotenv", path)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err = cmd.Output()
		if err != nil {
			return nil, path, fmt.Errorf("decrypt secret set %s/%s with sops: %w: %s", ref.Workspace, ref.Set, err, strings.TrimSpace(stderr.String()))
		}
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, path, fmt.Errorf("secret set %s/%s: %w", ref.Workspace, ref.Set, err)
		}
	}
	return parseDotenv(data), path, nil
}

func (l *LocalFileResolver) backend() string {
	if l.Backend == "" {
		return BackendSOPS
	}
	return l.Backend
}

func (l *LocalFileResolver) sopsBinary() string {
	if l.SOPSBinary == "" {
		return "sops"
	}
	return l.SOPSBinary
}

func (l *LocalFileResolver) sopsArgs(args ...string) []string {
	if l.SOPSConfig == "" {
		return args
	}
	return append([]string{"--config", l.SOPSConfig}, args...)
}

func (l *LocalFileResolver) path(ref Ref) string {
	suffix := ".sops.env"
	if l.backend() == BackendPlain {
		suffix = ".env"
	}
	return filepath.Join(l.Root, ref.Workspace, ref.Set+suffix)
}

func parseDotenv(data []byte) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(k)] = v
		}
	}
	return values
}

func encodeDotenv(values map[string]string) []byte {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		fmt.Fprintf(&out, "%s=%s\n", name, values[name])
	}
	return []byte(out.String())
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".conveyor-secrets-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
