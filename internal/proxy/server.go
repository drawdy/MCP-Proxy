package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/drawdy/MCP-Proxy/internal/backend"
	"github.com/drawdy/MCP-Proxy/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	SSEPath         string
	HealthEndpoints []string
	CORSOrigins     []string
}

var validToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type sessionRegistry struct {
	server *mcp.Server
	mu     sync.Mutex

	tools     map[string]string
	prompts   map[string]string
	resources map[string]string
	templates map[string]string
}

type desiredTool struct {
	tool         *mcp.Tool
	backend      *backend.Backend
	originalName string
}

type desiredPrompt struct {
	prompt       *mcp.Prompt
	backend      *backend.Backend
	originalName string
}

type desiredResource struct {
	resource    *mcp.Resource
	backend     *backend.Backend
	backendName string
}

type desiredTemplate struct {
	template    *mcp.ResourceTemplate
	backend     *backend.Backend
	backendName string
}

func NewHandler(manager *backend.Manager, options Options) (http.Handler, error) {
	if manager == nil {
		return nil, fmt.Errorf("backend manager is required")
	}
	if options.SSEPath == "" {
		options.SSEPath = "/sse"
	}
	if !strings.HasPrefix(options.SSEPath, "/") {
		return nil, fmt.Errorf("SSE path must start with /")
	}
	if len(options.HealthEndpoints) == 0 {
		options.HealthEndpoints = []string{"/healthz", "/readyz"}
	}

	mux := http.NewServeMux()
	seenHealthEndpoints := make(map[string]struct{}, len(options.HealthEndpoints))
	for _, endpoint := range options.HealthEndpoints {
		path := endpoint
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("health endpoint %q must start with /", path)
		}
		if _, exists := seenHealthEndpoints[path]; exists {
			continue
		}
		seenHealthEndpoints[path] = struct{}{}
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if path == "/readyz" && manager.ConfiguredCount() > 0 && manager.ConnectedCount() == 0 {
				http.Error(w, "no backend is connected", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("ok"))
			}
		})
	}

	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
		return newSessionServer(manager)
	}, &mcp.SSEOptions{MaxRequestBodyBytes: 4 << 20})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != options.SSEPath && r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		sseHandler.ServeHTTP(w, r)
	}))

	return corsMiddleware(mux, options.CORSOrigins), nil
}

func newSessionServer(manager *backend.Manager) *mcp.Server {
	registry := &sessionRegistry{
		tools:     make(map[string]string),
		prompts:   make(map[string]string),
		resources: make(map[string]string),
		templates: make(map[string]string),
	}
	var initialized sync.Once
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mcp-proxy", Version: version.Version},
		&mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{ListChanged: true},
				Prompts:   &mcp.PromptCapabilities{ListChanged: true},
				Resources: &mcp.ResourceCapabilities{ListChanged: true},
			},
			InitializedHandler: func(_ context.Context, request *mcp.InitializedRequest) {
				if request == nil || request.Session == nil {
					return
				}
				initialized.Do(func() {
					changes, unsubscribe := manager.SubscribeChanges()
					registry.sync(manager.Snapshot())
					go watchSession(request.Session, changes, unsubscribe, registry, manager)
				})
			},
		},
	)
	registry.server = server
	registry.sync(manager.Snapshot())
	return server
}

func watchSession(
	session *mcp.ServerSession,
	changes <-chan struct{},
	unsubscribe func(),
	registry *sessionRegistry,
	manager *backend.Manager,
) {
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		case <-changes:
			registry.sync(manager.Snapshot())
		}
	}
}

func (r *sessionRegistry) sync(snapshots []backend.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()

	desiredTools := make(map[string]desiredTool)
	desiredPrompts := make(map[string]desiredPrompt)
	desiredResources := make(map[string]desiredResource)
	desiredTemplates := make(map[string]desiredTemplate)
	usedTools := make(map[string]string)
	usedPrompts := make(map[string]string)

	for _, snapshot := range snapshots {
		if snapshot.Backend == nil {
			continue
		}
		for _, backendTool := range snapshot.Tools {
			if backendTool == nil {
				continue
			}
			name := qualifiedName(snapshot.Name, backendTool.Name, usedTools)
			if name == "" {
				continue
			}
			tool := *backendTool
			tool.Name = name
			if tool.InputSchema == nil {
				tool.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			desiredTools[name] = desiredTool{tool: &tool, backend: snapshot.Backend, originalName: backendTool.Name}
		}

		for _, backendPrompt := range snapshot.Prompts {
			if backendPrompt == nil {
				continue
			}
			name := qualifiedName(snapshot.Name, backendPrompt.Name, usedPrompts)
			if name == "" {
				continue
			}
			prompt := *backendPrompt
			prompt.Name = name
			desiredPrompts[name] = desiredPrompt{prompt: &prompt, backend: snapshot.Backend, originalName: backendPrompt.Name}
		}

		for _, backendResource := range snapshot.Resources {
			if backendResource == nil {
				continue
			}
			uri := resourceURI(snapshot.Name, backendResource.URI)
			if uri == "" {
				slog.Error("invalid backend resource URI", "backend", snapshot.Name, "uri", backendResource.URI)
				continue
			}
			resource := *backendResource
			resource.Name = snapshot.Name + "." + backendResource.Name
			resource.URI = uri
			desiredResources[uri] = desiredResource{
				resource:    &resource,
				backend:     snapshot.Backend,
				backendName: snapshot.Name,
			}
		}

		for _, backendTemplate := range snapshot.ResourceTemplates {
			if backendTemplate == nil {
				continue
			}
			uriTemplate := resourceURI(snapshot.Name, backendTemplate.URITemplate)
			if uriTemplate == "" {
				slog.Error("invalid backend resource template", "backend", snapshot.Name, "uri", backendTemplate.URITemplate)
				continue
			}
			template := *backendTemplate
			template.Name = snapshot.Name + "." + backendTemplate.Name
			template.URITemplate = uriTemplate
			desiredTemplates[uriTemplate] = desiredTemplate{
				template:    &template,
				backend:     snapshot.Backend,
				backendName: snapshot.Name,
			}
		}
	}

	r.removeMissingTools(desiredTools)
	r.removeMissingPrompts(desiredPrompts)
	r.removeMissingResources(desiredResources)
	r.removeMissingTemplates(desiredTemplates)

	for _, name := range sortedKeys(desiredTools) {
		item := desiredTools[name]
		fingerprint := fingerprint(item.tool)
		if r.tools[name] == fingerprint {
			continue
		}
		if err := addTool(r.server, item.tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := item.backend.CallTool(ctx, item.originalName, request.Params.Arguments)
			if err != nil {
				var toolError mcp.CallToolResult
				toolError.SetError(err)
				return &toolError, nil
			}
			return result, nil
		}); err != nil {
			slog.Error("capability registration failed", "error", err)
			continue
		}
		r.tools[name] = fingerprint
	}

	for _, name := range sortedKeys(desiredPrompts) {
		item := desiredPrompts[name]
		fingerprint := fingerprint(item.prompt)
		if r.prompts[name] == fingerprint {
			continue
		}
		r.server.AddPrompt(item.prompt, func(ctx context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			if request == nil || request.Params == nil {
				return nil, fmt.Errorf("prompt parameters are required")
			}
			params := *request.Params
			params.Name = item.originalName
			return item.backend.GetPrompt(ctx, &params)
		})
		r.prompts[name] = fingerprint
	}

	for _, uri := range sortedKeys(desiredResources) {
		item := desiredResources[uri]
		fingerprint := fingerprint(item.resource)
		if r.resources[uri] == fingerprint {
			continue
		}
		if err := addResource(r.server, item.resource, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			originalURI := restoreResourceURI(item.backendName, request.Params.URI)
			return readResource(ctx, item.backend, item.backendName, originalURI)
		}); err != nil {
			slog.Error("capability registration failed", "error", err)
			continue
		}
		r.resources[uri] = fingerprint
	}

	for _, uriTemplate := range sortedKeys(desiredTemplates) {
		item := desiredTemplates[uriTemplate]
		fingerprint := fingerprint(item.template)
		if r.templates[uriTemplate] == fingerprint {
			continue
		}
		if err := addResourceTemplate(r.server, item.template, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			originalURI := restoreResourceURI(item.backendName, request.Params.URI)
			return readResource(ctx, item.backend, item.backendName, originalURI)
		}); err != nil {
			slog.Error("capability registration failed", "error", err)
			continue
		}
		r.templates[uriTemplate] = fingerprint
	}
}

func (r *sessionRegistry) removeMissingTools(desired map[string]desiredTool) {
	var removed []string
	for name := range r.tools {
		if _, exists := desired[name]; !exists {
			removed = append(removed, name)
			delete(r.tools, name)
		}
	}
	if len(removed) > 0 {
		r.server.RemoveTools(removed...)
	}
}

func (r *sessionRegistry) removeMissingPrompts(desired map[string]desiredPrompt) {
	var removed []string
	for name := range r.prompts {
		if _, exists := desired[name]; !exists {
			removed = append(removed, name)
			delete(r.prompts, name)
		}
	}
	if len(removed) > 0 {
		r.server.RemovePrompts(removed...)
	}
}

func (r *sessionRegistry) removeMissingResources(desired map[string]desiredResource) {
	var removed []string
	for uri := range r.resources {
		if _, exists := desired[uri]; !exists {
			removed = append(removed, uri)
			delete(r.resources, uri)
		}
	}
	if len(removed) > 0 {
		r.server.RemoveResources(removed...)
	}
}

func (r *sessionRegistry) removeMissingTemplates(desired map[string]desiredTemplate) {
	var removed []string
	for uri := range r.templates {
		if _, exists := desired[uri]; !exists {
			removed = append(removed, uri)
			delete(r.templates, uri)
		}
	}
	if len(removed) > 0 {
		r.server.RemoveResourceTemplates(removed...)
	}
}

func addTool(server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register tool %q: %v", tool.Name, recovered)
		}
	}()
	if !validToolName.MatchString(tool.Name) {
		return fmt.Errorf("invalid tool name %q", tool.Name)
	}
	server.AddTool(tool, handler)
	return nil
}

func addResource(server *mcp.Server, resource *mcp.Resource, handler mcp.ResourceHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register resource %q: %v", resource.URI, recovered)
		}
	}()
	parsed, parseErr := url.Parse(resource.URI)
	if parseErr != nil || parsed.Scheme == "" {
		return fmt.Errorf("resource URI %q must be absolute", resource.URI)
	}
	server.AddResource(resource, handler)
	return nil
}

func addResourceTemplate(server *mcp.Server, template *mcp.ResourceTemplate, handler mcp.ResourceHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register resource template %q: %v", template.URITemplate, recovered)
		}
	}()
	server.AddResourceTemplate(template, handler)
	return nil
}

// Names that require rewriting always carry a digest of the full identity.
// Reserve that suffix shape for rewritten names so adding/removing a colliding
// capability cannot rename an existing one.
func qualifiedName(backendName, originalName string, used map[string]string) string {
	identity := backendName + "\x00" + originalName
	raw := backendName + "." + originalName
	candidate := raw
	if !validToolName.MatchString(raw) || strings.Contains(raw, "--") {
		clean := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
				return r
			}
			return '_'
		}, raw)
		sum := sha256.Sum256([]byte(identity))
		suffix := "--" + hex.EncodeToString(sum[:])
		if len(clean) > 128-len(suffix) {
			clean = clean[:128-len(suffix)]
		}
		candidate = clean + suffix
	}
	if previous, exists := used[candidate]; exists {
		slog.Error("duplicate capability name", "name", candidate, "identity", identity, "previous", previous)
		return ""
	}
	used[candidate] = identity
	return candidate
}

// Keep the entire original URI after an opaque, backend-specific scheme. This
// preserves every RFC 6570 expression, including reserved/query expansions.
func resourceURI(backendName, originalURI string) string {
	if !strings.Contains(originalURI, ":") {
		return ""
	}
	return resourceScheme(backendName) + ":" + originalURI
}

func restoreResourceURI(backendName, namespacedURI string) string {
	return strings.TrimPrefix(namespacedURI, resourceScheme(backendName)+":")
}

func resourceScheme(backendName string) string {
	return "mcp-proxy-" + hex.EncodeToString([]byte(backendName))
}

func readResource(ctx context.Context, b *backend.Backend, name, uri string) (*mcp.ReadResourceResult, error) {
	result, err := b.ReadResource(ctx, uri)
	if err != nil {
		return nil, err
	}
	for _, content := range result.Contents {
		if content != nil {
			content.URI = resourceURI(name, content.URI)
		}
	}
	return result, nil
}

func fingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func corsMiddleware(next http.Handler, allowedOrigins []string) http.Handler {
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"*"}
	}
	allowAll := false
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAll = true
		}
		allowed[origin] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, Last-Event-ID, Mcp-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, Last-Event-ID")
		w.Header().Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
