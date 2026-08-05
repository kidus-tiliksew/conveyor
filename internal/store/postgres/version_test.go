package postgres

import (
	"strings"
	"testing"
)

func TestValidatePostgresVersion(t *testing.T) {
	if minimumPostgresVersionNum != 150000 {
		t.Fatalf("minimum version=%d", minimumPostgresVersionNum)
	}
	if err := validatePostgresVersion(150000); err != nil {
		t.Fatalf("Postgres 15 rejected: %v", err)
	}
	if err := validatePostgresVersion(160003); err != nil {
		t.Fatalf("Postgres 16 rejected: %v", err)
	}
	err := validatePostgresVersion(140012)
	if err == nil {
		t.Fatal("Postgres 14 accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "15") || !strings.Contains(message, "140012") {
		t.Fatalf("version error is not actionable: %s", message)
	}
}
