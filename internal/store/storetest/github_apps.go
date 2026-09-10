package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
	st.ConfigureForgeTokenEncryptionKey(nil)
	if _, err = st.StoreWorkspaceGitHubApp(ctx, x.Workspace, app); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing key: %v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(key)
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
	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{42}, 32))
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = st.ListGitHubAppKeysForRedaction(ctx); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatalf("redaction must fail closed: %v", err)
	}
	_, err = st.GetWorkspaceGitHubAppStatus(ctx, x.Workspace)
	requireOK(t, err)
	st.ConfigureForgeTokenEncryptionKey(nil)
	if _, err = st.GetWorkspaceGitHubAppForUse(ctx, x.Workspace); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing read key: %v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(key)
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
