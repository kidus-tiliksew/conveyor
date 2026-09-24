package main

import (
	"context"
	"strings"
	"testing"
)

func TestOwnedDatabaseName(t *testing.T) {
	for _, name := range []string{"conveyor_ab12_test", "a12_test"} {
		if !safeDatabase.MatchString(name) {
			t.Fatalf("expected safe name %q", name)
		}
	}
	for _, name := range []string{"conveyor", "production", "Conveyor_x_test", "x_test", "a;drop_test"} {
		if safeDatabase.MatchString(name) {
			t.Fatalf("unsafe name accepted: %q", name)
		}
	}
}

func TestSanitizeCredentialShapedErrors(t *testing.T) {
	for _, message := range []string{"postgres://user:secret@host/db", "password=secret", "root:secret@tcp(host:3306)"} {
		if got := sanitize(message); got == message {
			t.Fatalf("credential-shaped diagnostic was not sanitized: %q", message)
		}
	}
	if got := sanitize("connection refused at 127.0.0.1:3306"); got != "connection refused at 127.0.0.1:3306" {
		t.Fatalf("actionable safe diagnostic changed: %q", got)
	}
}

func TestProductionParentRefusedBeforeConnection(t *testing.T) {
	for _, backend := range []string{"postgres", "singlestore"} {
		for _, action := range []string{"create", "drop", "probe"} {
			o := options{backend: backend, action: action, database: "conveyor_owned_test"}
			var err error
			if backend == "postgres" {
				err = postgres(context.Background(), o, "postgres://admin:secret@foreign.invalid/production")
			} else {
				err = singlestore(context.Background(), o, "admin:secret@tcp(foreign.invalid:3306)/production")
			}
			if err == nil || !strings.Contains(err.Error(), "must end in _test") {
				t.Fatalf("%s %s did not refuse before network access: %v", backend, action, err)
			}
		}
	}
}
