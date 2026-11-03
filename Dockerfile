FROM golang:1.26.8-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mcp-proxy ./cmd/mcp-proxy

FROM node:24-alpine

RUN apk add --no-cache ca-certificates tzdata openssh-client \
    && addgroup -S mcp \
    && adduser -S -G mcp -h /home/mcp mcp \
    && mkdir -p /etc/mcp-proxy /workspace /home/mcp/.npm \
        /home/mcp/.claude-server-commander \
        /home/mcp/.claude-server-commander-logs \
    && chown -R mcp:mcp /home/mcp /workspace

COPY --from=builder /out/mcp-proxy /usr/local/bin/mcp-proxy

ENV HOME=/home/mcp
WORKDIR /home/mcp
USER mcp

EXPOSE 3006

ENTRYPOINT ["mcp-proxy"]
CMD ["--config", "/etc/mcp-proxy/config.json", "--host", "0.0.0.0", "--port", "3006", "--sse-path", "/sse", "--health-endpoint", "/healthz", "--health-endpoint", "/readyz"]
