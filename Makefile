.PHONY: build test check

GO_IMAGE := golang:1.25.6-bookworm
GO := /usr/local/go/bin/go
GOFMT := /usr/local/go/bin/gofmt

build:
	docker run --rm -v "$(CURDIR)":/workspace -w /workspace $(GO_IMAGE) \
		sh -c 'mkdir -p dist && $(GO) build -o dist/docs-seed ./cmd/docs-seed'

test:
	docker run --rm -v "$(CURDIR)":/workspace -w /workspace $(GO_IMAGE) \
		sh -c '$(GO) test ./...'

check:
	docker run --rm -v "$(CURDIR)":/workspace -w /workspace $(GO_IMAGE) \
		sh -c '$(GOFMT) -w . && $(GO) mod tidy && $(GO) vet ./... && $(GO) test ./...'
