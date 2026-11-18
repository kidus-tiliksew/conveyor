package singlestore

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// greetingPacket builds a protocol 10 server greeting with the given lower
// capability flags.
func greetingPacket(capabilities uint16) []byte {
	payload := []byte{10}
	payload = append(payload, "5.7.32"...)
	payload = append(payload, 0)
	payload = append(payload, 1, 0, 0, 0)    // connection ID
	payload = append(payload, "12345678"...) // auth data, part one
	payload = append(payload, 0)             // filler
	payload = binary.LittleEndian.AppendUint16(payload, capabilities)
	payload = append(payload, 33, 2, 0, 0, 0, 21)    // charset, status, upper flags, auth length
	payload = append(payload, make([]byte, 10)...)   // reserved
	payload = append(payload, "123456789012\x00"...) // auth data, part two
	return append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}, payload...)
}

// responsePacket builds the first client packet with the given flags.
func responsePacket(flags uint32) []byte {
	payload := binary.LittleEndian.AppendUint32(nil, flags)
	payload = append(payload, make([]byte, 28)...)
	return append([]byte{byte(len(payload)), 0, 0, 1}, payload...)
}

// handshake runs a fake server greeting and a client response through a
// watched connection, in chunks of the given size, and returns what the
// observer reports.
func handshake(t *testing.T, greeting, response []byte, chunk int) (bool, string) {
	t.Helper()
	observer := &compressionObserver{requested: true}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	watched := observer.wrap(client)
	go func() {
		for i := 0; i < len(greeting); i += chunk {
			_, _ = server.Write(greeting[i:min(i+chunk, len(greeting))])
		}
		_, _ = io.Copy(io.Discard, server)
	}()
	if _, err := io.ReadFull(watched, make([]byte, len(greeting))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(response); i += chunk {
		if _, err := watched.Write(response[i:min(i+chunk, len(response))]); err != nil {
			t.Fatal(err)
		}
	}
	return observer.state()
}

// TestCompressionObserverReadsTheHandshake proves the reported state comes
// from the connection's own handshake: negotiated only when the client
// response sets CLIENT_COMPRESS, not_negotiated when it doesn't, and
// unknown when no handshake could be read (component-runtime invariant 43).
func TestCompressionObserverReadsTheHandshake(t *testing.T) {
	for _, tc := range []struct {
		name     string
		greeting []byte
		response []byte
		want     string
	}{
		{"server offers and client accepts", greetingPacket(0xf7ff), responsePacket(0x000aa20f | clientCompress), CompressionNegotiated},
		{"server offers and client declines", greetingPacket(0xf7ff), responsePacket(0x000aa20f), CompressionNotNegotiated},
		{"server does not offer", greetingPacket(0xf7df), responsePacket(0x000aa20f), CompressionNotNegotiated},
		{"error packet instead of greeting", append([]byte{9, 0, 0, 0}, "\xff\x10\x04Too many"...), responsePacket(0x000aa20f), CompressionUnknown},
		{"oversized greeting", append([]byte{0xff, 0xff, 0, 0}, make([]byte, 64)...), responsePacket(0x000aa20f), CompressionUnknown},
	} {
		for _, chunk := range []int{1, 3, 4096} {
			t.Run(tc.name, func(t *testing.T) {
				requested, got := handshake(t, tc.greeting, tc.response, chunk)
				if !requested || got != tc.want {
					t.Fatalf("chunk %d: state = %t %s, want true %s", chunk, requested, got, tc.want)
				}
			})
		}
	}
	if requested, got := (&compressionObserver{}).state(); requested || got != CompressionUnknown {
		t.Fatalf("an unobserved store reported %t %s", requested, got)
	}
	if requested, got := (&Store{}).Compression(); requested || got != CompressionUnknown {
		t.Fatalf("a store assembled without Open reported %t %s", requested, got)
	}
}

// TestCompressionObserverKeepsConnectionCapabilities proves the watched
// connection still exposes its descriptor for the driver's liveness check
// and its counting wrapper for read accounting.
func TestCompressionObserverKeepsConnectionCapabilities(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	watched := (&compressionObserver{}).wrap(raw)
	if _, ok := watched.(syscall.Conn); !ok {
		t.Fatal("the watched TCP connection hides its descriptor")
	}
	counted := dbmetrics.NewReadLedger().WrapConn(watched)
	if dbmetrics.ReadConnOf(counted) == nil {
		t.Fatal("read accounting cannot find its wrapper above the observer")
	}
}

func TestCompressionRequested(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		option []bool
		want   bool
	}{
		{"singlestore://root:fixture@localhost/conveyor_test", nil, true},
		{"singlestore://root:fixture@localhost/conveyor_test", []bool{false}, false},
		{"singlestore://root:fixture@localhost/conveyor_test?compress=false", nil, false},
		{"root:fixture@tcp(localhost:3306)/conveyor_test?compress=false", []bool{true}, true},
	} {
		cfg, err := connectionConfig(tc.raw, tc.option...)
		if err != nil {
			t.Fatal(err)
		}
		if got := compressionRequested(cfg); got != tc.want {
			t.Fatalf("%s %v: requested = %t, want %t", tc.raw, tc.option, got, tc.want)
		}
	}
	// An invalid parameter stops the open and echoes no value.
	for _, raw := range []string{
		"singlestore://root:fixture@localhost/conveyor_test?compress=maybe-secret",
		"singlestore://root:fixture@localhost/conveyor_test?compress=true&compress=false",
	} {
		_, err := connectionConfig(raw)
		if err == nil || err.Error() != "invalid SingleStore compression configuration" || strings.Contains(err.Error(), "maybe-secret") || strings.Contains(err.Error(), "fixture") {
			t.Fatalf("%s: error = %v", raw, err)
		}
	}
}
