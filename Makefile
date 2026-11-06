# Release builds never use cgo. The race detector needs it on some platforms, so tests enable it
# only for themselves. Nothing in Iris links C code.
GO ?= go

.PHONY: test race fixtures build web cov cov-check

test:
	$(GO) vet ./...
	CGO_ENABLED=0 $(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

fixtures:
	CGO_ENABLED=0 $(GO) test ./internal/hook/...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o iris ./cmd/iris

# Rebuild the embedded console. The placeholder in internal/webui/dist is what
# `go build` uses when Node has not run; this target replaces that tree.
web:
	npm ci --prefix web
	npm run build --prefix web
	rm -rf internal/webui/dist
	mkdir -p internal/webui/dist
	cp -a web/dist/. internal/webui/dist/

cov:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...

# Every statement is covered, as in snapper. There is no threshold to negotiate.
cov-check:
	$(GO) tool cover -func=coverage.out | awk '/^total:/ { print; exit ($$3 == "100.0%" ? 0 : 1) }'
