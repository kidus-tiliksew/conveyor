package httpapi

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestSendSignInMailRequiresSTARTTLSAndSendsExactlyOneLink(t *testing.T) {
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	certificateServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	body := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		read := bufio.NewReader(conn)
		write := bufio.NewWriter(conn)
		reply := func(line string) error {
			if _, e := fmt.Fprintf(write, "%s\r\n", line); e != nil {
				return e
			}
			return write.Flush()
		}
		command := func(prefix string) error {
			line, e := read.ReadString('\n')
			if e != nil {
				return e
			}
			if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return fmt.Errorf("command %q, want %s", line, prefix)
			}
			return nil
		}
		if e := reply("220 fake-smtp ESMTP"); e != nil {
			serverErr <- e
			return
		}
		if e := command("EHLO"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250-fake-smtp"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250 STARTTLS"); e != nil {
			serverErr <- e
			return
		}
		if e := command("STARTTLS"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("220 ready"); e != nil {
			serverErr <- e
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		if e := tlsConn.Handshake(); e != nil {
			serverErr <- e
			return
		}
		read = bufio.NewReader(tlsConn)
		write = bufio.NewWriter(tlsConn)
		if e := command("EHLO"); e != nil {
			serverErr <- e
			return
		}
		if e := reply("250 fake-smtp"); e != nil {
			serverErr <- e
			return
		}
		for _, prefix := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
			if e := command(prefix); e != nil {
				serverErr <- e
				return
			}
			response := "250 ok"
			if prefix == "DATA" {
				response = "354 end with dot"
			}
			if e := reply(response); e != nil {
				serverErr <- e
				return
			}
		}
		var lines []string
		for {
			line, e := read.ReadString('\n')
			if e != nil {
				serverErr <- e
				return
			}
			if strings.TrimSpace(line) == "." {
				break
			}
			lines = append(lines, line)
		}
		body <- strings.Join(lines, "")
		if e := reply("250 queued"); e != nil {
			serverErr <- e
			return
		}
		if e := command("QUIT"); e != nil {
			serverErr <- e
			return
		}
		serverErr <- reply("221 bye")
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	cfg := config.InvitationDelivery{Host: host, Port: port, From: "Conveyor <conveyor@example.test>", TLSConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	link := "https://conveyor.example/sign-in?token=one-secret"
	if err := sendSignInMail(cfg, "invitee@example.test", link); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	message := <-body
	if strings.Count(message, link) != 1 {
		t.Fatalf("message contains link %d times: %q", strings.Count(message, link), message)
	}
}

func TestSendSignInMailRefusesPlainSMTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "220 plain\r\n")
		read := bufio.NewReader(conn)
		read.ReadString('\n')
		fmt.Fprint(conn, "250 plain\r\n")
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	err = sendSignInMail(config.InvitationDelivery{Host: host, Port: port, From: "conveyor@example.test"}, "invitee@example.test", "https://example.test")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error=%v, want STARTTLS refusal", err)
	}
}
