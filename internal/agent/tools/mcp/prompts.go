package mcp

import (
	"context"
	"iter"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Prompt = mcp.Prompt

var allPrompts = csync.NewMap[string, []*Prompt]()

// Prompts returns all available MCP prompts.
func Prompts() iter.Seq2[string, []*Prompt] {
	return allPrompts.Seq2()
}

// Prompts returns all available MCP prompts.
// A nil receiver resolves the process-current owner exactly like the
// package-level Prompts function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) Prompts() iter.Seq2[string, []*Prompt] {
	if o == nil {
		return Prompts()
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return Prompts()
}

// GetPromptMessages retrieves the content of an MCP prompt with the given arguments.
func GetPromptMessages(ctx context.Context, cfg *config.ConfigStore, clientName, promptName string, args map[string]string) ([]string, error) {
	lease, err := getOrRenewClient(ctx, cfg, clientName)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	result, err := lease.session.GetPrompt(lease.ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, msg := range result.Messages {
		if msg.Role != "user" {
			continue
		}
		if textContent, ok := msg.Content.(*mcp.TextContent); ok {
			messages = append(messages, textContent.Text)
		}
	}
	return messages, nil
}

// GetPromptMessages retrieves the content of an MCP prompt with the given
// arguments.
// A nil receiver resolves the process-current owner exactly like the
// package-level GetPromptMessages function, so callers holding an optional
// owner can call the method unconditionally.
func (o *Owner) GetPromptMessages(ctx context.Context, cfg *config.ConfigStore, clientName, promptName string, args map[string]string) ([]string, error) {
	if o == nil {
		return GetPromptMessages(ctx, cfg, clientName, promptName, args)
	}
	lease, err := getOrRenewClientForOwner(o, ctx, cfg, clientName)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	result, err := lease.session.GetPrompt(lease.ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	var messages []string
	for _, msg := range result.Messages {
		if msg.Role != "user" {
			continue
		}
		if textContent, ok := msg.Content.(*mcp.TextContent); ok {
			messages = append(messages, textContent.Text)
		}
	}
	return messages, nil
}

// RefreshPrompts gets the updated list of prompts from the MCP and updates the
// global state.
func RefreshPrompts(ctx context.Context, name string) {
	lease, err := currentClientLease(ctx, name)
	if err != nil {
		slog.Warn("Refresh prompts: no session", "name", name)
		return
	}
	defer lease.close()

	prompts, err := getPrompts(lease.ctx, lease.session)
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
		updatePrompts(name, prompts)
		prev, _ := states.Get(name)
		prev.Counts.Prompts = len(prompts)
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

// RefreshPrompts gets the updated list of prompts from the MCP and updates the
// global state.
// A nil receiver resolves the process-current owner exactly like the
// package-level RefreshPrompts function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) RefreshPrompts(ctx context.Context, name string) {
	if o == nil {
		RefreshPrompts(ctx, name)
		return
	}
	lease, err := currentClientLeaseForOwner(o, ctx, name)
	if err != nil {
		slog.Warn("Refresh prompts: no session", "name", name)
		return
	}
	defer lease.close()

	prompts, err := getPrompts(lease.ctx, lease.session)
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
		updatePrompts(name, prompts)
		prev, _ := states.Get(name)
		prev.Counts.Prompts = len(prompts)
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

func refreshPrompts(name string, admission *serverAdmission) {
	lease, err := currentClientLeaseFor(admission.ctx, name, admission)
	if err != nil {
		return
	}
	defer lease.close()
	prompts, err := getPrompts(lease.ctx, lease.session)
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
		updatePrompts(name, prompts)
		prev, _ := states.Get(name)
		prev.Counts.Prompts = len(prompts)
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		broker.Publish(pubsub.UpdatedEvent, Event{
			Type: EventStateChanged, Name: name, State: StateConnected, Counts: counts,
		})
	})
}

func getPrompts(ctx context.Context, c *ClientSession) ([]*Prompt, error) {
	if c.InitializeResult().Capabilities.Prompts == nil {
		return nil, nil
	}
	var prompts []*Prompt
	for prompt, err := range c.Prompts(ctx, &mcp.ListPromptsParams{}) {
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, prompt)
	}
	return prompts, nil
}

// updatePrompts updates the global mcpPrompts and mcpClient2Prompts maps
func updatePrompts(mcpName string, prompts []*Prompt) {
	if len(prompts) == 0 {
		allPrompts.Del(mcpName)
		return
	}
	allPrompts.Set(mcpName, prompts)
}
