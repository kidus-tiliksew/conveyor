// Package connprobe stands in for a database server just long enough to read
// the identity a real driver sends, then closes the connection. A test uses it
// to prove which user and database a connection string reaches, through the
// real store open path and with no database running. It records no password.
package connprobe

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Handshake is the identity a client sent in its first message.
type Handshake struct {
	User, Database string
}

// Probe listens on a loopback address until its test ends.
type Probe struct {
	// Addr is the host:port to place in a connection string.
	Addr string
	seen chan Handshake
}

// Postgres starts a probe that reads a PostgreSQL startup message.
func Postgres(t *testing.T) *Probe {
	t.Helper()
	return start(t, readPostgresStartup)
}

// MySQL starts a probe that sends a MySQL greeting and reads the client's
// handshake response, as SingleStore's driver expects.
func MySQL(t *testing.T) *Probe {
	t.Helper()
	return start(t, readMySQLHandshake)
}

func start(t *testing.T, read func(net.Conn) (Handshake, error)) *Probe {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	probe := &Probe{Addr: listener.Addr().String(), seen: make(chan Handshake, 32)}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				if handshake, readErr := read(conn); readErr == nil {
					select {
					case probe.seen <- handshake:
					default:
					}
				}
			}()
		}
	}()
	return probe
}

// Require checks every handshake since the last check. It runs after the open
// that dialed the probe has returned: the probe records a handshake before it
// closes the connection the client is waiting on, so all of them are recorded,
// and nothing here waits. It fails when none arrived or one differs from want,
// and reports only the user and database.
func (p *Probe) Require(t *testing.T, label string, want Handshake) {
	t.Helper()
	seen := 0
	for {
		select {
		case got := <-p.seen:
			seen++
			if got != want {
				t.Errorf("%s: server saw user %q database %q, want user %q database %q", label, got.User, got.Database, want.User, want.Database)
			}
		default:
			if seen == 0 {
				t.Errorf("%s: no connection reached the probe", label)
			}
			return
		}
	}
}

const (
	postgresProtocol3  = 0x00030000
	postgresSSLRequest = 80877103
	postgresGSSRequest = 80877104
)

func readPostgresStartup(conn net.Conn) (Handshake, error) {
	for {
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return Handshake{}, err
		}
		length := binary.BigEndian.Uint32(header[:])
		if length < 8 || length > 1<<16 {
			return Handshake{}, errors.New("implausible startup length")
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return Handshake{}, err
		}
		switch binary.BigEndian.Uint32(body[:4]) {
		case postgresSSLRequest, postgresGSSRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return Handshake{}, err
			}
			continue
		case postgresProtocol3:
		default:
			// A cancel request carries no identity.
			return Handshake{}, errors.New("not a startup message")
		}
		fields := strings.Split(string(body[4:]), "\x00")
		handshake := Handshake{}
		for i := 0; i+1 < len(fields); i += 2 {
			switch fields[i] {
			case "user":
				handshake.User = fields[i+1]
			case "database":
				handshake.Database = fields[i+1]
			}
		}
		return handshake, nil
	}
}

// mysqlGreeting is a protocol 10 handshake offering 4.1 authentication, a
// database name in the response, and the native password plugin.
func mysqlGreeting() []byte {
	payload := []byte{0x0a}
	payload = append(payload, "5.7.99\x00"...)
	payload = append(payload, 1, 0, 0, 0)
	payload = append(payload, "abcdefgh\x00"...)
	payload = append(payload, 0x0d, 0xa2, 0x21, 0x02, 0x00, 0x08, 0x00, 21)
	payload = append(payload, make([]byte, 10)...)
	payload = append(payload, "ijklmnopqrst\x00"...)
	payload = append(payload, "mysql_native_password\x00"...)
	return append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}, payload...)
}

func readMySQLHandshake(conn net.Conn) (Handshake, error) {
	if _, err := conn.Write(mysqlGreeting()); err != nil {
		return Handshake{}, err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return Handshake{}, err
	}
	body := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	if _, err := io.ReadFull(conn, body); err != nil {
		return Handshake{}, err
	}
	const fixedPrefix = 4 + 4 + 1 + 23
	if len(body) <= fixedPrefix {
		return Handshake{}, errors.New("short handshake response")
	}
	rest := body[fixedPrefix:]
	user, rest, ok := cutNull(rest)
	if !ok || len(rest) == 0 {
		return Handshake{}, errors.New("handshake response has no user")
	}
	// The scrambled password is length-prefixed and skipped unread.
	authLength := int(rest[0])
	if len(rest) < 1+authLength {
		return Handshake{}, errors.New("short handshake authentication")
	}
	database, _, _ := cutNull(rest[1+authLength:])
	return Handshake{User: user, Database: database}, nil
}

func cutNull(data []byte) (string, []byte, bool) {
	for i, b := range data {
		if b == 0 {
			return string(data[:i]), data[i+1:], true
		}
	}
	return string(data), nil, false
}
