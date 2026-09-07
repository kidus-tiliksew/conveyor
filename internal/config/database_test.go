package config

import (
	"strings"
	"testing"
)

func TestDatabaseForURL(t *testing.T) {
	for _, tt := range []struct{ url, backend string }{
		{"", "memory"}, {"postgres://localhost/conveyor", "postgres"}, {"postgresql://localhost/conveyor", "postgres"}, {"host=localhost dbname=conveyor", "postgres"},
		{"singlestore://root@localhost/conveyor", "singlestore"}, {"mysql://root@localhost/conveyor", "singlestore"}, {"root:fixture@tcp(localhost:3306)/conveyor", "singlestore"}, {"root@unix(/socket)/conveyor", "singlestore"},
	} {
		t.Run(tt.backend+tt.url, func(t *testing.T) {
			got := DatabaseForURL(tt.url)
			if got.Backend != tt.backend || got.URL != tt.url {
				t.Fatalf("selection=%+v", got)
			}
		})
	}
}

func TestDatabaseAdmissionConfiguration(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "")
	for _, name := range []string{"postgres", "singlestore", "memory"} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Database = Database{Backend: name}
			_, err := normalize(c, "database admission")
			if name == "memory" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL") || !strings.Contains(err.Error(), "postgres://") || !strings.Contains(err.Error(), "singlestore://") {
				t.Fatalf("missing database URL: %v", err)
			}
			c.Database.URL = "postgres://localhost/conveyor_test"
			if name == "singlestore" {
				c.Database.URL = "singlestore://localhost/conveyor_test"
			}
			got, err := normalize(c, "database admission")
			if err != nil || got.Database.Backend != name {
				t.Fatalf("backend=%s err=%v", name, err)
			}
		})
	}
	c := validConfig()
	c.Database = Database{Backend: "unsupported"}
	_, err := normalize(c, "database admission")
	if err == nil {
		t.Fatal("unsupported backend accepted")
	}
	for _, name := range []string{"postgres", "singlestore", "memory"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("accepted backend %s missing from error %v", name, err)
		}
	}
}
