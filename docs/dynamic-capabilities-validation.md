# Dynamic capability aggregation validation

Date: 2026-11-02 (Asia/Shanghai). SDK: github.com/modelcontextprotocol/go-sdk v1.8.0.

## Reproduce

```sh
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

The tests use isolated localhost listeners, in-memory transports, and temporary backend servers. They do not modify the running Compose instance or bind port 3006.

## Coverage

- Ten frontends connect and call tools concurrently through traditional SSE, sharing two backend connections.
- Tools added or removed on a backend generate notifications for every existing client; newly added tools are callable and removed tools fail.
- Prompt lists retain descriptions and required arguments; GetPrompt receives the original name and arguments.
- Resources with identical URI suffixes and different schemes remain distinct. Templates preserve simple, reserved, query, path, list, and map expansions. Reads restore the original URI and return a proxy URI.
- A backend transport is terminated and its listener closed. Concurrent calls to the other backend continue succeeding; calls to the failed backend return errors. Reopening the address restores tools, prompts, resources, and templates for the original frontend sessions.
- Streamable HTTP backend template additions and removals propagate to SSE clients.
- Separate frontend server registries can be mutated independently. Changes between the initial snapshot and initialize are reconciled.
- Frontend disconnect/reconnect releases watcher goroutines; subscription cancellation is idempotent and removes manager watchers.
- Rediscovery errors retain the last successful capability list; initial failures retry without another notification. Identical rediscovery emits no change. Old sessions cannot mutate or clear the current state. Notifications arriving during initial discovery are retained.
- Illegal, Unicode, long, and colliding tool names remain deterministic, valid, distinct, and route to the original backend names.
- An independent raw HTTP/SSE test checks endpoint events, distinct message endpoints, POST 202 responses, legacy initialize (2024-11-05), tools/list, tools/call, and notifications/tools/list_changed. SDK v1.8.0 spells its session query parameter `sessionid` (lowercase); the proxy preserves the SDK endpoint verbatim.

## Remaining validation

A real MCP-Interceptor Chrome extension session is not verified. No MCP-Interceptor installation was identified in the inspected default Chrome extension directory on the host. The raw HTTP/SSE test is repeatable protocol evidence, but does not establish the extension's UI behavior or whether it refreshes its own tool cache after receiving notifications.

No production deployment or existing Docker Compose instance was changed.
