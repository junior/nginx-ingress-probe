# Build and push the probe image. Override REGISTRY / IMAGE / TAG for your registry —
# e.g. a private JFrog Artifactory Docker repo:
#   make buildx REGISTRY=artifactory.example.com/docker-local TAG=0.1.0
REGISTRY  ?= artifactory.example.com/docker-local
IMAGE     ?= nginx-ingress-probe
TAG       ?= 0.1.0
PLATFORMS ?= linux/amd64,linux/arm64
REF        := $(REGISTRY)/$(IMAGE):$(TAG)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILD_ARGS := --build-arg VERSION=$(TAG) --build-arg BUILD_TIME=$(BUILD_TIME)

.PHONY: build push buildx run test lint print

## build a single-arch image for this machine
build:
	docker build $(BUILD_ARGS) -t $(REF) .

## push it (after: docker login $(REGISTRY))
push:
	docker push $(REF)

## build multi-arch (amd64+arm64) and push in one step — needs buildx + a logged-in registry
buildx:
	docker buildx build --platform $(PLATFORMS) $(BUILD_ARGS) -t $(REF) --push .

## run locally with sample data
run:
	PROBE_DEMO=1 go run .

test:
	go test ./...

lint:
	go vet ./... && test -z "$$(gofmt -l .)"

print:
	@echo $(REF)
