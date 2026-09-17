package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// session wraps one *mcp.ClientSession. cmd is non-nil for stdio only
// (exit detection); the transport-owned cmd.Wait() is NEVER called here —
// single Wait owner is the transport Connection, reaped via Close().
type session struct {
	cs          *mcp.ClientSession
	cmd         *exec.Cmd
	callTimeout time.Duration
	mu          sync.Mutex
	closed      bool
}

var _ Session = (*session)(nil)

// withCallTimeout bounds every round trip: effective deadline =
// min(upstream ctx deadline, CallTimeout). The parent ctx is returned
// alongside for precedence mapping (cancel vs timeout disambiguation).
func withCallTimeout(parent context.Context, d time.Duration) (pctx, ctx context.Context, cancel context.CancelFunc) {
	if dl, ok := parent.Deadline(); ok && time.Until(dl) <= d {
		ctx, cancel = context.WithCancel(parent)
		return parent, ctx, cancel
	}
	ctx, cancel = context.WithTimeout(parent, d)
	return parent, ctx, cancel
}

// mapErr applies the DETERMINISTIC precedence: cancelled > timeout >
// process-exited > closed > refused > transport (first match wins).
// Cancellation is read from the CALLER ctx (our derived cap firing never
// reports cancelled); any expired deadline — caller-owned or configured —
// reports timeout. Refusal is detected by errors.Is against
// syscall.ECONNREFUSED (never string matching) and evaluated before the
// generic transport bucket.
func (s *session) mapErr(pctx, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if pctx.Err() == context.Canceled {
		return fmt.Errorf("proxy: %v: %w", err, ErrCallCancelled)
	}
	if ctx.Err() == context.DeadlineExceeded || pctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("proxy: %v: %w", err, ErrCallTimeout)
	}
	if childExited(s.cmd) {
		return fmt.Errorf("proxy: %v: %w", err, ErrProcessExited)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("proxy: %v: %w", err, ErrSessionClosed)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("proxy: %v: %w", err, ErrConnectionRefused)
	}
	return fmt.Errorf("proxy: %v: %w", err, ErrTransport)
}

// childExited reports whether a stdio child is dead without reaping
// (reaping would compete with the transport-owned Wait) and without
// touching exec.Cmd.ProcessState (unsynchronized: the transport's Wait
// goroutine writes it concurrently — reading races under -race). Sole
// signal is a read-only /proc stat check on linux (Z/X = dead; missing
// pid dir = gone). Non-linux without a safe signal cannot observe: fail
// toward transport (never misreport exited).
func childExited(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		return true
	}
	rest := data[bytes.LastIndexByte(data, byte(')'))+1:]
	fields := bytes.Fields(rest)
	if len(fields) == 0 {
		return true
	}
	switch fields[0][0] {
	case 'Z', 'X', 'x':
		return true
	}
	return false
}

// Tools collects the full downstream list through the SDK auto-paginating
// iterator (delegation — removal fails completeness). Bounded by the call
// timeout like any other round trip.
func (s *session) Tools(ctx context.Context) ([]mcp.Tool, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("proxy: tools on closed session: %w", ErrSessionClosed)
	}
	pctx, ctx, cancel := withCallTimeout(ctx, s.callTimeout)
	defer cancel()
	out := []mcp.Tool{}
	for t, err := range s.cs.Tools(ctx, &mcp.ListToolsParams{}) {
		if err != nil {
			return nil, s.mapErr(pctx, ctx, err)
		}
		out = append(out, *t)
	}
	return out, nil
}

// CallTool copies params then strips Meta ON THE COPY: caller-supplied
// metadata never bridges downstream, and caller-owned structs are never
// mutated (SDK-generated downstream metadata exempt). Post-close calls
// return ErrSessionClosed (typed, no panic/hang).
func (s *session) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("proxy: call on closed session: %w", ErrSessionClosed)
	}
	cp := *params
	cp.Meta = nil
	pctx, ctx, cancel := withCallTimeout(ctx, s.callTimeout)
	defer cancel()
	// v1.7 wrinkle (locked): SDK cancellation cleanup may complete up to
	// ~5s AFTER the deadline — the invariant is NO UNBOUNDED
	// caller-visible hang, not exact-timeout return (bounded completion
	// proven by test).
	res, err := s.cs.CallTool(ctx, &cp)
	if err != nil {
		return nil, s.mapErr(pctx, ctx, err)
	}
	return res, nil
}

// Close is idempotent: graceful session close first (the transport then
// reaps the child — stdin close, CloseGrace wait, SIGTERM, SIGKILL;
// single Wait owner, no double-Wait, no zombie), repeat calls return nil
// without touching the transport. SDK-owned semantics inherited: graceful
// close waits for in-flight calls (bounded by their call caps) before the
// force path runs — Close never hangs unboundedly, but it is not instant
// past a stuck call.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.cs.Close()
}
