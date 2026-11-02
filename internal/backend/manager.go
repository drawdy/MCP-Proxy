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
	"reflect"
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
	Name              string
	Backend           *Backend
	Tools             []*mcp.Tool
	Prompts           []*mcp.Prompt
	Resources         []*mcp.Resource
	ResourceTemplates []*mcp.ResourceTemplate
}

type Manager struct {
	backends map[string]*Backend
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	watchersMu sync.Mutex
	watchers   map[chan struct{}]struct{}
}

type Backend struct {
	name    string
	config  config.ServerConfig
	logger  *slog.Logger
	manager *Manager

	mu        sync.RWMutex
	session   *mcp.ClientSession
	tools     []*mcp.Tool
	prompts   []*mcp.Prompt
	resources []*mcp.Resource
	templates []*mcp.ResourceTemplate
}

type discoveredCapabilities struct {
	tools     []*mcp.Tool
	prompts   []*mcp.Prompt
	resources []*mcp.Resource
	templates []*mcp.ResourceTemplate

	toolsOK     bool
	promptsOK   bool
	resourcesOK bool
	templatesOK bool
}

func (c discoveredCapabilities) complete() bool {
	return c.toolsOK && c.promptsOK && c.resourcesOK && c.templatesOK
}

func NewManager(cfg config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		backends: make(map[string]*Backend, len(cfg.MCPServers)),
		watchers: make(map[chan struct{}]struct{}),
	}
	for name, serverConfig := range cfg.MCPServers {
		manager.backends[name] = &Backend{
			name:    name,
			config:  serverConfig,
			logger:  logger.With("backend", name),
			manager: manager,
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

// SubscribeChanges returns a coalescing notification channel. Consumers should
// fetch a fresh Snapshot after receiving a notification rather than assuming
// that every intermediate state is represented by one event.
func (m *Manager) SubscribeChanges() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.watchersMu.Lock()
	m.watchers[ch] = struct{}{}
	m.watchersMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.watchersMu.Lock()
			delete(m.watchers, ch)
			m.watchersMu.Unlock()
		})
	}
	return ch, cancel
}

func (m *Manager) notifyChange() {
	m.watchersMu.Lock()
	defer m.watchersMu.Unlock()
	for ch := range m.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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
				Name:              name,
				Backend:           backend,
				Tools:             append([]*mcp.Tool(nil), backend.tools...),
				Prompts:           append([]*mcp.Prompt(nil), backend.prompts...),
				Resources:         append([]*mcp.Resource(nil), backend.resources...),
				ResourceTemplates: append([]*mcp.ResourceTemplate(nil), backend.templates...),
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

		changes := make(chan struct{}, 1)
		session, sessionCancel, err := b.connect(ctx, changes)
		if err != nil {
			b.logger.Warn("backend connection failed; retrying", "error", err, "retry_in", delay)
			if !wait(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}

		capabilities := b.discover(ctx, session)
		b.mu.Lock()
		b.session = session
		b.tools = capabilities.tools
		b.prompts = capabilities.prompts
		b.resources = capabilities.resources
		b.templates = capabilities.templates
		b.mu.Unlock()
		b.manager.notifyChange()

		b.logger.Info("backend connected",
			"tools", len(capabilities.tools),
			"prompts", len(capabilities.prompts),
			"resources", len(capabilities.resources),
			"resource_templates", len(capabilities.templates))
		delay = time.Second

		waitResult := make(chan error, 1)
		go func() { waitResult <- session.Wait() }()

		retryDiscovery := !capabilities.complete()
		retryTimer := time.NewTicker(time.Second)
	connected:
		for {
			select {
			case <-changes:
				retryDiscovery = !b.refreshCapabilities(ctx, session)
				continue
			case <-retryTimer.C:
				if retryDiscovery {
					retryDiscovery = !b.refreshCapabilities(ctx, session)
				}
				continue
			case <-ctx.Done():
				retryTimer.Stop()
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

			break connected
		}

		retryTimer.Stop()
		if !wait(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (b *Backend) connect(parent context.Context, changes chan<- struct{}) (*mcp.ClientSession, context.CancelFunc, error) {
	sessionCtx, cancel := context.WithCancel(parent)
	notify := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcp-proxy", Version: version.Version},
		&mcp.ClientOptions{
			ToolListChangedHandler:     func(context.Context, *mcp.ToolListChangedRequest) { notify() },
			PromptListChangedHandler:   func(context.Context, *mcp.PromptListChangedRequest) { notify() },
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) { notify() },
		},
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
		// Keep sessionCtx alive for the lifetime of the MCP session.
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

func (b *Backend) discover(parent context.Context, session *mcp.ClientSession) discoveredCapabilities {
	ctx, cancel := context.WithTimeout(parent, discoveryTimeout)
	defer cancel()

	var result discoveredCapabilities
	caps := session.InitializeResult().Capabilities
	if caps == nil {
		caps = &mcp.ServerCapabilities{}
	}
	result.toolsOK = true
	if caps.Tools != nil {
		for item, err := range session.Tools(ctx, nil) {
			if err != nil {
				result.toolsOK = false
				b.logger.Warn("could not list backend tools", "error", err)
				break
			}
			if item != nil {
				result.tools = append(result.tools, item)
			}
		}
	}

	result.promptsOK = true
	if caps.Prompts != nil {
		for item, err := range session.Prompts(ctx, nil) {
			if err != nil {
				result.promptsOK = false
				b.logger.Warn("could not list backend prompts", "error", err)
				break
			}
			if item != nil {
				result.prompts = append(result.prompts, item)
			}
		}
	}

	result.resourcesOK = true
	if caps.Resources != nil {
		for item, err := range session.Resources(ctx, nil) {
			if err != nil {
				result.resourcesOK = false
				b.logger.Warn("could not list backend resources", "error", err)
				break
			}
			if item != nil {
				result.resources = append(result.resources, item)
			}
		}
	}

	result.templatesOK = true
	if caps.Resources != nil {
		for item, err := range session.ResourceTemplates(ctx, nil) {
			if err != nil {
				result.templatesOK = false
				b.logger.Warn("could not list backend resource templates", "error", err)
				break
			}
			if item != nil {
				result.templates = append(result.templates, item)
			}
		}
	}
	if !result.toolsOK {
		result.tools = nil
	}
	if !result.promptsOK {
		result.prompts = nil
	}
	if !result.resourcesOK {
		result.resources = nil
	}
	if !result.templatesOK {
		result.templates = nil
	}
	sort.Slice(result.tools, func(i, j int) bool { return result.tools[i].Name < result.tools[j].Name })
	sort.Slice(result.prompts, func(i, j int) bool { return result.prompts[i].Name < result.prompts[j].Name })
	sort.Slice(result.resources, func(i, j int) bool { return result.resources[i].URI < result.resources[j].URI })
	sort.Slice(result.templates, func(i, j int) bool { return result.templates[i].URITemplate < result.templates[j].URITemplate })
	return result
}

func (b *Backend) refreshCapabilities(parent context.Context, session *mcp.ClientSession) bool {
	b.mu.RLock()
	current := b.session == session
	b.mu.RUnlock()
	if !current {
		return true
	}

	discovered := b.discover(parent, session)

	b.mu.Lock()
	if b.session != session {
		b.mu.Unlock()
		return true
	}
	changed := false
	if discovered.toolsOK && !reflect.DeepEqual(b.tools, discovered.tools) {
		b.tools = discovered.tools
		changed = true
	}
	if discovered.promptsOK && !reflect.DeepEqual(b.prompts, discovered.prompts) {
		b.prompts = discovered.prompts
		changed = true
	}
	if discovered.resourcesOK && !reflect.DeepEqual(b.resources, discovered.resources) {
		b.resources = discovered.resources
		changed = true
	}
	if discovered.templatesOK && !reflect.DeepEqual(b.templates, discovered.templates) {
		b.templates = discovered.templates
		changed = true
	}
	b.mu.Unlock()

	if changed {
		b.logger.Info("backend capabilities updated")
		b.manager.notifyChange()
	}
	return discovered.complete()
}

func (b *Backend) clear(session *mcp.ClientSession) {
	b.mu.Lock()
	changed := b.session == session
	if changed {
		b.session = nil
		b.tools = nil
		b.prompts = nil
		b.resources = nil
		b.templates = nil
	}
	b.mu.Unlock()
	if changed {
		b.manager.notifyChange()
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

func (b *Backend) GetPrompt(ctx context.Context, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	b.mu.RLock()
	session := b.session
	b.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("backend %q is not connected", b.name)
	}
	return session.GetPrompt(ctx, params)
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
