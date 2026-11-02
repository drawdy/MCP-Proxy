package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/backend"
	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSSEProxySupportsConcurrentClientsAndReconnect(t *testing.T) {
	backendServer := mcp.NewServer(
		&mcp.Implementation{Name: "test-backend", Version: "1.0.0"},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}},
	)
	backendServer.AddTool(
		&mcp.Tool{
			Name:        "echo",
			Description: "Echo text back to the caller",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var input struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(request.Params.Arguments, &input); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: input.Text}},
			}, nil
		},
	)
	backendServer.AddResource(
		&mcp.Resource{URI: "test://greeting", Name: "greeting", MIMEType: "text/plain"},
		func(_ context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      request.Params.URI,
					MIMEType: "text/plain",
					Text:     "hello from backend",
				}},
			}, nil
		},
	)

	backendMux := http.NewServeMux()
	backendMux.Handle("/", mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
		return backendServer
	}, nil))
	backendHTTP := httptest.NewServer(backendMux)
	defer backendHTTP.Close()

	manager := backend.NewManager(config.Config{
		MCPServers: map[string]config.ServerConfig{
			"demo": {Type: "sse", URL: backendHTTP.URL},
		},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Close()

	waitFor(t, 5*time.Second, func() bool { return manager.ConnectedCount() == 1 })

	handler, err := NewHandler(manager, Options{
		SSEPath:         "/sse",
		HealthEndpoints: []string{"/healthz", "/readyz"},
		CORSOrigins:     []string{"*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyHTTP := httptest.NewServer(handler)
	defer proxyHTTP.Close()

	first := connectClient(t, proxyHTTP.URL+"/sse")
	second := connectClient(t, proxyHTTP.URL+"/sse")

	assertEchoTool(t, second, "second-client")
	if err := first.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}

	// Closing one frontend session must not tear down another session or the
	// shared backend connection.
	assertEchoTool(t, second, "still-connected")
	if manager.ConnectedCount() != 1 {
		t.Fatalf("backend connections = %d, want 1", manager.ConnectedCount())
	}

	resources, err := second.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	if len(resources.Resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resources.Resources))
	}
	if resources.Resources[0].URI == "test://greeting" {
		t.Fatal("resource URI should be namespaced by the proxy")
	}
	readResult, err := second.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: resources.Resources[0].URI})
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if len(readResult.Contents) != 1 || readResult.Contents[0].Text != "hello from backend" {
		t.Fatalf("unexpected resource result: %#v", readResult.Contents)
	}

	for i := 0; i < 100; i++ {
		reconnected := connectClient(t, proxyHTTP.URL+"/sse")
		assertEchoTool(t, reconnected, "reconnect")
		if err := reconnected.Close(); err != nil {
			t.Fatalf("close reconnected client: %v", err)
		}
	}

	assertEchoTool(t, second, "second-client-survived")
	if err := second.Close(); err != nil {
		t.Fatalf("close second client: %v", err)
	}
}

func connectClient(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "proxy-integration-test", Version: "1.0.0"}, nil)

	session, err := client.Connect(context.Background(), &mcp.SSEClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}
	return session
}

func assertEchoTool(t *testing.T, session *mcp.ClientSession, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := false
	for _, tool := range result.Tools {
		if tool.Name == "demo.echo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("namespaced tool demo.echo not found: %#v", result.Tools)
	}

	callResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "demo.echo",
		Arguments: map[string]any{"text": value},
	})
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if callResult.IsError {
		t.Fatalf("echo returned MCP error: %#v", callResult)
	}
	if len(callResult.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(callResult.Content))
	}
	textContent, ok := callResult.Content[0].(*mcp.TextContent)
	if !ok || textContent.Text != value {
		t.Fatalf("echo result = %#v, want %q", callResult.Content[0], value)
	}
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
