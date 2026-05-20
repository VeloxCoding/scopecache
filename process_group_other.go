// Process-group cleanup is Unix-only; see process_group_unix.go.
// On non-Unix builds CommandContext only cancels the direct child.
// Helper scripts that spawn children should `exec` the real
// command (no shell wrapper) or manage their own child cleanup;
// otherwise stop() may leave orphans behind.

//go:build !unix

package scopecache

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}
