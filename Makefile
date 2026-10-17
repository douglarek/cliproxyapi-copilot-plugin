GO_IMAGE ?= golang:1.26-bookworm
VERSION ?= 0.3.3
HOST_ARCH := $(shell uname -m)
ARCH ?= $(if $(filter aarch64 arm64,$(HOST_ARCH)),arm64,amd64)
ifeq ($(filter amd64 arm64,$(ARCH)),)
$(error unsupported ARCH '$(ARCH)'; use amd64 or arm64)
endif
PLUGIN_DIR := build/plugins/linux/$(ARCH)
PLUGIN_SO := $(PLUGIN_DIR)/cliproxyapi-copilot.so
CACHE_DIR := .cache
VERSION_LDFLAG := -X main.pluginVersion=$(VERSION)

.PHONY: test build build-local package clean

test:
	go test ./...

build:
	mkdir -p $(PLUGIN_DIR) $(CACHE_DIR)/go-build $(CACHE_DIR)/go-mod $(CACHE_DIR)/home
	docker run --rm \
		--platform linux/$(ARCH) \
		--user "$$(id -u):$$(id -g)" \
		-e HOME=/src/$(CACHE_DIR)/home \
		-e GOCACHE=/src/$(CACHE_DIR)/go-build \
		-e GOMODCACHE=/src/$(CACHE_DIR)/go-mod \
		-v "$(CURDIR):/src" \
		-w /src \
		$(GO_IMAGE) \
		sh -ec 'CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) go build -buildvcs=false -trimpath -ldflags "$(VERSION_LDFLAG)" -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cliproxyapi-copilot'

build-local:
	mkdir -p $(PLUGIN_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) go build -buildvcs=false -trimpath -ldflags "$(VERSION_LDFLAG)" -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cliproxyapi-copilot

package: build
	GOARCH=$(ARCH) scripts/package-release.sh "$(VERSION)"

clean:
	rm -rf build dist $(CACHE_DIR)
