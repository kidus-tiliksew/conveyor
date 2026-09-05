// Package singlestore is the experimental backend defined by DEC-38.
// Its schema, locks and driver types stay within this backend.
package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type Store struct {
	db  *sql.DB
	log *s2log.Store
}

var _ store.Backend = (*Store)(nil)

// connectionConfig accepts a URL or a MySQL DSN without exposing credentials in errors.
func connectionConfig(raw string) (*mysql.Config, error) {
	var cfg *mysql.Config
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "singlestore" && u.Scheme != "mysql") || u.Hostname() == "" || strings.Trim(u.Path, "/") == "" {
			return nil, fmt.Errorf("invalid SingleStore URL")
		}
		cfg = mysql.NewConfig()
		cfg.Net = "tcp"
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		cfg.Addr = net.JoinHostPort(u.Hostname(), port)
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		if u.User != nil {
			cfg.User = u.User.Username()
			cfg.Passwd, _ = u.User.Password()
		}
		if tls := u.Query().Get("tls"); tls != "" {
			cfg.TLSConfig = tls
		}
	} else {
		var err error
		cfg, err = mysql.ParseDSN(raw)
		if err != nil || cfg.DBName == "" {
			return nil, fmt.Errorf("invalid SingleStore DSN")
		}
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = false
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	cfg.Params["sql_mode"] = "'STRICT_ALL_TABLES'"
	return cfg, nil
}

func Open(ctx context.Context, raw string) (*Store, error) {
	cfg, err := connectionConfig(raw)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid SingleStore connection configuration")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	st := &Store{db: db, log: s2log.New(db)}
	if err = db.PingContext(ctx); err == nil {
		// SingleStore accepts SET time_zone but ignores it. Refuse a non-UTC
		// server instead of interpreting local DATETIME defaults as UTC.
		var zone string
		err = db.QueryRowContext(ctx, "SELECT @@system_time_zone").Scan(&zone)
		if err == nil && zone != "UTC" {
			err = fmt.Errorf("SingleStore system_time_zone must be UTC")
		}
		if err == nil {
			err = st.migrate(ctx)
		}
	}
	if err != nil {
		db.Close()
		return nil, translateBackendConflict(err)
	}
	return st, nil
}
func (s *Store) Close()              { s.db.Close() }
func (s *Store) Log() eventlog.Store { return s.log }
func (s *Store) IsDurable() bool     { return true }

// withTx owns rollback on callback failure, cancellation and panic. Commit errors
// cross the same sentinel translation boundary as callback errors.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return translateBackendConflict(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return translateBackendConflict(err)
	}
	return translateBackendConflict(tx.Commit())
}
func workspace(ctx context.Context) (string, error) {
	id, ok := store.WorkspaceFromContext(ctx)
	if !ok || id == "" {
		return "", store.ErrWorkspaceRequired
	}
	return id, nil
}
