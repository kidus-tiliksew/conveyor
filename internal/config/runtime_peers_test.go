package config

import (
	"strings"
	"testing"
)

// TestDeploymentRuntimePeers covers the runtime.peers deployment key
// (component-runtime, Peers): off by default, on when set, strict about
// unknown keys, and omitted from the rendered file while off.
func TestDeploymentRuntimePeers(t *testing.T) {
	base := string(deploymentWithDatabase(`{url: "postgres://db.invalid/conveyor"}`))
	for _, tc := range []struct {
		name, section string
		want          bool
		wantErr       string
	}{
		{name: "omitted"},
		{name: "explicit false", section: "runtime:\n  peers: false\n"},
		{name: "on", section: "runtime:\n  peers: true\n", want: true},
		{name: "unknown key", section: "runtime:\n  peer: true\n", wantErr: "field peer not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseDeployment([]byte(base+tc.section), "peers.yaml", nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Runtime.Peers != tc.want {
				t.Fatalf("peers=%t, want %t", cfg.Runtime.Peers, tc.want)
			}
			rendered, err := MarshalDeployment(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(rendered), "runtime:"); got != tc.want {
				t.Fatalf("rendered runtime section=%t, want %t:\n%s", got, tc.want, rendered)
			}
		})
	}
}
