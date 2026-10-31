package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGitReadRepoForTest builds a small temp git repo with two commits and
// returns its directory. The warm-up `git status` run settles the index
// stat-cache so byte-level .git/index snapshots are stable.
func newGitReadRepoForTest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	gitReadRunGit(t, dir, "init", "-q", "-b", "main")
	gitReadRunGit(t, dir, "config", "user.email", "test@example.com")
	gitReadRunGit(t, dir, "config", "user.name", "Test User")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o644))
	gitReadRunGit(t, dir, "add", "a.txt")
	gitReadRunGit(t, dir, "commit", "-q", "-m", "initial commit")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644))
	gitReadRunGit(t, dir, "add", "README.md")
	gitReadRunGit(t, dir, "commit", "-q", "-m", "add readme")

	gitReadRunGit(t, dir, "status", "--porcelain")
	return dir
}

// gitReadRunGit runs a git command in dir, failing the test with the
// combined output on error.
func gitReadRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := platform.Command(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
	return string(out)
}

// runGitReadTool marshals params and runs the tool. Requiring a nil Go
// error here IS the error-contract test for this tool: every
// model-correctable failure must come back as a response instead.
func runGitReadTool(t *testing.T, tool fantasy.AgentTool, params GitReadParams) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "c1",
		Name:  GitReadToolName,
		Input: string(raw),
	})
	require.NoError(t, err)
	return resp
}

// gitReadOutput extracts the text content of a tool response.
func gitReadOutput(resp fantasy.ToolResponse) string {
	return resp.Content
}

// gitRepoSnapshot captures everything that defines a repo's read-only
// state: HEAD, the index, the branch ref, and the whole working tree.
type gitRepoSnapshot struct {
	HEAD     []byte
	Index    []byte
	Branch   string
	Worktree map[string]string
}

// snapshotGitRepo captures a byte-for-byte snapshot of the repo state.
func snapshotGitRepo(t *testing.T, dir string) gitRepoSnapshot {
	t.Helper()

	snap := gitRepoSnapshot{Worktree: map[string]string{}}
	var err error

	snap.HEAD, err = os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	require.NoError(t, err)
	snap.Index, err = os.ReadFile(filepath.Join(dir, ".git", "index"))
	require.NoError(t, err)
	snap.Branch = strings.TrimSpace(gitReadRunGit(t, dir, "symbolic-ref", "HEAD"))

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.Contains(path, ".git") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		sum := sha256.Sum256(content)
		rel, relErr := filepath.Rel(dir, path)
		require.NoError(t, relErr)
		snap.Worktree[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	require.NoError(t, err)
	return snap
}

func TestGitReadOutputBufferCapsCapturedOutput(t *testing.T) {
	var out gitReadOutputBuffer
	out.limit = MaxOutputLength + 1

	first := strings.Repeat("a", MaxOutputLength)
	written, err := out.Write([]byte(first))
	require.NoError(t, err)
	assert.Equal(t, len(first), written)

	second := strings.Repeat("b", MaxOutputLength)
	written, err = out.Write([]byte(second))
	require.NoError(t, err)
	assert.Equal(t, len(second), written)
	assert.Len(t, out.Bytes(), MaxOutputLength+1)
	assert.True(t, strings.HasSuffix(out.String(), "b"))
}

func TestGitRead_HappyPath_EachOperation(t *testing.T) {
	t.Parallel()

	t.Run("status clean", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{Operation: "status"})
		require.False(t, resp.IsError, resp.Content)
		out := gitReadOutput(resp)
		assert.Contains(t, out, "##")
		assert.NotContains(t, out, "a.txt")
	})

	t.Run("status dirty", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "a.txt"), []byte("modified\n"), 0o644))
		resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{Operation: "status"})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "a.txt")
	})

	t.Run("diff unstaged", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "a.txt"),
			[]byte("hello world\n+appended\n"), 0o644))
		resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{Operation: "diff"})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "+appended")
	})

	t.Run("diff staged", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "a.txt"),
			[]byte("hello world\nstaged line\n"), 0o644))
		gitReadRunGit(t, dir, "add", "a.txt")
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "diff", Staged: true})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "staged line")
	})

	t.Run("log", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{Operation: "log"})
		require.False(t, resp.IsError, resp.Content)
		out := gitReadOutput(resp)
		assert.Contains(t, out, "initial commit")
		assert.Contains(t, out, "add readme")
		assert.Less(t, strings.Index(out, "add readme"), strings.Index(out, "initial commit"))
	})

	t.Run("log max_count", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "log", MaxCount: 1})
		require.False(t, resp.IsError, resp.Content)
		assert.Equal(t, 1, strings.Count(gitReadOutput(resp), "commit "))
	})

	t.Run("log patch", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "a.txt"),
			[]byte("hello world\n+appended\n"), 0o644))
		gitReadRunGit(t, dir, "add", "a.txt")
		gitReadRunGit(t, dir, "commit", "-q", "-m", "append")
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "log", Patch: true})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "+appended")
	})

	t.Run("show", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "show", Ref: "HEAD"})
		require.False(t, resp.IsError, resp.Content)
		out := gitReadOutput(resp)
		assert.Contains(t, out, "add readme")
		assert.Contains(t, out, "# Test")

		resp = runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "show", Ref: "HEAD", Path: "README.md"})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "# Test")
	})

	t.Run("blame", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "blame", Path: "a.txt"})
		require.False(t, resp.IsError, resp.Content)
		out := gitReadOutput(resp)
		assert.Contains(t, out, "Test User")
		assert.Contains(t, out, "hello world")
	})

	t.Run("branch_list", func(t *testing.T) {
		t.Parallel()
		dir := newGitReadRepoForTest(t)
		branch := strings.TrimSpace(
			gitReadRunGit(t, dir, "symbolic-ref", "--short", "HEAD"))
		resp := runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "branch_list"})
		require.False(t, resp.IsError, resp.Content)
		assert.Contains(t, gitReadOutput(resp), "* "+branch)

		resp = runGitReadTool(t, NewGitReadTool(dir),
			GitReadParams{Operation: "branch_list", IncludeRemote: true})
		require.False(t, resp.IsError, resp.Content)
	})
}

// TestGitRead_PathMustStayInWorkingDir proves outside paths are refused
// before any git process runs, and that the repo state survives.
func TestGitRead_PathMustStayInWorkingDir(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secrets.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret\n"), 0o644))

	for _, op := range []string{"diff", "log", "show", "blame"} {
		for _, path := range []string{
			"../secrets.txt",
			"-foo",
			outsideFile,
		} {
			t.Run(op+"/"+path, func(t *testing.T) {
				t.Parallel()
				dir := newGitReadRepoForTest(t)
				before := snapshotGitRepo(t, dir)

				resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{
					Operation: op,
					Ref:       "HEAD",
					Path:      path,
				})
				require.True(t, resp.IsError, "expected rejection, got: %s", resp.Content)

				after := snapshotGitRepo(t, dir)
				require.Equal(t, before, after)
			})
		}
	}
}

// TestGitRead_RefDashInjectionRejected proves a dash-prefixed ref is
// caught by VALIDATION — the error text is ours, not a git fatal — so the
// value never reached a git process.
func TestGitRead_RefDashInjectionRejected(t *testing.T) {
	t.Parallel()

	for _, op := range []string{"diff", "log", "show", "blame"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			dir := newGitReadRepoForTest(t)
			before := snapshotGitRepo(t, dir)

			resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{
				Operation: op,
				Ref:       "--upload-pack=/bin/sh",
				Path:      "a.txt",
			})
			require.True(t, resp.IsError)
			assert.Contains(t, resp.Content, "must not start with")
			assert.NotContains(t, resp.Content, "fatal")

			require.Equal(t, before, snapshotGitRepo(t, dir))
		})
	}
}

// TestGitRead_UnknownOperationRejected proves mutating or empty
// operations are refused without ever touching the repo.
func TestGitRead_UnknownOperationRejected(t *testing.T) {
	t.Parallel()

	dir := newGitReadRepoForTest(t)
	before := snapshotGitRepo(t, dir)

	for _, op := range []string{"reset", ""} {
		resp := runGitReadTool(t, NewGitReadTool(dir), GitReadParams{Operation: op})
		require.True(t, resp.IsError, "operation %q must be rejected", op)
		assert.Contains(t, resp.Content, "status, diff, log, show, blame, branch_list")
	}

	require.Equal(t, before, snapshotGitRepo(t, dir))
}

// TestGitRead_RepoStateUnchangedAfterEveryOperation proves the read-only
// guarantee byte-for-byte: for each operation, HEAD bytes, index bytes,
// the branch ref, and the full working tree must be identical before and
// after the call. Sequential within one test so any mutation is
// attributable to the preceding operation.
func TestGitRead_RepoStateUnchangedAfterEveryOperation(t *testing.T) {
	t.Parallel()

	dir := newGitReadRepoForTest(t)
	before := snapshotGitRepo(t, dir)

	operations := []GitReadParams{
		{Operation: "status"},
		{Operation: "diff"},
		{Operation: "log"},
		{Operation: "show", Ref: "HEAD"},
		{Operation: "blame", Path: "a.txt"},
		{Operation: "branch_list"},
		{Operation: "branch_list", IncludeRemote: true},
		{Operation: "diff", Staged: true},
		{Operation: "log", Patch: true, MaxCount: 5},
	}

	for _, op := range operations {
		resp := runGitReadTool(t, NewGitReadTool(dir), op)
		require.False(t, resp.IsError,
			"operation %+v must succeed: %s", op, resp.Content)
		after := snapshotGitRepo(t, dir)
		require.Equal(t, before, after, "state changed after %+v", op)
	}
}

// TestGitRead_ConcurrentFromSubdirectories simulates multiple agents
// scoped to different folders of one repo running read operations at the
// same time; all must succeed and leave the repo untouched.
func TestGitRead_ConcurrentFromSubdirectories(t *testing.T) {
	t.Parallel()

	dir := newGitReadRepoForTest(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "sub", "nested.txt"), []byte("nested\n"), 0o644))
	gitReadRunGit(t, dir, "add", "sub/nested.txt")
	gitReadRunGit(t, dir, "commit", "-q", "-m", "add nested")
	before := snapshotGitRepo(t, dir)

	ops := []GitReadParams{
		{Operation: "status"},
		{Operation: "log"},
		{Operation: "diff"},
	}

	const workers = 8
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workDir := dir
			if i%2 == 1 {
				// Half the agents are scoped to a subdirectory; git
				// walks up to the repo root itself.
				workDir = filepath.Join(dir, "sub")
			}
			tool := NewGitReadTool(workDir)
			resp := runGitReadTool(t, tool, ops[i%len(ops)])
			if resp.IsError {
				errCh <- fmt.Errorf("operation %+v failed: %s",
					ops[i%len(ops)], resp.Content)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	require.Equal(t, before, snapshotGitRepo(t, dir))
}

// TestGitRead_LogMaxCountClamped proves oversized max_count values are
// clamped and announced, while in-range values pass through silently.
func TestGitRead_LogMaxCountClamped(t *testing.T) {
	t.Parallel()

	dir := newGitReadRepoForTest(t)

	resp := runGitReadTool(t, NewGitReadTool(dir),
		GitReadParams{Operation: "log", MaxCount: 100000})
	require.False(t, resp.IsError, resp.Content)
	assert.True(t, strings.HasPrefix(gitReadOutput(resp), "(max_count 100000 clamped to 200)\n"),
		"output must start with the clamp notice, got: %s", resp.Content)

	resp = runGitReadTool(t, NewGitReadTool(dir),
		GitReadParams{Operation: "log", MaxCount: 5})
	require.False(t, resp.IsError, resp.Content)
	assert.NotContains(t, gitReadOutput(resp), "clamped")
}
