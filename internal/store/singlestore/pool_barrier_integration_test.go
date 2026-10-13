package singlestore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// requireNoConnectionsInUse fails when any connection stays held. A cancelled
// transaction context makes database/sql roll back and release the connection
// on its awaitDone goroutine, after the caller already returned, so an
// immediate Stats().InUse read races that release. Acquiring every pool slot
// waits for in-flight releases and still fails on a real leak. The package
// runs no parallel tests, so only the calling test's releases are pending.
func requireNoConnectionsInUse(t *testing.T, db *sql.DB) {
	t.Helper()
	n := db.Stats().MaxOpenConnections
	if n == 0 {
		t.Fatal("pool barrier requires a bounded pool")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range n {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("callback leaked %d connections", db.Stats().InUse-len(conns))
		}
		conns = append(conns, conn)
	}
}
