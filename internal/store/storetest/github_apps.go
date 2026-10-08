package storetest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runGitHubApps(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, _ := bootstrapOwner(t, x)
	key := bytes.Repeat([]byte{41}, 32)
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-fixture", ClientID: "client-fixture"}, PrivateKey: "conformance-app-private-key-material"}
	status, err := st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	if status.Connected {
		t.Fatal("new workspace connected")
	}
	st.ConfigureGitHubAppKeyEncryptionKey(nil)
	if _, err = st.StoreWorkspaceGitHubApp(ctx, x.Workspace, app); !errors.Is(err, store.ErrGitHubAppKey) {
		t.Fatalf("missing key: %v", err)
	}
	st.ConfigureGitHubAppKeyEncryptionKey(key)
	if _, err = st.StoreWorkspaceGitHubApp(ctx, "absent", app); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent workspace: %v", err)
	}
	saved, err := st.StoreWorkspaceGitHubApp(ctx, x.Workspace, app)
	requireOK(t, err)
	if !saved.Connected || saved.AppID != 41 || saved.WorkspaceID != x.Workspace || saved.ConnectedBy != store.ActorFromContext(ctx).ID || saved.ConnectedAt.IsZero() {
		t.Fatal("stored app metadata differs")
	}
	credential, err := st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace)
	requireOK(t, err)
	if credential.PrivateKey != app.PrivateKey {
		t.Fatal("app key did not round trip")
	}
	status, err = st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	if !reflect.DeepEqual(status, credential.WorkspaceGitHubAppStatus) {
		t.Fatal("status differs from use projection")
	}
	raw, err := json.Marshal(credential)
	requireOK(t, err)
	if strings.Contains(string(raw), app.PrivateKey) {
		t.Fatal("credential serializes key")
	}
	secrets, err := st.ListGitHubAppKeysForRedaction(ctx)
	requireOK(t, err)
	if !strings.Contains(strings.Join(secrets, "\n"), app.PrivateKey) {
		t.Fatal("stored app absent from restart redaction source")
	}
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{42}, 32))
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = st.ListGitHubAppKeysForRedaction(ctx); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("redaction must fail closed: %v", err)
	}
	_, err = st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	st.ConfigureGitHubAppKeyEncryptionKey(nil)
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace); !errors.Is(err, store.ErrGitHubAppKey) {
		t.Fatalf("missing read key: %v", err)
	}
	st.ConfigureGitHubAppKeyEncryptionKey(key)
	if _, err = st.RecordWorkspaceGitHubAppInstallation(ctx, x.Workspace, 99, 12, "org"); !errors.Is(err, store.ErrGitHubAppChanged) {
		t.Fatalf("stale app: %v", err)
	}
	installed, err := st.RecordWorkspaceGitHubAppInstallation(ctx, x.Workspace, 41, 12, "org")
	requireOK(t, err)
	if installed.InstallationID != 12 || installed.InstallationAccount != "org" || installed.InstallationRecordedAt == nil {
		t.Fatal("installation metadata differs")
	}
	// A caller cannot mutate the store by keeping a returned timestamp pointer.
	*installed.InstallationRecordedAt = installed.ConnectedAt
	check, err := st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	if check.InstallationRecordedAt == nil {
		t.Fatal("installation lost")
	}
	app.AppID = 42
	app.PrivateKey = "replacement-app-private-key-material"
	replacement, err := st.StoreWorkspaceGitHubApp(ctx, x.Workspace, app)
	requireOK(t, err)
	if replacement.InstallationID != 0 || replacement.InstallationRecordedAt != nil || replacement.InstallationAccount != "" {
		t.Fatal("replacement retained old installation")
	}
	requireOK(t, st.DeleteWorkspaceGitHubApp(ctx, x.Workspace))
	requireOK(t, st.DeleteWorkspaceGitHubApp(ctx, x.Workspace))
	status, err = st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	if status.Connected {
		t.Fatal("deleted app connected")
	}
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted use: %v", err)
	}

	// Task event readers do not expose workspace ledgers on durable backends.
	// Their native integration fixtures feed the same event assertions below.
	if !st.IsDurable() {
		events, err := st.ListEvents(ctx, "")
		requireOK(t, err)
		AssertGitHubAppEvents(t, events, x.Workspace, store.ActorFromContext(ctx).ID)
	}
}

// RunGitHubAppConformance allows native integration fixtures to check the
// encrypted row and workspace ledger alongside the shared public contract.
func RunGitHubAppConformance(t *testing.T, x Fixture) { runGitHubApps(t, x) }

func AssertGitHubAppEvents(t *testing.T, events []core.Event, workspace, actor string) {
	t.Helper()
	kinds := []string{}
	for _, event := range events {
		if !strings.HasPrefix(event.Kind, "workspace.github_app_") {
			continue
		}
		kinds = append(kinds, event.Kind)
		var payload map[string]any
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if len(payload) != 5 || payload["workspace_id"] != workspace || event.ActorID != actor {
			t.Fatal("app event contains unexpected fields or actor")
		}
		if strings.Contains(string(event.Payload), "private-key") {
			t.Fatal("app secret in event")
		}
	}
	want := []string{"workspace.github_app_connected", "workspace.github_app_installation_recorded", "workspace.github_app_replaced", "workspace.github_app_disconnected"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("app event lifecycle: %v", kinds)
	}
}

// LegacyGitHubAppKeyFixture is a sealed workspace GitHub App private key
// produced before the encryption identifiers were renamed (task
// 261007-f5c24c). The pre-rename SingleStore encryption helper at commit
// af81464168820a37dfa6e879636439f990d3c94c sealed Plaintext under Key
// (the nonsecret bytes 0x00 through 0x1f) with AES-256-GCM, a random 12-byte
// nonce, and additional data "workspace-app:"+Workspace. The renamed code
// must decrypt these bytes unchanged, so they are frozen here rather than
// produced by the renamed helper at test time (DEC-59 clause 2;
// req-delivery-and-forge AC-1.11).
var LegacyGitHubAppKeyFixture = struct {
	Workspace  string
	Plaintext  string
	Key        []byte
	Nonce      []byte
	Ciphertext []byte
}{
	Workspace:  "legacy-app-key-fixture",
	Plaintext:  "legacy-github-app-private-key-fixture",
	Key:        mustHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"),
	Nonce:      mustHex("e64da4613eb2859889852526"),
	Ciphertext: mustHex("9a53490767dff3a3c7401474a19cba031272038da47f39a489729e94380a448212cbf27bbda8615de2195881bbcf97b72e391eaf0a"),
}

func mustHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

// RunLegacyGitHubAppKeyRecovery proves that an App key sealed before the
// rename still decrypts through GetWorkspaceGitHubAppForUse and
// ListGitHubAppKeysForRedaction, and that wrong keys, another workspace's
// additional data, a short nonce, and tampered ciphertext still refuse. seed
// overwrites the stored nonce and ciphertext of an existing App row through
// backend-specific SQL and returns the database error, if any. A schema that
// refuses to store a short nonce (PostgreSQL migration 123 checks for 12
// bytes) satisfies the short-nonce refusal at the storage boundary.
func RunLegacyGitHubAppKeyRecovery(t *testing.T, st store.Backend, ctx context.Context, seed func(t *testing.T, workspace string, nonce, ciphertext []byte) error) {
	t.Helper()
	fixture := LegacyGitHubAppKeyFixture
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "legacy-app", ClientID: "legacy-client"}, PrivateKey: "placeholder-app-private-key-material"}
	other := fixture.Workspace + "-other"
	for _, workspace := range []string{fixture.Workspace, other} {
		if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), &config.Config{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{99}, 32))
	_, err := st.StoreWorkspaceGitHubApp(ctx, fixture.Workspace, app)
	requireOK(t, err)
	requireOK(t, seed(t, fixture.Workspace, fixture.Nonce, fixture.Ciphertext))

	st.ConfigureGitHubAppKeyEncryptionKey(fixture.Key)
	credential, err := st.GetWorkspaceGitHubAppForUse(ctx, fixture.Workspace)
	requireOK(t, err)
	if credential.PrivateKey != fixture.Plaintext || credential.AppID != app.AppID {
		t.Fatal("legacy App key ciphertext did not decrypt to its pre-rename plaintext")
	}
	secrets, err := st.ListGitHubAppKeysForRedaction(ctx)
	requireOK(t, err)
	if !slices.Contains(secrets, fixture.Plaintext) {
		t.Fatal("legacy App key absent from the redaction source")
	}

	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{42}, 32))
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, fixture.Workspace); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = st.ListGitHubAppKeysForRedaction(ctx); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("redaction with wrong key must fail closed: %v", err)
	}
	st.ConfigureGitHubAppKeyEncryptionKey(fixture.Key)
	if seedErr := seed(t, fixture.Workspace, fixture.Nonce[:11], fixture.Ciphertext); seedErr != nil {
		t.Logf("schema refuses a short nonce: %v", seedErr)
	} else if _, err = st.GetWorkspaceGitHubAppForUse(ctx, fixture.Workspace); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("short nonce: %v", err)
	}
	tampered := bytes.Clone(fixture.Ciphertext)
	tampered[0] ^= 1
	requireOK(t, seed(t, fixture.Workspace, fixture.Nonce, tampered))
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, fixture.Workspace); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("tampered ciphertext: %v", err)
	}
	requireOK(t, seed(t, fixture.Workspace, fixture.Nonce, fixture.Ciphertext))

	// The same bytes under another workspace fail the additional-data check.
	_, err = st.StoreWorkspaceGitHubApp(ctx, other, app)
	requireOK(t, err)
	requireOK(t, seed(t, other, fixture.Nonce, fixture.Ciphertext))
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, other); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("cross-workspace legacy replay: %v", err)
	}
	requireOK(t, st.DeleteWorkspaceGitHubApp(ctx, other))
	credential, err = st.GetWorkspaceGitHubAppForUse(ctx, fixture.Workspace)
	requireOK(t, err)
	if credential.PrivateKey != fixture.Plaintext {
		t.Fatal("legacy App key changed after refusals")
	}
}
