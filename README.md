# MCP-Proxy

A lightweight Go-based MCP proxy that exposes multiple backend MCP servers through one traditional Server-Sent Events (SSE) endpoint. It is designed as a stable replacement for the Node.js proxy used by MCP-Interceptor.

## Features

- Aggregate tools and resources from multiple MCP backends.
- Isolate backend names using the `backend.tool` naming convention.
- Give every frontend SSE connection its own MCP server session.
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

The initial release focuses on the traditional SSE frontend used by MCP-Interceptor. Backend connections support stdio, SSE, and Streamable HTTP. Prompts, resource templates, dynamic capability notifications, and broader protocol translation remain follow-up work.

## License

MIT. See [LICENSE](LICENSE).
