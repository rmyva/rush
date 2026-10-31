package mcp

import (
	"context"
	"fmt"
	"testing"

	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestListFeatureHelpersFetchEveryPage(t *testing.T) {
	ctx := context.Background()
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "pagination-regression"}, &modelmcp.ServerOptions{PageSize: 1})
	for i := range 3 {
		name := fmt.Sprintf("item-%d", i)
		modelmcp.AddTool(server, &modelmcp.Tool{Name: name}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
			return &modelmcp.CallToolResult{}, nil, nil
		})
		server.AddPrompt(&modelmcp.Prompt{Name: name}, func(context.Context, *modelmcp.GetPromptRequest) (*modelmcp.GetPromptResult, error) {
			return &modelmcp.GetPromptResult{}, nil
		})
		server.AddResource(&modelmcp.Resource{URI: "test://" + name}, nil)
	}
	serverTransport, clientTransport := modelmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client, err := modelmcp.NewClient(&modelmcp.Implementation{Name: "pagination-regression-client"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	wrappedClient := &ClientSession{ClientSession: client}

	tools, err := getTools(ctx, wrappedClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Errorf("getTools returned %d items, want 3", len(tools))
	}
	prompts, err := getPrompts(ctx, wrappedClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 3 {
		t.Errorf("getPrompts returned %d items, want 3", len(prompts))
	}
	resources, err := getResources(ctx, wrappedClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 3 {
		t.Errorf("getResources returned %d items, want 3", len(resources))
	}
}
