.PHONY: gqlgen codegen generate-proto setup

GQLGEN_VERSION := v0.17.89
BUF_VERSION := v1.66.0
GQLGEN_CACHE ?= /tmp/go-build-cache
GQLGEN_MODCACHE ?= /tmp/go-mod-cache
BUF := $(shell command -v buf 2>/dev/null || echo $(shell go env GOPATH)/bin/buf)

# Regenerate GraphQL code for the Go controller.
# `controller/graph/generated.go` is intentionally gitignored and should be
# recreated locally whenever the schema or gqlgen config changes.
gqlgen:
	cd controller && \
	GOCACHE=$(GQLGEN_CACHE) \
	GOMODCACHE=$(GQLGEN_MODCACHE) \
	go run github.com/99designs/gqlgen@$(GQLGEN_VERSION) generate --config graph/gqlgen.yml

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
