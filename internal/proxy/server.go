package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

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
	for _, endpoint := range options.HealthEndpoints {
		path := endpoint
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("health endpoint %q must start with /", path)
		}
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
		return newSessionServer(manager.Snapshot())
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

func newSessionServer(snapshots []backend.Snapshot) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mcp-proxy", Version: version.Version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}},
	)

	for _, snapshot := range snapshots {
		for _, backendTool := range snapshot.Tools {
			if backendTool == nil {
				continue
			}
			tool := *backendTool
			originalName := tool.Name
			tool.Name = snapshot.Name + "." + originalName
			if !validToolName.MatchString(tool.Name) {
				continue
			}
			if tool.InputSchema == nil {
				tool.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			backendRef := snapshot.Backend
			server.AddTool(&tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := backendRef.CallTool(ctx, originalName, request.Params.Arguments)
				if err != nil {
					var toolError mcp.CallToolResult
					toolError.SetError(err)
					return &toolError, nil
				}
				return result, nil
			})
		}

		for _, backendResource := range snapshot.Resources {
			if backendResource == nil {
				continue
			}
			resource := *backendResource
			originalURI := resource.URI
			resource.Name = snapshot.Name + "." + resource.Name
			resource.URI = resourceURI(snapshot.Name, originalURI)
			backendRef := snapshot.Backend
			server.AddResource(&resource, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return backendRef.ReadResource(ctx, originalURI)
			})
		}
	}
	return server
}

func resourceURI(backendName, originalURI string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(backendName))
	uri := base64.RawURLEncoding.EncodeToString([]byte(originalURI))
	return "mcp-proxy://resource/" + name + "/" + uri
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
