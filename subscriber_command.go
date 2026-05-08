// Subscriber-command bridge: a thin Go-side wrapper around the
// Subscribe primitive that invokes an operator-supplied executable on
// every wake-up. The cache itself never reads or writes item data —
// the command does the actual fetch (curl /tail), processing, and
// delete (curl /delete_up_to) using the cache's existing HTTP
// endpoints.
//
// One environment variable is set per command invocation:
//
//	SCOPECACHE_SCOPE   the reserved scope that triggered the wake-up
//	                   (_events or _inbox)
//
// Everything else the command needs (cache socket path, HTTP base URL,
// auth headers) is the operator's responsibility — the cache does not
// know how the command reaches itself.
//
// "Command," not "script": the bridge calls `exec.Command(path)`,
// which works for shell scripts, Python/PHP/Ruby with a shebang,
// and compiled binaries (Go, C, Rust, anything). Operators are not
// limited to interpreted scripts.
//
// Concurrency: one StartSubscriber call = one goroutine = one command
// invocation at a time, in strict order. Wake-ups arriving while a
// command is running coalesce in the cache's single-slot wake-up
// channel; the next loop iteration sees a single pending wake-up and
// triggers one more command run. The command never runs concurrently
// with itself.

package scopecache

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
)

// StartSubscriber spawns a goroutine that subscribes to scope, then
// invokes command + waits for it to exit on every wake-up. Returns
// a stop function that:
//
//  1. cancels the per-subscriber context — the in-flight cmd.Run
//     was started via exec.CommandContext, so cancellation SIGKILLs
//     the running process and cmd.Run returns immediately,
//  2. closes the subscription (which closes the wake-up channel,
//     which exits the goroutine's `for range` loop), and
//  3. blocks until the goroutine has fully exited.
//
// Step 1 bounds shutdown latency: stop returns within OS-process-
// kill latency regardless of what the command was doing. Adapters
// call stopSubscribers before shutting the public listener so a
// killed in-flight HTTP request sees a normal connection reset.
//
// stop is idempotent — safe to wire both into a signal-handler and
// a defer backstop.
//
// Errors at start-up:
//   - empty scope or command -> validation error
//   - non-reserved scope -> wraps ErrInvalidSubscribeScope
//   - duplicate subscribe -> wraps ErrAlreadySubscribed
//
// The command's exit code is logged but not interpreted; the next
// wake-up retries unconditionally. Backoff/alerting on repeated
// failures lives in the command itself.
func (gw *Gateway) StartSubscriber(scope, command string) (stop func(), err error) {
	if scope == "" {
		return nil, errors.New("scopecache: StartSubscriber: scope is required")
	}
	if command == "" {
		return nil, errors.New("scopecache: StartSubscriber: command is required")
	}

	// In-package caller — go directly to store.Subscribe instead of
	// the Gateway passthrough. External callers reach the cache
	// through *Gateway; internal code reaches *store directly.
	ch, unsub, err := gw.store.Subscribe(scope)
	if err != nil {
		return nil, fmt.Errorf("scopecache: StartSubscriber: subscribe %s: %w", scope, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// No initial pre-subscribe drain: the command handles all reads
		// itself, and on first wake-up will see whatever has accumulated
		// (the cache only suppresses wake-ups when the channel slot is
		// already filled, not when items already exist). If the cache
		// has pending items at Start time but no new write triggers a
		// wake-up, the command won't run until something writes — that's
		// a deliberate choice: the bridge never reads, never decides,
		// only forwards.
		defer close(done)
		for range ch {
			runSubscriberCommand(ctx, scope, command)
		}
	}()

	stop = func() {
		// Cancel before unsub so an in-flight cmd.Run unblocks
		// (exec.CommandContext SIGKILLs the process on cancel) before
		// the goroutine even loops back to check the channel. unsub
		// then closes ch so the for-range exits on the next iteration,
		// and <-done blocks until the goroutine has actually returned.
		cancel()
		unsub()
		<-done
	}
	return stop, nil
}

// StartReservedSubscribers wires the bridge to every reserved scope
// (`_events` and `_inbox`) when command is non-empty. Used by both
// adapters (cmd/scopecache and caddymodule) so start/stop order and
// error policy live in one place.
//
//   - command == "" returns a no-op stop and logs nothing.
//   - Per-scope subscribe errors are logged ("failed to subscribe
//     to %s: %v") and the loop continues; a half-wired bridge is
//     still useful and the operator can fix it without taking the
//     cache offline.
//   - A summary line ("%d subscriber(s) active, command=%s") fires
//     after the loop so operators can confirm the bridge came up.
//
// stop tears down subscriptions in reverse start-order. logf takes
// the standard (string, ...any) shape; pass log.Printf or any
// equivalent.
func (g *Gateway) StartReservedSubscribers(command string, logf func(string, ...any)) func() {
	if command == "" {
		return func() {}
	}
	stops := []func(){}
	for _, scope := range []string{EventsScopeName, InboxScopeName} {
		stop, err := g.StartSubscriber(scope, command)
		if err != nil {
			logf("subscriber: failed to subscribe to %s: %v", scope, err)
			continue
		}
		stops = append(stops, stop)
	}
	logf("subscriber: %d subscriber(s) active, command=%s", len(stops), command)
	return func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
}

// runSubscriberCommand executes the configured command once,
// blocking on its exit or on ctx cancellation (whichever fires
// first). Stdout/stderr are inherited so operators see the
// command's output in their normal log stream. SCOPECACHE_SCOPE is
// passed via env so a single command can serve both reserved
// scopes and branch on which one fired.
//
// configureProcessGroup wires cancellation to kill the whole
// process group on Unix (no orphan children from `curl ... &; wait`
// shapes); the non-Unix stub falls back to direct-child kill. See
// subscriber_command_unix.go.
func runSubscriberCommand(ctx context.Context, scope, command string) {
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = append(os.Environ(), "SCOPECACHE_SCOPE="+scope)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		// Non-zero exit, missing executable, signal-kill, or
		// context-cancel all land here. Log + move on; the next
		// wake-up retries.
		log.Printf("scopecache subscriber: %s (scope=%s): %v", command, scope, err)
	}
}
