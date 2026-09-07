.PHONY: build test vet clean

# Build the MCP server binary
build:
	mkdir -p bin/
	go build -o bin/mcp-server ./cmd/mcp-server

# Run tests. cmd/mcp-server/stdout_safety_test.go is the stdout-purity guard:
# it needs examples/repo present, and skips silently if that fixture is gone.
test:
	go test ./... -coverprofile=./cover.out -covermode=atomic -coverpkg=./...

vet:
	go vet ./...

clean:
	rm -f bin/mcp-server cover.out
