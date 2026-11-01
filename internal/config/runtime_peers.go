package config

// RuntimeSettings is the deployment file's runtime section
// (component-runtime, Peers).
type RuntimeSettings struct {
	// Peers declares that this daemon runs beside other daemons on one
	// database. On PostgreSQL the daemons wake each other through
	// LISTEN/NOTIFY; on a backend without change notification each daemon
	// tails every workspace every 30 seconds. Default false: a single daemon
	// opens no peer connection and runs no peer tail (DEC-99(2), (3)).
	Peers bool `yaml:"peers,omitempty" json:"peers,omitempty"`
}
