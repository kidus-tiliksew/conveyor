package postgres

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPoolConfigMaxConns(t *testing.T) {
	pgxDefault := int32(4)
	if cpus := int32(runtime.NumCPU()); cpus > pgxDefault {
		pgxDefault = cpus
	}
	differentFromDefault := pgxDefault + 3
	cases := []struct {
		name    string
		url     string
		options []OpenOption
		want    int32
	}{
		{name: "default", url: "postgres://u:secret@localhost:5432/conveyor", want: DefaultMaxConns},
		{name: "option", url: "postgres://u:secret@localhost:5432/conveyor", options: []OpenOption{WithMaxConns(7)}, want: 7},
		{name: "url wins over option", url: "postgres://u:secret@localhost:5432/conveyor?pool_max_conns=5", options: []OpenOption{WithMaxConns(7)}, want: 5},
		{name: "url wins over default", url: "postgres://u:secret@localhost:5432/conveyor?pool_max_conns=3", want: 3},
		{name: "keyword wins over option", url: "host=localhost user=u password=secret dbname=conveyor pool_max_conns=4", options: []OpenOption{WithMaxConns(9)}, want: 4},
		{name: "keyword default", url: "host=localhost user=u password=secret dbname=conveyor", want: DefaultMaxConns},
		{name: "explicit pgx default wins", url: "postgres://u:secret@localhost:5432/conveyor?pool_max_conns=" + strconv.Itoa(int(pgxDefault)), options: []OpenOption{WithMaxConns(differentFromDefault)}, want: pgxDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options, err := resolveOpenOptions(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			config, err := poolConfig(tc.url, options)
			if err != nil {
				t.Fatal(err)
			}
			if config.MaxConns != tc.want {
				t.Fatalf("MaxConns = %d, want %d", config.MaxConns, tc.want)
			}
			if _, ok := config.ConnConfig.RuntimeParams["pool_max_conns"]; ok {
				t.Fatal("pool_max_conns must not reach the server as a runtime parameter")
			}
			if _, ok := config.ConnConfig.Tracer.(pgxpool.AcquireTracer); !ok {
				t.Fatalf("tracer %T is not an acquire tracer", config.ConnConfig.Tracer)
			}
			if _, ok := config.ConnConfig.Tracer.(*requestTracer); !ok {
				t.Fatalf("tracer %T is not the request tracer", config.ConnConfig.Tracer)
			}
		})
	}
	t.Run("keeps other options", testPoolConfigKeepsOtherOptions)
	t.Run("refuses invalid pool_max_conns without secrets", testPoolConfigRefusesInvalidPoolMaxConns)
}

func testPoolConfigKeepsOtherOptions(t *testing.T) {
	options, err := resolveOpenOptions([]OpenOption{WithMaxConns(12)})
	if err != nil {
		t.Fatal(err)
	}
	config, err := poolConfig("postgres://u:secret@localhost:5432/conveyor?pool_min_conns=2&pool_max_conn_lifetime=90m&pool_health_check_period=45s&application_name=conveyor-test&sslmode=disable", options)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxConns != 12 || config.MinConns != 2 || config.MaxConnLifetime != 90*time.Minute || config.HealthCheckPeriod != 45*time.Second {
		t.Fatalf("pool options = max %d min %d lifetime %s health %s", config.MaxConns, config.MinConns, config.MaxConnLifetime, config.HealthCheckPeriod)
	}
	if got := config.ConnConfig.RuntimeParams["application_name"]; got != "conveyor-test" {
		t.Fatalf("application_name = %q", got)
	}
	for _, name := range []string{"pool_min_conns", "pool_max_conn_lifetime", "pool_health_check_period"} {
		if _, ok := config.ConnConfig.RuntimeParams[name]; ok {
			t.Fatalf("%s must be consumed by the pool parser", name)
		}
	}
}

func testPoolConfigRefusesInvalidPoolMaxConns(t *testing.T) {
	options, err := resolveOpenOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"postgres://u:sup3rsecret@localhost:5432/conveyor?pool_max_conns=many",
		"postgres://u:sup3rsecret@localhost:5432/conveyor?pool_max_conns=0",
		"host=localhost user=u password=sup3rsecret dbname=conveyor pool_max_conns=-2",
	} {
		_, err := poolConfig(url, options)
		if err == nil {
			t.Fatalf("%s: expected an error", strings.ReplaceAll(url, "sup3rsecret", "xxx"))
		}
		if strings.Contains(err.Error(), "sup3rsecret") {
			t.Fatalf("error leaks the password: %v", err)
		}
		if !strings.HasPrefix(err.Error(), "configure Postgres pool: ") {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestOpenRefusesNonPositiveMaxConns(t *testing.T) {
	for _, n := range []int32{0, -1} {
		// The URL names an unroutable host: refusal must happen before any
		// parse or connection attempt.
		_, err := Open(t.Context(), "postgres://u:secret@192.0.2.1:5432/conveyor?connect_timeout=1", WithMaxConns(n))
		if !errors.Is(err, errNonPositiveMaxConns) {
			t.Fatalf("WithMaxConns(%d) error = %v", n, err)
		}
	}
}
