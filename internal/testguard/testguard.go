// Package testguard keeps test binaries away from the operator's real global
// Rush state. Tests that reach config.Init or app.New without their own
// isolation otherwise read, migrate and rewrite the real global config files.
package testguard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// globalEnv is every variable through which global Rush state is located.
// Tests that set their own values with t.Setenv still override these.
var globalEnv = map[string]string{
	"RUSH_GLOBAL_CONFIG": "config",
	"XDG_CONFIG_HOME":    "config",
	"RUSH_GLOBAL_DATA":   "data",
	"XDG_DATA_HOME":      "data",
	"RUSH_CACHE_DIR":     "cache",
}

// rootEnv marks a process whose global state is already isolated. Test
// binaries re-invoked as child processes inherit it together with the
// parent's (possibly per-test) paths, which must be kept as they are.
const rootEnv = "RUSH_TESTGUARD_ROOT"

// Isolate points global Rush state at a fresh temporary root for the whole
// process and returns a function that removes it. RUSH_GLOBAL_DIR is cleared
// because it would override the per-path variables. In a child of an isolated
// process the inherited environment is kept and cleanup is a no-op.
func Isolate() (cleanup func(), err error) {
	if os.Getenv(rootEnv) != "" {
		return func() {}, nil
	}
	root, err := os.MkdirTemp("", "rush-test-global-*")
	if err != nil {
		return nil, err
	}
	for name, sub := range globalEnv {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
		if err := os.Setenv(name, dir); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	if err := os.Unsetenv("RUSH_GLOBAL_DIR"); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := os.Setenv(rootEnv, root); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return func() {
		// The marker stays set on purpose: callers exit right after, and a
		// background goroutine that resolves global state in that window must
		// still see an isolated process instead of panicking after PASS.
		_ = os.RemoveAll(root)
	}, nil
}

// Main runs m with global Rush state isolated and exits with its code.
func Main(m *testing.M) {
	cleanup, err := Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: isolate global state: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
