package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// statementProxy counts the statements one daemon sends to SingleStore
// (component-verification-strategy, Idle-runtime checks). SingleStore keeps
// no per-database transaction counter that excludes the test's own reads,
// so the daemon connects through this TCP proxy and the test connects
// directly. The proxy forwards every byte unchanged and parses the MySQL
// client-to-server packets: after the handshake, a client packet with
// sequence number 0 starts a command. COM_QUERY and COM_STMT_EXECUTE each
// count as one statement; every other command is recorded without counting.
// The daemon's DSN disables TLS, so every packet is plain.
type statementProxy struct {
	listener net.Listener
	upstream string
	started  time.Time

	mu       sync.Mutex
	commands []proxyCommand
	conns    int
	wg       sync.WaitGroup
}

type proxyCommand struct {
	at        time.Time
	conn      int
	command   byte
	statement bool
	text      string
}

const (
	mysqlComQuit        = 0x01
	mysqlComQuery       = 0x03
	mysqlComPing        = 0x0e
	mysqlComStmtPrepare = 0x16
	mysqlComStmtExecute = 0x17
	mysqlComStmtClose   = 0x19
)

func mysqlCommandName(command byte) string {
	switch command {
	case mysqlComQuit:
		return "COM_QUIT"
	case mysqlComQuery:
		return "COM_QUERY"
	case mysqlComPing:
		return "COM_PING"
	case mysqlComStmtPrepare:
		return "COM_STMT_PREPARE"
	case mysqlComStmtExecute:
		return "COM_STMT_EXECUTE"
	case mysqlComStmtClose:
		return "COM_STMT_CLOSE"
	}
	return fmt.Sprintf("command 0x%02x", command)
}

func startStatementProxy(t *testing.T, upstream string) *statementProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &statementProxy{listener: listener, upstream: upstream, started: time.Now()}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

func (p *statementProxy) addr() string { return p.listener.Addr().String() }

func (p *statementProxy) close() {
	p.listener.Close()
	p.wg.Wait()
}

func (p *statementProxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		p.conns++
		id := p.conns
		p.mu.Unlock()
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			_, _ = io.Copy(client, server)
			client.Close()
		}()
		go func() {
			defer p.wg.Done()
			p.forwardClient(id, client, server)
			server.Close()
		}()
	}
}

// forwardClient copies client packets to the server one at a time and
// records each command that starts with sequence number 0. The handshake
// response and authentication packets carry sequence numbers from 1.
func (p *statementProxy) forwardClient(id int, client, server net.Conn) {
	reader := bufio.NewReader(client)
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if header[3] == 0 && length > 0 {
			p.record(id, payload)
		}
		if _, err := server.Write(append(header, payload...)); err != nil {
			return
		}
		header = make([]byte, 4)
	}
}

func (p *statementProxy) record(id int, payload []byte) {
	command := proxyCommand{at: time.Now(), conn: id, command: payload[0]}
	switch payload[0] {
	case mysqlComQuery, mysqlComStmtPrepare:
		command.text = string(payload[1:])
		command.statement = payload[0] == mysqlComQuery
	case mysqlComStmtExecute:
		command.statement = true
		if len(payload) >= 5 {
			command.text = fmt.Sprintf("statement %d", uint32(payload[1])|uint32(payload[2])<<8|uint32(payload[3])<<16|uint32(payload[4])<<24)
		}
	}
	p.mu.Lock()
	p.commands = append(p.commands, command)
	p.mu.Unlock()
}

// since returns the commands recorded at or after from.
func (p *statementProxy) since(from time.Time) []proxyCommand {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []proxyCommand
	for _, command := range p.commands {
		if !command.at.Before(from) {
			out = append(out, command)
		}
	}
	return out
}

func countStatements(commands []proxyCommand) int {
	n := 0
	for _, command := range commands {
		if command.statement {
			n++
		}
	}
	return n
}

// describeProxyCommands renders commands for a failure message, one per line, with the
// offset from base.
func describeProxyCommands(base time.Time, commands []proxyCommand) string {
	if len(commands) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for _, command := range commands {
		text := command.text
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		fmt.Fprintf(&b, "  +%6.2fs conn=%d %s statement=%t %s\n", command.at.Sub(base).Seconds(), command.conn, mysqlCommandName(command.command), command.statement, strings.Join(strings.Fields(text), " "))
	}
	return b.String()
}

// TestStatementProxyCountsCommands drives the proxy with a fake server and
// client: the handshake packets carry sequence numbers from 1 and are not
// commands; COM_QUERY and COM_STMT_EXECUTE count; COM_PING and
// COM_STMT_PREPARE are recorded without counting; a multi-packet payload's
// continuation is not a command.
func TestStatementProxyCountsCommands(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		b, _ := io.ReadAll(conn)
		received <- b
	}()
	proxy := startStatementProxy(t, upstream.Addr().String())
	client, err := net.Dial("tcp", proxy.addr())
	if err != nil {
		t.Fatal(err)
	}
	packet := func(seq byte, payload ...byte) []byte {
		n := len(payload)
		return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
	}
	var wire []byte
	wire = append(wire, packet(1, 0x0d, 0xa2, 0x00, 0x00)...) // handshake response
	wire = append(wire, packet(3, 'p', 'w')...)               // auth switch response
	wire = append(wire, packet(0, append([]byte{mysqlComQuery}, "SELECT 1"...)...)...)
	wire = append(wire, packet(0, mysqlComPing)...)
	wire = append(wire, packet(0, append([]byte{mysqlComStmtPrepare}, "SELECT ?"...)...)...)
	wire = append(wire, packet(0, mysqlComStmtExecute, 7, 0, 0, 0, 0, 1, 0, 0, 0)...)
	wire = append(wire, packet(1, mysqlComQuery, 'x')...) // continuation, not a command
	if _, err = client.Write(wire); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case got := <-received:
		if string(got) != string(wire) {
			t.Fatalf("proxy changed the bytes:\n got %x\nwant %x", got, wire)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream received nothing")
	}
	commands := proxy.since(time.Time{})
	if len(commands) != 4 || countStatements(commands) != 2 {
		t.Fatalf("commands=%d statements=%d, want 4 and 2:\n%s", len(commands), countStatements(commands), describeProxyCommands(proxy.started, commands))
	}
	want := []string{"COM_QUERY", "COM_PING", "COM_STMT_PREPARE", "COM_STMT_EXECUTE"}
	for i, command := range commands {
		if mysqlCommandName(command.command) != want[i] {
			t.Fatalf("command %d=%s, want %s", i, mysqlCommandName(command.command), want[i])
		}
	}
	if commands[0].text != "SELECT 1" || commands[3].text != "statement 7" {
		t.Fatalf("texts=%q %q", commands[0].text, commands[3].text)
	}
}
