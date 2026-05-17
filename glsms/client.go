// Package glsms is a client for reading and sending SMS messages through a
// GL.iNet router's JSON-RPC interface (firmware 4.x, e.g. GL-X3000 / Spitz AX).
package glsms

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/GehirnInc/crypt"
	_ "github.com/GehirnInc/crypt/md5_crypt"
	_ "github.com/GehirnInc/crypt/sha256_crypt"
	_ "github.com/GehirnInc/crypt/sha512_crypt"
)

// Client talks to the GL.iNet RPC endpoint. It is safe for concurrent use; the
// session is refreshed automatically when it expires.
type Client struct {
	endpoint string
	username string
	password string
	http     *http.Client

	mu  sync.Mutex
	sid string
	id  int64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client (e.g. for custom
// timeouts or to allow self-signed TLS).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New creates a Client. host is the router address ("192.168.8.1" or a full URL
// like "http://192.168.8.1"). username is usually "root".
func New(host, username, password string, opts ...Option) *Client {
	endpoint := host
	if !hasScheme(endpoint) {
		endpoint = "http://" + endpoint
	}
	endpoint = trimSlash(endpoint) + "/rpc"
	c := &Client{
		endpoint: endpoint,
		username: username,
		password: password,
		// Generous: a single send_sms can block server-side until the modem
		// confirms delivery (see SMS.Send timeoutSec). Override via
		// WithHTTPClient if you need something tighter.
		http: &http.Client{Timeout: 180 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func (c *Client) nextID() int64 {
	c.id++
	return c.id
}

// rawCall performs a single JSON-RPC request without session handling.
func (c *Client) rawCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rpc transport: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var r rpcResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode rpc response: %w (body: %s)", err, truncate(string(data), 200))
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

type challengeResult struct {
	Alg        json.Number `json:"alg"`
	Salt       string      `json:"salt"`
	Nonce      string      `json:"nonce"`
	HashMethod string      `json:"hash-method"`
}

// login performs the GL.iNet challenge/response handshake and stores the sid.
// Caller must hold c.mu.
func (c *Client) login(ctx context.Context) error {
	raw, err := c.rawCall(ctx, "challenge", map[string]string{"username": c.username})
	if err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	var ch challengeResult
	if err := json.Unmarshal(raw, &ch); err != nil {
		return fmt.Errorf("decode challenge: %w", err)
	}

	magic, err := cryptMagic(ch.Alg.String())
	if err != nil {
		return err
	}
	hashedPw, err := magic.crypter.Generate([]byte(c.password), []byte(magic.prefix+ch.Salt))
	if err != nil {
		return fmt.Errorf("crypt password: %w", err)
	}

	sum := sha256.Sum256([]byte(c.username + ":" + hashedPw + ":" + ch.Nonce))
	loginHash := hex.EncodeToString(sum[:])

	raw, err = c.rawCall(ctx, "login", map[string]string{
		"username": c.username,
		"hash":     loginHash,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var lr struct {
		Sid string `json:"sid"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return fmt.Errorf("decode login: %w", err)
	}
	if lr.Sid == "" {
		return fmt.Errorf("login: empty sid")
	}
	c.sid = lr.Sid
	return nil
}

type cryptSpec struct {
	crypter crypt.Crypter
	prefix  string
}

func cryptMagic(alg string) (cryptSpec, error) {
	switch alg {
	case "1":
		return cryptSpec{crypt.MD5.New(), "$1$"}, nil
	case "5":
		return cryptSpec{crypt.SHA256.New(), "$5$"}, nil
	case "6":
		return cryptSpec{crypt.SHA512.New(), "$6$"}, nil
	default:
		return cryptSpec{}, fmt.Errorf("unsupported crypt alg %q", alg)
	}
}

// Login forces an authentication handshake. It is optional; Call() logs in
// lazily and re-authenticates when the session expires.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login(ctx)
}

// isAuthError reports whether err indicates an expired/invalid session.
func isAuthError(err error) bool {
	var re *rpcError
	if e, ok := err.(*rpcError); ok {
		re = e
	}
	// GL.iNet returns code -32000 ("Access denied") for stale sessions.
	return re != nil && (re.Code == -32000 || re.Code == 401)
}

// Call invokes service.method on the router with the given params, handling
// session creation and one automatic re-login on an auth failure. The JSON
// result is unmarshalled into out (may be nil to discard).
func (c *Client) Call(ctx context.Context, service, method string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sid == "" {
		if err := c.login(ctx); err != nil {
			return err
		}
	}

	raw, err := c.rawCall(ctx, "call", []any{c.sid, service, method, params})
	if isAuthError(err) {
		// Session expired: re-login once and retry.
		c.sid = ""
		if lerr := c.login(ctx); lerr != nil {
			return lerr
		}
		raw, err = c.rawCall(ctx, "call", []any{c.sid, service, method, params})
	}
	if err != nil {
		return fmt.Errorf("%s.%s: %w", service, method, err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s.%s result: %w", service, method, err)
		}
	}
	return nil
}

func hasScheme(s string) bool {
	for i := 0; i < len(s)-2; i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			return true
		}
	}
	return false
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
