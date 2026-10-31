package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Tool = mcp.Tool

// ToolResult represents the result of running an MCP tool.
type ToolResult struct {
	IsError   bool
	Type      string
	Content   string
	Data      []byte
	MediaType string
}

var allTools = csync.NewMap[string, []*Tool]()

// Tools returns all available MCP tools.
func Tools() iter.Seq2[string, []*Tool] {
	return allTools.Seq2()
}

// Tools returns all available MCP tools.
// A nil receiver resolves the process-current owner exactly like the
// package-level Tools function, so callers holding an optional owner can call
// the method unconditionally.
func (o *Owner) Tools() iter.Seq2[string, []*Tool] {
	if o == nil {
		return Tools()
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return Tools()
}

// GetServerToolNames returns the names of all tools registered for the given server.
func GetServerToolNames(name string) []string {
	tools, ok := allTools.Get(name)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// GetServerToolNames returns the names of all tools registered for the given
// server.
// A nil receiver resolves the process-current owner exactly like the
// package-level GetServerToolNames function, so callers holding an optional
// owner can call the method unconditionally.
func (o *Owner) GetServerToolNames(name string) []string {
	if o == nil {
		return GetServerToolNames(name)
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return GetServerToolNames(name)
}

// RunTool runs an MCP tool with the given input parameters.
func RunTool(ctx context.Context, cfg *config.ConfigStore, name, toolName string, input string) (ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return ToolResult{}, fmt.Errorf("error parsing parameters: %s", err)
	}

	lease, err := getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return ToolResult{}, err
	}
	defer lease.close()
	result, err := lease.session.CallTool(lease.ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return ToolResult{}, err
	}

	if len(result.Content) == 0 {
		return ToolResult{Type: "text", Content: "", IsError: result.IsError}, nil
	}

	var textParts []string
	var imageData []byte
	var imageMimeType string
	var audioData []byte
	var audioMimeType string

	for _, v := range result.Content {
		switch content := v.(type) {
		case *mcp.TextContent:
			textParts = append(textParts, content.Text)
		case *mcp.ImageContent:
			if imageData == nil {
				imageData = content.Data
				imageMimeType = content.MIMEType
			}
		case *mcp.AudioContent:
			if audioData == nil {
				audioData = content.Data
				audioMimeType = content.MIMEType
			}
		default:
			textParts = append(textParts, fmt.Sprintf("%v", v))
		}
	}

	textContent := strings.Join(textParts, "\n")

	// We need to make sure the data is base64
	// when using something like docker + playwright the data was not returned correctly.
	if imageData != nil {
		return ToolResult{
			IsError:   result.IsError,
			Type:      "image",
			Content:   textContent,
			Data:      ensureRawBytes(imageData),
			MediaType: imageMimeType,
		}, nil
	}

	if audioData != nil {
		return ToolResult{
			IsError:   result.IsError,
			Type:      "media",
			Content:   textContent,
			Data:      ensureRawBytes(audioData),
			MediaType: audioMimeType,
		}, nil
	}

	return ToolResult{
		IsError: result.IsError,
		Type:    "text",
		Content: textContent,
	}, nil
}

// RunTool runs an MCP tool with the given input parameters.
// A nil receiver resolves the process-current owner exactly like the
// package-level RunTool function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) RunTool(ctx context.Context, cfg *config.ConfigStore, name, toolName string, input string) (ToolResult, error) {
	if o == nil {
		return RunTool(ctx, cfg, name, toolName, input)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return ToolResult{}, fmt.Errorf("error parsing parameters: %s", err)
	}

	lease, err := getOrRenewClientForOwner(o, ctx, cfg, name)
	if err != nil {
		return ToolResult{}, err
	}
	defer lease.close()
	result, err := lease.session.CallTool(lease.ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return ToolResult{}, err
	}

	if len(result.Content) == 0 {
		return ToolResult{Type: "text", Content: "", IsError: result.IsError}, nil
	}

	var textParts []string
	var imageData []byte
	var imageMimeType string
	var audioData []byte
	var audioMimeType string

	for _, v := range result.Content {
		switch content := v.(type) {
		case *mcp.TextContent:
			textParts = append(textParts, content.Text)
		case *mcp.ImageContent:
			if imageData == nil {
				imageData = content.Data
				imageMimeType = content.MIMEType
			}
		case *mcp.AudioContent:
			if audioData == nil {
				audioData = content.Data
				audioMimeType = content.MIMEType
			}
		default:
			textParts = append(textParts, fmt.Sprintf("%v", v))
		}
	}

	textContent := strings.Join(textParts, "\n")

	if imageData != nil {
		return ToolResult{
			IsError:   result.IsError,
			Type:      "image",
			Content:   textContent,
			Data:      ensureRawBytes(imageData),
			MediaType: imageMimeType,
		}, nil
	}

	if audioData != nil {
		return ToolResult{
			IsError:   result.IsError,
			Type:      "media",
			Content:   textContent,
			Data:      ensureRawBytes(audioData),
			MediaType: audioMimeType,
		}, nil
	}

	return ToolResult{
		IsError: result.IsError,
		Type:    "text",
		Content: textContent,
	}, nil
}

// RefreshTools gets the updated list of tools from the MCP and updates the
// global state.
func RefreshTools(ctx context.Context, cfg *config.ConfigStore, name string) {
	lease, err := currentClientLease(ctx, name, cfg)
	if err != nil {
		slog.Warn("Refresh tools: no session", "name", name)
		return
	}
	defer lease.close()

	tools, err := getTools(lease.ctx, lease.session)
	if err != nil {
		var counts Counts
		lease.publishIfCurrent(lease.ctx, func() {
			previous, _ := states.Get(name)
			counts = previous.Counts
			setState(name, StateError, err, lease.session, counts)
		}, func() {
			publishStateEvent(name, StateError, err, counts)
		})
		return
	}

	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		toolCount := updateTools(cfg, name, tools)
		prev, _ := states.Get(name)
		prev.Counts.Tools = toolCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

// RefreshTools gets the updated list of tools from the MCP and updates the
// global state.
// A nil receiver resolves the process-current owner exactly like the
// package-level RefreshTools function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) RefreshTools(ctx context.Context, cfg *config.ConfigStore, name string) {
	if o == nil {
		RefreshTools(ctx, cfg, name)
		return
	}
	lease, err := currentClientLeaseForOwner(o, ctx, name, cfg)
	if err != nil {
		slog.Warn("Refresh tools: no session", "name", name)
		return
	}
	defer lease.close()

	tools, err := getTools(lease.ctx, lease.session)
	if err != nil {
		var counts Counts
		lease.publishIfCurrent(lease.ctx, func() {
			previous, _ := states.Get(name)
			counts = previous.Counts
			setState(name, StateError, err, lease.session, counts)
		}, func() {
			publishStateEvent(name, StateError, err, counts)
		})
		return
	}

	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		toolCount := updateTools(cfg, name, tools)
		prev, _ := states.Get(name)
		prev.Counts.Tools = toolCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

func refreshTools(ctx context.Context, cfg *config.ConfigStore, name string, admission *serverAdmission) {
	lease, err := currentClientLeaseFor(ctx, name, admission)
	if err != nil {
		return
	}
	defer lease.close()

	tools, err := getTools(lease.ctx, lease.session)
	if err != nil {
		var counts Counts
		lease.publishIfCurrent(lease.ctx, func() {
			previous, _ := states.Get(name)
			counts = previous.Counts
			setState(name, StateError, err, lease.session, counts)
		}, func() {
			publishStateEvent(name, StateError, err, counts)
		})
		return
	}
	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		toolCount := updateTools(cfg, name, tools)
		prev, _ := states.Get(name)
		prev.Counts.Tools = toolCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		broker.Publish(pubsub.UpdatedEvent, Event{
			Type: EventStateChanged, Name: name, State: StateConnected, Counts: counts,
		})
	})
}

func getTools(ctx context.Context, session *ClientSession) ([]*Tool, error) {
	// Always call ListTools to get the actual available tools.
	// The InitializeResult Capabilities.Tools field may be an empty object {},
	// which is valid per MCP spec, but we still need to call ListTools to discover tools.
	var tools []*Tool
	for tool, err := range session.Tools(ctx, &mcp.ListToolsParams{}) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func updateTools(cfg *config.ConfigStore, name string, tools []*Tool) int {
	mcpCfg, ok := cfg.MCPConfig(name)
	if ok {
		tools = filterTools(mcpCfg, tools)
	}
	if len(tools) == 0 {
		allTools.Del(name)
		return 0
	}
	allTools.Set(name, tools)
	return len(tools)
}

// filterTools filters tools based on enabled_tools (allow list) and
// disabled_tools (deny list) from the MCP config.
func filterTools(mcpCfg config.MCPConfig, tools []*Tool) []*Tool {
	if len(mcpCfg.EnabledTools) > 0 {
		filtered := make([]*Tool, 0, len(mcpCfg.EnabledTools))
		for _, tool := range tools {
			if slices.Contains(mcpCfg.EnabledTools, tool.Name) {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}

	if len(mcpCfg.DisabledTools) > 0 {
		filtered := make([]*Tool, 0, len(tools))
		for _, tool := range tools {
			if !slices.Contains(mcpCfg.DisabledTools, tool.Name) {
				filtered = append(filtered, tool)
			}
		}
		tools = filtered
	}

	return tools
}

// ensureRawBytes normalizes MCP media data into raw binary bytes.
//
// The MCP Go SDK's json.Unmarshal normally base64-decodes
// ImageContent.Data into raw bytes automatically. However, some MCP
// transports (notably Docker over stdio) can deliver data in
// unexpected formats. This function handles both cases:
//
//   - If data looks like a valid base64 string (ASCII-only, decodable)
//     it is decoded and the raw bytes are returned.
//   - If data is already raw binary (contains bytes > 127) it is
//     returned as-is.
func ensureRawBytes(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	normalized := normalizeBase64Input(data)
	if decoded, ok := decodeBase64(normalized); ok {
		return decoded
	}

	// Already raw binary — return unchanged.
	return data
}

func normalizeBase64Input(data []byte) []byte {
	normalized := strings.Join(strings.Fields(string(data)), "")
	return []byte(normalized)
}

func decodeBase64(data []byte) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}

	for _, b := range data {
		if b > 127 {
			return nil, false
		}
	}

	s := string(data)
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		return decoded, true
	}
	decoded, err = base64.RawStdEncoding.DecodeString(s)
	if err == nil {
		return decoded, true
	}
	return nil, false
}
