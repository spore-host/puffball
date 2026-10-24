.PHONY: all build build-all clean install test lint fmt check check-fmt deps run help vuln gen-docs check-docs check-release-version changelog-fix probe probe-paid fixtures sweep-check

# Variables
BINARY_NAME=puff
VERSION?=dev
BUILD_DIR=bin
MAIN_PATH=.

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod

# Build flags
LDFLAGS=-ldflags "-X github.com/spore-host/puffball/cmd.Version=$(VERSION) -s -w"

# G0 probes and the post-session sweep. They talk to the AWS account named by
# AWS_PROFILE. Paid probes refuse to run unless REAL=1 is set.
PROBE=$(GOCMD) run ./scripts/probe
# Raw responses carry account IDs, so they go to the gitignored .probe/ and only
# reach testdata/ through scripts/scrub-fixtures.
PROBE_OUT?=.probe

# Default target
all: test build

## gen-docs: Regenerate the committed command/flag reference (docs-gen/) from the CLI.
gen-docs:
	$(GOCMD) run . gen-docs --out docs-gen

## check-docs: Drift gate — regenerate and fail if the committed reference is stale.
check-docs: gen-docs
	git diff --exit-code docs-gen/ || { echo "::error::docs-gen/ is stale — run 'make gen-docs' and commit"; exit 1; }

## check-release-version: Release guard — build with the real release ldflags
## and confirm the binary reports the tag. Run automatically by the release
## workflow.
##   make check-release-version TAG=vX.Y.Z
check-release-version:
	@scripts/check-release-version.sh $(TAG)

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

## build-all: Build for multiple platforms
build-all:
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(MAIN_PATH)
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(MAIN_PATH)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(MAIN_PATH)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PATH)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(MAIN_PATH)
	@echo "Multi-platform build complete"

## install: Install the binary to system
install: build
	@echo "Installing $(BINARY_NAME)..."
	@sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/
	@echo "Installation complete"

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	@$(GOCLEAN)
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

## test: Run tests
test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

## check: Fast pre-commit gate — format, vet, lint, short tests
check: check-fmt
	$(GOCMD) vet ./...
	$(MAKE) lint
	$(GOTEST) -short ./...

## lint: Run linter
# Keep in step with the version pinned in .github/workflows/ci.yml (Lint job);
# cmd/ci_gates_test.go fails if the two drift apart.
#
# --max-same-issues=0: the default (3) hides every repeat of the same message
# (lagotto#183: a run reporting 8 findings was really hiding 105).
GOLANGCI_LINT_VERSION=2.15.0
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed. Run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b $$(go env GOPATH)/bin v$(GOLANGCI_LINT_VERSION)" && exit 1)
	@golangci-lint version 2>/dev/null | grep -q "version $(GOLANGCI_LINT_VERSION) " || echo "warning: CI pins golangci-lint $(GOLANGCI_LINT_VERSION); local $$(golangci-lint version 2>/dev/null | awk '{print $$4}') may report different findings"
	golangci-lint run --max-same-issues=0

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@$(GOCMD) fmt ./...
	@echo "Format complete"

## check-fmt: Formatting gate — REPORT drift, don't fix it. Run in CI.
#
# Distinct from `fmt`, which rewrites files and always exits 0, so it can never
# fail a build.
check-fmt:
	@files=$$(gofmt -l . 2>/dev/null | grep -v '^vendor/' || true); \
	if [ -n "$$files" ]; then \
	  echo "::error::these files are not gofmt-clean — run 'gofmt -w' on them:"; \
	  echo "$$files" | sed 's/^/  /'; \
	  echo; gofmt -d $$files; \
	  exit 1; \
	fi; \
	echo "✓ gofmt clean"

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy
	@echo "Dependencies downloaded"

## vuln: Run govulncheck
vuln:
	@echo "Running govulncheck..."
	@govulncheck ./...
	@echo "No known vulnerabilities"

## run: Run the application
run: build
	@$(BUILD_DIR)/$(BINARY_NAME)

## probe: G0 read-only probes (devices in every region, Price List API); no spend
probe:
	$(PROBE) devices --out $(PROBE_OUT)/devices
	$(PROBE) pricing --out $(PROBE_OUT)/pricing

## probe-paid: G0(c) Braket tasks and G0(e) Cost Explorer; billable, requires REAL=1
probe-paid:
	@[ "$(REAL)" = "1" ] || { echo "probe-paid spends money: G0(c) Braket tasks and G0(e) Cost Explorer requests. Re-run with REAL=1."; exit 1; }
	$(PROBE) tasks --real --out $(PROBE_OUT)/tasks
	$(PROBE) costs --real --out $(PROBE_OUT)/costs

## fixtures: Refresh testdata/devices/ from every Braket region, scrubbed
fixtures:
	$(PROBE) devices --out $(PROBE_OUT)/devices
	$(GOCMD) run ./scripts/scrub-fixtures --in $(PROBE_OUT)/devices --out testdata/devices

## sweep-check: Assert no queued/running puffball-tagged tasks and no spending limits remain, in any region
sweep-check:
	$(PROBE) sweep-check

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'

.DEFAULT_GOAL := help

## changelog-fix: Merge duplicate groups in [Unreleased] (the conflict changelog_test.go rejects)
changelog-fix:
	python3 scripts/changelog-consolidate.py CHANGELOG.md
