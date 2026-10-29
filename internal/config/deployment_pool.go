package config

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// DatabaseMaxConnsEnv is the environment fallback for database.max_conns.
const DatabaseMaxConnsEnv = "CONVEYOR_DATABASE_MAX_CONNS"

var (
	errDatabaseMaxConns    = errors.New("database.max_conns must be an integer from 1 to 2147483647")
	errDatabaseMaxConnsEnv = errors.New(DatabaseMaxConnsEnv + " must be an integer from 1 to 2147483647")
)

// resolveDeploymentDatabaseMaxConns validates the PostgreSQL pool size when
// the deployment file loads, before and independent of any connection
// string, and applies the environment fallback with the same
// file-over-environment precedence as database.url. A present variable is
// validated even when the file value wins. Errors name the setting, never
// its value. An explicit pool_max_conns in the connection string still wins
// when the pool is built (component-runtime, Durable backend selection).
func resolveDeploymentDatabaseMaxConns(database *Database, lookupEnv func(string) (string, bool)) error {
	if database.MaxConns != nil && !validMaxConns(*database.MaxConns) {
		return errDatabaseMaxConns
	}
	raw, present := lookupEnv(DatabaseMaxConnsEnv)
	raw = strings.TrimSpace(raw)
	if !present || raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !validMaxConns(value) {
		return errDatabaseMaxConnsEnv
	}
	if database.MaxConns == nil {
		database.MaxConns = &value
	}
	return nil
}

func validMaxConns(value int64) bool {
	return value >= 1 && value <= math.MaxInt32
}
