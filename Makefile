# SPDX-License-Identifier: Apache-2.0

# Common development tasks. Run `make help` to list them.

export CGO_ENABLED := 0

GO       ?= go
PKG      := ./cmd/asmai
PLATFORMS := linux/amd64 darwin/arm64

.DEFAULT_GOAL := help

.PHONY: help build dist test notices fmt vet tidy clean

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## Build asmai for this host into bin/asmai
	$(GO) build -trimpath -o bin/asmai $(PKG)

dist: ## Build asmai for each CI platform into dist/<os>-<arch>/asmai
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -o dist/$$os-$$arch/asmai $(PKG)"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -o dist/$$os-$$arch/asmai $(PKG) || exit 1; \
	done

test: ## Run the development tests
	$(GO) test ./...

notices: ## Generate THIRD_PARTY_NOTICES from the Go modules compiled into asmai
	$(GO) run ./internal/cmd/gen-notices

fmt: ## Format the Go sources
	$(GO) fmt ./...

vet: ## Report suspicious constructs
	$(GO) vet ./...

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

clean: ## Remove build output
	rm -rf bin dist
