GO ?= go
TAGS_TEST := testing no_ui

# Release settings. Override on the command line, e.g.  make release VERSION=0.1.1
REGISTRY ?= seoulorigin
VERSION ?= 0.1.0
PLATFORMS ?= linux/amd64,linux/arm64
BUILDER ?= argus-builder
HUB_IMAGE := $(REGISTRY)/argus-hub
AGENT_IMAGE := $(REGISTRY)/argus-agent
CHART_REPO := oci://registry-1.docker.io/$(REGISTRY)

.PHONY: ui hub agent build test vet images push-images chart-push release helm-lint builder clean
.DEFAULT_GOAL := build

ui:
	cd internal/site && npm ci --no-audit --no-fund && npm run build

hub: ui
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/argus-hub ./internal/cmd/hub

agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/argus-agent ./internal/cmd/agent

build: hub agent

test:
	$(GO) vet -tags='$(TAGS_TEST)' ./...
	$(GO) test -tags='$(TAGS_TEST)' ./...

helm-lint:
	helm lint deploy/helm/argus

# A buildx builder that can produce several architectures in one build.
builder:
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || docker buildx create --name $(BUILDER) --driver docker-container >/dev/null
	@docker buildx inspect $(BUILDER) --bootstrap >/dev/null

# Build both images for this machine only and load them into the local Docker (for trying a change).
images: builder
	docker buildx build --builder $(BUILDER) --load -f deploy/docker/Dockerfile.hub   -t $(HUB_IMAGE):$(VERSION) .
	docker buildx build --builder $(BUILDER) --load -f deploy/docker/Dockerfile.agent -t $(AGENT_IMAGE):$(VERSION) .

# Build both images for every platform and push them to the registry (needs `docker login`).
push-images: builder
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) --push -f deploy/docker/Dockerfile.hub   -t $(HUB_IMAGE):$(VERSION) .
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) --push -f deploy/docker/Dockerfile.agent -t $(AGENT_IMAGE):$(VERSION) .

# Package the chart with this version (chart version and image tag are the same number) and push it
# as an OCI artifact (needs `helm registry login registry-1.docker.io`).
chart-push: helm-lint
	rm -rf dist && mkdir -p dist
	helm package deploy/helm/argus --version $(VERSION) --app-version $(VERSION) -d dist
	helm push dist/argus-$(VERSION).tgz $(CHART_REPO)

# One command: images for every platform, then the chart that points at them.
release: push-images chart-push
	@echo
	@echo "Released $(VERSION). Anyone can install it with:"
	@echo "  helm install argus oci://registry-1.docker.io/$(REGISTRY)/argus --version $(VERSION) -n argus --create-namespace"

clean:
	rm -rf bin dist internal/site/dist
