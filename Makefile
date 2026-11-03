.PHONY: gqlgen codegen generate-proto setup

BUF_VERSION := v1.66.0
BUF := $(shell command -v buf 2>/dev/null || echo $(shell go env GOPATH)/bin/buf)

# Regenerate GraphQL code for the Go controller. Commit the regenerated
# `controller/graph/generated.go` and `models_gen.go` — they are tracked.
#
# gqlgen is a `tool` dependency in controller/go.mod, so its version and its
# golang.org/x/tools come from go.mod/go.sum. Do not go back to
# `go run gqlgen@<version>`: that builds with gqlgen's own pinned x/tools,
# which fails on newer Go toolchains ("package "time" without types was
# imported") after gqlgen has already deleted the generated files.
gqlgen:
	cd controller && go tool gqlgen generate --config graph/gqlgen.yml

# Regenerate both Go and TypeScript types from the shared schema.
codegen: gqlgen
	cd admin && npm run codegen

# Generate protobuf Go code from proto/connector/v1/connector.proto
generate-proto:
	@command -v buf >/dev/null 2>&1 || { \
		echo "buf not found, installing via go install..."; \
		go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION); \
	}
	$(BUF) generate

# First-time setup after cloning — generates all code needed for build
setup: generate-proto gqlgen
	@echo "Setup complete. You can now build:"
	@echo "  cd controller && go build ./..."
	@echo "  cd connector  && cargo build"
