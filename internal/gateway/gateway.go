// Package gateway assembles the gateway: transport-injected Gateway
// core with construction/serving separable from tunnel bootstrap.
// STAGED ORDER (locked): acquire config bytes + source label -> G1
// parse/validate/select -> create upstream server -> register/reserve
// natives -> create selected G2 sessions -> construct G3 proxy
// (discover/filter/qualify/collision-check/register) -> begin serving
// injected upstream transport. Tunnel attach is the PRODUCTION BOOTSTRAP
// step AFTER core, never inside core (core serves any injected transport
// without creds; tunnel credential validation lives ONLY in bootstrap).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ChiaYuChang/local-mcp/internal/config"
	"github.com/ChiaYuChang/local-mcp/internal/proxy"
	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/filesystem"
	"github.com/ChiaYuChang/local-mcp/internal/tools/git"
	"github.com/ChiaYuChang/local-mcp/internal/tools/jj"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
	"github.com/ChiaYuChang/local-mcp/internal/tools/test"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// LoadSource acquires config bytes with an origin label: path "-" reads
// stdin, any other path reads that file. Errors wrap with source context
// ("config file <path>:" / "config <stdin>:"). Called by bootstrap;
// Compose takes bytes+label (G1-deferred item landing here).
func LoadSource(path string, stdin io.Reader) ([]byte, string, error) {
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, "", fmt.Errorf("config <stdin>: %w", err)
		}
		return data, "config <stdin>:", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("config file %s: %w", path, err)
	}
	return data, fmt.Sprintf("config file %s:", path), nil
}

// DialSession creates one downstream session for a selected server.
// DialConfigured is the production dialer (G2 constructors with G1-built
// env; optional list-changed handler threaded through, nil preserves
// exact G2 behavior); tests inject stub factories.
type DialSession func(ctx context.Context, name string, cfg config.ServerConfig) (proxy.Session, error)

// DialConfigured routes G1-built env into G2 constructors only (no
// second policy source): local servers spawn via DialStdio, remote via
// DialHTTP with static headers. onChanged, when non-nil, is forwarded to
// the G2 optional list-changed handler (P4 reachable-but-wired-off).
func DialConfigured(po proxy.Options, onChanged func(context.Context, *mcp.ToolListChangedRequest)) DialSession {
	return func(ctx context.Context, name string, cfg config.ServerConfig) (proxy.Session, error) {
		var hs []func(context.Context, *mcp.ToolListChangedRequest)
		if onChanged != nil {
			hs = append(hs, onChanged)
		}
		switch cfg.Type {
		case "local":
			return proxy.DialStdio(ctx, cfg.Command, cfg.Environment, po, hs...)
		case "remote":
			return proxy.DialHTTP(ctx, cfg.URL, cfg.Headers, nil, po, hs...)
		default:
			return nil, fmt.Errorf("gateway: downstream %q unknown type %q", name, cfg.Type)
		}
	}
}

// Options carries everything Compose needs. ServeTransport is
// caller-created (transport ownership: serving uses it only, never
// closes it — single-owner against SDK semantics; no double-close
// across core/bootstrap/harness). DialSession nil selects
// DialConfigured (production path). PageSize 0 takes the SDK default.
type Options struct {
	WorkspaceRoot  string
	GitRoot        string
	JJRoot         string
	ConfigData     []byte
	ConfigOrigin   string
	Profiles       []string
	ServeTransport mcp.Transport
	PageSize       int
	DialSession    DialSession
	ProxyOptions   proxy.Options
	NativeEnv      []string
	OnListChanged  func(context.Context, *mcp.ToolListChangedRequest)
}

// Gateway is the composed serving core: upstream server, proxy, sessions
// (creation order, for reverse-order rollback/close), filesystem root,
// and native VCS structs (introspection seam for env proofs).
type Gateway struct {
	upstream  *mcp.Server
	proxy     *proxy.Proxy
	sessions  []proxy.Session
	fs        *filesystem.FileSystem
	git       *git.Git
	jj        *jj.JJ
	transport mcp.Transport
	mu        sync.Mutex
	served    bool
}

// nativeTools is the FROZEN native set through the single registry path:
// 8 filesystem + 4 git + 4 jj + echo (RETAINED ToolEcho implementation,
// registered like any native — no second registration path, echo joins
// the uniqueness check).
func nativeTools(fs *filesystem.FileSystem, g *git.Git, j *jj.JJ, h *secrets.SecretHider) []tools.Tool {
	out := filesystem.NativeTools(fs, h)
	out = append(out, git.NativeTools(g, h)...)
	out = append(out, jj.NativeTools(j, h)...)
	return append(out, test.ToolEcho{})
}

// Compose runs every stage through registration and returns a ready
// Gateway (fail-fast BEFORE serving, WITH partial-startup ROLLBACK:
// reverse-order close of created sessions + fs close; cleanup errors
// joined, never replacing the original startup error).
func Compose(ctx context.Context, opts Options) (*Gateway, error) {
	if opts.ServeTransport == nil {
		return nil, fmt.Errorf("gateway: serve transport required")
	}
	dial := opts.DialSession
	if dial == nil {
		dial = DialConfigured(opts.ProxyOptions, opts.OnListChanged)
	}
	// G1 parse/validate/select (LoadYAML validates internally).
	// Absent config bytes mean an empty gateway (natives only) — the
	// bootstrap default; never a parse error.
	var cfg config.GatewayConfig
	if len(opts.ConfigData) > 0 {
		var err error
		cfg, err = config.LoadYAML(opts.ConfigData)
		if err != nil {
			return nil, fmt.Errorf("gateway: %s %w", opts.ConfigOrigin, err)
		}
	}
	active := config.SelectActive(cfg, opts.Profiles)

	// Natives: roots + Hider singletons.
	fs, err := filesystem.New(opts.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("gateway: workspace: %w", err)
	}
	// Rollback ledger from here on (fs owned).
	var sessions []proxy.Session
	rollback := func(first error) (*Gateway, error) {
		var errs []error
		for i := len(sessions) - 1; i >= 0; i-- {
			if cerr := sessions[i].Close(); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		if cerr := fs.Close(); cerr != nil {
			errs = append(errs, cerr)
		}
		return nil, errors.Join(append([]error{first}, errs...)...)
	}
	h, err := filesystem.LoadHider(fs)
	if err != nil {
		_, rerr := rollback(fmt.Errorf("gateway: hider: %w", err))
		return nil, rerr
	}
	g, err := git.NewGit(opts.GitRoot, opts.NativeEnv)
	if err != nil {
		_, rerr := rollback(fmt.Errorf("gateway: git: %w", err))
		return nil, rerr
	}
	j, err := jj.NewJJ(opts.JJRoot, opts.NativeEnv)
	if err != nil {
		_, rerr := rollback(fmt.Errorf("gateway: jj: %w", err))
		return nil, rerr
	}

	// Upstream server + native reservation (natives first, single
	// uniqueness check shared with the proxy below).
	var sopts *mcp.ServerOptions
	if opts.PageSize > 0 {
		sopts = &mcp.ServerOptions{PageSize: opts.PageSize}
	}
	upstream := mcp.NewServer(&mcp.Implementation{Name: "local-mcp-gateway", Version: "0.0.1"}, sopts)
	natives := nativeTools(fs, g, j, h)
	nativeNames := make([]string, 0, len(natives))
	for _, t := range natives {
		nativeNames = append(nativeNames, t.Name())
	}

	// Selected G2 sessions (creation order recorded for rollback).
	type namedDown struct {
		name string
		cfg  config.ServerConfig
		sess proxy.Session
	}
	var downs []namedDown
	for _, name := range active {
		scfg := cfg.Servers[name]
		sess, err := dial(ctx, name, scfg)
		if err != nil {
			_, rerr := rollback(fmt.Errorf("gateway: downstream %q: %w", name, err))
			return nil, rerr
		}
		sessions = append(sessions, sess)
		downs = append(downs, namedDown{name: name, cfg: scfg, sess: sess})
	}

	// G3 proxy (deny converted mechanically; shape re-validated there).
	pdowns := make([]proxy.Downstream, 0, len(downs))
	for _, d := range downs {
		deny := make([]proxy.Gate[string], 0, len(d.cfg.Deny))
		for _, gc := range d.cfg.Deny {
			deny = append(deny, proxy.Gate[string]{Type: gc.Type, Value: gc.Params["value"]})
		}
		pdowns = append(pdowns, proxy.Downstream{Name: d.name, Session: d.sess, Deny: deny})
	}
	px, err := proxy.NewProxy(pdowns, h, nativeNames)
	if err != nil {
		_, rerr := rollback(fmt.Errorf("gateway: proxy: %w", err))
		return nil, rerr
	}

	// Register natives + proxied on the upstream server (single path:
	// every tool through tools.Tool.Register or the forwarding closure;
	// inline AddTool bypass forbidden).
	for _, t := range natives {
		if err := t.Register(upstream); err != nil {
			_, rerr := rollback(fmt.Errorf("gateway: register native %q: %w", t.Name(), err))
			return nil, rerr
		}
	}
	for _, e := range px.Exposed() {
		e := e
		if e.Server == "" {
			continue // native: registered directly above, never forwarded
		}
		mcp.AddTool(upstream, &mcp.Tool{
			Name: e.Name,
			// Narrowed-guarantee wording (G3 cannot pin Description;
			// G4 assigns it here): configured patterns on explicit
			// textual fields only.
			Description: "Proxied downstream tool " + e.Server + "/" + e.OrigName + "; outputs masked to configured patterns on textual fields only.",
			InputSchema: e.Schema,
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			// Forward continuation fields from the RAW request params
			// (raw Arguments preserve fidelity — no map round-trip;
			// the decoded args above serve SDK input validation only).
			fwd := &mcp.CallToolParams{Name: e.OrigName, Arguments: args}
			if req != nil && req.Params != nil {
				fwd.Arguments = req.Params.Arguments
				fwd.InputResponses = req.Params.InputResponses
				fwd.RequestState = req.Params.RequestState
			}
			res, err := px.CallTool(ctx, e.Name, fwd)
			return res, nil, err
		})
	}
	return &Gateway{
		upstream: upstream, proxy: px, sessions: sessions,
		fs: fs, git: g, jj: j, transport: opts.ServeTransport,
	}, nil
}

// Serve begins serving the injected upstream transport (blocking).
// Transport ownership stays with the caller (never closed here);
// a second Serve is rejected.
func (g *Gateway) Serve(ctx context.Context) error {
	g.mu.Lock()
	if g.served {
		g.mu.Unlock()
		return fmt.Errorf("gateway: already serving")
	}
	g.served = true
	g.mu.Unlock()
	return g.upstream.Run(ctx, g.transport)
}

// Close shuts down in reverse creation order (sessions, then fs).
// Never called by Serve; owned by bootstrap/rollback.
func (g *Gateway) Close() error {
	var errs []error
	g.mu.Lock()
	sessions := g.sessions
	g.sessions = nil
	g.mu.Unlock()
	for i := len(sessions) - 1; i >= 0; i-- {
		if err := sessions[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if g.fs != nil {
		if err := g.fs.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run composes then serves (convenience for one-shot use).
func Run(ctx context.Context, opts Options) error {
	gw, err := Compose(ctx, opts)
	if err != nil {
		return err
	}
	defer gw.Close()
	return gw.Serve(ctx)
}
