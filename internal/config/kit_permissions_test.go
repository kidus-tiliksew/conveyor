package config

import (
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"strings"
	"testing"
)

func TestKitPermissionLocalScope(t *testing.T) {
	c := &Config{KitPermissions: []KitPermissionGrant{{Server: "https://factory.test", Workspace: "demo", Repository: "repo", Binding: "api", Actions: []verification.VerificationPermission{{Kind: "network", Binding: "api", Target: "https://API.test/"}}}}}
	got, err := c.KitActions("https://factory.test/", "demo", "repo")
	if err != nil || len(got) != 1 || got[0].Target != "https://api.test:443" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, scope := range [][3]string{{"https://other.test", "demo", "repo"}, {"https://factory.test", "other", "repo"}, {"https://factory.test", "demo", "other"}} {
		if _, err := c.KitActions(scope[0], scope[1], scope[2]); err == nil {
			t.Fatal("foreign scope acquired local permission")
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "kit_permissions") || strings.Contains(string(encoded), "API.test") {
		t.Fatal("local grants escaped configuration projection")
	}
	c.KitPermissions[0].Actions[0].Binding = "other"
	if err := c.KitPermissions[0].Validate(); err == nil {
		t.Fatal("cross-binding grant accepted")
	}
}
