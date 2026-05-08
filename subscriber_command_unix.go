// Process-group lifecycle for the subscriber bridge on Unix-like
// platforms. Linux, macOS, the BSDs all have process groups; Windows
// has a different process-tree model that's handled in the
// _other.go sibling.

//go:build unix

package scopecache

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup wires `cmd` so a context cancellation kills
// the entire process group, not just the direct child.
//
// Without this, exec.CommandContext's default cancellation behaviour
// is "SIGKILL Process.Pid" — which only targets the script the
// bridge spawned. A subscriber script that backgrounds work
// (`curl ... &; wait`, `python long_drain.py &; sleep 60`) would
// have the shell wrapper killed but its children orphaned and
// reparented to PID 1, so they can keep running after stop returns.
//
// The setup has two parts:
//
//  1. SysProcAttr{Setpgid: true} makes the child the leader of a
//     new process group with PGID == its own PID. (Pgid=0 is the
//     default-zero value, which the kernel interprets as "use the
//     child's PID as the new group ID".)
//
//  2. Override cmd.Cancel (added in Go 1.20) so context cancellation
//     sends SIGKILL to the negative PID — the kernel convention for
//     "this whole process group". Every descendant the script
//     spawned within that group dies.
//
// cmd.Wait then returns (the direct child got SIGKILL'd), the
// bridge goroutine sees the closed wake-up channel, and stop()
// returns bounded by OS kill latency.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			// Cancel can race with Start — if the cmd never started,
			// there is no group to kill. Returning nil lets the
			// exec.Cmd default kill-the-process path become a no-op
			// (it also checks Process == nil), and cmd.Run returns
			// the context's cancellation error rather than a kill
			// error.
			return nil
		}
		// Negative PID => kill every member of the group whose PGID
		// equals the absolute value. Errors are ignored: the caller
		// already treats cancel as best-effort "process is gone or
		// about to be."
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
