APP        := gateway
WORKER_APP := gateway-worker
MODULE     := github.com/tianlinzz/agent-cli-gateway
CMD        := ./cmd/gateway
WORKER_CMD := ./cmd/gateway-worker
DIST       := dist

VERSION    := dev
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w \
  -X main.version=$(VERSION) \
  -X main.commit=$(COMMIT) \
  -X main.buildTime=$(BUILD_TIME)

.PHONY: build build-worker dev run clean vet fmt test test-race test-integration \
        generate docker

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

# Build the runtime image (multi-stage: nsjail builder + Go build + runtime).
# The nsjail stage compiles google/nsjail on Linux; build with buildx when on
# a darwin host, or in Linux CI. See Dockerfile for the full security model.
docker:
	docker build -t agent-gateway:$(VERSION) .
