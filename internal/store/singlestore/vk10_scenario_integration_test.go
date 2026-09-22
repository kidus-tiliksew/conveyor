package singlestore

import (
	"os"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/vk10fixture"
)

func TestVK10SingleStoreScenarioIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("missing SingleStore evidence: CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	vk10fixture.Run(t, func(t *testing.T) store.Backend { return integrationStore(t) })
}
