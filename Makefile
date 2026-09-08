.PHONY: build test vet clean deploy

# Build the MCP server binary
build:
	mkdir -p bin/
	go build -o bin/ops-repo-mcp ./cmd/ops-repo-mcp

# Run tests. cmd/ops-repo-mcp/stdout_safety_test.go is the stdout-purity guard:
# it needs examples/repo present, and skips silently if that fixture is gone.
test:
	go test ./... -coverprofile=./cover.out -covermode=atomic -coverpkg=./...

vet:
	go vet ./...

clean:
	rm -f bin/ops-repo-mcp cover.out

# Create the next release tag locally, without pushing it.
#
# Release tags are plain major integers (v1, v2, ...). The release workflow
# strips the leading "v" and compiles the rest into main.serverVersion, so the
# tag name is the version.
#
# The tag is deliberately NOT pushed: the push is what triggers the public
# release, and that is not something a build target should do on its own. The
# exact push command is printed instead.
deploy:
	@set -eu; \
	git fetch --tags --quiet origin 2>/dev/null \
	  || echo "warning: could not fetch from origin, using local tags only" >&2; \
	if [ -n "$$(git status --porcelain --untracked-files=no)" ]; then \
	  echo "error: tracked files have uncommitted changes; a tag created now would not contain them" >&2; \
	  git status --short --untracked-files=no >&2; \
	  exit 1; \
	fi; \
	latest=$$(git tag --list 'v[0-9]*' | grep -E '^v[0-9]+$$' | sed 's/^v//' | sort -n | tail -n1); \
	tag="v$$(( $${latest:-0} + 1 ))"; \
	if git rev-parse -q --verify "refs/tags/$$tag" >/dev/null; then \
	  echo "error: $$tag already exists" >&2; exit 1; \
	fi; \
	git tag -a "$$tag" -m "Release $$tag"; \
	shown="none"; [ -z "$$latest" ] || shown="v$$latest"; \
	printf 'latest:  %s\n' "$$shown"; \
	printf 'created: %s on %s (%s)\n' "$$tag" "$$(git rev-parse --short HEAD)" "$$(git branch --show-current)"; \
	git push origin "$$tag"
