package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/drawdy/MCP-Proxy/internal/config"
	"github.com/drawdy/MCP-Proxy/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	connectTimeout    = 30 * time.Second
	discoveryTimeout  = 15 * time.Second
	maxReconnectDelay = 30 * time.Second
)

type Snapshot struct {
	Name      string
	Backend   *Backend
	Tools     []*mcp.Tool
	Resources []*mcp.Resource
}

type Manager struct {
	backends map[string]*Backend
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type Backend struct {
	name   string
	config config.ServerConfig
	logger *slog.Logger

	mu        sync.RWMutex
	session   *mcp.ClientSession
	tools     []*mcp.Tool
	resources []*mcp.Resource
}

func NewManager(cfg config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{backends: make(map[string]*Backend, len(cfg.MCPServers))}
	for name, serverConfig := range cfg.MCPServers {
		manager.backends[name] = &Backend{
			name:   name,
			config: serverConfig,
			logger: logger.With("backend", name),
		}
	}
	return manager
}

func (m *Manager) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	for _, backend := range m.backends {
		m.wg.Add(1)
		go func(b *Backend) {
			defer m.wg.Done()
			b.run(ctx)
		}(backend)
	}
}

func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

func (m *Manager) ConfiguredCount() int { return len(m.backends) }

func (m *Manager) ConnectedCount() int {
	count := 0
	for _, backend := range m.backends {
		if backend.isConnected() {
			count++
		}
	}
	return count
}

func (m *Manager) Snapshot() []Snapshot {
	names := make([]string, 0, len(m.backends))
	for name := range m.backends {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]Snapshot, 0, len(names))
	for _, name := range names {
		backend := m.backends[name]
		backend.mu.RLock()
		if backend.session != nil {
			result = append(result, Snapshot{
				Name:      name,
				Backend:   backend,
				Tools:     append([]*mcp.Tool(nil), backend.tools...),
				Resources: append([]*mcp.Resource(nil), backend.resources...),
			})
		}
		backend.mu.RUnlock()
	}
	return result
}

func (b *Backend) isConnected() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.session != nil
}

func (b *Backend) run(ctx context.Context) {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		session, sessionCancel, err := b.connect(ctx)
		if err != nil {
			b.logger.Warn("backend connection failed; retrying", "error", err, "retry_in", delay)
			if !wait(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}

		tools, resources := b.discover(ctx, session)
		b.mu.Lock()
		b.session = session
		b.tools = tools
		b.resources = resources
		b.mu.Unlock()

		b.logger.Info("backend connected", "tools", len(tools), "resources", len(resources))
		delay = time.Second

		waitResult := make(chan error, 1)
		go func() { waitResult <- session.Wait() }()

		select {
		case <-ctx.Done():
			_ = session.Close()
			<-waitResult
			sessionCancel()
			b.clear(session)
			return
		case waitErr := <-waitResult:
			sessionCancel()
			b.clear(session)
			if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
				b.logger.Warn("backend disconnected; retrying", "error", waitErr, "retry_in", delay)
			} else {
				b.logger.Info("backend disconnected; retrying", "retry_in", delay)
			}
		}

		if !wait(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (b *Backend) connect(parent context.Context) (*mcp.ClientSession, context.CancelFunc, error) {
	sessionCtx, cancel := context.WithCancel(parent)
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcp-proxy", Version: version.Version},
		&mcp.ClientOptions{},
	)
	transport, err := b.newTransport()
	if err != nil {
		cancel()
		return nil, nil, err
	}

	type connectResult struct {
		session *mcp.ClientSession
		err     error
	}
	resultCh := make(chan connectResult, 1)
	go func() {
		session, connectErr := client.Connect(sessionCtx, transport, nil)
		resultCh <- connectResult{session: session, err: connectErr}
	}()

	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			cancel()
			return nil, nil, fmt.Errorf("connect: %w", result.err)
		}
		// Keep sessionCtx alive for the lifetime of the MCP session. Cancelling
		// a temporary handshake context here would immediately close the
		// transport after Client.Connect returned successfully.
		return result.session, cancel, nil
	case <-timer.C:
		cancel()
		return nil, nil, fmt.Errorf("connect timed out after %s", connectTimeout)
	case <-parent.Done():
		cancel()
		return nil, nil, parent.Err()
	}
}

func (b *Backend) newTransport() (mcp.Transport, error) {
	switch b.config.BackendType() {
	case "stdio":
		cmd := exec.Command(b.config.Command, b.config.Args...)
		cmd.Dir = b.config.CWD
		cmd.Env = mergeEnvironment(os.Environ(), b.config.Env)
		cmd.Stderr = os.Stderr
		return &mcp.CommandTransport{Command: cmd}, nil
	case "sse":
		return &mcp.SSEClientTransport{Endpoint: b.config.URL, HTTPClient: newHTTPClient(b.config.Headers)}, nil
	case "streamable-http":
		return &mcp.StreamableClientTransport{Endpoint: b.config.URL, HTTPClient: newHTTPClient(b.config.Headers)}, nil
	default:
		return nil, fmt.Errorf("unsupported backend type %q", b.config.BackendType())
	}
}

func (b *Backend) discover(parent context.Context, session *mcp.ClientSession) ([]*mcp.Tool, []*mcp.Resource) {
	ctx, cancel := context.WithTimeout(parent, discoveryTimeout)
	defer cancel()

	var tools []*mcp.Tool
	if result, err := session.ListTools(ctx, nil); err != nil {
		b.logger.Warn("could not list backend tools", "error", err)
	} else {
		tools = result.Tools
	}

	var resources []*mcp.Resource
	if result, err := session.ListResources(ctx, nil); err != nil {
		b.logger.Debug("backend does not expose resources or listing failed", "error", err)
	} else {
		resources = result.Resources
	}
	return tools, resources
}

func (b *Backend) clear(session *mcp.ClientSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == session {
		b.session = nil
		b.tools = nil
		b.resources = nil
	}
}

func (b *Backend) CallTool(ctx context.Context, name string, arguments any) (*mcp.CallToolResult, error) {
	b.mu.RLock()
	session := b.session
	b.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("backend %q is not connected", b.name)
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
}

func (b *Backend) ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error) {
	b.mu.RLock()
	session := b.session
	b.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("backend %q is not connected", b.name)
	}
	return session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for key, value := range t.headers {
		clone.Header.Set(key, value)
	}
	return t.base.RoundTrip(clone)
}

func newHTTPClient(headers map[string]string) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Transport: &headerTransport{base: base, headers: headers}}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}
