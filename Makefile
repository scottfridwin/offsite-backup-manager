# offsite-backup-manager — developer tasks
#
# Usage: make <target>

GO            ?= go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -s -w -X main.version=$(VERSION)
GO_VERSION    ?= 1.25
IMAGE_PREFIX  ?= ghcr.io/scottfridwin/offsite-backup-manager

.PHONY: all build build-primary build-node test lint vet tidy fmt clean \
        docker-primary docker-node

all: build

build: build-primary build-node

build-primary:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/primary ./cmd/primary

build-node:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/node ./cmd/node

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin

docker-primary:
	docker build -f build/Dockerfile --build-arg TARGET=primary --build-arg VERSION=$(VERSION) \
		-t $(IMAGE_PREFIX)-primary:$(VERSION) .

docker-node:
	docker build -f build/Dockerfile --build-arg TARGET=node --build-arg VERSION=$(VERSION) \
		-t $(IMAGE_PREFIX)-node:$(VERSION) .
