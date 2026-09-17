// Package proxy is the transport/session layer: it constructs and
// lifecycle-manages local stdio and remote static-header Streamable HTTP
// downstream sessions, exposing a transport-agnostic Session to G3.
// NO discovery, policy, namespace, forwarding, or config-source logic
// lives here. G1 config shapes are consumed (sanitized child env);
// nothing is imported from discovery/policy/namespace code.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Sentinel set for downstream call outcomes (deterministic precedence:
// cancelled > timeout > process-exited > closed > refused > transport).
var (
	ErrCallTimeout       = errors.New("downstream call timed out")
	ErrCallCancelled     = errors.New("downstream call cancelled")
	ErrProcessExited     = errors.New("downstream process exited")
	ErrSessionClosed     = errors.New("session closed")
	ErrConnectionRefused = errors.New("downstream connection refused")
	ErrTransport         = errors.New("downstream transport failure")
)

// Session is the frozen transport-agnostic interface G3 programs to.
// Tools collects the full downstream tool list (pagination delegated to
// the SDK iterator — zero hand-rolled cursor code). CallTool copies params
// then strips Meta on the copy (caller-supplied metadata never bridged;
// never mutates caller-owned structs). Close is idempotent. There is
// deliberately NO Alive(): a passive flag cannot mean liveness; G3 calls
// and handles typed results. Observed-dead state stays internal.
type Session interface {
	Tools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
}

// Options carries the three validated timeouts. Zero values take frozen
// defaults; negatives are rejected.
type Options struct {
	// ConnectTimeout bounds the Connect/initialize handshake (startup
	// ctx owned here — discovery uses this bound).
	ConnectTimeout time.Duration
	// CallTimeout caps every Tools/CallTool round trip (effective
	// deadline = min(upstream ctx deadline, CallTimeout)).
	CallTimeout time.Duration
	// CloseGrace is the stdio graceful-shutdown window: mapped to
	// CommandTransport.TerminateDuration (stdin close, wait grace,
	// SIGTERM, SIGKILL). HTTP sessions ignore it.
	CloseGrace time.Duration
}

const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultCallTimeout    = 60 * time.Second
	DefaultCloseGrace     = 5 * time.Second
)

func (o Options) resolve() (Options, error) {
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.CloseGrace == 0 {
		o.CloseGrace = DefaultCloseGrace
	}
	if o.ConnectTimeout < 0 || o.CallTimeout < 0 || o.CloseGrace < 0 {
		return Options{}, fmt.Errorf("proxy: timeouts must be positive: %+v", o)
	}
	return o, nil
}

// DialStdio spawns command[0] with args, stdin/stdout pipes, and a child
// env that EQUALS baseline+explicit (G1 BuildEnv — verified by test), then
// runs the bounded Connect/initialize handshake. The handshake ctx is
// owned here (ConnectTimeout); a spawned-but-silent child fails
// construction within the bound (no startup hang). No separate Tools
// probe: redundant with the Connect handshake (gate note).
func DialStdio(ctx context.Context, command []string, env map[string]string, opts Options) (Session, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, fmt.Errorf("proxy: stdio command required")
	}
	o, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = config.BuildEnv(os.Environ(), env)
	client := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "0.0.1"}, nil)
	t := &mcp.CommandTransport{Command: cmd, TerminateDuration: o.CloseGrace}
	cctx, cancel := context.WithTimeout(ctx, o.ConnectTimeout)
	defer cancel()
	cs, err := client.Connect(cctx, t, nil)
	if err != nil {
		// No session exists to own cmd.Wait(): reap here to avoid a
		// zombie. Sequential with any transport-side Wait (concurrent
		// double-Wait returns an error, never hangs); on this path no
		// competitor is live.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		return nil, fmt.Errorf("proxy: stdio connect: %w", err)
	}
	return &session{cs: cs, cmd: cmd, callTimeout: o.CallTimeout}, nil
}

// headerRoundTripper injects static configured headers into every
// outgoing request. Static only: there is deliberately NO runtime
// credential acquisition here (no OAuth handler, no credential flow).
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, v := range h.headers {
		out.Header.Set(k, v)
	}
	return h.base.RoundTrip(out)
}

// DialHTTP connects a Streamable HTTP session with static configured
// headers and NO reconnect: MaxRetries<0 (v1.7 maps 0 to 5 retries;
// negative disables) plus DisableStandaloneSSE (no standalone-SSE
// auto-reconnect). A dropped stream/call triggers zero reconnect
// attempts (proven by request-counter test). The handshake ctx is owned
// here (ConnectTimeout); an accepted-but-never-initializing endpoint
// fails construction within the bound. The caller's httpClient is never
// mutated (cloned before wrapping).
func DialHTTP(ctx context.Context, endpoint string, headers map[string]string, httpClient *http.Client, opts Options) (Session, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("proxy: HTTP endpoint required")
	}
	o, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	hc := &http.Client{}
	if httpClient != nil {
		// Shallow copy (never mutates the caller's client); Transport
		// is replaced below, the rest shared read-only.
		cp := *httpClient
		hc = &cp
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = headerRoundTripper{base: base, headers: headers}
	t := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           hc,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "0.0.1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, o.ConnectTimeout)
	defer cancel()
	// v1.7 wrinkle (locked): against a fully-silent endpoint the SDK's
	// best-effort notifications/cancelled teardown (5s per attempt,
	// discover + legacy-initialize) can add ~10s past the handshake
	// bound before Connect reports. Invariant is bounded failure, not
	// exact-bound return (proven by test).
	cs, err := client.Connect(cctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: HTTP connect: %w", err)
	}
	return &session{cs: cs, cmd: nil, callTimeout: o.CallTimeout}, nil
}
