# Release builds never use cgo. The race detector needs it on some platforms, so tests enable it
# only for themselves. Nothing in Iris links C code.
GO ?= go
VERSION ?= dev

# web/go.mod keeps the Node workspace outside Go's recursive package discovery.
# Keep ./... for every Go-wide check: new application packages must count too.
.PHONY: test race fixtures build image web cov cov-check check-all lint vuln secrets pins refresh

test:
	$(GO) vet ./...
	CGO_ENABLED=0 $(GO) test ./...

race:
	# Exhaustive SQLite rollback sweeps need more time under race instrumentation.
	CGO_ENABLED=1 $(GO) test -race -timeout=30m ./...

fixtures:
	CGO_ENABLED=0 $(GO) test ./internal/hook/...

build: web
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-X github.com/mateusz-klatt/iris/internal/buildinfo.Version=$(VERSION)" -o iris ./cmd/iris

# The Docker context excludes .git. Capture source identity here, before Node generates assets.
# A failed status check must not claim that the checkout is clean.
image:
	docker compose -f deploy/compose.yaml build --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(shell git rev-parse --verify HEAD)" --build-arg MODIFIED="$(if $(shell git status --porcelain --untracked-files=normal || echo unknown),true,false)"

# Rebuild the ignored embedded console. The tracked .keep lets plain Go builds
# compile without Node; such a binary reports a missing console until this runs.
# WEB_DIST names a console built elsewhere. The release job builds it in a job of its
# own, so no code from web/node_modules runs where its credentials are.
web:
ifeq ($(WEB_DIST),)
	npm ci --ignore-scripts --prefix web
	npm run build --prefix web
	rm -rf internal/webui/dist
	mkdir -p internal/webui/dist
	cp -a web/dist/. internal/webui/dist/
else
	test -f "$(WEB_DIST)/index.html"
	rm -rf internal/webui/dist
	mkdir -p internal/webui/dist
	cp -a "$(WEB_DIST)/." internal/webui/dist/
endif
	touch internal/webui/dist/.keep

cov:
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...

# Every statement is covered. There is no threshold to negotiate, and the gate counts
# statements: a percentage rounds 10306 of 10309 to 100.0%.
cov-check:
	$(GO) run ./.github/covermerge -check coverage.out

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

# Updates Go modules, web packages, the tool pins above, workflow actions (to the full SHA of
# their newest release) and Docker base digests, then runs test and check-all. It never
# commits: read its summary, then open a pull request. GITHUB_TOKEN raises GitHub's rate limit.
refresh:
	$(GO) run ./.github/refresh
