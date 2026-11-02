BINARY := bin/mcp-proxy

.PHONY: build run test race vet clean

build:
	mkdir -p bin
	go build -trimpath -o $(BINARY) ./cmd/mcp-proxy

run:
	go run ./cmd/mcp-proxy --config config.json

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist
