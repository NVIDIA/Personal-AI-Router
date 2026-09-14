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
	build-services macos-dev-local-network ab ab-context run check verify lint typecheck contracts headers \
	headers-fix test test-desktop test-services clean \
	mlx mlx-install mlx-status mlx-models mlx-pull mlx-delete mlx-set-port \
	mlx-serve mlx-port mlx-ask mlx-ab mlx-reload-cost mlx-uninstall

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

# ---------------------------------------------------------------------------
# MLX (Apple Silicon). See docs/mlx.mdx. Every target here is a thin wrapper --
# scripts/mlx.mjs holds the other end of nvpair-engine-manager's stdio JSON-RPC
# pipe, because that service has no HTTP control surface to curl.
# ---------------------------------------------------------------------------

# A default small enough to download in seconds and text-only, which matters:
# mlx_lm serves text models, so a vision model (Qwen3-VL, ...) will list in the
# catalogue and then fail to load. Override on any target: make mlx-pull MODEL=...
MODEL ?= mlx-community/Llama-3.2-1B-Instruct-4bit

# Where mlx_lm.server listens. Change it with mlx-set-port, which persists.
ENGINE_PORT ?= 8081

# The proxy prefers :8080 (mlx_lm.server's documented port) and falls back from
# :8090 when something else already holds it -- which is common. Ask the running
# process rather than assuming.
# -a is load-bearing: lsof ORs its selection options by default, so without it
# `-c mlx-proxy -iTCP` lists every listening socket on the machine and the first
# match is some unrelated process.
MLX_PORT = $(shell lsof -nP -a -c mlx-proxy -iTCP -sTCP:LISTEN -Fn 2>/dev/null \
	| sed -n 's/^n.*:\([0-9][0-9]*\)$$/\1/p' | head -1)

mlx: build-services mlx-install ## Build PAIR and install the MLX engine (start here)
	@printf '\nMLX installed. Next: make mlx-pull, then make mlx-serve.\n'

mlx-install: ## Install the MLX engine (uv + a virtualenv + mlx-lm)
	node scripts/mlx.mjs install

mlx-status: ## Show installed / running / healthy / port for the MLX engine
	node scripts/mlx.mjs status

mlx-models: ## List downloaded MLX models, marking the one resident in memory
	node scripts/mlx.mjs models

mlx-pull: ## Download a model (MODEL=<hugging-face-repo-id>)
	node scripts/mlx.mjs pull $(MODEL)

mlx-delete: ## Delete a model from the shared Hugging Face cache (MODEL=...)
	node scripts/mlx.mjs delete $(MODEL)

mlx-uninstall: ## Remove the MLX engine and its virtualenv
	node scripts/mlx.mjs uninstall

# Also the way to adopt a server you run yourself: point the engine at its port
# and `make mlx-serve` will route to that process rather than spawning its own.
mlx-set-port: ## Persistently move the MLX engine to a port (ENGINE_PORT=8089)
	node scripts/mlx.mjs port $(ENGINE_PORT)

# The whole router, headless: the broker spawns discovery, the scheduler and all
# three engine proxies, then advertises this node; the script also starts the
# MLX engine, which the broker does not do on its own. Runs in the foreground --
# Ctrl-C to stop. `make run` is the same thing with the desktop app on top.
mlx-serve: ## Run the router and the MLX engine in the foreground (Ctrl-C to stop)
	node scripts/mlx.mjs serve

mlx-port: ## Print the port mlx-proxy is listening on
	@if [ -n '$(MLX_PORT)' ]; then printf '%s\n' '$(MLX_PORT)'; \
	else printf 'mlx-proxy is not running. Start it with: make mlx-serve\n'; exit 1; fi

# The first request for a model is also what loads it, so this can take a while
# on a cold engine and be instant afterwards. That is the routing policy working,
# not a stall.
#
# MAX_TOKENS is generous because a thinking model spends its budget reasoning
# before it emits a single character of content: at 60 tokens a Qwen3.8 with
# thinking enabled returns finish_reason=length and an empty content field,
# which reads exactly like a broken route and is not one.
PROMPT ?= In one sentence, what does a router do?
MAX_TOKENS ?= 512

mlx-ask: ## Send a chat completion through mlx-proxy (MODEL=..., PROMPT=..., needs mlx-serve)
	@if [ -z '$(MLX_PORT)' ]; then \
		printf 'mlx-proxy is not running. In another terminal: make mlx-serve\n'; exit 1; fi
	@printf 'routing through mlx-proxy on :%s\n\n' '$(MLX_PORT)'
	@jq -n --arg m '$(MODEL)' --arg p '$(PROMPT)' --argjson t $(MAX_TOKENS) \
		'{model:$$m, messages:[{role:"user",content:$$p}], max_tokens:$$t}' \
		| curl -sS http://127.0.0.1:$(MLX_PORT)/v1/chat/completions \
			-H 'Content-Type: application/json' --data-binary @- \
		| (jq -r '.choices[0].message.content // .choices[0].message.reasoning_content // .' 2>/dev/null || cat)

# The two halves of the routing measurement in docs/mlx.mdx. mlx-ab needs no
# engine at all; mlx-reload-cost needs the engine installed.
mlx-ab: ## A/B the residency-preferring routing policy against the control arm
	cd $(SERVICES)/mlx-proxy && go test -run TestRoutingPolicyAB -v .

mlx-reload-cost: ## Measure what one MLX model swap costs, in seconds
	python3 $(SERVICES)/mlx-proxy/bench/reload_cost.py \
		--server "$$HOME/Library/Application Support/Nvidia Corporation/Personal AI Router/engine-bin/mlx/venv/bin/mlx_lm.server" \
		--model-a mlx-community/Llama-3.2-1B-Instruct-4bit \
		--model-b mlx-community/Qwen2.5-0.5B-Instruct-4bit

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

# macOS 15+ gates mDNS behind a per-app Local Network grant, and only offers it
# to a bundle carrying a usage string. The Electron npm installs has none, so a
# dev run cannot discover nodes and is never prompted. Re-run after `npm ci`.
# See docs/macos-local-network.md -- the grant follows the app that LAUNCHED the
# tree, so this alone is not enough from an editor's integrated terminal.
macos-dev-local-network: ## Make the dev Electron promptable for macOS Local Network access
	./scripts/macos-dev-local-network.sh

# A/B the cluster: one node, the other, then both. MODEL_A/MODEL_B are the ids
# the router advertises -- a locally built model is addressed by absolute path,
# so the path IS the node selector (see tests/README.md).
MODEL_A ?= $(HOME)/models/Qwen3-VL-8B-Instruct-4bit
# The peer's model id. A locally built model is addressed by absolute path, and
# that path contains the OWNER's home directory -- so this is the peer's path,
# not yours. Override per run: make ab MODEL_B=/Users/<peer>/models/<model>
MODEL_B ?= $(error set MODEL_B to the peer node's model id, e.g. /Users/<peer>/models/<model>)
# Derived, not hardcoded: these are only labels in the report, and a hostname
# that gets renamed would otherwise leave the benchmark quietly mislabelling its
# own output.
NAME_A  ?= $(shell scutil --get LocalHostName 2>/dev/null || hostname -s)
NAME_B  ?= peer

ab: ## Benchmark node A, node B, then both (vars: MODEL_A MODEL_B NAME_A NAME_B)
	python3 tests/ab_bench.py \
		--model-a "$(MODEL_A)" --model-b "$(MODEL_B)" \
		--name-a "$(NAME_A)" --name-b "$(NAME_B)" $(AB_ARGS)

ab-context: ## Find each node's usable context window
	python3 tests/ab_bench.py \
		--model-a "$(MODEL_A)" --model-b "$(MODEL_B)" \
		--name-a "$(NAME_A)" --name-b "$(NAME_B)" \
		--mode a --repeat 1 --max-tokens 8 --prompt-sizes 8 --context-probe

# The standalone bundle the TUI and the services installers use. The desktop app
# runs desktop/cli-bin instead, which build-binaries produces.
build-services: ## Stage the standalone services bundle in services/build/bin
	cd $(SERVICES) && ./build.sh

# npm writes into node_modules, so its timestamp trails the lockfile only when
# the lockfile actually changed.
$(NODE_MODULES): $(DESKTOP)/package-lock.json
	cd $(DESKTOP) && npm ci
	@touch $@
