package testguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// saveEnv lets t.Setenv restore every variable Isolate touches.
func saveEnv(t *testing.T) {
	t.Helper()
	for name := range globalEnv {
		t.Setenv(name, os.Getenv(name))
	}
	t.Setenv("RUSH_GLOBAL_DIR", os.Getenv("RUSH_GLOBAL_DIR"))
	t.Setenv(rootEnv, "")
}

func TestCleanupRetainsIsolationMarker(t *testing.T) {
	saveEnv(t)

	cleanup, err := Isolate()
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv(rootEnv)
	cleanup()

	if got := os.Getenv(rootEnv); got != root {
		t.Fatalf("isolation marker = %q after cleanup, want %q", got, root)
	}
}

func TestIsolatePointsGlobalStateAtTempRoot(t *testing.T) {
	saveEnv(t)
	t.Setenv("RUSH_GLOBAL_DIR", "real-global-dir")

	cleanup, err := Isolate()
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv(rootEnv)
	if root == "" {
		t.Fatal("isolated root not exported")
	}
	for name, sub := range globalEnv {
		if got, want := os.Getenv(name), filepath.Join(root, sub); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if _, ok := os.LookupEnv("RUSH_GLOBAL_DIR"); ok {
		t.Fatal("RUSH_GLOBAL_DIR not cleared")
	}
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root not removed: %v", err)
	}
}

// A test binary re-invoked as a child must keep the paths its parent test
// chose, or it writes state where the parent never looks.
func TestIsolateInChildKeepsInheritedPaths(t *testing.T) {
	saveEnv(t)
	parent := t.TempDir()
	t.Setenv(rootEnv, parent)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(parent, "per-test-data"))

	cleanup, err := Isolate()
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if got := os.Getenv("RUSH_GLOBAL_DATA"); !strings.HasSuffix(got, "per-test-data") {
		t.Fatalf("RUSH_GLOBAL_DATA replaced: %q", got)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("parent root removed by child cleanup: %v", err)
	}
}
