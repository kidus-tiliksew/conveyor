package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestResolveConveyordDebugAddress(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	accepted := []struct {
		name         string
		flagAddr     string
		flagExplicit bool
		allowRemote  bool
		environment  map[string]string
		want, source string
	}{
		{name: "unset disables", want: "", source: ""},
		{name: "environment fallback", environment: map[string]string{debugAddrEnv: "127.0.0.1:6060"}, want: "127.0.0.1:6060", source: debugAddrEnv},
		{name: "flag wins over environment", flagAddr: "127.0.0.2:7070", flagExplicit: true, environment: map[string]string{debugAddrEnv: "127.0.0.1:6060"}, want: "127.0.0.2:7070", source: "flag"},
		{name: "explicit empty flag disables", flagExplicit: true, environment: map[string]string{debugAddrEnv: "127.0.0.1:6060"}, want: "", source: ""},
		{name: "unexplicit flag value ignored", flagAddr: "127.0.0.1:6060", want: "", source: ""},
		{name: "loopback range", flagAddr: "127.10.20.30:0", flagExplicit: true, want: "127.10.20.30:0", source: "flag"},
		{name: "ipv6 loopback", flagAddr: "[::1]:6060", flagExplicit: true, want: "[::1]:6060", source: "flag"},
		{name: "opt-in accepts unspecified", flagAddr: "0.0.0.0:6060", flagExplicit: true, allowRemote: true, want: "0.0.0.0:6060", source: "flag"},
		{name: "opt-in accepts hostname from environment", allowRemote: true, environment: map[string]string{debugAddrEnv: "debug.internal:6060"}, want: "debug.internal:6060", source: debugAddrEnv},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			got, source, err := resolveConveyordDebugAddress(tc.flagAddr, tc.flagExplicit, tc.allowRemote, env(tc.environment))
			if err != nil || got != tc.want || source != tc.source {
				t.Fatalf("resolve = %q, %q, %v; want %q, %q", got, source, err, tc.want, tc.source)
			}
		})
	}

	refused := []struct {
		name, addr  string
		allowRemote bool
		want        []string
	}{
		{name: "empty host", addr: ":6060", want: []string{"loopback", "-" + debugAllowRemoteFlag}},
		{name: "unspecified ipv4", addr: "0.0.0.0:6060", want: []string{"loopback", "-" + debugAllowRemoteFlag}},
		{name: "unspecified ipv6", addr: "[::]:6060", want: []string{"loopback", "-" + debugAllowRemoteFlag}},
		{name: "private address", addr: "10.0.0.1:6060", want: []string{"loopback", "-" + debugAllowRemoteFlag}},
		{name: "localhost hostname", addr: "localhost:6060", want: []string{"loopback", "-" + debugAllowRemoteFlag}},
		{name: "missing port", addr: "127.0.0.1", want: []string{"expected host:port"}},
		{name: "unbracketed ipv6", addr: "::1:6060", want: []string{"expected host:port"}},
		{name: "named port", addr: "127.0.0.1:http", want: []string{"decimal port"}},
		{name: "port out of range", addr: "127.0.0.1:70000", want: []string{"decimal port"}},
		{name: "opt-in still requires a port", addr: "0.0.0.0", allowRemote: true, want: []string{"expected host:port"}},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			for _, explicit := range []bool{true, false} {
				environment := map[string]string{}
				flagAddr := tc.addr
				wantSource := "flag"
				if !explicit {
					environment[debugAddrEnv], flagAddr, wantSource = tc.addr, "", debugAddrEnv
				}
				got, _, err := resolveConveyordDebugAddress(flagAddr, explicit, tc.allowRemote, env(environment))
				if err == nil || got != "" {
					t.Fatalf("explicit=%v resolve = %q, %v; want refusal", explicit, got, err)
				}
				message := err.Error()
				for _, want := range append([]string{fmt.Sprintf("%q", tc.addr), wantSource}, tc.want...) {
					if !strings.Contains(message, want) {
						t.Fatalf("explicit=%v error %q lacks %q", explicit, message, want)
					}
				}
			}
		})
	}
}

func TestStartConveyordDebugServerDisabledListensNowhere(t *testing.T) {
	listen := func(network, address string) (net.Listener, error) {
		t.Fatalf("listen(%q, %q) called with the debug listener disabled", network, address)
		return nil, nil
	}
	var logged []string
	debug, err := startConveyordDebugServer("", "", listen, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil || debug != nil {
		t.Fatalf("start = %v, %v; want no server", debug, err)
	}
	if len(logged) != 0 {
		t.Fatalf("disabled listener logged %q", logged)
	}
	if err := debug.Close(); err != nil {
		t.Fatalf("closing the absent listener: %v", err)
	}
}

func TestConveyordDebugServerServesPprofAndClosesOnShutdown(t *testing.T) {
	fs := flag.NewFlagSet("conveyord", flag.ContinueOnError)
	addrValue := fs.String(debugAddrFlag, "", "")
	allowRemote := fs.Bool(debugAllowRemoteFlag, false, "")
	if err := fs.Parse([]string{"-" + debugAddrFlag, "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	addr, source, err := resolveConveyordDebugAddress(*addrValue, flagWasSet(fs, debugAddrFlag), *allowRemote, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	debug, err := startConveyordDebugServer(addr, source, net.Listen, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = debug.Close() })
	host, port, err := net.SplitHostPort(debug.addr)
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("bound address %q, %v; want a kernel-chosen loopback port", debug.addr, err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], debug.addr) || !strings.Contains(logged[0], "from flag") {
		t.Fatalf("startup log %q lacks the bound address and source", logged)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path string) (int, []byte) {
		t.Helper()
		response, err := client.Get("http://" + debug.addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return response.StatusCode, body
	}
	if status, body := get("/debug/pprof/"); status != http.StatusOK || !strings.Contains(string(body), "heap") {
		t.Fatalf("index status=%d body=%.200q", status, body)
	}
	for _, profile := range []string{"heap", "allocs"} {
		// A binary profile is gzip-compressed protobuf.
		if status, body := get("/debug/pprof/" + profile); status != http.StatusOK || len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
			t.Fatalf("%s status=%d body prefix=%x, want a gzip profile", profile, status, body[:min(len(body), 4)])
		}
	}
	if status, body := get("/debug/pprof/goroutine?debug=1"); status != http.StatusOK || !strings.Contains(string(body), "goroutine profile:") {
		t.Fatalf("goroutine status=%d body=%.200q", status, body)
	}
	if status, _ := get("/debug/pprof/cmdline"); status != http.StatusNotFound {
		t.Fatalf("cmdline status=%d, want 404", status)
	}

	conveyordShutdown{Timeout: time.Second, Debug: debug}.Run()
	select {
	case served := <-debug.served:
		if !errors.Is(served, http.ErrServerClosed) {
			t.Fatalf("debug server stopped with %v, want http.ErrServerClosed", served)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("debug server still serving after shutdown")
	}
	if conn, err := net.DialTimeout("tcp", debug.addr, time.Second); err == nil {
		conn.Close()
		t.Fatalf("debug address %s accepts connections after shutdown", debug.addr)
	}
}

func TestConveyordAPIHandlerServesNoPprof(t *testing.T) {
	// net/http/pprof is linked into this binary, so its handlers sit on
	// http.DefaultServeMux; the API router must not reach them.
	recorder := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	if body := recorder.Body.Bytes(); recorder.Code != http.StatusOK || len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("default mux status=%d, want the pprof registration this test guards against", recorder.Code)
	}
	handler := httpapi.NewServer(store.NewMemory()).Handler()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine?debug=1"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if (len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b) || strings.Contains(body, "goroutine profile:") || strings.Contains(body, "Types of profiles available") {
			t.Fatalf("API handler served a profile at %s: status=%d", path, recorder.Code)
		}
	}
}

func TestConveyordRefusesRemoteDebugAddress(t *testing.T) {
	if path := os.Getenv("CONVEYOR_DEBUG_REFUSAL_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0", "-" + debugAddrFlag, "0.0.0.0:6060"}
		main()
		t.Fatal("conveyord started with a non-loopback debug address")
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(envPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConveyordRefusesRemoteDebugAddress$")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CONVEYOR_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env,
		// A missing deployment file proves the refusal precedes the config load.
		"CONVEYOR_DEBUG_REFUSAL_CONFIG="+filepath.Join(dir, "missing.yaml"),
		"CONVEYOR_ENV_FILE="+envPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("conveyord exit err=%v output=%s", err, output)
	}
	startupLog := string(output)
	if !strings.Contains(startupLog, `refusing debug address "0.0.0.0:6060" from flag`) || !strings.Contains(startupLog, "-"+debugAllowRemoteFlag) {
		t.Fatalf("startup log lacks the refusal: %s", startupLog)
	}
	if strings.Contains(startupLog, "load config") || strings.Contains(startupLog, "listening on") || strings.Contains(startupLog, "debug listener serving") {
		t.Fatalf("refusal did not stop startup before the config load: %s", startupLog)
	}
}
