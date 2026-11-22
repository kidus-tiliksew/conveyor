package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// JSON-RPC error codes the stdio bridge writes for a request it cannot
// complete (component-cli-onboarding invariant 46). -32700 is the JSON-RPC
// parse error; the others sit in the implementation-defined server range.
const (
	mcpStdioParseError            = -32700
	mcpStdioMissingCredential     = -32001
	mcpStdioUnreadableCredentials = -32002
	mcpStdioUnauthorized          = -32003
	mcpStdioForbidden             = -32004
	mcpStdioInvalidResponse       = -32005
	mcpStdioUnreachable           = -32006
	mcpStdioBrokenConnection      = -32007
)

// mcpStdioCmd runs the local stdio bridge that DEC-145 registers for an agent
// tool whose native MCP registration can neither run a header helper nor
// expand an environment reference in a header value. It is a form of the
// token-print bridge: the registration names only the server and the
// credential file, and the bridge reads the credential the way `auth token`
// does (req-cli-authentication AC-4.2; component-cli-onboarding invariants 45
// and 46).
func mcpStdioCmd() *cobra.Command {
	var credentialsFile string
	command := &cobra.Command{
		Use:   "stdio",
		Short: "Forward MCP messages from standard input to the server, with the token from a credential file",
		Long: "Forward MCP messages from standard input to the server, with the token from a credential file.\n\n" +
			"The bridge reads one JSON-RPC message per line and sends it to <server>/mcp. " +
			"It reads the token for the server from --credentials-file before each request and never uses CONVEYOR_API_TOKEN. " +
			"It writes each answer as one line to standard output.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("server") {
				return errors.New("mcp stdio requires --server")
			}
			base, err := normalizeServerURL(serverFlag)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("credentials-file") || strings.TrimSpace(credentialsFile) == "" {
				return errors.New("mcp stdio requires --credentials-file")
			}
			if !filepath.IsAbs(credentialsFile) {
				return errors.New("credentials-file must be absolute")
			}
			bridge := newMCPStdioBridge(base, credentialsFile, cmd.OutOrStdout(), cmd.ErrOrStderr())
			return bridge.serve(cmd.Context(), cmd.InOrStdin())
		},
	}
	command.Flags().StringVar(&credentialsFile, "credentials-file", "", "absolute saved-credential file that holds the token for --server")
	return command
}

type mcpStdioBridge struct {
	base            string
	endpoint        string
	credentialsFile string
	client          *http.Client

	writeMu sync.Mutex
	stdout  io.Writer
	stderr  io.Writer
}

func newMCPStdioBridge(base, credentialsFile string, stdout, stderr io.Writer) *mcpStdioBridge {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &mcpStdioBridge{
		base:            base,
		endpoint:        base + "/mcp",
		credentialsFile: credentialsFile,
		// The client has no Timeout: a long tool wait such as await_review
		// ends only when the server answers (invariant 45). A redirect is
		// never followed, so the Authorization header goes to no other URL.
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		stdout: stdout,
		stderr: stderr,
	}
}

// serve reads one message per line until standard input closes, forwards each
// message on its own goroutine so a request never waits for an earlier
// answer, and returns after every forwarded message has finished.
func (b *mcpStdioBridge) serve(ctx context.Context, input io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// bufio.Reader.ReadBytes has no line limit, unlike bufio.Scanner's
	// 64 KiB default; the server enforces its own request size.
	reader := bufio.NewReader(input)
	var inFlight sync.WaitGroup
	var readErr error
	for {
		line, err := reader.ReadBytes('\n')
		if message := bytes.TrimSpace(line); len(message) > 0 {
			inFlight.Add(1)
			go func(message []byte) {
				defer inFlight.Done()
				b.handle(ctx, message)
			}(message)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	inFlight.Wait()
	if readErr != nil {
		return fmt.Errorf("read standard input: %w", readErr)
	}
	return nil
}

// mcpStdioFailure is a request failure with fixed text. It never carries a
// loader error, an HTTP error, a header, or a response body, so no failure
// path can print the token.
type mcpStdioFailure struct {
	code    int
	message string
}

func (b *mcpStdioBridge) handle(ctx context.Context, message []byte) {
	if !json.Valid(message) {
		b.fail(json.RawMessage("null"), true, mcpStdioFailure{code: mcpStdioParseError, message: "Parse error: the input line is not JSON."})
		return
	}
	id, isRequest := mcpStdioMessageID(message)
	response, failure := b.forward(ctx, message)
	if failure != nil {
		b.fail(id, isRequest, *failure)
		return
	}
	if response == nil {
		return
	}
	if !isRequest && response.isError {
		// A notification has no answer. An error answer to it is a failed
		// notification, which writes nothing (invariant 46).
		b.diagnose("the server returned an error for a notification")
		return
	}
	b.writeLine(response.line)
}

// mcpStdioMessageID returns the raw ID of a JSON object message and whether
// the message is a request. An object without an id member is a
// notification. Any other JSON value is forwarded as a request with an
// unknown, null ID so that a failure still produces an answer.
func mcpStdioMessageID(message []byte) (json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(message, &object); err != nil {
		return json.RawMessage("null"), true
	}
	raw, ok := object["id"]
	if !ok {
		return nil, false
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return json.RawMessage("null"), true
	}
	return json.RawMessage(compact.Bytes()), true
}

type mcpStdioResponse struct {
	line    []byte
	isError bool
}

// forward reads the credential, POSTs the message bytes unchanged, and
// returns the answer as one compact line. A nil response and nil failure
// means the server accepted the message with 202 and nothing is written.
func (b *mcpStdioBridge) forward(ctx context.Context, message []byte) (*mcpStdioResponse, *mcpStdioFailure) {
	token, failure := b.readToken()
	if failure != nil {
		return nil, failure
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(message))
	if err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioUnreachable, message: fmt.Sprintf("The bridge cannot build a request for %s.", b.base)}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	answer, err := b.client.Do(request)
	if err != nil {
		return nil, mcpStdioTransportFailure(b.base, err)
	}
	defer answer.Body.Close()
	switch answer.StatusCode {
	case http.StatusAccepted:
		_, _ = io.Copy(io.Discard, answer.Body)
		return nil, nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, answer.Body)
		return nil, &mcpStdioFailure{code: mcpStdioUnauthorized, message: fmt.Sprintf("The server rejected the stored credential for %s with HTTP 401. Run `conveyor auth login --server %s`.", b.base, b.base)}
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, answer.Body)
		return nil, &mcpStdioFailure{code: mcpStdioForbidden, message: fmt.Sprintf("The server refused the request with HTTP 403 for the stored credential of %s.", b.base)}
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, answer.Body)
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: fmt.Sprintf("The server answered with HTTP %d and no MCP message.", answer.StatusCode)}
	}
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioBrokenConnection, message: "The connection to the server broke before the response was complete."}
	}
	return mcpStdioCheckResponse(body, token)
}

// readToken reads the credential for the canonical server from the named file
// on every call, as `auth token --credentials-file` does, so a rotated token
// applies to the next request. It never reads CONVEYOR_API_TOKEN
// (component-cli-onboarding invariants 5 and 45; DEC-145(2), DEC-145(3)).
func (b *mcpStdioBridge) readToken() (string, *mcpStdioFailure) {
	config, err := loadLocalAuthConfigFile(b.credentialsFile)
	if err != nil {
		return "", &mcpStdioFailure{code: mcpStdioUnreadableCredentials, message: "The bridge cannot read or parse the credentials file."}
	}
	token := config.Servers[b.base].Token
	if token == "" {
		return "", &mcpStdioFailure{code: mcpStdioMissingCredential, message: fmt.Sprintf("No stored credential for %s in the credentials file. Run `conveyor auth login --server %s`.", b.base, b.base)}
	}
	if !mcpStdioValidHeaderValue(token) {
		return "", &mcpStdioFailure{code: mcpStdioMissingCredential, message: fmt.Sprintf("The stored credential for %s is not a valid header value. Run `conveyor auth login --server %s`.", b.base, b.base)}
	}
	return token, nil
}

func mcpStdioValidHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func mcpStdioTransportFailure(base string, err error) *mcpStdioFailure {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	switch {
	case errors.As(err, &opErr) && opErr.Op == "dial",
		errors.As(err, &dnsErr),
		errors.As(err, &certErr),
		errors.As(err, &unknownAuthority),
		errors.As(err, &hostnameErr),
		errors.As(err, &invalidCert):
		return &mcpStdioFailure{code: mcpStdioUnreachable, message: fmt.Sprintf("The bridge cannot connect to the server at %s.", base)}
	default:
		return &mcpStdioFailure{code: mcpStdioBrokenConnection, message: "The connection to the server broke before the response was complete."}
	}
}

// mcpStdioCheckResponse accepts only a JSON-RPC response object and returns it
// compacted to one line without re-encoding its values. A response that
// carries the sent token in any form is withheld.
func mcpStdioCheckResponse(body []byte, token string) (*mcpStdioResponse, *mcpStdioFailure) {
	if !json.Valid(body) {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a response that is not JSON."}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a JSON response that is not a JSON-RPC message."}
	}
	var version string
	_, hasResult := object["result"]
	rawError, hasError := object["error"]
	if json.Unmarshal(object["jsonrpc"], &version) != nil || version != "2.0" || hasResult == hasError {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a JSON response that is not a JSON-RPC message."}
	}
	if mcpStdioCarriesToken(body, token) {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server response contained the stored credential, so the bridge withheld it."}
	}
	var line bytes.Buffer
	if err := json.Compact(&line, body); err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a response that is not JSON."}
	}
	return &mcpStdioResponse{line: line.Bytes(), isError: hasError && !bytes.Equal(bytes.TrimSpace(rawError), []byte("null"))}, nil
}

// mcpStdioCarriesToken reports whether a JSON body holds the token as raw
// bytes or inside any decoded string or key, which covers every JSON escape.
// A string that itself holds JSON, such as a tools/call text result, is
// searched the same way.
func mcpStdioCarriesToken(body []byte, token string) bool {
	if token == "" {
		return false
	}
	if bytes.Contains(body, []byte(token)) {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	return mcpStdioValueCarriesToken(value, token, 0)
}

const mcpStdioNestedJSONDepth = 8

func mcpStdioValueCarriesToken(value any, token string, depth int) bool {
	switch typed := value.(type) {
	case string:
		if strings.Contains(typed, token) {
			return true
		}
		trimmed := strings.TrimSpace(typed)
		if depth < mcpStdioNestedJSONDepth && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, `"`)) {
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			var nested any
			if decoder.Decode(&nested) == nil {
				return mcpStdioValueCarriesToken(nested, token, depth+1)
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.Contains(key, token) || mcpStdioValueCarriesToken(item, token, depth) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if mcpStdioValueCarriesToken(item, token, depth) {
				return true
			}
		}
	}
	return false
}

type mcpStdioErrorLine struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   mcpStdioRPCErr  `json:"error"`
}

type mcpStdioRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fail answers a request with a JSON-RPC error that keeps its ID and names
// the cause. A failed notification writes nothing to standard output.
func (b *mcpStdioBridge) fail(id json.RawMessage, isRequest bool, failure mcpStdioFailure) {
	b.diagnose(failure.message)
	if !isRequest {
		return
	}
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	line, err := json.Marshal(mcpStdioErrorLine{JSONRPC: "2.0", ID: id, Error: mcpStdioRPCErr{Code: failure.code, Message: failure.message}})
	if err != nil {
		return
	}
	b.writeLine(line)
}

// writeLine writes one complete line under a lock, so concurrent answers
// never interleave on standard output.
func (b *mcpStdioBridge) writeLine(line []byte) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.stdout.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		fmt.Fprintln(b.stderr, "conveyor mcp stdio: cannot write to standard output")
	}
}

// diagnose writes fixed diagnostic text to standard error. Callers pass only
// bridge-authored text, never a loader error, header, or response body.
func (b *mcpStdioBridge) diagnose(message string) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	fmt.Fprintln(b.stderr, "conveyor mcp stdio:", message)
}
