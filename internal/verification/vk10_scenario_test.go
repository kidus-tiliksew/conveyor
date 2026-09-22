package verification_test

import (
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/vk10fixture"
	"testing"
)

func TestVK10Scenario(t *testing.T) {
	vk10fixture.Run(t, func(t *testing.T) store.Backend {
		b := store.NewVolatileBackend()
		t.Cleanup(b.Close)
		return b
	})
}
