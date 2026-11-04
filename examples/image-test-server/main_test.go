package main

import (
	"bytes"
	"context"
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/backend"
	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/drawdy/MCP-Proxy/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExampleThroughProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newServer()
	backendHTTP := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer backendHTTP.Close()

	cfg, err := config.Load("proxy-config.json")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.MCPServers["image-test"]
	if !ok || entry.Type != "sse" || entry.URL != "http://127.0.0.1:3007/sse" {
		t.Fatalf("unexpected example configuration: %#v", cfg)
	}
	entry.URL = backendHTTP.URL + "/sse"
	cfg.MCPServers["image-test"] = entry
	manager := backend.NewManager(cfg, nil)
	manager.Start(ctx)
	defer manager.Close()
	for manager.ConnectedCount() != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("example backend did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	handler, err := proxy.NewHandler(manager, proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	proxyHTTP := httptest.NewServer(handler)
	defer proxyHTTP.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "example-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.SSEClientTransport{Endpoint: proxyHTTP.URL + "/sse"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools.Tools))
	}

	call := func(name string) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "image-test." + name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s returned a tool error: %#v", name, result)
		}
		return result
	}
	assertText := func(content mcp.Content, contains string) {
		t.Helper()
		text, ok := content.(*mcp.TextContent)
		if !ok || !strings.Contains(text.Text, contains) {
			t.Fatalf("content = %#v, want text containing %q", content, contains)
		}
	}

	gallery := call("image_gallery")
	if len(gallery.Content) != 5 {
		t.Fatalf("gallery blocks = %d, want 5", len(gallery.Content))
	}
	assertText(gallery.Content[0], "landscape PNG")
	assertText(gallery.Content[2], "portrait JPEG")
	assertText(gallery.Content[4], "End of image gallery test.")
	for _, want := range []struct {
		index, width, height int
		format               string
	}{{1, 640, 360, "png"}, {3, 360, 520, "jpeg"}} {
		content, ok := gallery.Content[want.index].(*mcp.ImageContent)
		if !ok || content.MIMEType != "image/"+want.format {
			t.Fatalf("block %d = %#v, want image/%s", want.index, gallery.Content[want.index], want.format)
		}
		decoded, format, err := image.Decode(bytes.NewReader(content.Data))
		if err != nil {
			t.Fatal(err)
		}
		if format != want.format || decoded.Bounds().Dx() != want.width || decoded.Bounds().Dy() != want.height {
			t.Fatalf("image = %s %v, want %s %dx%d", format, decoded.Bounds(), want.format, want.width, want.height)
		}
	}
	text := call("text_only")
	if len(text.Content) != 1 {
		t.Fatalf("text blocks = %d, want 1", len(text.Content))
	}
	assertText(text.Content[0], "Text-only MCP result")
	svg := call("svg_notice")
	if len(svg.Content) != 2 {
		t.Fatalf("SVG blocks = %d, want 2", len(svg.Content))
	}
	assertText(svg.Content[0], "intentionally unsupported")
	content, ok := svg.Content[1].(*mcp.ImageContent)
	if !ok || content.MIMEType != "image/svg+xml" || !bytes.Contains(content.Data, []byte("<svg")) {
		t.Fatalf("SVG content = %#v", svg.Content[1])
	}
}
