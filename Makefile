.PHONY: gqlgen

GQLGEN_VERSION := v0.17.89
GQLGEN_CACHE ?= /tmp/go-build-cache
GQLGEN_MODCACHE ?= /tmp/go-mod-cache

# Regenerate GraphQL code for the Go controller.
# `controller/graph/generated.go` is intentionally gitignored and should be
# recreated locally whenever the schema or gqlgen config changes.
gqlgen:
	cd controller && \
	GOCACHE=$(GQLGEN_CACHE) \
	GOMODCACHE=$(GQLGEN_MODCACHE) \
	go run github.com/99designs/gqlgen@$(GQLGEN_VERSION) generate --config graph/gqlgen.yml
