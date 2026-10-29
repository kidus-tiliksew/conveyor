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
				for _, leaked := range []string{"pa55word", "postgres://", "singlestore://", "lots", "2147483648", "99999999999999999999"} {
					if strings.Contains(err.Error(), leaked) && !strings.Contains(tc.wantErr, "cannot unmarshal") {
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
