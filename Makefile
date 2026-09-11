# Every Go command runs inside a container so the toolchain matches the go.mod
# directive and a local install cannot change the result.
GO_IMAGE   := golang:1.26-alpine
NODE_IMAGE := node:24-alpine
CACHE      := /tmp/gocache-m365
# Keep this on the version .github/workflows/ci.yml pins, or the local gate and
# CI disagree over an idiom one of them does not know yet.
MODERNIZE  := v0.23.0
GOCYCLO    := v0.6.0
DOCKER_GO   = docker run --rm -v "$(CURDIR)":/src -w /src -v $(CACHE):/gocache \
              -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod \
              -e GOTMPDIR=/gocache/tmp -e GOBIN=/gocache/bin

.PHONY: help ui build check fmt test lint up down logs clean cache

help:
	@echo "ui     Build the browser interface and embed it"
	@echo "build  Build the binary into bin/"
	@echo "check  Run every gate: gofmt, vet, tests, staticcheck, modernize, gocyclo"
	@echo "fmt    Apply gofmt"
	@echo "test   Run the test suite with the race detector"
	@echo "up     Build and start the container"
	@echo "down   Stop the container"

# The cache directories must exist before they are mounted: go refuses to start
# when GOTMPDIR points at a missing path. This is phony rather than a directory
# target because the parent outlives its children: the system empties /tmp and
# removes the empty tmp and bin subdirectories while build and mod keep content,
# and a directory target would then see the parent and skip the recipe.
cache:
	@mkdir -p $(CACHE)/build $(CACHE)/mod $(CACHE)/tmp $(CACHE)/bin

# go:embed cannot reach outside its own package directory, so the Vite output is
# copied into pkg/webui/dist and committed from there.
ui:
	docker run --rm -v "$(CURDIR)/web":/app -w /app $(NODE_IMAGE) \
		sh -c 'npm ci --no-audit --no-fund && npm run build'
	rm -rf pkg/webui/dist
	cp -R web/dist pkg/webui/dist

build: cache
	$(DOCKER_GO) -e CGO_ENABLED=0 $(GO_IMAGE) go build -o bin/m365-bridge ./cmd/cli

fmt: cache
	$(DOCKER_GO) $(GO_IMAGE) gofmt -w cmd pkg

test: cache
	$(DOCKER_GO) -e CGO_ENABLED=1 $(GO_IMAGE) sh -c \
		'apk add --no-cache gcc musl-dev >/dev/null && go test ./... -count=1 -race'

check: cache
	$(DOCKER_GO) -e CGO_ENABLED=1 $(GO_IMAGE) sh -c '\
		set -e; \
		apk add --no-cache gcc musl-dev git >/dev/null; \
		echo "--- gofmt ---"; test -z "$$(gofmt -l cmd pkg)" || { gofmt -l cmd pkg; exit 1; }; \
		echo "--- vet ---"; go vet ./...; \
		echo "--- test ---"; go test ./... -count=1 -race; \
		echo "--- staticcheck ---"; \
		[ -x /gocache/bin/staticcheck ] || go install honnef.co/go/tools/cmd/staticcheck@v0.7.0; \
		/gocache/bin/staticcheck ./...; \
		echo "--- modernize ---"; \
		go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@$(MODERNIZE) ./...; \
		echo "--- gocyclo ---"; \
		[ -x /gocache/bin/gocyclo ] || go install github.com/fzipp/gocyclo/cmd/gocyclo@$(GOCYCLO); \
		/gocache/bin/gocyclo -over 10 -ignore "vendor/|helper-projects/|\.gocache/|web/" .'

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker logs -f m365bridge

clean:
	rm -rf bin web/dist
