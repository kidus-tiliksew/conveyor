package storetest

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestProductionFactoryRequiresEveryCapability(t *testing.T) {
	newFixture := func(*testing.T, []config.Repo) Fixture { return Fixture{} }
	for bits := 0; bits < 8; bits++ {
		capabilities := Capabilities{Identity: bits&1 != 0, Membership: bits&2 != 0, Tokens: bits&4 != 0}
		factory := Factory{New: newFixture, Capabilities: capabilities, ProductionCapable: true}
		if err := factory.validate(); (err == nil) != (bits == 7) {
			t.Errorf("capabilities=%+v validation=%v", capabilities, err)
		}
		factory.ProductionCapable = false
		if err := factory.validate(); err != nil {
			t.Errorf("test backend optional capabilities rejected: %v", err)
		}
	}
	if err := (Factory{}).validate(); err == nil {
		t.Fatal("missing factory accepted")
	}
}
