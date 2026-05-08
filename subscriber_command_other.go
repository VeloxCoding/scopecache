// Process-group cleanup is Unix-only; see
// subscriber_command_unix.go. On non-Unix builds CommandContext
// only cancels the direct child. Subscriber scripts that spawn
// children should `exec` the real command (no shell wrapper) or
// manage their own child cleanup; otherwise stop() may leave
// orphans behind.

//go:build !unix

package scopecache

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}
