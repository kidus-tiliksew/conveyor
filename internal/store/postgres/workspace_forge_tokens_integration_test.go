package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestWorkspaceForgeTokenEncryptedLifecycleIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	workspaceA := "workspace-forge-" + core.NewTaskID()
	workspaceB := "workspace-forge-" + core.NewTaskID()
	for _, workspaceID := range []string{workspaceA, workspaceB} {
		if seeded, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(t.Context(), workspaceID), isolationConfig(workspaceID)); err != nil || !seeded {
			t.Fatalf("seed %s=%t err=%v", workspaceID, seeded, err)
		}
	}
	ctxA := store.WithActor(store.WithWorkspace(t.Context(), workspaceA), store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser})
	if _, err := st.StoreWorkspaceForgeToken(ctxA, workspaceA, "missing-key-secret", "login"); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing-key store err=%v", err)
	}

	key := bytes.Repeat([]byte{0x61}, 32)
	st.ConfigureForgeTokenEncryptionKey(key)
	first := "github_pat_workspace-first-plain-secret"
	status, err := st.StoreWorkspaceForgeToken(ctxA, workspaceA, first, "first-login")
	if err != nil || !status.Configured || status.ForgeLogin != "first-login" || status.StoredAt.IsZero() {
		t.Fatalf("first status=%+v err=%v", status, err)
	}
	var nonce, ciphertext []byte
	var rows int
	if err = st.pool.QueryRow(ctxA, `SELECT cipher_nonce,ciphertext,(SELECT count(*) FROM workspace_forge_tokens WHERE workspace_id=$1) FROM workspace_forge_tokens WHERE workspace_id=$1`, workspaceA).Scan(&nonce, &ciphertext, &rows); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || rows != 1 || bytes.Contains(ciphertext, []byte(first)) || bytes.Equal(ciphertext, []byte(first)) {
		t.Fatalf("nonce=%d rows=%d plaintext ciphertext=%t", len(nonce), rows, bytes.Contains(ciphertext, []byte(first)))
	}
	if got, useErr := st.GetWorkspaceForgeTokenForUse(ctxA, workspaceA); useErr != nil || got.Token != first || got.WorkspaceID != workspaceA || got.ForgeLogin != "first-login" {
		t.Fatalf("use=%+v err=%v", got, useErr)
	}
	if foreign, statusErr := st.GetWorkspaceForgeTokenStatus(store.WithWorkspace(t.Context(), workspaceB), workspaceB); statusErr != nil || foreign.Configured {
		t.Fatalf("foreign status=%+v err=%v", foreign, statusErr)
	}
	if _, useErr := st.GetWorkspaceForgeTokenForUse(store.WithWorkspace(t.Context(), workspaceB), workspaceB); !errors.Is(useErr, store.ErrNotFound) {
		t.Fatalf("foreign use err=%v", useErr)
	}

	second := "github_pat_workspace-second-plain-secret"
	replaced, err := st.StoreWorkspaceForgeToken(ctxA, workspaceA, second, "second-login")
	if err != nil || replaced.ForgeLogin != "second-login" {
		t.Fatalf("replace=%+v err=%v", replaced, err)
	}
	if values, listErr := st.ListForgeTokensForRedaction(ctxA); listErr != nil || len(values) != 1 || values[0] != second {
		t.Fatalf("redaction values=%v err=%v", values, listErr)
	}

	var auditPayloads []string
	rowsQuery, err := st.pool.Query(ctxA, `SELECT payload_json::text FROM events WHERE workspace_id=$1 AND kind IN ('workspace.forge_token_stored','workspace.forge_token_replaced') ORDER BY id`, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	for rowsQuery.Next() {
		var payload string
		if err = rowsQuery.Scan(&payload); err != nil {
			rowsQuery.Close()
			t.Fatal(err)
		}
		auditPayloads = append(auditPayloads, payload)
	}
	if err = rowsQuery.Err(); err != nil {
		rowsQuery.Close()
		t.Fatal(err)
	}
	rowsQuery.Close()
	if len(auditPayloads) != 2 || strings.Contains(strings.Join(auditPayloads, "\n"), first) || strings.Contains(strings.Join(auditPayloads, "\n"), second) {
		t.Fatalf("secret-bearing workspace forge-token events: %v", auditPayloads)
	}

	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{0x62}, 32))
	if _, err = st.GetWorkspaceForgeTokenForUse(ctxA, workspaceA); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatalf("wrong-key err=%v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(nil)
	if _, err = st.GetWorkspaceForgeTokenForUse(ctxA, workspaceA); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing-key err=%v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(key)

	if err = st.DeleteWorkspaceForgeToken(ctxA, workspaceA); err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteWorkspaceForgeToken(ctxA, workspaceA); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if metadata, statusErr := st.GetWorkspaceForgeTokenStatus(ctxA, workspaceA); statusErr != nil || metadata.Configured {
		t.Fatalf("deleted metadata=%+v err=%v", metadata, statusErr)
	}
	var deletedEvents int
	if err = st.pool.QueryRow(ctxA, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='workspace.forge_token_deleted'`, workspaceA).Scan(&deletedEvents); err != nil || deletedEvents != 1 {
		t.Fatalf("delete events=%d err=%v", deletedEvents, err)
	}
}
