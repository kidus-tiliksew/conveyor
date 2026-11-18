// Package singlestore implements the SingleStore backend governed by DEC-39.
// Its schema, locks and driver types stay within this backend.
package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

type Store struct {
	gitHubAppKeyEncryptionState
	db            *sql.DB
	log           eventlog.Store
	changes       *changes.Hub
	hotReadLayout map[string]bool
	// reads attributes received bytes to store-method labels; nil for a
	// store assembled without Open (component-persistence, Database read
	// accounting).
	reads *dbmetrics.ReadLedger
	// compression reads protocol compression from the handshakes of this
	// store's connections; nil for a store assembled without Open.
	compression *compressionObserver
	// peerAccepted holds the task-event position each workspace's last
	// successful PeerMarks read accepted; peerMu serializes those reads.
	peerMu       sync.Mutex
	peerAccepted map[string]int64
}

var _ store.Backend = (*Store)(nil)

// connectionConfig accepts a URL or a MySQL DSN without exposing credentials in errors.
func connectionConfig(raw string, compress ...bool) (*mysql.Config, error) {
	var cfg *mysql.Config
	dsnCompression := false
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
		if value, ok := u.Query()["compress"]; ok {
			if len(value) != 1 {
				return nil, fmt.Errorf("invalid SingleStore compression configuration")
			}
			enabled, err := strconv.ParseBool(value[0])
			if err != nil || cfg.Apply(mysql.EnableCompression(enabled)) != nil {
				return nil, fmt.Errorf("invalid SingleStore compression configuration")
			}
			dsnCompression = true
		}
	} else {
		var err error
		cfg, err = mysql.ParseDSN(raw)
		if err != nil || cfg.DBName == "" {
			return nil, fmt.Errorf("invalid SingleStore DSN")
		}
		if _, query, ok := strings.Cut(raw[strings.LastIndex(raw, "/")+1:], "?"); ok {
			params, err := url.ParseQuery(query)
			if err != nil {
				return nil, fmt.Errorf("invalid SingleStore DSN")
			}
			_, dsnCompression = params["compress"]
		}
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if len(compress) > 0 || !dsnCompression {
		requested := true
		if len(compress) > 0 {
			requested = compress[0]
		}
		if err := cfg.Apply(mysql.EnableCompression(requested)); err != nil {
			return nil, fmt.Errorf("invalid SingleStore compression configuration")
		}
	}
	// DATETIME(6) rejects nanosecond fractions in strict SingleStore sessions.
	// Apply this to every write, including future aggregate SQL (component-persistence).
	if err := cfg.Apply(mysql.TimeTruncate(time.Microsecond)); err != nil {
		return nil, fmt.Errorf("invalid SingleStore timestamp configuration")
	}
	cfg.MultiStatements = false
	// A prepared statement sends each argument longer than the packet size
	// divided by its parameter count plus one through COM_STMT_SEND_LONG_DATA,
	// which SingleStore refuses with error 1706. Interpolated arguments travel
	// inside one text query, so a pinned claim snapshot of a few MiB fits
	// (component-persistence, Migration runners and ledgers).
	cfg.InterpolateParams = true
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

// compressionRequested reports whether cfg asks the driver for protocol
// compression. The driver keeps the setting unexported, so it is read back
// from the configuration's own DSN form.
func compressionRequested(cfg *mysql.Config) bool {
	dsn := cfg.FormatDSN()
	_, query, _ := strings.Cut(dsn[strings.LastIndex(dsn, "/")+1:], "?")
	params, _ := url.ParseQuery(query)
	return params.Get("compress") == "true"
}

// Open connects, migrates, and checks the server. It logs nothing about
// compression: conveyord reports it once at startup through Compression,
// and CLI opens report nothing (component-runtime invariant 43).
func Open(ctx context.Context, raw string, compress ...bool) (*Store, error) {
	cfg, err := connectionConfig(raw, compress...)
	if err != nil {
		return nil, err
	}
	// The counting dial attributes received bytes to store-method labels
	// (component-persistence, Database read accounting).
	reads := dbmetrics.NewReadLedger()
	// The handshake observer reports compression without a statement of its
	// own (component-runtime invariant 43; component-persistence
	// invariant 28).
	compression := &compressionObserver{requested: compressionRequested(cfg)}
	cfg.DialFunc = countingDialFunc(reads, compression.wrap)
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid SingleStore connection configuration")
	}
	// The metered connector records request query counts and durations
	// (component-persistence, Request database metrics and pool sizing).
	db := sql.OpenDB(newMeteredConnector(connector, time.Now))
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	hub := changes.NewHub()
	st := &Store{db: db, log: logqueue.Notify(s2log.New(db), hub), changes: hub, reads: reads, compression: compression}
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
		if err == nil {
			err = st.checkEventsPrerequisite(ctx)
		}
		if err == nil {
			err = st.initHotReadLayout(ctx)
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
	s.beginNotices(tx)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
		finishNotices(tx, committed)
	}()
	if err = fn(tx); err != nil {
		return translateBackendConflict(err)
	}
	if err = commitTx(ctx, tx); err != nil {
		return translateBackendConflict(err)
	}
	committed = true
	return nil
}
func workspace(ctx context.Context) (string, error) {
	id, ok := store.WorkspaceFromContext(ctx)
	if !ok || id == "" {
		return "", store.ErrWorkspaceRequired
	}
	return id, nil
}
