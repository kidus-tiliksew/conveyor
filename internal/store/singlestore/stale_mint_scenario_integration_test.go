package singlestore

import (
	"os"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/stalemintfixture"
)

// Real-dispatch stage-mint cases on SingleStore (component-work-orders; DEC-62).
func TestStaleMintScenarioSingleStoreIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("missing SingleStore evidence: CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	stalemintfixture.Run(t, func(t *testing.T) store.Backend { return integrationStore(t) })
}
