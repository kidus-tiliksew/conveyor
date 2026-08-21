package postgres

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestForgeTokenEncryptedLifecycleIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	key := bytes.Repeat([]byte{0x41}, 32)
	st.ConfigureForgeTokenEncryptionKey(key)
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "Forge Org", Email: "forge@example.test", DisplayName: "Forge Owner"}, "bootstrap-secret"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), "bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithActor(t.Context(), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})

	first := "github_pat_first-plain-secret-value"
	status, err := st.StoreForgeToken(ctx, owner.ID, first, "first-login")
	if err != nil || !status.Configured || status.ForgeLogin != "first-login" || status.StoredAt.IsZero() {
		t.Fatalf("first status=%+v err=%v", status, err)
	}
	var nonce, ciphertext []byte
	var rows int
	if err = st.pool.QueryRow(ctx, `SELECT cipher_nonce,ciphertext,(SELECT count(*) FROM user_forge_tokens) FROM user_forge_tokens WHERE user_id=$1`, owner.ID).Scan(&nonce, &ciphertext, &rows); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || rows != 1 || bytes.Contains(ciphertext, []byte(first)) || bytes.Equal(ciphertext, []byte(first)) {
		t.Fatalf("nonce=%d rows=%d plaintext ciphertext=%t", len(nonce), rows, bytes.Contains(ciphertext, []byte(first)))
	}
	if got, err := st.GetForgeTokenForUse(ctx, owner.ID); err != nil || got.Token != first || got.ForgeLogin != "first-login" {
		t.Fatalf("use=%+v err=%v", got, err)
	}

	second := "github_pat_second-plain-secret-value"
	replaced, err := st.StoreForgeToken(ctx, owner.ID, second, "second-login")
	if err != nil || replaced.ForgeLogin != "second-login" {
		t.Fatalf("replace=%+v err=%v", replaced, err)
	}
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM user_forge_tokens WHERE user_id=$1`, owner.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	if values, err := st.ListForgeTokensForRedaction(ctx); err != nil || len(values) != 1 || values[0] != second {
		t.Fatalf("redaction values=%v err=%v", values, err)
	}
	var auditPayloads []string
	rowsQuery, err := st.pool.Query(ctx, `SELECT payload_json::text FROM deployment_events WHERE kind IN ('identity.forge_token_stored','identity.forge_token_replaced') ORDER BY id`)
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
		t.Fatalf("secret-bearing forge-token audit payloads: %v", auditPayloads)
	}
	metadata, err := st.GetForgeTokenStatus(ctx, owner.ID)
	if err != nil || !metadata.Configured || metadata.ForgeLogin != "second-login" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}

	st.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{0x42}, 32))
	if _, err = st.GetForgeTokenForUse(ctx, owner.ID); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatalf("wrong-key err=%v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(nil)
	if _, err = st.GetForgeTokenForUse(ctx, owner.ID); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing-key err=%v", err)
	}
	st.ConfigureForgeTokenEncryptionKey(key)

	if _, err = st.DeactivateIdentityUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if metadata, err = st.GetForgeTokenStatus(ctx, owner.ID); err != nil || metadata.Configured || metadata.ForgeLogin != "" {
		t.Fatalf("inactive metadata=%+v err=%v", metadata, err)
	}
	if _, err = st.GetForgeTokenForUse(ctx, owner.ID); !errors.Is(err, store.ErrForgeTokenOwnerInactive) {
		t.Fatalf("inactive use err=%v", err)
	}
	if err = st.DeleteForgeToken(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteForgeToken(ctx, owner.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestForgeTokenMissingKeyRejectsStoreIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	if _, err := st.BootstrapIdentity(t.Context(), config.FirstOperatorIdentity{OrganizationName: "No Key", Email: "nokey@example.test", DisplayName: "No Key"}, "bootstrap-secret"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(t.Context(), "bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.StoreForgeToken(t.Context(), owner.ID, "candidate", "login"); !errors.Is(err, store.ErrForgeTokenKey) {
		t.Fatalf("missing-key store err=%v", err)
	}
	var rows int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM user_forge_tokens`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}
