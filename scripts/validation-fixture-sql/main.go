// Command validation-fixture-sql performs the bounded SQL operations needed by
// repository validation fixtures. Credentials are read only from the named
// environment variable and are never emitted.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var safeDatabase = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}_test$`)

type options struct {
	backend  string
	action   string
	dsnEnv   string
	database string
	timeout  time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.backend, "backend", "", "postgres or singlestore")
	flag.StringVar(&o.action, "action", "", "create, drop, or probe")
	flag.StringVar(&o.dsnEnv, "dsn-env", "", "environment variable containing the DSN")
	flag.StringVar(&o.database, "database", "", "owned validation database")
	flag.DurationVar(&o.timeout, "timeout", 20*time.Second, "operation timeout")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "validation fixture %s %s failed: %s\n", o.backend, o.action, sanitize(err.Error()))
		os.Exit(2)
	}
}

func run(o options) error {
	if o.backend != "postgres" && o.backend != "singlestore" {
		return errors.New("backend must be postgres or singlestore")
	}
	if o.action != "create" && o.action != "drop" && o.action != "probe" {
		return errors.New("action must be create, drop, or probe")
	}
	if o.dsnEnv == "" {
		return errors.New("dsn-env is required")
	}
	dsn := os.Getenv(o.dsnEnv)
	if dsn == "" {
		return fmt.Errorf("required configuration %s is unset", o.dsnEnv)
	}
	if o.action != "probe" && !safeDatabase.MatchString(o.database) {
		return errors.New("owned database must match [a-z][a-z0-9_]{2,62}_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	if o.backend == "postgres" {
		return postgres(ctx, o, dsn)
	}
	return singlestore(ctx, o, dsn)
}

func postgres(ctx context.Context, o options, dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid PostgreSQL URL")
	}
	if o.action != "probe" {
		cfg.ConnConfig.Database = "postgres"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("client initialization: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("network/readiness probe for %s: %w", endpoint(cfg.ConnConfig.Host, cfg.ConnConfig.Port), err)
	}
	switch o.action {
	case "create":
		_, err = pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{o.database}.Sanitize())
		if err != nil {
			return fmt.Errorf("database creation: %w", err)
		}
	case "drop":
		_, err = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{o.database}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			return fmt.Errorf("owned database teardown: %w", err)
		}
	case "probe":
		var version, current string
		if err = pool.QueryRow(ctx, "SELECT version(), current_database()").Scan(&version, &current); err != nil {
			return fmt.Errorf("backend probe: %w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"identity": "postgres", "version": version, "configuration": endpoint(cfg.ConnConfig.Host, cfg.ConnConfig.Port), "instance": current,
		})
	}
	return nil
}

func singlestore(ctx context.Context, o options, dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return errors.New("invalid SingleStore DSN")
	}
	if o.action != "probe" {
		cfg.DBName = ""
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return errors.New("SingleStore client initialization failed")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("network/readiness probe for %s: %w", mysqlEndpoint(cfg.Addr), err)
	}
	quoted := "`" + strings.ReplaceAll(o.database, "`", "``") + "`"
	switch o.action {
	case "create":
		_, err = db.ExecContext(ctx, "CREATE DATABASE "+quoted+" PARTITIONS 2")
		if err != nil {
			return fmt.Errorf("database creation: %w", err)
		}
	case "drop":
		_, err = db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted)
		if err != nil {
			return fmt.Errorf("owned database teardown: %w", err)
		}
	case "probe":
		var version, current string
		if err = db.QueryRowContext(ctx, "SELECT @@version, DATABASE()").Scan(&version, &current); err != nil {
			return fmt.Errorf("backend probe: %w", err)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"identity": "singlestore", "version": version, "configuration": mysqlEndpoint(cfg.Addr), "instance": current,
		})
	}
	return nil
}

func endpoint(host string, port uint16) string {
	return fmt.Sprintf("%s:%d", host, port)
}

func mysqlEndpoint(addr string) string {
	if addr == "" {
		return "configured endpoint"
	}
	return addr
}

// sanitize is a final defense against drivers including credentials in an
// error. It deliberately returns only the stable error class after URL-like
// or password-bearing text is detected.
func sanitize(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(message, "://") || strings.Contains(lower, "password=") || strings.Contains(lower, "@tcp(") {
		return "operation failed; inspect the protected retained log for the redacted diagnostic"
	}
	return message
}
