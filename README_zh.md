# MCP-Proxy

基于 Go 实现的轻量级 MCP Proxy，将多个后端 MCP Server 聚合为一个传统 SSE 接口，目标是稳定替代 MCP-Interceptor 原先使用的 Node.js Proxy Server。

[English](README.md)

## 功能

- 聚合多个 MCP 后端的 Tools 和 Resources。
- 使用 `backend.tool` 命名空间隔离工具。
- 每个前端 SSE 客户端拥有独立 MCP Server Session。
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

首阶段聚焦 MCP-Interceptor 所需的传统 SSE 前端兼容。后端支持 stdio、SSE 和 Streamable HTTP。Prompts、Resource Templates、动态能力变更通知及更完整的协议转换能力留待后续迭代。

## 许可证

MIT，详见 [LICENSE](LICENSE)。
