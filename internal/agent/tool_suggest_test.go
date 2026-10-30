package agent

import (
	"context"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func testAgentWithTools(names ...string) *sessionAgent {
	var tools []fantasy.AgentTool
	for _, name := range names {
		tools = append(tools, fantasy.NewAgentTool(name, "test", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		}))
	}
	for _, tool := range tools {
		_ = tool.Info()
	}
	return &sessionAgent{tools: csync.NewSliceFrom(tools)}
}

func TestAugmentUnknownToolResultConcurrentInfo(t *testing.T) {
	t.Parallel()

	a := testAgentWithTools("bash", "grep", "view", "edit")
	const callers = 32
	start := make(chan struct{})
	ready := make(chan struct{}, callers)
	results := make(chan string, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			results <- a.augmentUnknownToolResult(errResult("tool not found: gash")).Content
		}()
	}
	for range callers {
		<-ready
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		require.Contains(t, result, `did you mean "bash"?`)
	}
}

func errResult(content string) message.ToolResult {
	return message.ToolResult{Content: content, IsError: true}
}

func TestAugmentUnknownToolResult(t *testing.T) {
	t.Parallel()

	a := testAgentWithTools("bash", "grep", "view", "edit")

	t.Run("close match suggests the real tool", func(t *testing.T) {
		t.Parallel()
		out := a.augmentUnknownToolResult(errResult("tool not found: gash"))
		require.Contains(t, out.Content, `did you mean "bash"?`)
	})
	t.Run("another close match", func(t *testing.T) {
		t.Parallel()
		out := a.augmentUnknownToolResult(errResult("tool not found: brep"))
		require.Contains(t, out.Content, `did you mean "grep"?`)
	})
	t.Run("prefix match", func(t *testing.T) {
		t.Parallel()
		out := a.augmentUnknownToolResult(errResult("tool not found: bas"))
		require.Contains(t, out.Content, `did you mean "bash"?`)
	})
	t.Run("distant name gets the available list", func(t *testing.T) {
		t.Parallel()
		out := a.augmentUnknownToolResult(errResult("tool not found: totally_bogus_tool"))
		require.Contains(t, out.Content, "no close match")
		require.Contains(t, out.Content, "bash, grep, view, edit")
	})
	t.Run("non tool-not-found errors pass through", func(t *testing.T) {
		t.Parallel()
		in := errResult("File not found: nope.txt")
		require.Equal(t, in, a.augmentUnknownToolResult(in))
	})
	t.Run("successful results pass through", func(t *testing.T) {
		t.Parallel()
		in := message.ToolResult{Content: "ok"}
		require.Equal(t, in, a.augmentUnknownToolResult(in))
	})
}

func TestToolNameSuggestion(t *testing.T) {
	t.Parallel()

	available := []string{"bash", "grep", "view", "edit", "batch_output"}
	require.Equal(t, "bash", toolNameSuggestion("gash", available))
	require.Equal(t, "grep", toolNameSuggestion("brep", available))
	require.Equal(t, "", toolNameSuggestion("totally_unrelated_name", available))
}
