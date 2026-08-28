# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Developer entry points for the Personal AI Router monorepo. These targets wrap
# the canonical build commands documented in docs/building.mdx; they do not
# reimplement them.
#
# Linux and macOS only. Windows development uses the desktop npm scripts and
# services\build.bat directly. Recipes stay POSIX sh so they behave the same
# under dash and bash.

.DEFAULT_GOAL := help

DESKTOP := desktop
SERVICES := services
NODE_MODULES := $(DESKTOP)/node_modules
GO_MODULES := $(patsubst %/go.mod,%,$(wildcard $(SERVICES)/*/go.mod))

# Minimums documented in docs/building.mdx.
MIN_GO := 1.25
MIN_NODE := 25.5.0

.PHONY: help dev tools deps-go deps-node build build-binaries build-desktop \
	build-services run check verify lint typecheck contracts headers \
	headers-fix test test-desktop test-services clean

help: ## List available targets
	@printf 'Personal AI Router — development targets\n\n'
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN { FS = ":.*## " } { printf "  \033[1m%-16s\033[0m%s\n", $$1, $$2 }'
	@printf '\nRun any target from the repository root.\n'

dev: tools deps-go deps-node ## Install everything needed to build and run PAIR
	@printf '\nDependencies installed. Start the desktop app with: make run\n'

build: build-binaries build-desktop ## Build the service binaries and the desktop bundles

run: $(NODE_MODULES) ## Start the desktop app in development mode
	cd $(DESKTOP) && npm start

# Mirrors the repository-wide header gate plus the desktop half of the CI
# validate and monorepo-gate stages (ci/pipeline.yml). CI additionally rebuilds
# the binaries and runs the Go suites; use make build-binaries and
# make test-services for those.
check: headers verify lint typecheck contracts test-desktop ## Run the CI gates

test: test-desktop test-services ## Run the desktop unit tests and the Go tests

clean: ## Remove built binaries, bundles, and packages
	rm -rf $(DESKTOP)/cli-bin $(DESKTOP)/out $(DESKTOP)/dist $(DESKTOP)/release \
		$(DESKTOP)/coverage $(SERVICES)/build $(SERVICES)/dist
	rm -f $(DESKTOP)/*.tsbuildinfo
	@for module in $(GO_MODULES); do \
		component=`basename "$$module"`; \
		rm -f "$$module/$$component" "$$module/$$component.exe"; \
	done
	@printf 'Removed build output. Dependencies in %s are untouched.\n' '$(NODE_MODULES)'

tools: ## Report the required toolchain versions
	@at_least() { printf '%s\n%s\n' "$$2" "$$1" | sort -V -C; }; \
	missing=''; \
	for tool in go node npm jq; do \
		command -v "$$tool" >/dev/null 2>&1 || missing="$$missing $$tool"; \
	done; \
	if [ -n "$$missing" ]; then \
		for tool in $$missing; do \
			case "$$tool" in \
			go) printf 'missing go: install Go %s or newer from https://go.dev/dl/\n' '$(MIN_GO)' ;; \
			node|npm) printf 'missing %s: install Node.js %s or newer from https://nodejs.org/\n' "$$tool" '$(MIN_NODE)' ;; \
			jq) printf 'missing jq: brew install jq, sudo apt install jq, or sudo dnf install jq\n' ;; \
			esac; \
		done; \
		printf 'See docs/building.mdx for the full prerequisite list.\n'; \
		exit 1; \
	fi; \
	go_version=`go version | awk '{ print $$3 }' | sed 's/^go//'`; \
	node_version=`node --version | sed 's/^v//'`; \
	printf 'go %s, node %s, npm %s, %s\n' \
		"$$go_version" "$$node_version" "`npm --version`" "`jq --version`"; \
	at_least "$$go_version" '$(MIN_GO)' \
		|| printf 'warning: Go %s is older than the supported minimum %s\n' "$$go_version" '$(MIN_GO)'; \
	at_least "$$node_version" '$(MIN_NODE)' \
		|| printf 'warning: Node.js %s is older than the supported minimum %s\n' "$$node_version" '$(MIN_NODE)'

# `go mod download` rewrites go.mod indirect requirements, which desynchronizes
# the sibling modules that pin them. Compiling to /dev/null fills the module and
# build caches under the default readonly module mode instead, and emits no
# binaries: desktop/cli-bin is the only binary output PAIR runs.
deps-go: ## Fetch and compile the Go dependencies of every services module
	@for module in $(GO_MODULES); do \
		printf 'go build: %s\n' "$$module"; \
		(cd "$$module" && go build -o /dev/null ./...) || exit 1; \
	done

deps-node: $(NODE_MODULES) ## Install the desktop npm dependencies
	@printf 'npm packages: %s is current with %s\n' '$(NODE_MODULES)' '$(DESKTOP)/package-lock.json'

verify: $(NODE_MODULES) ## Verify the desktop build scripts match package.json
	cd $(DESKTOP) && npm run verify:build-scripts

lint: $(NODE_MODULES) ## Lint the desktop sources
	cd $(DESKTOP) && npm run lint

typecheck: $(NODE_MODULES) ## Typecheck the desktop node, web, and test projects
	cd $(DESKTOP) && npm run typecheck

contracts: $(NODE_MODULES) ## Check the desktop and services JSON-RPC contract surface
	cd $(DESKTOP) && npm run service-contracts:check

# Covers the whole monorepo, not just desktop, and needs no npm install: the
# checker is dependency-free so a services-only clone can run it too.
headers: ## Check that every file carries the SPDX copyright and license header
	node scripts/spdx-headers.mjs

headers-fix: ## Insert the SPDX header into any file that is missing one
	node scripts/spdx-headers.mjs --fix

test-desktop: $(NODE_MODULES) ## Run the desktop unit tests
	cd $(DESKTOP) && npm run test:unit

# services/tests builds the component binaries it drives into a temporary
# directory, so the cross-process suite does not need build-services first.
test-services: ## Run go test in every services module
	@for module in $(GO_MODULES); do \
		printf '\ngo test: %s\n' "$$module"; \
		(cd "$$module" && go test ./...) || exit 1; \
	done

build-binaries: $(NODE_MODULES) ## Compile the Go service binaries into desktop/cli-bin
	cd $(DESKTOP) && npm run build:modular-binaries

build-desktop: $(NODE_MODULES) ## Build the Electron main, preload, renderer, and CLI bundles
	cd $(DESKTOP) && npm run build

# The standalone bundle the TUI and the services installers use. The desktop app
# runs desktop/cli-bin instead, which build-binaries produces.
build-services: ## Stage the standalone services bundle in services/build/bin
	cd $(SERVICES) && ./build.sh

# npm writes into node_modules, so its timestamp trails the lockfile only when
# the lockfile actually changed.
$(NODE_MODULES): $(DESKTOP)/package-lock.json
	cd $(DESKTOP) && npm ci
	@touch $@
