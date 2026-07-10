package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalFileResolver resolves refs from plain env-format files laid out
// as <Root>/<workspace>/<set>.env. It is the Phase 1 development
// backend; the production local path is the same layout SOPS-encrypted
// (spec §10.1), decrypted at resolve time.
//
// TODO(phase1): SOPS decryption (shell out to sops, or go library) when
// the file has a .sops.env suffix.
type LocalFileResolver struct {
	Root string
}

func (l *LocalFileResolver) Resolve(_ context.Context, ref Ref) (string, error) {
	path := filepath.Join(l.Root, ref.Workspace, ref.Set+".env")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secret set %s/%s: %w", ref.Workspace, ref.Set, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && k == ref.Name {
			return v, nil
		}
	}
	return "", fmt.Errorf("secret %s not found in %s", ref.Name, path)
}
