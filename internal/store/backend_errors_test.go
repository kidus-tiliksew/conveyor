package store_test

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// These tests cover the driver-free helpers both durable backends log through.
// Their failure messages never print a prepared message, because a failing one
// can hold the fixture secret.

func TestDriverLogMessageMatchesRegisteredSecretsAtUse(t *testing.T) {
	secret := "fixture-shared-secret-" + core.NewTaskID()
	if !strings.Contains(store.DriverLogMessage("value "+secret), secret) {
		t.Fatal("control failed: an unregistered value was changed")
	}
	redact.RegisterSecret(secret, time.Time{})
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	clean := store.DriverLogMessage("value " + secret + " encoded " + encoded)
	if strings.Contains(clean, secret) || strings.Contains(clean, encoded) {
		t.Fatal("prepared message holds the registered secret")
	}
	if clean != "value [REDACTED:exact] encoded [REDACTED:encoded]" {
		t.Fatalf("prepared message of %d bytes is not the two placeholders", len(clean))
	}
}

func TestDriverLogMessageRedactsBeforeItCuts(t *testing.T) {
	const visible = 6
	token := "ghp_" + strings.Repeat("F", 36)
	// The padding ends in a space because the token pattern needs a word
	// boundary before the token.
	clean := store.DriverLogMessage(strings.Repeat("x", store.DriverLogMessageLimit-visible-1) + " " + token)
	if strings.Contains(clean, token[:visible]) {
		t.Fatal("the cut left the start of a token in the message")
	}
	if len(clean) != store.DriverLogMessageLimit {
		t.Fatalf("prepared message is %d bytes, want %d", len(clean), store.DriverLogMessageLimit)
	}
}

func TestDriverLogMessageBound(t *testing.T) {
	if store.DriverLogMessageLimit != 1024 {
		t.Fatalf("limit = %d, want 1024", store.DriverLogMessageLimit)
	}
	if got := store.DriverLogMessage("short"); got != "short" {
		t.Fatalf("a short message changed: %q", got)
	}
	exact := strings.Repeat("x", store.DriverLogMessageLimit)
	if got := store.DriverLogMessage(exact); got != exact {
		t.Fatalf("a message at the bound changed to %d bytes", len(got))
	}
	cut := store.DriverLogMessage("x" + strings.Repeat("é", store.DriverLogMessageLimit))
	if len(cut) != store.DriverLogMessageLimit-1 && len(cut) != store.DriverLogMessageLimit {
		t.Fatalf("cut message is %d bytes", len(cut))
	}
	if !utf8.ValidString(cut) {
		t.Fatal("the cut split a rune")
	}
}

type backendOperationMarker struct{}

func TestBackendOperationNamesNearestExportedCaller(t *testing.T) {
	backendPackage := reflect.TypeOf(backendOperationMarker{}).PkgPath()
	lookup := func() string { return store.BackendOperation(backendPackage) }
	if got := lookup(); got != "TestBackendOperationNamesNearestExportedCaller" {
		t.Fatalf("operation = %q", got)
	}
	if got := store.BackendOperation("example.test/no/such/package"); got != "unknown" {
		t.Fatalf("operation with no caller in the package = %q, want unknown", got)
	}
}

func TestExportedBackendOperation(t *testing.T) {
	const backendPackage = "example.test/backend"
	for function, want := range map[string]string{
		backendPackage + ".(*Store).Claim":       "Claim",
		backendPackage + ".(*Store).Claim.func1": "Claim",
		backendPackage + ".Store.Claim":          "Claim",
		backendPackage + ".Open":                 "Open",
		backendPackage + ".Open.func2":           "Open",
	} {
		if got, ok := store.ExportedBackendOperation(backendPackage, function); !ok || got != want {
			t.Fatalf("operation of %q = %q, %v; want %q", function, got, ok, want)
		}
	}
	for _, function := range []string{
		backendPackage + ".(*Store).withTx",
		backendPackage + ".(*Store).withTx.func1",
		backendPackage + ".translate",
		backendPackage + ".(*meteredConn).QueryContext",
		backendPackage + "/db.(*Queries).Get",
		"example.test/other.(*Store).Claim",
	} {
		if got, ok := store.ExportedBackendOperation(backendPackage, function); ok {
			t.Fatalf("operation of %q = %q, want none", function, got)
		}
	}
}
