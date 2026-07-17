package postgres

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func integrationDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse CONVEYOR_TEST_DATABASE_URL: %v", err)
	}
	databaseName := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(databaseName, "_test") {
		t.Fatalf("refusing integration test database %q: database name must end in _test", databaseName)
	}
	return databaseURL
}
