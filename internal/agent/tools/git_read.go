package tools

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// GitReadToolName is the name of the read-only git tool.
const GitReadToolName = "git_read"

const (
	// gitReadTimeout bounds a single invocation. Local git read operations
	// are fast; 30 seconds is generous headroom for huge blame or log -p
	// output while bounding a pathological repo so it cannot hang the turn.
	gitReadTimeout = 30 * time.Second

	// gitLogDefaultMaxCount is the default number of commits shown by log.
	gitLogDefaultMaxCount = 20

	// gitLogHardCap is the upper bound on log's max_count; larger values
	// are clamped so the model cannot request unbounded output.
	gitLogHardCap = 200
)

// GitReadParams is the input for the git_read tool.
type GitReadParams struct {
	Operation     string `json:"operation" description:"The read-only git operation to perform. One of: status, diff, log, show, blame, branch_list"`
	Ref           string `json:"ref,omitempty" description:"A commit/ref/branch/tag (e.g. \"HEAD~1\", \"main\", \"v1.2.3\", or a range like \"HEAD~2..HEAD\"). Optional for diff/log/blame; REQUIRED for show. Must not start with '-'"`
	Path          string `json:"path,omitempty" description:"Optional pathspec limiting the operation to one file/directory, relative to the working directory. Required for blame. Must not start with '-' and must stay inside the working directory"`
	Staged        bool   `json:"staged,omitempty" description:"diff only: set true (boolean) to diff the staged changes against HEAD instead of the unstaged working-tree changes"`
	ContextLines  int    `json:"context_lines,omitempty" description:"diff only: number of context lines around hunks (passes -U<n>). 0 = git default (3)"`
	MaxCount      int    `json:"max_count,omitempty" description:"log only: maximum number of commits to show (default 20, hard cap 200 — larger values are clamped)"`
	Patch         bool   `json:"patch,omitempty" description:"log only: set true (boolean) to include each commit's diff (-p)"`
	IncludeRemote bool   `json:"include_remote,omitempty" description:"branch_list only: set true (boolean) to also list remote-tracking branches (adds -a)"`
}

// GitReadResponseMetadata describes how the git_read call was served.
type GitReadResponseMetadata struct {
	Operation string `json:"operation"`
	Truncated bool   `json:"truncated"`
}

//go:embed git_read.md.tpl
var gitReadDescriptionTmpl []byte

var gitReadDescriptionTpl = template.Must(
	template.New("gitReadDescription").
		Parse(string(gitReadDescriptionTmpl)),
)

type gitReadDescriptionData struct {
	TimeoutSeconds int
	MaxLogCommits  int
	MaxOutputLen   int
}

func gitReadDescription() string {
	return renderTemplate(gitReadDescriptionTpl, gitReadDescriptionData{
		TimeoutSeconds: int(gitReadTimeout.Seconds()),
		MaxLogCommits:  gitLogHardCap,
		MaxOutputLen:   MaxOutputLength,
	})
}

// gitReadArgv builds the git argument vector for the requested operation.
// It validates ref and path before any process is spawned, so an
// argument-injection attempt never reaches git.
func gitReadArgv(workingDir string, params GitReadParams) (args []string, clampNotice string, errResp string) {
	switch params.Operation {
	case "status":
		return []string{"status", "--porcelain=v1", "--branch"}, "", ""

	case "diff":
		args = []string{"diff"}
		if params.Staged {
			args = append(args, "--cached")
		}
		if params.ContextLines > 0 {
			args = append(args, fmt.Sprintf("-U%d", params.ContextLines))
		}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, "", ""

	case "log":
		n := params.MaxCount
		if n == 0 {
			n = gitLogDefaultMaxCount
		}
		if n > gitLogHardCap {
			clampNotice = fmt.Sprintf("(max_count %d clamped to %d)\n", params.MaxCount, gitLogHardCap)
			n = gitLogHardCap
		}
		args = []string{"log", "-n", fmt.Sprintf("%d", n)}
		if params.Patch {
			args = append(args, "-p")
		}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, clampNotice, ""

	case "show":
		if params.Ref == "" {
			return nil, "", "show requires a ref (e.g. HEAD, a branch, or a commit hash)"
		}
		args = []string{"show", params.Ref}
		if params.Path != "" {
			args = append(args, "--", params.Path)
		}
		return args, "", ""

	case "blame":
		if params.Path == "" {
			return nil, "", "blame requires a path"
		}
		args = []string{"blame"}
		if params.Ref != "" {
			args = append(args, params.Ref)
		}
		args = append(args, "--", params.Path)
		return args, "", ""

	case "branch_list":
		args = []string{"branch", "--list", "-vv"}
		if params.IncludeRemote {
			args = append(args, "-a")
		}
		return args, "", ""

	default:
		return nil, "", fmt.Sprintf(
			"unknown operation %q; supported operations: status, diff, log, show, blame, branch_list",
			params.Operation)
	}
}

// pathInsideWorkingDir rejects paths starting with '-' and paths that
// resolve outside the working directory. Shared by the git_read and
// run_command tools. Unlike view.go, there is
// no permission escape hatch: neither tool may touch anything outside
// the working dir, by construction.
func pathInsideWorkingDir(workingDir, path string) string {
	if strings.HasPrefix(path, "-") {
		return fmt.Sprintf("invalid path %q: paths must not start with '-'", path)
	}

	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Sprintf("cannot resolve working directory %q: %v", workingDir, err)
	}

	absTarget := ""
	if filepath.IsAbs(path) {
		// Join never treats an absolute second element as a replacement
		// (filepath.Join("D:\\w", "C:\\x") is "D:\\w\\C:\\x", nonsense that
		// would silently pass the Rel check below), so absolute paths are
		// used as-is.
		absTarget, err = filepath.Abs(path)
	} else {
		absTarget, err = filepath.Abs(filepath.Join(workingDir, path))
	}
	if err != nil {
		return fmt.Sprintf("invalid path %q: %v", path, err)
	}

	rel, err := filepath.Rel(absWorkingDir, absTarget)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Sprintf(
			"path %q must stay inside the working directory", path)
	}
	return ""
}

// gitReadOutputBuffer bounds bytes retained from the child process while still
// reporting writes as fully consumed so git can finish normally.
type gitReadOutputBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	limit int
}

func (b *gitReadOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written := len(p)
	if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return written, nil
}

func (b *gitReadOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// NewGitReadTool builds the read-only git tool scoped to workingDir.
func NewGitReadTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GitReadToolName,
		gitReadDescription(),
		func(ctx context.Context, params GitReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			args, clampNotice, errResp := gitReadArgv(workingDir, params)
			if errResp != "" {
				return fantasy.NewTextErrorResponse(errResp), nil
			}

			usesRef := params.Operation == "diff" || params.Operation == "log" ||
				params.Operation == "show" || params.Operation == "blame"
			if usesRef && params.Ref != "" && strings.HasPrefix(params.Ref, "-") {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"invalid ref %q: refs must not start with '-'", params.Ref)), nil
			}

			if usesRef && params.Path != "" {
				if msg := pathInsideWorkingDir(workingDir, params.Path); msg != "" {
					return fantasy.NewTextErrorResponse(msg), nil
				}
			}

			runCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
			defer cancel()

			cmd := platform.Command(runCtx, "git", args...)
			cmd.Dir = workingDir
			out := &gitReadOutputBuffer{limit: MaxOutputLength + 1}
			cmd.Stdout = out
			cmd.Stderr = out
			err := cmd.Run()
			if err != nil {
				text := strings.TrimSpace(truncateOutput(out.String()))
				if text == "" {
					text = err.Error()
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"git %s failed: %s", params.Operation, text)), nil
			}

			raw := out.String()
			text := truncateOutput(raw)
			if clampNotice != "" {
				text = clampNotice + text
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(text),
				GitReadResponseMetadata{
					Operation: params.Operation,
					Truncated: len(raw) > MaxOutputLength,
				},
			), nil
		},
	)
}
