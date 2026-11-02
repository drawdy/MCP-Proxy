package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiscoveryPreservesFailedClassesAndRejectsStaleSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	add := func(name string) {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	}
	add("one")
	var fail atomic.Bool
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "tools/list" && fail.Load() {
				return nil, errors.New("temporary discovery failure")
			}
			return next(ctx, method, r)
		}
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, e := s.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, e := c.Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	m := NewManager(config.Config{MCPServers: map[string]config.ServerConfig{"demo": {}}}, nil)
	b := m.backends["demo"]
	b.session = cs
	if !b.refreshCapabilities(ctx, cs) || len(b.tools) != 1 {
		t.Fatal("initial discovery")
	}
	changes, unsub := m.SubscribeChanges()
	defer unsub()
	// Identical rediscovery must not publish a change.
	b.refreshCapabilities(ctx, cs)
	select {
	case <-changes:
		t.Fatal("spurious notification")
	default:
	}
	add("two")
	fail.Store(true)
	if b.refreshCapabilities(ctx, cs) {
		t.Fatal("expected discovery failure")
	}
	if len(b.tools) != 1 || b.tools[0].Name != "one" {
		t.Fatal("failed discovery cleared tools")
	}
	select {
	case <-changes:
		t.Fatal("failure published empty state")
	default:
	}
	fail.Store(false)
	if !b.refreshCapabilities(ctx, cs) || len(b.tools) != 2 {
		t.Fatal("retry did not recover")
	}
	select {
	case <-changes:
	default:
		t.Fatal("missing recovery notification")
	}
	b.mu.Lock()
	b.session = nil
	b.mu.Unlock()
	b.refreshCapabilities(ctx, cs)
	if len(b.tools) != 2 {
		t.Fatal("stale session mutated capabilities")
	}
	b.clear(cs)
	if len(b.tools) != 2 {
		t.Fatal("stale clear mutated capabilities")
	}
	unsub()
	unsub()
	m.watchersMu.Lock()
	defer m.watchersMu.Unlock()
	if len(m.watchers) != 0 {
		t.Fatal("subscriber leak")
	}
}

func TestInitialDiscoveryRetriesAndRetainsNotifications(t *testing.T) {
	for _, mode := range []string{"failure", "notification"} {
		t.Run(mode, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: "initial", Version: "1"}, nil)
			add := func(name string) {
				s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{}, nil
				})
			}
			add("one")
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
					if method != "tools/list" || calls.Add(1) != 1 {
						return next(ctx, method, r)
					}
					if mode == "failure" {
						return nil, errors.New("first list failed")
					}
					result, err := next(ctx, method, r)
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return result, err
				}
			})
			h := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil))
			defer h.Close()
			m := NewManager(config.Config{MCPServers: map[string]config.ServerConfig{"demo": {Type: "sse", URL: h.URL}}}, nil)
			m.Start(context.Background())
			defer m.Close()
			if mode == "notification" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("discovery never started")
				}
				add("two")
				close(release)
			}
			want := 1
			if mode == "notification" {
				want = 2
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				snap := m.Snapshot()
				if len(snap) == 1 && len(snap[0].Tools) == want {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("initial discovery did not converge")
		})
	}
}
