package config

import (
	"os"
	"strings"
	"testing"
)

// unsetEnv removes key for the rest of the test; t.Setenv restores it.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func deploymentWithDatabase(database string) []byte {
	return []byte(`workspace: default
database: ` + database + `
execution_settings:
  control_plane:
    triage: {model: gpt-luna, timeout: 20m}
repos: []
`)
}

func TestDeploymentDatabaseMaxConns(t *testing.T) {
	const url = `url: "postgres://conveyor:pa55word@db.invalid/conveyor"`
	unset := "\x00unset"
	cases := []struct {
		name     string
		database string
		env      string
		want     int64 // 0 means nil
		wantErr  string
	}{
		{name: "omitted key and variable", database: "{" + url + "}", env: unset},
		{name: "explicit null", database: "{" + url + ", max_conns: null}", env: unset},
		{name: "file setting", database: "{" + url + ", max_conns: 48}", env: unset, want: 48},
		{name: "file maximum", database: "{" + url + ", max_conns: 2147483647}", env: unset, want: 2147483647},
		{name: "variable fallback", database: "{" + url + "}", env: "24", want: 24},
		{name: "variable with spaces", database: "{" + url + "}", env: " 12 ", want: 12},
		{name: "file wins over variable", database: "{" + url + ", max_conns: 48}", env: "24", want: 48},
		{name: "empty variable is absent", database: "{" + url + "}", env: ""},
		{name: "whitespace variable is absent", database: "{" + url + "}", env: "  \t"},
		{name: "file zero", database: "{" + url + ", max_conns: 0}", env: unset, wantErr: "database.max_conns must be an integer from 1 to 2147483647"},
		{name: "file negative", database: "{" + url + ", max_conns: -1}", env: unset, wantErr: "database.max_conns must be an integer from 1 to 2147483647"},
		{name: "file overflow", database: "{" + url + ", max_conns: 2147483648}", env: unset, wantErr: "database.max_conns must be an integer from 1 to 2147483647"},
		{name: "file non-integer", database: "{" + url + ", max_conns: many}", env: unset, wantErr: "cannot unmarshal"},
		{name: "file empty string", database: "{" + url + `, max_conns: ""}`, env: unset, wantErr: "cannot unmarshal"},
		{name: "variable malformed", database: "{" + url + "}", env: "lots", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "variable zero", database: "{" + url + "}", env: "0", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "variable negative", database: "{" + url + "}", env: "-4", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "variable overflow", database: "{" + url + "}", env: "2147483648", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "variable huge", database: "{" + url + "}", env: "99999999999999999999", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "invalid variable fails when file wins", database: "{" + url + ", max_conns: 48}", env: "lots", wantErr: "CONVEYOR_DATABASE_MAX_CONNS must be an integer from 1 to 2147483647"},
		{name: "singlestore keeps a valid setting", database: `{backend: singlestore, url: "singlestore://conveyor:pa55word@db.invalid:3306/conveyor", max_conns: 20}`, env: unset, want: 20},
		{name: "singlestore validates the setting", database: `{backend: singlestore, url: "singlestore://conveyor:pa55word@db.invalid:3306/conveyor", max_conns: 0}`, env: unset, wantErr: "database.max_conns must be an integer from 1 to 2147483647"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == unset {
				unsetEnv(t, DatabaseMaxConnsEnv)
			} else {
				t.Setenv(DatabaseMaxConnsEnv, tc.env)
			}
			cfg, err := ParseDeployment(deploymentWithDatabase(tc.database), "pool.yaml", nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				for _, leaked := range []string{"pa55word", "postgres://", "singlestore://", "lots", "many", "2147483648", "99999999999999999999"} {
					if strings.Contains(err.Error(), leaked) {
						t.Fatalf("error leaks %q: %v", leaked, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if cfg.Database.MaxConns != nil {
					t.Fatalf("MaxConns = %d, want nil", *cfg.Database.MaxConns)
				}
				return
			}
			if cfg.Database.MaxConns == nil || *cfg.Database.MaxConns != tc.want {
				t.Fatalf("MaxConns = %v, want %d", cfg.Database.MaxConns, tc.want)
			}
		})
	}
}

// TestDeploymentSingleStoreCompression covers the daemon's compression
// settings after backend inference: the file key wins, then the variable,
// and an absent pair leaves the URL parameter and the default to the
// SingleStore open. The variable must be a boolean for an effective
// SingleStore backend, even when the file wins, and the refusal echoes no
// value. PostgreSQL and memory ignore the variable (component-runtime
// invariant 43).
func TestDeploymentSingleStoreCompression(t *testing.T) {
	const (
		singlestoreURL = `url: "singlestore://conveyor:pa55word@db.invalid:3306/conveyor"`
		mysqlDSN       = `url: "conveyor:pa55word@tcp(db.invalid:3306)/conveyor?compress=false"`
		postgresURL    = `url: "postgres://conveyor:pa55word@db.invalid/conveyor"`
		unset          = "\x00unset"
		absent         = 0
		on             = 1
		off            = 2
	)
	for _, tc := range []struct {
		name, database, env string
		want                int
		wantBackend         string
		wantErr             bool
	}{
		{name: "explicit singlestore default leaves the parameter and default", database: "{backend: singlestore, " + singlestoreURL + "}", env: unset, want: absent, wantBackend: "singlestore"},
		{name: "inferred singlestore default", database: "{" + singlestoreURL + "}", env: unset, want: absent, wantBackend: "singlestore"},
		{name: "environment disables explicit singlestore", database: "{backend: singlestore, " + singlestoreURL + "}", env: "false", want: off, wantBackend: "singlestore"},
		{name: "environment disables inferred singlestore", database: "{" + singlestoreURL + "}", env: "false", want: off, wantBackend: "singlestore"},
		{name: "environment enables inferred mysql dsn over its parameter", database: "{" + mysqlDSN + "}", env: " true ", want: on, wantBackend: "singlestore"},
		{name: "environment accepts other boolean forms", database: "{" + singlestoreURL + "}", env: "0", want: off, wantBackend: "singlestore"},
		{name: "file enables over environment", database: "{backend: singlestore, " + singlestoreURL + ", singlestore_compress: true}", env: "false", want: on, wantBackend: "singlestore"},
		{name: "file disables over environment", database: "{" + singlestoreURL + ", singlestore_compress: false}", env: "true", want: off, wantBackend: "singlestore"},
		{name: "file setting without environment", database: "{" + singlestoreURL + ", singlestore_compress: false}", env: unset, want: off, wantBackend: "singlestore"},
		{name: "invalid environment stops explicit singlestore", database: "{backend: singlestore, " + singlestoreURL + "}", env: "maybe-secret", wantErr: true},
		{name: "invalid environment stops inferred singlestore", database: "{" + singlestoreURL + "}", env: "maybe-secret", wantErr: true},
		{name: "invalid environment stops singlestore when the file wins", database: "{" + singlestoreURL + ", singlestore_compress: true}", env: "maybe-secret", wantErr: true},
		{name: "empty environment stops singlestore", database: "{" + singlestoreURL + "}", env: "", wantErr: true},
		{name: "explicit postgres ignores invalid environment", database: "{backend: postgres, " + postgresURL + "}", env: "maybe-secret", want: absent, wantBackend: "postgres"},
		{name: "inferred postgres ignores invalid environment", database: "{" + postgresURL + "}", env: "maybe-secret", want: absent, wantBackend: "postgres"},
		{name: "postgres ignores valid environment", database: "{" + postgresURL + "}", env: "false", want: absent, wantBackend: "postgres"},
		{name: "memory ignores invalid environment", database: "{backend: memory}", env: "maybe-secret", want: absent, wantBackend: "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetEnv(t, "CONVEYOR_DATABASE_URL")
			if tc.env == unset {
				unsetEnv(t, "CONVEYOR_SINGLESTORE_COMPRESS")
			} else {
				t.Setenv("CONVEYOR_SINGLESTORE_COMPRESS", tc.env)
			}
			cfg, err := ParseDeployment(deploymentWithDatabase(tc.database), "compression.yaml", nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid compression setting was accepted")
				}
				if err.Error() != "CONVEYOR_SINGLESTORE_COMPRESS must be a boolean" {
					t.Fatalf("error = %v", err)
				}
				for _, leaked := range []string{"maybe-secret", "pa55word", "singlestore://", "db.invalid"} {
					if strings.Contains(err.Error(), leaked) {
						t.Fatalf("error leaks %q: %v", leaked, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Database.Backend != tc.wantBackend {
				t.Fatalf("backend = %q, want %q", cfg.Database.Backend, tc.wantBackend)
			}
			got := absent
			if cfg.Database.SingleStoreCompress != nil {
				got = off
				if *cfg.Database.SingleStoreCompress {
					got = on
				}
			}
			if got != tc.want {
				t.Fatalf("compression setting = %v, want %d (0 absent, 1 on, 2 off)", cfg.Database.SingleStoreCompress, tc.want)
			}
		})
	}
}

func TestDeploymentDatabaseMaxConnsStaysOutOfPolicy(t *testing.T) {
	t.Setenv(DatabaseMaxConnsEnv, "40")
	cfg, err := ParseDeployment(deploymentWithDatabase(`{url: "postgres://db.invalid/conveyor"}`), "pool.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.MaxConns == nil || *cfg.Database.MaxConns != 40 {
		t.Fatalf("MaxConns = %v", cfg.Database.MaxConns)
	}
	policy, err := MarshalPolicyDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(policy), "max_conns") || strings.Contains(string(policy), "40") {
		t.Fatalf("policy document carries the pool size:\n%s", policy)
	}
	rendered, err := MarshalDeployment(&Config{Database: DatabaseForURL("postgres://db.invalid/conveyor")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "max_conns") {
		t.Fatalf("deployment rendering invents max_conns:\n%s", rendered)
	}
}
