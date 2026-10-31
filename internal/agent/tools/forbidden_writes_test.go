package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckForbiddenWriteResolvedBlocksSymlinkedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o644))
	require.NoError(t, os.Symlink(target, link))
	t.Setenv(ForbiddenWritesEnv, target)
	require.ErrorContains(t, CheckForbiddenWriteResolved(link), ForbiddenWritesEnv)
}

func TestCheckForbiddenWriteResolvedNonexistentTailThroughSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	linkDir := filepath.Join(root, "alias")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.Symlink(realDir, linkDir))
	target := filepath.Join(realDir, "new.txt")
	t.Setenv(ForbiddenWritesEnv, target)
	require.ErrorContains(t, CheckForbiddenWriteResolved(filepath.Join(linkDir, "new.txt")), ForbiddenWritesEnv)
}

func TestCheckForbiddenWriteResolved_AllowsUnrelatedPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv(ForbiddenWritesEnv, filepath.Join(root, "blocked.txt"))
	require.NoError(t, CheckForbiddenWriteResolved(filepath.Join(root, "allowed.txt")))
}

func TestCheckForbiddenWrite_NoEnvVar_AllowsAll(t *testing.T) {
	t.Setenv(ForbiddenWritesEnv, "")
	require.NoError(t, CheckForbiddenWrite(filepath.Join(t.TempDir(), "anything.txt")))
}

func TestCheckForbiddenWrite_MatchingPath_Errors(t *testing.T) {
	dir := t.TempDir()
	forbidden := filepath.Join(dir, "stdout-target.json")

	t.Setenv(ForbiddenWritesEnv, forbidden)

	err := CheckForbiddenWrite(forbidden)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RUSH_FORBID_WRITES",
		"error must name the env var so the model can self-correct")
	assert.Contains(t, err.Error(), "stdout-target.json")
}

func TestCheckForbiddenWrite_NonMatchingPath_Allows(t *testing.T) {
	dir := t.TempDir()
	forbidden := filepath.Join(dir, "blocked.json")
	allowed := filepath.Join(dir, "allowed.json")

	t.Setenv(ForbiddenWritesEnv, forbidden)

	require.NoError(t, CheckForbiddenWrite(allowed))
}

func TestCheckForbiddenWrite_MultipleEntries(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	c := filepath.Join(dir, "c.json")

	t.Setenv(ForbiddenWritesEnv, a+","+b)

	require.Error(t, CheckForbiddenWrite(a), "first entry must be blocked")
	require.Error(t, CheckForbiddenWrite(b), "second entry must be blocked")
	require.NoError(t, CheckForbiddenWrite(c), "non-listed must pass")
}

func TestCheckForbiddenWrite_WhitespaceTolerance(t *testing.T) {
	dir := t.TempDir()
	forbidden := filepath.Join(dir, "x.json")

	// Orchestrators may serialise CSVs with surrounding whitespace.
	t.Setenv(ForbiddenWritesEnv, "  "+forbidden+"  ,")

	require.Error(t, CheckForbiddenWrite(forbidden))
}

func TestCheckForbiddenWrite_PathNormalisation(t *testing.T) {
	dir := t.TempDir()
	// Use a relative-looking path that resolves to the same absolute path.
	abs := filepath.Join(dir, "deep", "..", "shallow.json")
	cleanAbs := filepath.Join(dir, "shallow.json")

	t.Setenv(ForbiddenWritesEnv, abs)

	// Should block the cleaned form too — both resolve to the same file.
	require.Error(t, CheckForbiddenWrite(cleanAbs),
		"abs of forbidden entry must match cleaned target path")
}

func TestCheckForbiddenWrite_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific case folding")
	}
	dir := t.TempDir()
	upper := filepath.Join(dir, "FILE.JSON")
	lower := strings.ToLower(upper)

	t.Setenv(ForbiddenWritesEnv, upper)

	require.Error(t, CheckForbiddenWrite(lower),
		"on Windows, paths must match case-insensitively")
}

func TestCheckForbiddenWrite_EnvVarConst(t *testing.T) {
	// Document the exact env var name as a stable contract for
	// orchestrators that set it before exec'ing rush.
	assert.Equal(t, "RUSH_FORBID_WRITES", ForbiddenWritesEnv,
		"orchestrators rely on this exact env var name — do not rename without coordinating")
}

// TestCheckForbiddenWrite_RealOrchestratorScenario reproduces the
// shamir-db .tmp-audit-D.json failure mode: the orchestrator shell-
// redirects rush's stdout into a file, and the model then writes to
// the SAME file via the write tool, corrupting the JSON envelope.
func TestCheckForbiddenWrite_RealOrchestratorScenario(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".tmp-audit-D.json")

	// Orchestrator sets the env var before exec'ing rush.
	t.Setenv(ForbiddenWritesEnv, target)

	// Touch the file as if the orchestrator started writing to it.
	require.NoError(t, os.WriteFile(target, []byte(`{"envelope":1}`), 0o644))

	// Model tries to use write tool on the same path → must be blocked.
	err := CheckForbiddenWrite(target)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "forbidden")
}
