package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPStdioHelperProcess acts as a deterministic cross-platform child binary
// when invoked with "-- <mode>" by stdio MCP client tests.
func TestMCPStdioHelperProcess(t *testing.T) {
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" {
		return
	}

	switch mode {
	case "stderr-fail":
		// Emit > 4 KB of prefix noise followed by a diagnostic tail line.
		noise := strings.Repeat("startup-trace-line-padding-0123456789abcdef\n", 140)
		_, _ = os.Stderr.WriteString(noise)
		_, _ = os.Stderr.WriteString("MCP_AUTH_FAILURE: invalid workspace token\n")
		os.Exit(2)
	case "stderr-hang":
		_, _ = os.Stderr.WriteString("MCP_STDIO_STALLED_WAITING_FOR_BACKEND\n")
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
}

func TestMCPToolWrapperSanitization(t *testing.T) {
	wrapper := &MCPToolWrapper{
		serverName: "context-7.dev",
		toolDef: &mcp.Tool{
			Name:        "resolve/library-id",
			Description: "Resolve library package identifier",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"libraryName": map[string]any{"type": "string"},
				},
			},
		},
	}

	if got := wrapper.Name(); got != "mcp__context_7_dev__resolve_library_id" {
		t.Fatalf("unexpected sanitized tool name: %s", got)
	}
	schema := wrapper.Schema()
	if schema["description"] == "" {
		t.Fatal("expected schema description to be preserved")
	}
	inputSchema, ok := schema["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema is not map[string]any, got %T", schema["input_schema"])
	}
	if inputSchema["type"] != "object" {
		t.Fatalf("expected input_schema type 'object', got %v", inputSchema["type"])
	}
}

func TestNewHTTPClientTransportTimeoutsAndHeaders(t *testing.T) {
	defaultCfg := ServerConfig{Name: "s1"}
	if got := defaultCfg.connectTimeout(); got != DefaultConnectTimeout {
		t.Fatalf("default connectTimeout = %s, want %s", got, DefaultConnectTimeout)
	}
	customCfg := ServerConfig{Name: "s2", TimeoutSec: 9}
	if got := customCfg.connectTimeout(); got != 9*time.Second {
		t.Fatalf("TimeoutSec connectTimeout = %s, want 9s", got)
	}

	plainClient := newHTTPClient(nil, DefaultConnectTimeout)
	if plainClient == http.DefaultClient {
		t.Fatal("newHTTPClient(nil, ...) must not return global http.DefaultClient")
	}
	if plainClient.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %s, want 0 so streaming SSE bodies stay open", plainClient.Timeout)
	}
	plainTr, ok := plainClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", plainClient.Transport)
	}
	if plainTr.ResponseHeaderTimeout != DefaultConnectTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", plainTr.ResponseHeaderTimeout, DefaultConnectTimeout)
	}
	if plainTr.TLSHandshakeTimeout != DefaultConnectTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", plainTr.TLSHandshakeTimeout, DefaultConnectTimeout)
	}

	t.Setenv("LOOMCODE_TEST_MCP_SECRET", "sec-42")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hdrClient := newHTTPClient(map[string]string{
		"Authorization": "Bearer $LOOMCODE_TEST_MCP_SECRET",
	}, 6*time.Second)
	hrt, ok := hdrClient.Transport.(*headerRoundTripper)
	if !ok {
		t.Fatalf("expected *headerRoundTripper, got %T", hdrClient.Transport)
	}
	innerTr, ok := hrt.base.(*http.Transport)
	if !ok || innerTr.ResponseHeaderTimeout != 6*time.Second {
		t.Fatalf("expected inner *http.Transport with 6s ResponseHeaderTimeout, got %T (%+v)", hrt.base, innerTr)
	}

	resp, err := hdrClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("hdrClient.Get failed: %v", err)
	}
	_ = resp.Body.Close()
	if gotAuth != "Bearer sec-42" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sec-42")
	}
}

func TestConnectHandshakeTimeoutOnHungHTTPAndSSEServers(t *testing.T) {
	// Case 1: Server accepts TCP connection and never sends HTTP response headers.
	releaseHeaderSrv := make(chan struct{})
	hungHeaderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseHeaderSrv:
		}
	}))
	defer func() {
		close(releaseHeaderSrv)
		hungHeaderSrv.CloseClientConnections()
		hungHeaderSrv.Close()
	}()

	httpClient := NewClient(ServerConfig{
		Name:                   "hung-http",
		URL:                    hungHeaderSrv.URL,
		Transport:              "http",
		connectTimeoutOverride: 150 * time.Millisecond,
	})
	defer httpClient.Close()

	start := time.Now()
	err := httpClient.Connect(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error connecting to hung HTTP MCP server")
	}
	if elapsed > 5*time.Second {
		t.Errorf("hung HTTP server took %s to time out", elapsed)
	}

	// Case 2: SSE server flushes 200 OK headers immediately, then stalls before sending endpoint event.
	releaseSSESrv := make(chan struct{})
	hungSSESrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-releaseSSESrv:
		}
	}))
	defer func() {
		close(releaseSSESrv)
		hungSSESrv.CloseClientConnections()
		hungSSESrv.Close()
	}()

	sseClient := NewClient(ServerConfig{
		Name:                   "hung-sse",
		URL:                    hungSSESrv.URL,
		Transport:              "sse",
		connectTimeoutOverride: 150 * time.Millisecond,
	})
	defer sseClient.Close()

	start = time.Now()
	err = sseClient.Connect(context.Background())
	elapsed = time.Since(start)
	if err == nil {
		t.Fatal("expected error connecting to hung SSE MCP server")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected handshake timeout error, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("hung SSE handshake took %s to time out", elapsed)
	}
}

func TestStdioStderrDiagnosticsBoundedTailAndCleanClose(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Subprocess writes >4 KB to stderr and exits non-zero.
	failClient := NewClient(ServerConfig{
		Name:    "broken-stdio",
		Command: exe,
		Args:    []string{"-test.run=^TestMCPStdioHelperProcess$", "--", "stderr-fail"},
	})
	defer failClient.Close()

	err = failClient.Connect(context.Background())
	if err == nil {
		t.Fatal("expected Connect failure when stdio child exits non-zero")
	}
	if !strings.Contains(err.Error(), "MCP_AUTH_FAILURE: invalid workspace token") {
		t.Errorf("expected stderr tail diagnostic in error, got:\n%v", err)
	}
	if len(err.Error()) > maxStderrCaptureBytes+512 {
		t.Errorf("stderr ring buffer did not bound error length: got %d bytes", len(err.Error()))
	}

	// Subprocess hangs without speaking JSON-RPC; bounded handshake timeout aborts and surfaces stderr.
	hangClient := NewClient(ServerConfig{
		Name:                   "hung-stdio",
		Command:                exe,
		Args:                   []string{"-test.run=^TestMCPStdioHelperProcess$", "--", "stderr-hang"},
		connectTimeoutOverride: 200 * time.Millisecond,
	})
	start := time.Now()
	err = hangClient.Connect(context.Background())
	elapsed := time.Since(start)
	hangClient.Close()

	if err == nil {
		t.Fatal("expected timeout error when stdio child hangs during handshake")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded in hung stdio error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "MCP_STDIO_STALLED_WAITING_FOR_BACKEND") {
		t.Errorf("expected captured stderr from hung child, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("hung stdio Connect took too long (%s)", elapsed)
	}

	// Missing binary returns descriptive error cleanly.
	missingClient := NewClient(ServerConfig{
		Name:    "missing-stdio",
		Command: "loomcode-definitely-nonexistent-mcp-server-bin",
	})
	defer missingClient.Close()
	if err := missingClient.Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "missing-stdio") {
		t.Errorf("expected missing-stdio error, got: %v", err)
	}
}

func TestContext7MCP(t *testing.T) {
	if os.Getenv("LOOMCODE_LIVE_TESTS") != "1" {
		t.Skip("Skipping live MCP network test (set LOOMCODE_LIVE_TESTS=1 to enable)")
	}
	cfg := ServerConfig{
		Name:    "context7",
		Command: "npx",
		Args:    []string{"-y", "@upstash/context7-mcp"},
	}

	client := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Log("Connecting to context7 MCP server...")
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()
	t.Log("Connected successfully")

	t.Log("Listing tools...")
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	t.Logf("Got %d tools:", len(tools))
	for _, tool := range tools {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	if len(tools) == 0 {
		t.Fatal("No tools returned")
	}

	// Print the input schema of the first tool
	t.Logf("Input schema: %+v", tools[0].InputSchema)

	// Call resolve-library-id with "bubbles"
	t.Log("Calling resolve-library-id with 'bubbles'...")
	text, isError, err := client.CallTool(ctx, "resolve-library-id", map[string]any{
		"query":       "charmbracelet/bubbles",
		"libraryName": "bubbles",
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	t.Logf("isError: %v", isError)
	t.Logf("Result: %s", truncate(text, 500))

	// Test tool name sanitization and schema
	wrapper := &MCPToolWrapper{
		serverName: "context7",
		toolDef:    tools[0],
		client:     client,
	}
	t.Logf("Sanitized tool name: %s", wrapper.Name())

	schema := wrapper.Schema()
	t.Logf("Schema name: %s", schema["name"])
	t.Logf("Schema has description: %v", schema["description"] != nil && schema["description"] != "")
	inputSchema, ok := schema["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema is not map[string]any, got %T", schema["input_schema"])
	}
	t.Logf("input_schema type field: %v", inputSchema["type"])
	t.Logf("input_schema has properties: %v", inputSchema["properties"] != nil)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes total)", len(s))
}
