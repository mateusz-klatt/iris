# Release builds never use cgo. The race detector needs it on some platforms, so tests enable it
# only for themselves. Nothing in Iris links C code.
GO ?= go

.PHONY: test race fixtures build web cov cov-check check-all lint vuln secrets pins

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
	npm ci --ignore-scripts --prefix web
	npm run build --prefix web
	rm -rf internal/webui/dist
	mkdir -p internal/webui/dist
	cp -a web/dist/. internal/webui/dist/

cov:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...

# Every statement is covered, as in snapper. There is no threshold to negotiate.
cov-check:
	$(GO) tool cover -func=coverage.out | awk '/^total:/ { print; exit ($$3 == "100.0%" ? 0 : 1) }'

# Static checks, run in CI by the check-all job. Each tool runs at a pinned version
# through `go run`, so nothing needs installing and go.mod does not change. Dependabot
# does not see these pins; bump them here.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION ?= v1.9.0
GITLEAKS_VERSION ?= v8.30.1

check-all: lint vuln secrets pins

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Scans the full git history, so a shallow clone checks less.
secrets:
	$(GO) run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git --redact --no-banner .

pins:
	$(GO) run ./.github/pincheck .github/workflows
