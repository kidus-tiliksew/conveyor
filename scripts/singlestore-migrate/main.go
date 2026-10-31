// Command singlestore-migrate applies the backend's numbered schema migrations
// without starting a server or bootstrapping application events. It does not
// create, copy, validate, or synchronize the operator-owned events_columnstore.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
)

func main() {
	raw := os.Getenv("CONVEYOR_DATABASE_URL")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "CONVEYOR_DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := singlestore.Migrate(ctx, raw); err != nil {
		// Driver errors can include connection or row data. Keep this operator
		// entrypoint credential-safe rather than printing the underlying error.
		fmt.Fprintln(os.Stderr, "SingleStore schema migration failed; keep traffic stopped and inspect the migration ledger and schema")
		os.Exit(1)
	}
	fmt.Println("SingleStore numbered schema migrations complete; operator event cutover remains separate")
}
