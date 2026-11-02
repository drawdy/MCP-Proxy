# MCP-Proxy

基于 Go 实现的轻量级 MCP Proxy，将多个后端 MCP Server 聚合为一个传统 SSE 接口，目标是稳定替代 MCP-Interceptor 原先使用的 Node.js Proxy Server。

[English](README.md)

## 功能

- 聚合多个 MCP 后端的 Tools、Prompts、Resources 和 Resource Templates。
- 使用 `backend.tool` 命名空间隔离工具。
- 每个前端 SSE 客户端拥有独立 MCP Server Session 和能力注册表。
- 后端能力变化时，通过官方 SDK 的 List Changed 通知更新已连接客户端。
- 后端连接共享，断线后指数退避自动重连。
- 支持 stdio、传统 SSE、Streamable HTTP 三种后端。
- 配置支持环境变量展开。
- 提供健康检查、就绪检查和优雅关闭。
- 支持本地运行和 Docker Compose 部署。

## 快速开始

要求 Go 1.26.8 或更高版本。

```sh
cp config.example.json config.json
go run ./cmd/mcp-proxy --config config.json
```

默认 SSE 地址：`http://127.0.0.1:3006/sse`。

## Docker Compose 部署

```sh
cp config.example.json config.json
mkdir -p workspace
docker compose -f compose.yml up -d --build
docker compose -f compose.yml logs -f
docker compose -f compose.yml down
```

默认只监听宿主机 `127.0.0.1:3006`。如需局域网访问，在 `.env` 中设置 `MCP_PROXY_BIND=0.0.0.0`，可通过 `MCP_PROXY_PORT` 修改宿主机端口。

配置文件以只读方式挂载到容器内的 `/etc/mcp-proxy/config.json`。文件型 MCP Server 可访问宿主机项目目录下的 `workspace`。npm 缓存使用独立 Docker volume，避免只读文件系统和临时文件系统权限问题。

## 配置

兼容常见的 `mcpServers` 配置结构：

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

后端类型为 `stdio`、`sse`、`streamable-http`。未指定 `type` 时，无 URL 的配置按 stdio 处理；URL 路径包含 SSE 路径时按 SSE 处理，否则按 Streamable HTTP 处理。

支持字段：`command`、`args`、`env`、`cwd`、`url`、`headers`、`type` 和可选的 `name`。环境变量可用于 command、args、url、cwd、env 和 headers 的值。

## 接口

- `GET /sse`：传统 MCP SSE 接口。
- `POST <message endpoint>`：由官方 Go MCP SDK 管理。
- `GET /healthz`：进程健康检查。
- `GET /readyz`：后端就绪检查。

## 本地开发

```sh
make test
make race
make vet
make build
```

## 当前范围

前端使用官方 Go MCP SDK 的传统 SSE 实现，支持工具、Prompt、资源列表变更通知；Resource Template 变更使用资源列表通知。后端支持 stdio、SSE 和 Streamable HTTP。断线后撤除对应能力，重连后重新发现并恢复。重新发现某类能力失败时保留该类最近成功的列表，并自动重试。

工具和 Prompt 使用 `backend.name`。需要规范化或截断的工具名追加完整 SHA-256 摘要，长度不超过 128 个 ASCII 字符；包含保留标记 `--` 的名称也会转换，以避免冲突。资源及模板使用 `mcp-proxy-<后端名的十六进制>:<原始 URI>`，保留原 scheme 和 RFC 6570 表达式，读取结果也返回代理 URI。客户端应使用列表中实际公布的名称和 URI。

集成测试覆盖十客户端并发、独立注册表、动态能力更新、后端故障恢复、初始化竞态及 watcher 释放。`TestLegacySSEWireCompatibility` 通过原始 HTTP/SSE 验证 endpoint event、SDK 的独立 `sessionid` 查询参数、POST 消息、MCP initialize、tools/list、tools/call 和 tools/list_changed。详见[验证记录](docs/dynamic-capabilities-validation.md)。当前开发环境尚未完成真实 MCP-Interceptor Chrome 扩展联调。

## 许可证

MIT，详见 [LICENSE](LICENSE)。
