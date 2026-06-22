package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runMCP(cacheDir string) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "read-once-hook", Version: "1.0.0"}, nil)

	type ClearCacheInput struct {
		FilePath string `json:"filePath" jsonschema:"description=The absolute path of the file to clear from the cache"`
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "readOnceClearCache",
		Description: "Clear a file from the read-once hook cache. Use this when the file was evicted from your context and the hook is blocking you from reading it again.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ClearCacheInput) (*mcp.CallToolResult, any, error) {
		err := clearFileGlobal(cacheDir, in.FilePath)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error clearing cache: %v", err)}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Cache cleared successfully. You can now read the file."}},
		}, nil, nil
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, &mcp.StdioTransport{})
}
