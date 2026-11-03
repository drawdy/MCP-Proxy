package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This deliberately uses HTTP/SSE directly, independently of the SDK client.
// The production transport and all JSON-RPC dispatch still belong to the SDK.
func TestLegacySSEWireCompatibility(t *testing.T) {
	b := richBackend()
	imageBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/pWQAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	b.AddTool(&mcp.Tool{Name: "image", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "image-start"},
			&mcp.ImageContent{Data: imageBytes, MIMEType: "image/png"},
			&mcp.TextContent{Text: "image-end"},
		}}, nil
	})
	h := backendHTTP(t, b)
	_, endpoint := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "sse", URL: h.URL}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := func() (*http.Response, *bufio.Scanner) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		req.Header.Set("Accept", "text/event-stream")
		r, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { r.Body.Close() })
		if r.StatusCode != 200 {
			t.Fatal(r.Status)
		}
		return r, bufio.NewScanner(r.Body)
	}
	event := func(scan *bufio.Scanner) (string, string) {
		t.Helper()
		var kind, data string
		for scan.Scan() {
			line := scan.Text()
			if line == "" && data != "" {
				return kind, data
			}
			if strings.HasPrefix(line, "event:") {
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		t.Fatalf("SSE ended: %v", scan.Err())
		return "", ""
	}
	_, scan := open()
	kind, messageEndpoint := event(scan)
	if kind != "endpoint" {
		t.Fatalf("first event %q", kind)
	}
	base, _ := url.Parse(endpoint)
	relative, e := url.Parse(messageEndpoint)
	if e != nil {
		t.Fatal(e)
	}
	if relative.Query().Get("sessionid") == "" {
		t.Fatalf("missing sessionId: %s", messageEndpoint)
	}
	postURL := base.ResolveReference(relative).String()
	other, scan2 := open()
	_, otherEndpoint := event(scan2)
	if messageEndpoint == otherEndpoint {
		t.Fatal("shared SSE endpoint")
	}
	other.Body.Close()
	post := func(body string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", postURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusAccepted {
			content, _ := io.ReadAll(r.Body)
			t.Fatalf("POST %s: %s", r.Status, content)
		}
	}
	reply := func(id float64) map[string]any {
		t.Helper()
		for {
			kind, data := event(scan)
			if kind != "message" {
				continue
			}
			var v map[string]any
			if e := json.Unmarshal([]byte(data), &v); e != nil {
				t.Fatal(e)
			}
			if v["id"] == id {
				if v["error"] != nil {
					t.Fatalf("RPC error: %s", data)
				}
				return v
			}
		}
	}
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"interceptor-wire-test","version":"1"}}}`)
	init := reply(1)
	caps := init["result"].(map[string]any)["capabilities"].(map[string]any)
	if caps["tools"].(map[string]any)["listChanged"] != true {
		t.Fatal("listChanged not advertised")
	}
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if !strings.Contains(fingerprintJSON(reply(2)), "demo.echo") {
		t.Fatal("missing namespaced tool")
	}
	addEcho(b, "wire")
	for {
		_, data := event(scan)
		if strings.Contains(data, `"method":"notifications/tools/list_changed"`) {
			break
		}
	}
	post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"demo.wire","arguments":{"text":"wire-ok"}}}`)
	if !strings.Contains(fingerprintJSON(reply(3)), "wire-ok") {
		t.Fatal("dynamic tool call failed")
	}

	// Image blocks must survive backend aggregation and the legacy SSE JSON wire format.
	post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"demo.image","arguments":{}}}`)
	imageReply := reply(4)
	imageResult := imageReply["result"].(map[string]any)
	content := imageResult["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("image result content blocks = %d, want 3", len(content))
	}
	if content[0].(map[string]any)["text"] != "image-start" || content[2].(map[string]any)["text"] != "image-end" {
		t.Fatalf("mixed text/image order changed: %#v", content)
	}
	imageBlock := content[1].(map[string]any)
	if imageBlock["type"] != "image" || imageBlock["mimeType"] != "image/png" {
		t.Fatalf("image metadata changed: %#v", imageBlock)
	}
	if imageBlock["data"] != base64.StdEncoding.EncodeToString(imageBytes) {
		t.Fatal("image Base64 payload changed in proxy response")
	}
}

func fingerprintJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSessionRegistriesAndInitializationGap(t *testing.T) {
	b := richBackend()
	h := backendHTTP(t, b)
	manager, _ := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "sse", URL: h.URL}})
	first, second := newSessionServer(manager), newSessionServer(manager)
	if first == second {
		t.Fatal("shared server")
	}
	// Change capabilities after the initial snapshot and before initialize.
	addEcho(b, "during-initialize")
	waitFor(t, 5*time.Second, func() bool { return len(manager.Snapshot()[0].Tools) == 2 })
	connect := func(server *mcp.Server) *mcp.ClientSession {
		t.Helper()
		st, ct := mcp.NewInMemoryTransports()
		ss, e := server.Connect(context.Background(), st, nil)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { ss.Close() })
		c := mcp.NewClient(&mcp.Implementation{Name: "isolation-test", Version: "1"}, nil)
		s, e := c.Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: "2024-11-05"})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, c := connect(first), connect(second)
	waitFor(t, 5*time.Second, func() bool { r, e := a.ListTools(context.Background(), nil); return e == nil && len(r.Tools) == 2 })
	first.RemoveTools("demo.echo")
	r, e := a.ListTools(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Tools) != 1 {
		t.Fatal("first registry not changed")
	}
	assertEchoTool(t, c, "isolated")
	if len(manager.Snapshot()[0].Tools) != 2 {
		t.Fatal("frontend modified shared backend")
	}
}
