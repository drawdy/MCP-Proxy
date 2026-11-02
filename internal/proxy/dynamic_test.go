package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/backend"
	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"
)

func addEcho(s *mcp.Server, name string) {
	s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: echoText(r.Params.Arguments)}}}, nil
	})
}

func echoText(raw json.RawMessage) string {
	var input struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &input)
	return input.Text
}

func richBackend() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "dynamic-test", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
		Tools: &mcp.ToolCapabilities{ListChanged: true}, Prompts: &mcp.PromptCapabilities{ListChanged: true}, Resources: &mcp.ResourceCapabilities{ListChanged: true},
	}})
	addEcho(s, "echo")
	s.AddPrompt(&mcp.Prompt{Name: "greet", Description: "Say hello", Arguments: []*mcp.PromptArgument{{Name: "who", Description: "Recipient", Required: true}}}, func(_ context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		if r.Params.Name != "greet" {
			return nil, fmt.Errorf("wrong prompt route: %s", r.Params.Name)
		}
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello " + r.Params.Arguments["who"]}}}}, nil
	})
	read := func(_ context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: r.Params.URI, Text: r.Params.URI}}}, nil
	}
	s.AddResource(&mcp.Resource{Name: "fixed", URI: "test://fixed"}, read)
	s.AddResource(&mcp.Resource{Name: "other-scheme", URI: "other://fixed"}, read)
	for i, uri := range []string{"test://items/{id}", "test://paths/{+path}{?q,lang}", "test://lists{/segments*}{?keys*}"} {
		s.AddResourceTemplate(&mcp.ResourceTemplate{Name: fmt.Sprintf("items%d", i), URITemplate: uri}, read)
	}
	return s
}

func newTestProxy(t *testing.T, cfg map[string]config.ServerConfig) (*backend.Manager, string) {
	t.Helper()
	m := backend.NewManager(config.Config{MCPServers: cfg}, nil)
	m.Start(context.Background())
	t.Cleanup(m.Close)
	waitFor(t, 5*time.Second, func() bool { return m.ConnectedCount() == len(cfg) })
	h, err := NewHandler(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return m, front.URL + "/sse"
}

func backendHTTP(t *testing.T, s *mcp.Server) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(h.Close)
	return h
}

func TestDynamicCapabilitiesTenClientsAndRecovery(t *testing.T) {
	flaky, healthy := richBackend(), richBackend()
	var down atomic.Bool
	flakyHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return flaky }, nil)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		flakyHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(first.Close)
	second := backendHTTP(t, healthy)
	manager, endpoint := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "sse", URL: first.URL}, "stable": {Type: "sse", URL: second.URL}})
	var sessions [10]*mcp.ClientSession
	var changed [10]atomic.Int64
	var promptsChanged, resourcesChanged [10]atomic.Int64
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Go(func() {
			c := mcp.NewClient(&mcp.Implementation{Name: "concurrent", Version: "1"}, &mcp.ClientOptions{
				ToolListChangedHandler:     func(context.Context, *mcp.ToolListChangedRequest) { changed[i].Add(1) },
				PromptListChangedHandler:   func(context.Context, *mcp.PromptListChangedRequest) { promptsChanged[i].Add(1) },
				ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) { resourcesChanged[i].Add(1) },
			})
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			t.Cleanup(cancel)
			s, err := c.Connect(ctx, &mcp.SSEClientTransport{Endpoint: endpoint}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			sessions[i] = s
		})
	}
	wg.Wait()
	for _, s := range sessions {
		if s == nil {
			t.Fatal("missing session")
		}
		t.Cleanup(func() { s.Close() })
	}
	for _, s := range sessions {
		wg.Go(func() { assertEchoTool(t, s, "concurrent") })
	}
	wg.Wait()

	checkAll := func(name string, present bool) {
		t.Helper()
		waitFor(t, 8*time.Second, func() bool {
			for _, s := range sessions {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				r, e := s.ListTools(ctx, nil)
				cancel()
				if e != nil {
					return false
				}
				found := false
				for _, tool := range r.Tools {
					if tool.Name == name {
						found = true
					}
				}
				if found != present {
					return false
				}
			}
			return true
		})
	}
	addEcho(flaky, "new")
	checkAll("demo.new", true)
	waitFor(t, 5*time.Second, func() bool {
		for i := range changed {
			if changed[i].Load() == 0 {
				return false
			}
		}
		return true
	})
	for _, s := range sessions {
		r, e := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "demo.new", Arguments: map[string]any{}})
		if e != nil || r.IsError {
			t.Fatalf("new tool: %v %v", r, e)
		}
	}
	// Metadata replacement must notify existing clients and preserve routing.
	flaky.AddPrompt(&mcp.Prompt{Name: "extra"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	flaky.AddResource(&mcp.Resource{Name: "extra", URI: "test://extra"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	waitFor(t, 5*time.Second, func() bool {
		for i := range sessions {
			if promptsChanged[i].Load() == 0 || resourcesChanged[i].Load() == 0 {
				return false
			}
		}
		return true
	})
	var beforeRemove [10]int64
	for i := range changed {
		beforeRemove[i] = changed[i].Load()
	}
	flaky.RemoveTools("new")
	checkAll("demo.new", false)
	waitFor(t, 5*time.Second, func() bool {
		for i := range changed {
			if changed[i].Load() <= beforeRemove[i] {
				return false
			}
		}
		return true
	})
	flaky.RemovePrompts("extra")
	flaky.RemoveResources("test://extra")
	waitFor(t, 5*time.Second, func() bool {
		for _, s := range sessions {
			p, e := s.ListPrompts(context.Background(), nil)
			if e != nil || len(p.Prompts) != 2 {
				return false
			}
			r, e := s.ListResources(context.Background(), nil)
			if e != nil || len(r.Resources) != 4 {
				return false
			}
		}
		return true
	})
	for _, s := range sessions {
		r, e := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "demo.new", Arguments: map[string]any{}})
		if e == nil && !r.IsError {
			t.Fatal("removed tool still callable")
		}
	}

	// Kill all backend transport connections and reject reconnect attempts until recovery.
	down.Store(true)
	first.CloseClientConnections()
	first.Close()
	checkAll("demo.echo", false)
	if manager.ConnectedCount() != 1 {
		t.Fatalf("connected = %d", manager.ConnectedCount())
	}
	for _, s := range sessions {
		wg.Go(func() {
			for range 10 {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				r, e := s.CallTool(ctx, &mcp.CallToolParams{Name: "stable.echo", Arguments: map[string]any{}})
				if e != nil || r.IsError {
					t.Errorf("healthy backend affected: %v %v", r, e)
				}
				r, e = s.CallTool(ctx, &mcp.CallToolParams{Name: "demo.echo", Arguments: map[string]any{}})
				if e == nil && !r.IsError {
					t.Error("offline backend succeeded")
				}
				cancel()
			}
		})
	}
	wg.Wait()
	for _, s := range sessions {
		p, e := s.ListPrompts(context.Background(), nil)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range p.Prompts {
			if strings.HasPrefix(v.Name, "demo.") {
				t.Fatal("offline prompt retained")
			}
		}
		r, e := s.ListResources(context.Background(), nil)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range r.Resources {
			if strings.HasPrefix(v.URI, resourceScheme("demo")) {
				t.Fatal("offline resource retained")
			}
		}
	}
	// Reopen the same address using a fresh backend server. Existing frontends survive.
	recovered := richBackend()
	listener, err := net.Listen("tcp", first.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	restored := httptest.NewUnstartedServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return recovered }, nil))
	restored.Listener.Close()
	restored.Listener = listener
	restored.Start()
	t.Cleanup(func() { manager.Close(); restored.Close() })
	checkAll("demo.echo", true)
	for _, s := range sessions {
		assertEchoTool(t, s, "recovered")
		assertRichCapabilities(t, s)
	}
	// Disconnecting one client must leave the rest and shared connections alive.
	sessions[0].Close()
	assertEchoTool(t, sessions[1], "survives")
}

func assertRichCapabilities(t *testing.T, s *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, e := s.ListPrompts(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	var found bool
	for _, v := range p.Prompts {
		if v.Name == "demo.greet" {
			found = true
			if v.Description != "Say hello" || len(v.Arguments) != 1 || !v.Arguments[0].Required {
				t.Fatalf("prompt metadata: %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("missing recovered prompt")
	}
	r, e := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "demo.greet", Arguments: map[string]string{"who": "world"}})
	if e != nil {
		t.Fatal(e)
	}
	if r.Messages[0].Content.(*mcp.TextContent).Text != "Hello world" {
		t.Fatal(r)
	}
	templates, e := s.ListResourceTemplates(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(templates.ResourceTemplates) != 6 {
		t.Fatalf("templates: %v", templates)
	}
	for _, v := range templates.ResourceTemplates {
		tmpl, e := uritemplate.New(v.URITemplate)
		if e != nil {
			t.Fatal(e)
		}
		uri, e := tmpl.Expand(uritemplate.Values{"id": uritemplate.String("42"), "path": uritemplate.String("a/b"), "q": uritemplate.String("two words"), "lang": uritemplate.String("zh"), "segments": uritemplate.List("a", "b"), "keys": uritemplate.KV("x", "y")})
		if e != nil {
			t.Fatal(e)
		}
		b := "demo"
		if strings.HasPrefix(v.Name, "stable.") {
			b = "stable"
		}
		result, e := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if e != nil {
			t.Fatalf("read %s: %v", uri, e)
		}
		if result.Contents[0].Text != restoreResourceURI(b, uri) || result.Contents[0].URI != uri {
			t.Fatalf("URI route: %+v", result.Contents[0])
		}
	}
	resources, e := s.ListResources(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(resources.Resources) != 4 {
		t.Fatalf("scheme collision: %v", resources)
	}
}

func TestQualifiedNamesAndURIIdentity(t *testing.T) {
	inputs := []string{"echo", "a b", "a?b", "a_b", strings.Repeat("x", 200), strings.Repeat("x", 199) + "y", "你好", "", "foo--bar"}
	used := map[string]string{}
	names := map[string]string{}
	for _, v := range inputs {
		n := qualifiedName("demo", v, used)
		if !validToolName.MatchString(n) {
			t.Fatalf("invalid: %q", n)
		}
		names[v] = n
	}
	for i := len(inputs) - 1; i >= 0; i-- {
		v := inputs[i]
		if n := qualifiedName("demo", v, map[string]string{}); n != names[v] {
			t.Fatalf("unstable name: %s %s", n, names[v])
		}
	}
	for _, uri := range []string{"test://same", "other://same", "urn:example:{id}", "https://host/{+path}{?q}"} {
		proxy := resourceURI("demo", uri)
		if restoreResourceURI("demo", proxy) != uri {
			t.Fatal(uri)
		}
	}
}

func watcherCount() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "/proxy.watchSession(")
}
func TestFrontendWatchersReleased(t *testing.T) {
	h := backendHTTP(t, richBackend())
	_, endpoint := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "sse", URL: h.URL}})
	baseline := watcherCount()
	for range 20 {
		s := connectClient(t, endpoint)
		t.Cleanup(func() { s.Close() })
		assertEchoTool(t, s, "cleanup")
		s.Close()
	}
	waitFor(t, 5*time.Second, func() bool { return watcherCount() == baseline })
}

func TestStreamableBackendDynamicTemplates(t *testing.T) {
	b := richBackend()
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return b }, nil))
	t.Cleanup(h.Close)
	_, endpoint := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "streamable-http", URL: h.URL}})
	s := connectClient(t, endpoint)
	t.Cleanup(func() { s.Close() })
	template := &mcp.ResourceTemplate{Name: "dynamic", URITemplate: "urn:dynamic:{id}"}
	b.AddResourceTemplate(template, func(_ context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: r.Params.URI, Text: r.Params.URI}}}, nil
	})
	waitFor(t, 5*time.Second, func() bool {
		r, e := s.ListResourceTemplates(context.Background(), nil)
		return e == nil && len(r.ResourceTemplates) == 4
	})
	result, e := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: resourceURI("demo", "urn:dynamic:42")})
	if e != nil {
		t.Fatal(e)
	}
	if result.Contents[0].Text != "urn:dynamic:42" {
		t.Fatal(result)
	}
	b.RemoveResourceTemplates(template.URITemplate)
	waitFor(t, 5*time.Second, func() bool {
		r, e := s.ListResourceTemplates(context.Background(), nil)
		return e == nil && len(r.ResourceTemplates) == 3
	})
	if _, e := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: resourceURI("demo", "urn:dynamic:42")}); e == nil {
		t.Fatal("removed template still readable")
	}
}

func TestNormalizedToolsRemainCallable(t *testing.T) {
	b := mcp.NewServer(&mcp.Implementation{Name: "names", Version: "1"}, nil)
	originals := []string{"a b", "a?b", "a_b", strings.Repeat("x", 200), strings.Repeat("x", 199) + "y", "你好"}
	for _, name := range originals {
		b.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.Params.Name}}}, nil
		})
	}
	h := backendHTTP(t, b)
	_, endpoint := newTestProxy(t, map[string]config.ServerConfig{"demo": {Type: "sse", URL: h.URL}})
	s := connectClient(t, endpoint)
	t.Cleanup(func() { s.Close() })
	tools, e := s.ListTools(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(tools.Tools) != len(originals) {
		t.Fatalf("tools lost: %v", tools)
	}
	for _, name := range originals {
		proxyName := qualifiedName("demo", name, map[string]string{})
		r, e := s.CallTool(context.Background(), &mcp.CallToolParams{Name: proxyName, Arguments: map[string]any{}})
		if e != nil {
			t.Fatal(e)
		}
		if r.IsError || r.Content[0].(*mcp.TextContent).Text != name {
			t.Fatalf("wrong original tool route %q: %+v", name, r)
		}
	}
}
