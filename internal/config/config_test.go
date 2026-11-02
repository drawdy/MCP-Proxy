package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSupportsExistingStdioAndRemoteConfig(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
  "mcpServers": {
    "local": {
      "command": "npx",
      "args": ["-y", "some-mcp-server"],
      "env": {"API_TOKEN": "$MCP_TEST_TOKEN"}
    },
    "remote-sse": {
      "url": "https://example.test/sse"
    },
    "remote-http": {
      "type": "streamable-http",
      "url": "https://example.test/mcp",
      "headers": {"Authorization": "Bearer $MCP_TEST_TOKEN"}
    }
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["local"].BackendType(); got != "stdio" {
		t.Fatalf("local backend type = %q, want stdio", got)
	}
	if got := cfg.MCPServers["remote-sse"].BackendType(); got != "sse" {
		t.Fatalf("SSE backend type = %q, want sse", got)
	}
	if got := cfg.MCPServers["remote-http"].BackendType(); got != "streamable-http" {
		t.Fatalf("HTTP backend type = %q, want streamable-http", got)
	}
	if got := cfg.MCPServers["local"].Env["API_TOKEN"]; got != "test-token" {
		t.Fatalf("environment expansion = %q, want test-token", got)
	}
	if got := cfg.MCPServers["remote-http"].Headers["Authorization"]; got != "Bearer test-token" {
		t.Fatalf("header expansion = %q, want Bearer test-token", got)
	}
}

func TestLoadRejectsInvalidBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"mcpServers":{"_bad":{"command":"echo"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid backend name error")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"mcpServers":{"local":{"command":"echo","unexpected":true}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}
