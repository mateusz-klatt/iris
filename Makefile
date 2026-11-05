# Release builds never use cgo. The race detector needs it on some platforms, so tests enable it
# only for themselves. Nothing in Iris links C code.
GO ?= go

.PHONY: test race fixtures build

test:
	$(GO) vet ./...
	CGO_ENABLED=0 $(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

fixtures:
	CGO_ENABLED=0 $(GO) test ./internal/hook/...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o iris ./cmd/iris
