// Boot-time init-command bridge: invokes an operator-supplied
// executable once, synchronously, so the script can populate the
// cache from a source of truth via the same HTTP endpoints any
// other client uses (/append, /warm, /rebuild). The full boot-flow
// contract — private socket, public-listener bind ordering,
// subscriber start-after-init, failure handling — lives in
// scopecache-core-rfc.md §2.7. This file owns the code-level
// invariants only.
//
// Adapter contract: both adapters wrap RunInitCommand in their own
// runInitWithPrivateSocket helper that binds a temp AF_UNIX socket
// serving the cache routes, sets SCOPECACHE_SOCKET_PATH in
// extraEnv, runs the script, then tears the socket down. The
// script reaches the cache via
// `curl --unix-socket "$SCOPECACHE_SOCKET_PATH" http://localhost/...`
// regardless of deployment shape.
//
// `_events` wipe: RunInitCommand always wipes `_events` after the
// script exits (success or failure), so subscribers attaching after
// init see an empty stream. Init writes auto-populate `_events`
// with duplicates of the source-of-truth state; forwarding those
// through a drain would loop the data back where it came from.
//
// Cancellation: ctx is the caller's cancellation handle (typically
// a SIGINT/SIGTERM signal context, or caddy.Context for the
// module). Cancellation SIGKILLs the entire process group via
// configureProcessGroup so child processes from
// `curl ... &; wait` shapes do not leak. No default timeout —
// large-dataset rebuilds can legitimately take minutes; callers
// wrap with context.WithTimeout when they want a hard cap.
//
// Errors are logged and returned: the caller decides whether a
// failed init is fatal or recoverable. stdout/stderr are inherited.
// logf may be nil — the helper then runs without lifecycle logging.

package scopecache

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
)

// RunInitCommand executes command synchronously, blocking until it
// exits or ctx is cancelled. Returns nil when command is empty
// (sentinel for "not configured") or cmd.Run succeeds; otherwise
// returns the exec error wrapped with the command path. Always
// wipes `_events` after the script exits (success or failure) — see
// file-header for the adapter, cancellation, and logging contracts.
func (g *Gateway) RunInitCommand(ctx context.Context, command string, extraEnv []string, logf func(string, ...any)) error {
	if command == "" {
		return nil
	}
	if logf != nil {
		logf("init: running %s", command)
	}
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	runErr := cmd.Run()

	// Wipe `_events` regardless of the script's exit code (rationale
	// in file-header). A failure on the wipe is logged but not
	// surfaced — an empty cache plus a stale event stream is still
	// a working cache, and the next operator action will overwrite it.
	if _, delErr := g.DeleteUpTo(EventsScopeName, math.MaxUint64); delErr != nil && logf != nil {
		logf("init: clear %s: %v", EventsScopeName, delErr)
	}

	if runErr != nil {
		if logf != nil {
			logf("init: %s: %v", command, runErr)
		}
		return fmt.Errorf("scopecache init command %s: %w", command, runErr)
	}
	if logf != nil {
		logf("init: %s: completed", command)
	}
	return nil
}
