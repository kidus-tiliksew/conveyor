package dispatch_test

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/publicationfixture"
)

// D7 on PostgreSQL: the automatic sweep re-enqueues failed publications on
// the durable queue and the production handlers publish them.
func TestAutomaticPublicationRecoveryPostgresIntegration(t *testing.T) {
	raw := disposablePostgresURL(t, "Automatic publication recovery")
	publicationfixture.Run(t, func(t *testing.T) store.Backend { return postgresSchemaBackend(t, raw, "autopub_") })
}
