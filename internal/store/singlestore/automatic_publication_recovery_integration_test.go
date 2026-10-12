package singlestore

import (
	"os"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/publicationfixture"
)

// D7 on SingleStore: the automatic sweep re-enqueues failed publications on
// the durable queue and the production handlers publish them.
func TestAutomaticPublicationRecoverySingleStoreIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("missing SingleStore evidence: CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	publicationfixture.Run(t, func(t *testing.T) store.Backend { return integrationStore(t) })
}
