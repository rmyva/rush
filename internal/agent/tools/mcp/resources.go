package mcp

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Resource = mcp.Resource

type ResourceContents = mcp.ResourceContents

var allResources = csync.NewMap[string, []*Resource]()

var resourcesBeforePublishHook struct {
	sync.Mutex
	fn func()
}

func runResourcesBeforePublishHook() {
	resourcesBeforePublishHook.Lock()
	hook := resourcesBeforePublishHook.fn
	resourcesBeforePublishHook.Unlock()
	if hook != nil {
		hook()
	}
}

// Resources returns all available MCP resources.
func Resources() iter.Seq2[string, []*Resource] {
	return allResources.Seq2()
}

// Resources returns all available MCP resources.
// A nil receiver resolves the process-current owner exactly like the
// package-level Resources function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) Resources() iter.Seq2[string, []*Resource] {
	if o == nil {
		return Resources()
	}
	// The registry data is shared name-keyed state; owner-scoping of reads
	// happens through IsConfigured filtering at consumers.
	return Resources()
}

// ListResources returns the current resources for an MCP server.
func ListResources(ctx context.Context, cfg *config.ConfigStore, name string) ([]*Resource, error) {
	lease, err := getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	defer lease.close()

	resources, err := getResources(lease.ctx, lease.session)
	if err != nil {
		return nil, err
	}

	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		resourceCount := updateResources(name, resources)
		prev, _ := states.Get(name)
		prev.Counts.Resources = resourceCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
	return resources, nil
}

// ListResources returns the current resources for an MCP server.
// A nil receiver resolves the process-current owner exactly like the
// package-level ListResources function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) ListResources(ctx context.Context, cfg *config.ConfigStore, name string) ([]*Resource, error) {
	if o == nil {
		return ListResources(ctx, cfg, name)
	}
	lease, err := getOrRenewClientForOwner(o, ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	defer lease.close()

	resources, err := getResources(lease.ctx, lease.session)
	if err != nil {
		return nil, err
	}

	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		resourceCount := updateResources(name, resources)
		prev, _ := states.Get(name)
		prev.Counts.Resources = resourceCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
	return resources, nil
}

// ReadResource reads the contents of a resource from an MCP server.
func ReadResource(ctx context.Context, cfg *config.ConfigStore, name, uri string) ([]*ResourceContents, error) {
	lease, err := getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	result, err := lease.session.ReadResource(lease.ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// ReadResource reads the contents of a resource from an MCP server.
// A nil receiver resolves the process-current owner exactly like the
// package-level ReadResource function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) ReadResource(ctx context.Context, cfg *config.ConfigStore, name, uri string) ([]*ResourceContents, error) {
	if o == nil {
		return ReadResource(ctx, cfg, name, uri)
	}
	lease, err := getOrRenewClientForOwner(o, ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	defer lease.close()
	result, err := lease.session.ReadResource(lease.ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// RefreshResources gets the updated list of resources from the MCP and updates the
// global state.
func RefreshResources(ctx context.Context, name string) {
	lease, err := currentClientLease(ctx, name)
	if err != nil {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}
	defer lease.close()

	resources, err := getResources(lease.ctx, lease.session)
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

	runResourcesBeforePublishHook()
	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		resourceCount := updateResources(name, resources)
		prev, _ := states.Get(name)
		prev.Counts.Resources = resourceCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

// RefreshResources gets the updated list of resources from the MCP and updates
// the global state.
// A nil receiver resolves the process-current owner exactly like the
// package-level RefreshResources function, so callers holding an optional
// owner can call the method unconditionally.
func (o *Owner) RefreshResources(ctx context.Context, name string) {
	if o == nil {
		RefreshResources(ctx, name)
		return
	}
	lease, err := currentClientLeaseForOwner(o, ctx, name)
	if err != nil {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}
	defer lease.close()

	resources, err := getResources(lease.ctx, lease.session)
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

	runResourcesBeforePublishHook()
	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		resourceCount := updateResources(name, resources)
		prev, _ := states.Get(name)
		prev.Counts.Resources = resourceCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		publishStateEvent(name, StateConnected, nil, counts)
	})
}

func refreshResources(name string, admission *serverAdmission) {
	lease, err := currentClientLeaseFor(admission.ctx, name, admission)
	if err != nil {
		return
	}
	defer lease.close()
	resources, err := getResources(lease.ctx, lease.session)
	if err != nil {
		lease.publishIfCurrent(lease.ctx, func() {
			previous, _ := states.Get(name)
			setState(name, StateError, err, lease.session, previous.Counts)
		}, func() {
			previous, _ := states.Get(name)
			publishStateEvent(name, StateError, err, previous.Counts)
		})
		return
	}
	var counts Counts
	lease.publishIfCurrent(lease.ctx, func() {
		resourceCount := updateResources(name, resources)
		prev, _ := states.Get(name)
		prev.Counts.Resources = resourceCount
		counts = prev.Counts
		setState(name, StateConnected, nil, lease.session, counts)
	}, func() {
		broker.Publish(pubsub.UpdatedEvent, Event{
			Type: EventStateChanged, Name: name, State: StateConnected, Counts: counts,
		})
	})
}

func getResources(ctx context.Context, c *ClientSession) ([]*Resource, error) {
	if c.InitializeResult().Capabilities.Resources == nil {
		return nil, nil
	}
	var resources []*Resource
	for resource, err := range c.Resources(ctx, &mcp.ListResourcesParams{}) {
		if err != nil {
			// Handle "Method not found" errors from MCP servers that don't support resources/list.
			if isMethodNotFoundError(err) {
				slog.Warn("MCP server does not support resources/list", "error", err)
				return nil, nil
			}
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

// isMethodNotFoundError checks if the error is a JSON-RPC "Method not found" error.
func isMethodNotFoundError(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr != nil && rpcErr.Code == jsonrpc.CodeMethodNotFound
}

func updateResources(name string, resources []*Resource) int {
	if len(resources) == 0 {
		allResources.Del(name)
		return 0
	}
	allResources.Set(name, resources)
	return len(resources)
}
