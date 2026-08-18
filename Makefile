APP        := gateway
WORKER_APP := gateway-worker
MODULE     := github.com/tianlinzz/agent-cli-gateway
CMD        := ./cmd/gateway
WORKER_CMD := ./cmd/gateway-worker
DIST       := dist

VERSION    := dev
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

# Target platform for image builds. The darwin dev host is arm64 but the
# deployment targets are amd64; without this, building the base on one arch
# and FROM-ing it on another fails with "no match for platform in manifest".
# Override with PLATFORM=linux/arm64 (etc.) when needed.
PLATFORM ?= linux/amd64

# Agent CLI versions baked into the base image. Default to the
# npm "latest" tag; pass explicit versions to pin, or empty to skip a CLI.
# (A literal "latest" is cached by Docker's layer cache — pass an explicit
# version or --no-cache to pick up newly published releases.)
CODEX_VERSION  ?= latest
CLAUDE_VERSION ?= latest
KIMI_VERSION   ?= latest

LDFLAGS := -s -w \
  -X main.version=$(VERSION) \
  -X main.commit=$(COMMIT) \
  -X main.buildTime=$(BUILD_TIME)

.PHONY: build build-worker dev run clean vet fmt test test-race test-integration \
        generate docker image-base image-product

# ---------------------------------------------------------------------------
# Build / test
# ---------------------------------------------------------------------------

# Build the gateway binary (API process + worker Supervisor) into bin/.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) $(CMD)

# Build the per-session worker child binary into bin/.
build-worker:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/$(WORKER_APP) $(WORKER_CMD)

dev:
	./scripts/dev.sh

run: build
	./bin/$(APP)

clean:
	rm -rf bin $(DIST)

vet:
	go vet ./...

fmt:
	gofmt -w agent api runtime worker adapters config cmd integration internal/archtest

# Run all unit + integration tests.
test: vet
	go test ./...

# Run tests with the race detector (CI).
test-race: vet
	go test -race ./...

# Container-level integration tests only (hermetic stub worker; no agent CLI
# and no real nsjail needed, so they run on the darwin dev host too).
test-integration:
	go test ./integration/ -v

# Regenerate the worker gRPC stubs after editing worker/proto/worker.proto.
generate:
	protoc --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  worker/proto/worker.proto

# Build the base image (nginx + Go + Node 22 + Python3 + the agent CLIs +
# nsjail; pure environment, no startup command). See docker/base/Dockerfile.
image-base:
	docker build --platform $(PLATFORM) -f docker/base/Dockerfile \
	  --build-arg CODEX_VERSION=$(CODEX_VERSION) \
	  --build-arg CLAUDE_VERSION=$(CLAUDE_VERSION) \
	  --build-arg KIMI_VERSION=$(KIMI_VERSION) \
	  -t agent-gateway-base:$(VERSION) .

# Build the product image on top of the base image: adds the
# gateway/gateway-worker binaries and the startup contract. The nsjail stage
# in the base compiles on Linux; build with buildx when on a darwin host, or
# in Linux CI. See Dockerfile for the full security model.
image-product: image-base
	docker build --platform $(PLATFORM) \
	  --build-arg BASE_IMAGE=agent-gateway-base:$(VERSION) \
	  -t agent-gateway:$(VERSION) .

# Convenience alias for the full product image build.
docker: image-product
