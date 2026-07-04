APP        := acg
MODULE     := github.com/tianlinzz/agent-cli-gateway
CMD        := ./cmd/gateway
DIST       := dist

VERSION    := dev
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w \
  -X main.version=$(VERSION) \
  -X main.commit=$(COMMIT) \
  -X main.buildTime=$(BUILD_TIME)

# Internal registry for the agent-gateway images.
REGISTRY      := micr.cloud.mioffice.cn/agent-getaway
BASE_IMAGE    := $(REGISTRY)/agent-gateway-base
GATEWAY_IMAGE := $(REGISTRY)/agent-gateway
BASE_TAG      := go1.25-node22-py3
IMAGE_TAG     ?= latest

.PHONY: build run clean vet test test-race test-pkg \
        docker-base docker-gateway docker-push-base docker-push-gateway

# ---------------------------------------------------------------------------
# Build / test
# ---------------------------------------------------------------------------

# Build the gateway binary into bin/.
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(APP) $(CMD)

run: build
	./bin/$(APP)

clean:
	rm -rf bin $(DIST)

vet:
	go vet ./...

# Run all unit tests.
test: vet
	go test ./...

# Run tests with race detector.
test-race: vet
	go test -race ./...

# Run a specific package's tests verbosely. Usage: make test-pkg PKG=./server/
test-pkg: vet
	go test -v $(PKG)

# ---------------------------------------------------------------------------
# Docker images
#
# The gateway runs claude CLI as a subprocess, so the image bundles the full
# agent runtime (Go binary + claude CLI + Node + Python + git). Two images:
#   - agent-gateway-base: toolchain (Go + Node + Python + git), rebuilt rarely.
#   - agent-gateway:      base + claude CLI (native install) + gateway binary.
#
# Target arch is linux/amd64 (cloud platform); cross-built locally via buildx.
# Push uses skopeo because `docker push` auth is broken against this Harbor
# (skopeo's containers/image auth handshake works). See design doc:
#   docs/superpowers/specs/2026-07-02-containerization-design.md
#
# Push targets need REGISTRY_USER and REGISTRY_PASS env vars set.
# ---------------------------------------------------------------------------

# Build base image and load into local docker (for testing).
docker-base:
	docker buildx build --platform linux/amd64 \
		-t $(BASE_IMAGE):$(BASE_TAG) \
		-f docker/base/Dockerfile --load docker/base

# Build gateway image and load into local docker (for testing).
docker-gateway:
	docker buildx build --platform linux/amd64 \
		-t $(GATEWAY_IMAGE):$(IMAGE_TAG) --load .

# Export base image to oci tar and push via skopeo.
docker-push-base:
	docker buildx build --platform linux/amd64 \
		--output type=oci,dest=/tmp/acg-base.tar \
		-t $(BASE_IMAGE):$(BASE_TAG) -f docker/base/Dockerfile docker/base
	skopeo copy --all \
		--dest-authfile <(printf '{"auths":{"%s":{"auth":"%s"}}}' \
			'$(REGISTRY)' "$$(printf '%s:%s' '$(REGISTRY_USER)' '$(REGISTRY_PASS)' | base64)") \
		oci-archive:/tmp/acg-base.tar \
		docker://$(BASE_IMAGE):$(BASE_TAG)

# Export gateway image to oci tar and push via skopeo.
docker-push-gateway:
	docker buildx build --platform linux/amd64 \
		--output type=oci,dest=/tmp/acg-gateway.tar \
		-t $(GATEWAY_IMAGE):$(IMAGE_TAG) .
	skopeo copy --all \
		--dest-authfile <(printf '{"auths":{"%s":{"auth":"%s"}}}' \
			'$(REGISTRY)' "$$(printf '%s:%s' '$(REGISTRY_USER)' '$(REGISTRY_PASS)' | base64)") \
		oci-archive:/tmp/acg-gateway.tar \
		docker://$(GATEWAY_IMAGE):$(IMAGE_TAG)
