// Package mcp implements Model Context Protocol (MCP) client connections over stdio, Streamable HTTP, and SSE.
package mcp

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Starchaser1024/LoomCode/internal/tools"
)

const (
	// DefaultConnectTimeout bounds MCP TCP/TLS connection establishment, HTTP response headers,
	// and JSON-RPC initialize handshakes when ServerConfig.TimeoutSec is unset.
	DefaultConnectTimeout = 15 * time.Second

	maxStderrCaptureBytes = 4096
	stdioTerminateGrace   = 500 * time.Millisecond
)

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9_]`)

type ServerConfig struct {
	Name       string            `yaml:"name"`
	Command    string            `yaml:"command"`
	Args       []string          `yaml:"args"`
	URL        string            `yaml:"url"`
	Transport  string            `yaml:"transport"`
	Headers    map[string]string `yaml:"headers"`
	Env        map[string]string `yaml:"env"`
	TimeoutSec int               `yaml:"timeout_sec"`

	// connectTimeoutOverride is used by unit tests to exercise sub-second timeouts.
	connectTimeoutOverride time.Duration
}

func (c *ServerConfig) IsStdio() bool {
	return c.Command != ""
}

func (c *ServerConfig) connectTimeout() time.Duration {
	if c.connectTimeoutOverride > 0 {
		return c.connectTimeoutOverride
	}
	if c.TimeoutSec > 0 {
		return time.Duration(c.TimeoutSec) * time.Second
	}
	return DefaultConnectTimeout
}

// transportKind picks the HTTP transport variant. Empty/"http"/"streamable" →
// Streamable HTTP (2025-03-26 spec); "sse" → legacy SSE (2024-11-05 spec).
func (c *ServerConfig) transportKind() string {
	switch strings.ToLower(c.Transport) {
	case "sse":
		return "sse"
	default:
		return "http"
	}
}

// headerRoundTripper injects fixed headers onto every outgoing request.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, os.ExpandEnv(v))
	}
	return h.base.RoundTrip(clone)
}

// newHTTPClient builds an HTTP client with bounded dial, TLS, and response-header
// timeouts while leaving Client.Timeout unset (0) so long-lived SSE / Streamable HTTP
// event streams are not prematurely aborted after handshake completes.
func newHTTPClient(headers map[string]string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}

	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.DialContext = (&net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	tr.TLSHandshakeTimeout = timeout
	tr.ResponseHeaderTimeout = timeout
	tr.ExpectContinueTimeout = 1 * time.Second
	tr.IdleConnTimeout = 90 * time.Second

	var rt http.RoundTripper = tr
	if len(headers) > 0 {
		rt = &headerRoundTripper{
			base:    tr,
			headers: headers,
		}
	}
	return &http.Client{Transport: rt}
}

// boundedBuffer retains the trailing max bytes written to it in a thread-safe ring slice.
// Attaching it to stdio child stderr prevents OSC/log noise from corrupting the TUI while
// preserving startup failure diagnostics.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	return &boundedBuffer{max: maxBytes}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b == nil || b.max <= 0 || n == 0 {
		return n, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if n >= b.max {
		b.buf = append(b.buf[:0], p[n-b.max:]...)
		return n, nil
	}
	if over := len(b.buf) + n - b.max; over > 0 {
		copy(b.buf, b.buf[over:])
		b.buf = b.buf[:len(b.buf)-over]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *boundedBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

type Client struct {
	mu         sync.Mutex
	config     ServerConfig
	session    *mcp.ClientSession
	sdkClient  *mcp.Client
	httpClient *http.Client
	cmd        *exec.Cmd
	stderrBuf  *boundedBuffer
	cancel     context.CancelFunc
}

func NewClient(config ServerConfig) *Client {
	return &Client{config: config}
}

func (c *Client) Connect(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	impl := &mcp.Implementation{Name: "loomcode", Version: "0.1.0"}
	c.sdkClient = mcp.NewClient(impl, nil)
	timeout := c.config.connectTimeout()

	var (
		transport mcp.Transport
		cmd       *exec.Cmd
		stderrBuf *boundedBuffer
	)

	switch {
	case c.config.IsStdio():
		stderrBuf = newBoundedBuffer(maxStderrCaptureBytes)
		cmd = exec.Command(c.config.Command, c.config.Args...)
		cmd.Env = os.Environ()
		for k, v := range c.config.Env {
			cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
		}
		// Capture stderr into a bounded buffer rather than inheriting the parent TTY
		// (which leaks child logs/OSC queries into the TUI) or discarding diagnostics.
		cmd.Stderr = stderrBuf
		c.mu.Lock()
		c.cmd = cmd
		c.stderrBuf = stderrBuf
		c.mu.Unlock()

		transport = &mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: stdioTerminateGrace,
		}
	case c.config.URL != "":
		httpClient := newHTTPClient(c.config.Headers, timeout)
		c.mu.Lock()
		c.httpClient = httpClient
		c.mu.Unlock()
		if c.config.transportKind() == "sse" {
			transport = &mcp.SSEClientTransport{Endpoint: c.config.URL, HTTPClient: httpClient}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: c.config.URL, HTTPClient: httpClient}
		}
	default:
		return fmt.Errorf("MCP server %s: neither command nor url configured", c.config.Name)
	}

	// Keep connCtx alive after a successful handshake (SSEClientTransport binds its
	// long-lived stream to the connect context) while enforcing a strict handshake deadline.
	connCtx, connCancel := context.WithCancel(ctx)
	var timedOut atomic.Bool
	var timer *time.Timer
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > timeout {
		timer = time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			connCancel()
		})
	}

	session, err := c.sdkClient.Connect(connCtx, transport, nil)
	if timer != nil {
		timer.Stop()
	}

	if err != nil {
		connCancel()
		terminateProcess(cmd)

		cause := err
		if timedOut.Load() && ctx.Err() == nil {
			cause = fmt.Errorf("handshake timed out after %s: %w", timeout, context.DeadlineExceeded)
		}
		if stderrTail := strings.TrimSpace(stderrBuf.String()); stderrTail != "" {
			return fmt.Errorf("connect MCP server %s: %w (stderr: %s)", c.config.Name, cause, stderrTail)
		}
		return fmt.Errorf("connect MCP server %s: %w", c.config.Name, cause)
	}

	c.mu.Lock()
	c.session = session
	c.cancel = connCancel
	c.mu.Unlock()
	return nil
}

func (c *Client) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.config.connectTimeout())
		defer cancel()
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("MCP server %s: session not connected", c.config.Name)
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return "", true, fmt.Errorf("MCP server %s: session not connected", c.config.Name)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", true, err
	}
	var parts []string
	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "(no output)"
	}
	return text, result.IsError, nil
}

func (c *Client) Close() {
	c.mu.Lock()
	session := c.session
	c.session = nil
	cancel := c.cancel
	c.cancel = nil
	httpClient := c.httpClient
	c.httpClient = nil
	cmd := c.cmd
	c.cmd = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if session != nil {
		_ = session.Close()
	}
	if httpClient != nil {
		httpClient.CloseIdleConnections()
	}
	terminateProcess(cmd)
}

func terminateProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Manager handles multiple MCP servers

type Manager struct {
	configs map[string]ServerConfig
	clients map[string]*Client
}

func NewManager() *Manager {
	return &Manager{
		configs: make(map[string]ServerConfig),
		clients: make(map[string]*Client),
	}
}

func (m *Manager) LoadConfigs(configs []ServerConfig) {
	for _, cfg := range configs {
		m.configs[cfg.Name] = cfg
	}
}

type ServerInfo struct {
	Name         string
	Instructions string
}

type ConnectResult struct {
	Mgr     *Manager
	Tools   []tools.Tool
	Servers []ServerInfo
	Errors  []string
}

func (m *Manager) ConnectAll(ctx context.Context) ConnectResult {
	var errs []string
	var registered []tools.Tool
	var servers []ServerInfo
	for name, cfg := range m.configs {
		client := NewClient(cfg)
		if err := client.Connect(ctx); err != nil {
			msg := fmt.Sprintf("MCP server '%s': %s", name, err)
			log.Println(msg)
			errs = append(errs, msg)
			continue
		}
		m.clients[name] = client

		info := ServerInfo{Name: name}
		if initResult := client.session.InitializeResult(); initResult != nil {
			info.Instructions = initResult.Instructions
		}
		servers = append(servers, info)

		toolDefs, err := client.ListTools(ctx)
		if err != nil {
			msg := fmt.Sprintf("MCP server '%s' list tools: %s", name, err)
			log.Println(msg)
			errs = append(errs, msg)
			continue
		}

		for _, td := range toolDefs {
			registered = append(registered, &MCPToolWrapper{
				serverName: name,
				toolDef:    td,
				client:     client,
			})
		}
	}
	return ConnectResult{Mgr: m, Tools: registered, Servers: servers, Errors: errs}
}

func (m *Manager) RegisterAllTools(ctx context.Context, registry *tools.Registry) []string {
	result := m.ConnectAll(ctx)
	for _, t := range result.Tools {
		registry.Register(t)
	}
	return result.Errors
}

func (m *Manager) Shutdown() {
	for _, client := range m.clients {
		client.Close()
	}
	m.clients = make(map[string]*Client)
}

func (m *Manager) Close() {
	m.Shutdown()
}

// MCPToolWrapper adapts an MCP tool to the Tool interface

type MCPToolWrapper struct {
	serverName string
	toolDef    *mcp.Tool
	client     *Client
}

func (w *MCPToolWrapper) Name() string {
	return "mcp__" + SanitizeName(w.serverName) + "__" + SanitizeName(w.toolDef.Name)
}

func SanitizeName(name string) string {
	return nonAlphanumeric.ReplaceAllString(name, "_")
}
func (w *MCPToolWrapper) Description() string          { return w.toolDef.Description }
func (w *MCPToolWrapper) Category() tools.ToolCategory { return tools.CategoryCommand }
func (w *MCPToolWrapper) ShouldDefer() bool            { return true }

func (w *MCPToolWrapper) Schema() map[string]any {
	inputSchema := w.toolDef.InputSchema
	if inputSchema == nil {
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"name":         w.Name(),
		"description":  w.Description(),
		"input_schema": inputSchema,
	}
}

func (w *MCPToolWrapper) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	text, isError, err := w.client.CallTool(ctx, w.toolDef.Name, args)
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("MCP tool call failed: %s", err), IsError: true}
	}
	return tools.ToolResult{Output: text, IsError: isError}
}
