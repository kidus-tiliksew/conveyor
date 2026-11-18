package singlestore

import (
	"encoding/binary"
	"net"
	"sync"
	"syscall"
)

// Protocol compression negotiation states that Store.Compression reports
// (component-runtime invariant 43).
const (
	CompressionNegotiated    = "negotiated"
	CompressionNotNegotiated = "not_negotiated"
	CompressionUnknown       = "unknown"
)

// clientCompress is CLIENT_COMPRESS in the MySQL protocol capability flags.
const clientCompress = 0x20

// maxGreetingBytes bounds what the observer buffers of a server greeting. A
// MySQL protocol 10 greeting is far smaller; a longer packet is not one.
const maxGreetingBytes = 4096

// compressionObserver reads protocol compression from the handshakes of the
// store's own connections. It sends no statement and opens no connection of
// its own (component-persistence invariant 28). The server greeting states
// whether the server offers CLIENT_COMPRESS. The first client packet, the
// handshake response or the TLS request that precedes it, states whether
// the driver accepted it; the driver sets the flag only when it was
// requested and the server offered it, and then compresses that connection.
type compressionObserver struct {
	requested bool

	mu         sync.Mutex
	observed   bool
	advertised bool
	negotiated bool
}

// Compression reports whether the configuration requested protocol
// compression and what the handshakes of the store's connections showed:
// CompressionNegotiated when a connection's handshake turned compression on,
// CompressionNotNegotiated when a connection's handshake left it off, and
// CompressionUnknown when no handshake was observed. It never queries the
// database (component-runtime invariant 43; component-persistence
// invariant 28).
func (s *Store) Compression() (requested bool, negotiation string) {
	if s.compression == nil {
		return false, CompressionUnknown
	}
	return s.compression.state()
}

func (o *compressionObserver) state() (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case !o.observed:
		return o.requested, CompressionUnknown
	case o.negotiated:
		return o.requested, CompressionNegotiated
	default:
		return o.requested, CompressionNotNegotiated
	}
}

// record keeps the first complete handshake. Every connection of one store
// shares one configuration and one server, so later handshakes agree.
func (o *compressionObserver) record(advertised, negotiated bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.observed {
		return
	}
	o.observed, o.advertised, o.negotiated = true, advertised, negotiated
}

// wrap returns c with the observer watching its handshake. A connection
// that exposes its raw descriptor keeps exposing it, so the driver's
// liveness check still works.
func (o *compressionObserver) wrap(c net.Conn) net.Conn {
	if o == nil || c == nil {
		return c
	}
	watched := &handshakeConn{Conn: c, observer: o}
	if raw, ok := c.(syscall.Conn); ok {
		return &handshakeSyscallConn{handshakeConn: watched, raw: raw}
	}
	return watched
}

// handshakeConn reads the capability flags of the server greeting and of the
// first client packet, then passes bytes through untouched.
type handshakeConn struct {
	net.Conn
	observer *compressionObserver
	// done is set once both packets were read or the stream can't be read
	// as a handshake; Read and Write then only forward.
	done       bool
	greeting   []byte
	advertised bool
	greeted    bool
	response   []byte
}

func (c *handshakeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.done && !c.greeted && n > 0 {
		c.greeting = append(c.greeting, p[:n]...)
		c.parseGreeting()
	}
	return n, err
}

func (c *handshakeConn) Write(p []byte) (int, error) {
	if !c.done && c.greeted {
		c.response = append(c.response, p...)
		c.parseResponse()
	}
	return c.Conn.Write(p)
}

// parseGreeting reads the protocol 10 greeting: protocol version, the server
// version string, connection ID, eight bytes of auth data, a filler byte,
// and the lower two bytes of the capability flags.
func (c *handshakeConn) parseGreeting() {
	if len(c.greeting) < 4 {
		return
	}
	size := int(c.greeting[0]) | int(c.greeting[1])<<8 | int(c.greeting[2])<<16
	if size == 0 || size > maxGreetingBytes {
		c.stop()
		return
	}
	if len(c.greeting) < 4+size {
		return
	}
	payload := c.greeting[4 : 4+size]
	c.greeting = nil
	if payload[0] != 10 {
		c.stop()
		return
	}
	end := -1
	for i := 1; i < len(payload); i++ {
		if payload[i] == 0 {
			end = i
			break
		}
	}
	offset := end + 1 + 4 + 8 + 1
	if end < 0 || offset+2 > len(payload) {
		c.stop()
		return
	}
	c.advertised = binary.LittleEndian.Uint16(payload[offset:])&clientCompress != 0
	c.greeted = true
}

// parseResponse reads the client capability flags, the first four payload
// bytes of the first packet the client sends after the greeting.
func (c *handshakeConn) parseResponse() {
	if len(c.response) < 8 {
		return
	}
	flags := binary.LittleEndian.Uint32(c.response[4:8])
	c.observer.record(c.advertised, flags&clientCompress != 0)
	c.stop()
}

func (c *handshakeConn) stop() {
	c.done, c.greeting, c.response = true, nil, nil
}

// NetConn returns the wrapped connection, so dbmetrics.ReadConnOf can still
// find a counting wrapper below this one.
func (c *handshakeConn) NetConn() net.Conn { return c.Conn }

type handshakeSyscallConn struct {
	*handshakeConn
	raw syscall.Conn
}

func (c *handshakeSyscallConn) SyscallConn() (syscall.RawConn, error) { return c.raw.SyscallConn() }
