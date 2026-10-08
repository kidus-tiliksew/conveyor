package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// legacyVolatileGitHubAppKey is a sealed workspace GitHub App private key
// produced before the encryption identifiers were renamed (task
// 261007-f5c24c). The pre-rename volatile seal helper at commit
// af81464168820a37dfa6e879636439f990d3c94c sealed plaintext under the
// nonsecret key bytes 0x00 through 0x1f with AES-256-GCM, a random 12-byte
// nonce, and additional data "workspace-app:"+workspace. The bytes are frozen
// so the renamed helper cannot produce its own compatibility fixture.
var legacyVolatileGitHubAppKey = struct {
	workspace, plaintext   string
	key, nonce, ciphertext string
}{
	workspace:  "legacy-app-key-fixture",
	plaintext:  "legacy-github-app-private-key-fixture",
	key:        "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	nonce:      "d5595ba253093ab47a7c75e4",
	ciphertext: "279d3707fef0559ec7b19dee8db1ba46d4a80522e7e264857160bcb561e6acddd03aa4415443cf28335d9b046b8c21fd44f91c9074",
}

func decodeLegacyHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// DEC-59 clause 2; req-delivery-and-forge AC-1.11: an App key sealed before
// the rename decrypts through App-key retrieval and redaction recovery, and
// the refusal paths are unchanged.
func TestVolatileGitHubAppKeyDecryptsPreRenameRecord(t *testing.T) {
	fixture := legacyVolatileGitHubAppKey
	key := decodeLegacyHex(t, fixture.key)
	nonce := decodeLegacyHex(t, fixture.nonce)
	ciphertext := decodeLegacyHex(t, fixture.ciphertext)
	m := NewVolatileBackend().(*volatileMemory)
	seed := func(workspace, owner string, nonce, ciphertext []byte) {
		m.lock()
		defer m.unlock()
		if m.workspaceGitHubApps == nil {
			m.workspaceGitHubApps = map[string]workspaceAppRecord{}
		}
		m.workspaceGitHubApps[workspace] = workspaceAppRecord{
			status: core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: workspace, AppID: 41, AppSlug: "legacy-app"},
			sealed: gitHubAppKeyRecord{Owner: owner, Nonce: nonce, Ciphertext: ciphertext, AppSlug: "legacy-app"},
		}
	}
	ctx := context.Background()
	owner := "workspace-app:" + fixture.workspace
	seed(fixture.workspace, owner, nonce, ciphertext)

	m.ConfigureGitHubAppKeyEncryptionKey(key)
	credential, err := m.GetWorkspaceGitHubAppForUse(ctx, fixture.workspace)
	if err != nil || credential.PrivateKey != fixture.plaintext {
		t.Fatalf("pre-rename record: key matches=%v err=%v", credential.PrivateKey == fixture.plaintext, err)
	}
	secrets, err := m.ListGitHubAppKeysForRedaction(ctx)
	if err != nil || !slices.Contains(secrets, fixture.plaintext) {
		t.Fatalf("redaction source lacks the pre-rename key: err=%v", err)
	}

	m.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{42}, 32))
	if _, err = m.GetWorkspaceGitHubAppForUse(ctx, fixture.workspace); !errors.Is(err, ErrGitHubAppKeyDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = m.ListGitHubAppKeysForRedaction(ctx); !errors.Is(err, ErrGitHubAppKeyDecrypt) {
		t.Fatalf("redaction with wrong key must fail closed: %v", err)
	}
	m.ConfigureGitHubAppKeyEncryptionKey(nil)
	if _, err = m.GetWorkspaceGitHubAppForUse(ctx, fixture.workspace); !errors.Is(err, ErrGitHubAppKey) {
		t.Fatalf("missing key: %v", err)
	}
	m.ConfigureGitHubAppKeyEncryptionKey(key)
	tampered := bytes.Clone(ciphertext)
	tampered[0] ^= 1
	for name, record := range map[string]struct {
		owner             string
		nonce, ciphertext []byte
	}{
		"other workspace additional data": {"workspace-app:" + fixture.workspace + "-other", nonce, ciphertext},
		"short nonce":                     {owner, nonce[:11], ciphertext},
		"tampered ciphertext":             {owner, nonce, tampered},
	} {
		seed(fixture.workspace, record.owner, record.nonce, record.ciphertext)
		if _, err = m.GetWorkspaceGitHubAppForUse(ctx, fixture.workspace); !errors.Is(err, ErrGitHubAppKeyDecrypt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
