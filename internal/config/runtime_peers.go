package config

// RuntimeSettings is the deployment file's runtime section
// (component-runtime, Peers).
type RuntimeSettings struct {
	// Peers declares that this daemon runs beside other daemons on one
	// database. On PostgreSQL the daemons wake each other through
	// LISTEN/NOTIFY; on a backend without change notification each daemon
	// runs one peer check for all its workspaces every 30 seconds. Default
	// false: a single daemon opens no peer connection and runs no peer check
	// (DEC-99(2), (3)).
	Peers bool `yaml:"peers,omitempty" json:"peers,omitempty"`
}
