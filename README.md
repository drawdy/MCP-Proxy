# MCP-Proxy

A lightweight Go-based MCP proxy that exposes multiple backend MCP servers through one traditional Server-Sent Events (SSE) endpoint. It is designed as a stable replacement for the Node.js proxy used by MCP-Interceptor.

## Features

- Aggregate tools, prompts, resources, and resource templates from multiple MCP backends.
- Isolate backend names using the `backend.tool` naming convention.
- Give every frontend SSE connection its own MCP server session and capability registry.
- Update connected clients through SDK list-changed notifications when backend capabilities change.
- Share backend connections and reconnect them with exponential backoff.
- Support stdio, legacy SSE, and Streamable HTTP backends.
- Expand environment variables in configuration values.
- Provide health/readiness endpoints and graceful shutdown.
- Run locally or with Docker Compose.

## Quick start

Requirements: Go 1.26.8 or newer.

```sh
cp config.example.json config.json
go run ./cmd/mcp-proxy --config config.json
```

The default endpoint is `http://127.0.0.1:3006/sse`.

## Docker Compose

```sh
cp config.example.json config.json
mkdir -p workspace
docker compose -f compose.yml up -d --build
docker compose -f compose.yml logs -f
docker compose -f compose.yml down
```

The default host binding is `127.0.0.1:3006`. Set `MCP_PROXY_BIND=0.0.0.0` in `.env` to expose it on network interfaces, and optionally set `MCP_PROXY_PORT`.

The configuration is mounted read-only at `/etc/mcp-proxy/config.json`. Mount or place file-server data under `./workspace`. A named volume stores the npm cache for stdio servers launched through `npx`.

## Configuration

The configuration retains the common `mcpServers` shape:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
    },
    "remote-sse": {
      "url": "http://127.0.0.1:9000/sse"
    },
    "remote-http": {
      "type": "streamable-http",
      "url": "http://127.0.0.1:9001/mcp",
      "headers": {
        "Authorization": "Bearer ${REMOTE_TOKEN}"
      }
    }
  }
}
```

Backend types are `stdio`, `sse`, and `streamable-http`. If `type` is omitted, a URL ending in an SSE path is treated as SSE; otherwise a URL is treated as Streamable HTTP. Entries without a URL are treated as stdio.

Supported fields: `command`, `args`, `env`, `cwd`, `url`, `headers`, `type`, and optional `name`. Environment variables are expanded in command, arguments, URL, cwd, env values, and header values.

## Endpoints

- `GET /sse` — traditional MCP SSE transport.
- `POST <message endpoint>` — created and managed by the official Go MCP SDK.
- `GET /healthz` — process health.
- `GET /readyz` — backend readiness.

## Development

```sh
make test
make race
make vet
make build
```

## Compatibility and scope

The frontend uses the official Go MCP SDK's traditional SSE transport. Existing sessions receive tool, prompt, and resource list-changed notifications; resource template changes use the resource notification. Backend connections support stdio, SSE, and Streamable HTTP. A disconnected backend's capabilities are removed and restored after reconnect. Failed rediscovery preserves the last successful list for each capability type and retries automatically.

Tools and prompts use `backend.name`. Tool names requiring normalization or truncation receive a deterministic SHA-256 suffix (maximum 128 ASCII characters); names containing the reserved `--` marker are also rewritten to avoid collisions. Resources and templates use `mcp-proxy-<hex backend name>:<original URI>`, preserving the original scheme and RFC 6570 expressions. Read results use the proxy URI as well. Clients should use the advertised names and URIs.

Integration tests cover ten concurrent SSE clients, independent registries, live capability changes, backend failure/recovery, initialization races, and watcher cleanup. `TestLegacySSEWireCompatibility` independently verifies endpoint events, distinct SDK `sessionid` query parameters, POST messages, MCP initialize, tools/list, tools/call, tools/list_changed, and mixed text/image tool results over HTTP/SSE. The image assertion checks that MIME type, Base64 payload, and content-block order are preserved. See [validation notes](docs/dynamic-capabilities-validation.md). A live MCP-Interceptor Chrome extension session has not been verified in this development environment.

## License

MIT. See [LICENSE](LICENSE).
