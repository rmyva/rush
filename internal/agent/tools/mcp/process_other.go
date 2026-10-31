//go:build windows

package mcp

import (
	"os/exec"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// configureStdioProcess makes context cancellation tree-kill a stdio MCP
// server child on Windows instead of just the direct process.
//
// Windows has no POSIX process groups, so unlike process_unix.go this can't
// use Setpgid+killpg. But the same leak shape applies here: a stdio server
// that spawns its own children (an npx-based server launching node, for
// example) leaves those children with PPID pointing at the now-dead direct
// child once the session's context is cancelled, because os/exec's default
// Cancel hook only calls cmd.Process.Kill() on the direct process — and the
// MCP SDK's own graceful-shutdown path (pipeRWC.Close in the go-sdk) does
// the same single-process kill on a normal session close. session.KillProcess
// shells out to `taskkill /F /T /PID` (falling back to TerminateProcess),
// which kills the whole descendant tree Windows recorded for the pid — the
// exact approach exec_windows.go already uses for background shell commands
// hitting this same class of bug.
func configureStdioProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// This is best-effort tree killing, not a Windows Job Object; descendants
		// that escape the process tree before taskkill runs may survive cancellation.
		return session.KillProcess(cmd.Process.Pid)
	}

	// Without a WaitDelay, a leaked descendant that keeps a stdio pipe open can
	// block cmd.Wait indefinitely even after the tree is killed.
	cmd.WaitDelay = 5 * time.Second
}
