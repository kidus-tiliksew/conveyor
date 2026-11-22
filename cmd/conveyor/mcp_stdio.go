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
	"strconv"
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
			// normalizeServerURL keeps an empty query marker, as in
			// https://host/team?. The bridge appends /mcp to the base, so
			// it refuses that form instead of posting to a query string.
			if strings.ContainsAny(base, "?#") {
				return errors.New("server URL must not contain credentials, a query, or a fragment")
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
	loginHint       string
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
		loginHint:       mcpStdioLoginHint(base, credentialsFile),
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

// mcpStdioLoginHint names `conveyor auth login --server <base>`, as
// invariant 46 requires. That command writes only the default credentials
// file, so when the bridge reads another file the hint also says how the
// bridge can see the new credential.
func mcpStdioLoginHint(base, credentialsFile string) string {
	hint := fmt.Sprintf("Run `conveyor auth login --server %s`.", base)
	defaultFile, err := localAuthConfigPath()
	if err != nil {
		return hint + fmt.Sprintf(" That command updates the default credentials file, but this bridge reads %s. Point --credentials-file at the default file, or copy the entry for %s into %s.", credentialsFile, base, credentialsFile)
	}
	if filepath.Clean(defaultFile) == filepath.Clean(credentialsFile) {
		return hint
	}
	return hint + fmt.Sprintf(" That command updates %s, but this bridge reads %s. Point --credentials-file at %s, or copy the entry for %s into %s.", defaultFile, credentialsFile, defaultFile, base, credentialsFile)
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
		// Remove only the line delimiter, so the POST carries the message
		// bytes unchanged. A line of only whitespace is not a message.
		message := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(bytes.TrimSpace(message)) > 0 {
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
	request := mcpStdioRequestOf(message)
	response, failure := b.forward(ctx, message, request)
	if failure != nil {
		b.fail(request.id, request.isRequest, *failure)
		return
	}
	if response == nil {
		return
	}
	if !request.isRequest {
		// A notification has no answer, so nothing reaches standard output.
		// An error answer to it is a failed notification (invariant 46).
		if response.isError {
			b.diagnose("the server returned an error for a notification")
		}
		return
	}
	b.writeLine(response.line)
}

// mcpStdioRequest describes one input message: its compact raw ID, whether it
// is a request, and whether its ID is known.
type mcpStdioRequest struct {
	id        json.RawMessage
	isRequest bool
	idKnown   bool
}

func mcpStdioRequestOf(message []byte) mcpStdioRequest {
	id, isRequest := mcpStdioMessageID(message)
	var object map[string]json.RawMessage
	known := isRequest && json.Unmarshal(message, &object) == nil
	if known {
		_, known = object["id"]
	}
	return mcpStdioRequest{id: id, isRequest: isRequest, idKnown: known}
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
func (b *mcpStdioBridge) forward(ctx context.Context, message []byte, expect mcpStdioRequest) (*mcpStdioResponse, *mcpStdioFailure) {
	token, failure := b.readToken()
	if failure != nil {
		return nil, failure
	}
	request, err := newMCPStdioHTTPRequest(ctx, b.endpoint, message, token)
	if err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioUnreachable, message: fmt.Sprintf("The bridge cannot build a request for %s.", b.base)}
	}
	answer, err := b.client.Do(request)
	if err != nil {
		return nil, mcpStdioTransportFailure(b.base, err)
	}
	// Every answer except a 200 is closed unread. A proxy that holds a
	// discarded body open then cannot delay the error or the exit at EOF.
	defer answer.Body.Close()
	switch answer.StatusCode {
	case http.StatusAccepted:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, &mcpStdioFailure{code: mcpStdioUnauthorized, message: fmt.Sprintf("The server rejected the stored credential for %s with HTTP 401. %s", b.base, b.loginHint)}
	case http.StatusForbidden:
		return nil, &mcpStdioFailure{code: mcpStdioForbidden, message: fmt.Sprintf("The server refused the request with HTTP 403 for the stored credential of %s.", b.base)}
	case http.StatusOK:
	default:
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: fmt.Sprintf("The server answered with HTTP %d and no MCP message.", answer.StatusCode)}
	}
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		return nil, &mcpStdioFailure{code: mcpStdioBrokenConnection, message: "The connection to the server broke before the response was complete."}
	}
	return mcpStdioCheckResponse(body, token, expect)
}

// newMCPStdioHTTPRequest builds the POST for one message. GetBody stays nil,
// so the transport never sends the request a second time, not even after a
// reused connection fails before it writes a byte.
func newMCPStdioHTTPRequest(ctx context.Context, endpoint string, message []byte, token string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	return request, nil
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
		return "", &mcpStdioFailure{code: mcpStdioMissingCredential, message: fmt.Sprintf("No stored credential for %s in the credentials file. %s", b.base, b.loginHint)}
	}
	if !mcpStdioValidHeaderValue(token) {
		return "", &mcpStdioFailure{code: mcpStdioMissingCredential, message: fmt.Sprintf("The stored credential for %s is not a valid header value. %s", b.base, b.loginHint)}
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

// mcpStdioCheckResponse accepts only a JSON-RPC response object for the
// request that produced it and returns it compacted to one line without
// re-encoding its values. A response that carries the sent token in any form
// is withheld.
func mcpStdioCheckResponse(body []byte, token string, expect mcpStdioRequest) (*mcpStdioResponse, *mcpStdioFailure) {
	notJSON := &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a response that is not JSON."}
	notRPC := &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a JSON response that is not a JSON-RPC message."}
	if !json.Valid(body) {
		return nil, notJSON
	}
	// The token scan reads every member, repeated keys included, before any
	// map decode can drop one.
	scan := mcpStdioScan(body, token)
	if scan.carriesToken {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server response contained the stored credential, so the bridge withheld it."}
	}
	if scan.duplicateKey {
		return nil, notRPC
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, notRPC
	}
	var version string
	_, hasResult := object["result"]
	rawError, hasError := object["error"]
	if json.Unmarshal(object["jsonrpc"], &version) != nil || version != "2.0" || hasResult == hasError {
		return nil, notRPC
	}
	if hasError && !mcpStdioValidErrorObject(rawError) {
		return nil, notRPC
	}
	if expect.isRequest && !mcpStdioIDMatches(object, expect) {
		return nil, &mcpStdioFailure{code: mcpStdioInvalidResponse, message: "The server sent a response whose ID does not match the request."}
	}
	var line bytes.Buffer
	if err := json.Compact(&line, body); err != nil {
		return nil, notJSON
	}
	return &mcpStdioResponse{line: line.Bytes(), isError: hasError}, nil
}

// mcpStdioValidErrorObject reports whether a JSON-RPC error member is an
// object with an integer code and a string message.
func mcpStdioValidErrorObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return false
	}
	// Decode each member into any, so the JSON type decides: a quoted
	// number stays a string and null stays nil. Decoding into json.Number
	// or string would accept both.
	code, ok := mcpStdioDecodeAny(object["code"]).(json.Number)
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(code.String(), 10, 64); err != nil {
		return false
	}
	_, ok = mcpStdioDecodeAny(object["message"]).(string)
	return ok
}

// mcpStdioDecodeAny decodes one JSON value with numbers kept as json.Number.
// It returns nil for an absent, null, or invalid value.
func mcpStdioDecodeAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

// mcpStdioIDMatches compares the response ID with the request ID. Two
// string IDs match when their decoded strings are equal, because the /mcp
// handler's json.Encoder escapes <, >, and & in the echoed ID. Any other ID
// matches as compact raw JSON, so a numeric ID is never converted to a float
// and a string never matches a number. A request whose ID is unknown,
// because the input was not a JSON object, accepts only an absent or null ID.
func mcpStdioIDMatches(object map[string]json.RawMessage, expect mcpStdioRequest) bool {
	raw, ok := object["id"]
	var compact bytes.Buffer
	if ok && json.Compact(&compact, raw) != nil {
		return false
	}
	if !expect.idKnown {
		return !ok || compact.String() == "null"
	}
	if !ok {
		return false
	}
	var got, want string
	if json.Unmarshal(compact.Bytes(), &got) == nil && json.Unmarshal(expect.id, &want) == nil &&
		bytes.HasPrefix(compact.Bytes(), []byte(`"`)) && bytes.HasPrefix(expect.id, []byte(`"`)) {
		return got == want
	}
	return bytes.Equal(compact.Bytes(), expect.id)
}

// mcpStdioNestedJSONDepth bounds how many times the scan decodes JSON held in
// a string. A string at the limit that still looks like JSON counts as one
// that carries the token, so the bound never lets a token through.
const mcpStdioNestedJSONDepth = 8

type mcpStdioScanResult struct {
	carriesToken bool
	duplicateKey bool
}

// mcpStdioScan streams every token of a JSON body. It reports the token in
// raw bytes or in any decoded string or key, which covers every JSON escape,
// and it reports a repeated key in any object of the body. A string that
// itself holds JSON, such as a tools/call text result, is scanned the same
// way.
func mcpStdioScan(body []byte, token string) mcpStdioScanResult {
	var result mcpStdioScanResult
	if token != "" && bytes.Contains(body, []byte(token)) {
		result.carriesToken = true
	}
	scanner := &mcpStdioScanner{token: token, result: &result}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := scanner.value(decoder, 0, true); err != nil {
		// The body passed json.Valid, so a scan error is unexpected.
		// Withhold the response rather than print what was not scanned.
		result.carriesToken = true
	}
	return result
}

type mcpStdioScanner struct {
	token  string
	result *mcpStdioScanResult
}

func (s *mcpStdioScanner) value(decoder *json.Decoder, depth int, outer bool) error {
	next, err := decoder.Token()
	if err != nil {
		return err
	}
	switch typed := next.(type) {
	case json.Delim:
		switch typed {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if outer && keys[key] {
					s.result.duplicateKey = true
				}
				keys[key] = true
				s.text(key, depth)
				if err := s.value(decoder, depth, outer); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := s.value(decoder, depth, outer); err != nil {
					return err
				}
			}
		}
		_, err := decoder.Token()
		return err
	case string:
		s.text(typed, depth)
	}
	return nil
}

func (s *mcpStdioScanner) text(value string, depth int) {
	if s.token != "" && strings.Contains(value, s.token) {
		s.result.carriesToken = true
		return
	}
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, `"`) {
		return
	}
	if !json.Valid([]byte(trimmed)) {
		return
	}
	if depth >= mcpStdioNestedJSONDepth {
		s.result.carriesToken = true
		return
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if err := s.value(decoder, depth+1, false); err != nil {
		s.result.carriesToken = true
	}
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
