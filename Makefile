# Vornik Community Edition — injected at export time (the upstream Makefile
# carries operator/Enterprise build + deploy ops that don't ship publicly).
.PHONY: build test vet fmt lint docs-gen

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# lint: formatting + vet. The artifact-purity gates run via `go test`
# (internal/architecture) and in CI (.github/workflows/ci.yaml).
lint: vet
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

# docs-gen: regenerate the generated customer-docs pages (CLI/config reference +
# the CE/EE editions matrix) from source.
docs-gen:
	go run ./cmd/docs-gen all
