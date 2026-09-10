GO ?= go
VERSION ?= dev
OUTPUT ?= ../firmware/internal/gatehelper/assets

.PHONY: test build assets clean

test:
	$(GO) test ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o gate ./cmd/gate

assets:
	$(GO) run ./cmd/package -version '$(VERSION)' -output '$(OUTPUT)'

clean:
	rm -f gate
