package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"
)

const (
	debugAddrFlag        = "debug-addr"
	debugAllowRemoteFlag = "debug-addr-allow-remote"
	debugAddrEnv         = "CONVEYOR_DEBUG_ADDR"

	// The read bounds stop a stalled client from holding a connection,
	// including a symbol lookup body; neither limits how long a CPU profile
	// or trace response runs.
	debugReadHeaderTimeout = 10 * time.Second
	debugReadTimeout       = 30 * time.Second
)

// resolveConveyordDebugAddress resolves the opt-in profiling listener address
// with the same flag-over-environment precedence as the shutdown budget. An
// empty result disables the listener. Without allowRemote the host must be a
// literal loopback IP, so a misconfiguration cannot publish profiles; a
// hostname is refused because its resolution is outside the daemon's control
// (component-runtime, The debug listener).
func resolveConveyordDebugAddress(flagAddr string, flagExplicit, allowRemote bool, getenv func(string) string) (string, string, error) {
	value, source := flagAddr, "flag"
	if !flagExplicit {
		value, source = getenv(debugAddrEnv), debugAddrEnv
	}
	if value == "" {
		return "", "", nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", "", fmt.Errorf("invalid debug address %q from %s: expected host:port: %w", value, source, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", "", fmt.Errorf("invalid debug address %q from %s: expected a decimal port from 0 to 65535", value, source)
	}
	if allowRemote {
		return value, source, nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", "", fmt.Errorf("refusing debug address %q from %s: the host must be a loopback IP address (127.0.0.0/8 or [::1]); set -%s to serve profiles on another address", value, source, debugAllowRemoteFlag)
	}
	return value, source, nil
}

// conveyordDebugServer is the running profiling listener. Close stops it and
// drops open connections immediately, so an in-flight CPU profile or trace
// never holds the shutdown drain.
type conveyordDebugServer struct {
	server *http.Server
	addr   string
	served chan error
}

func (d *conveyordDebugServer) Close() error {
	if d == nil {
		return nil
	}
	return d.server.Close()
}

// conveyordDebugHandler registers only net/http/pprof handlers on a dedicated
// mux. The API router is a chi router that never reaches
// http.DefaultServeMux, where importing net/http/pprof also registers them.
// No cmdline handler is registered, so process arguments stay off the
// listener.
func conveyordDebugHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startConveyordDebugServer binds and serves the profiling listener. An empty
// address starts nothing and never calls listen. A serve error after startup
// is logged and leaves the API running.
func startConveyordDebugServer(addr, source string, listen func(network, address string) (net.Listener, error), logf func(string, ...any)) (*conveyordDebugServer, error) {
	if addr == "" {
		return nil, nil
	}
	listener, err := listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("debug listener: %w", err)
	}
	debug := &conveyordDebugServer{
		server: &http.Server{Handler: conveyordDebugHandler(), ReadHeaderTimeout: debugReadHeaderTimeout, ReadTimeout: debugReadTimeout},
		addr:   listener.Addr().String(),
		served: make(chan error, 1),
	}
	go func() {
		err := debug.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("debug listener on %s stopped: %v", debug.addr, err)
		}
		debug.served <- err
	}()
	logf("conveyord debug listener serving net/http/pprof on %s from %s", debug.addr, source)
	return debug, nil
}
