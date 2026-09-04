#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# build.sh — NVIDIA Personal AI Router build script for Linux and macOS.
#
# Mirrors build.bat. Reads versions.json with jq, builds the fourteen worker
# binaries with -X main.Version=... ldflags, then copies them into the
# repo-root staging bundle at:
#
#   build/bin/
#
# nvpair-ui-broker is the backend entry point in that bundle. Product packaging
# places the graphical UI alongside the bundle; the UI launches the broker, which
# supervises the workers. Requires bash, go, and jq on PATH.
#
# Windows uses build.bat; this script intentionally refuses to run there.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
VERSIONS_FILE="$ROOT/versions.json"

case "$(uname -s)" in
    Linux)  PLATFORM="linux"  ;;
    Darwin) PLATFORM="darwin" ;;
    *)
        echo "ERROR: unsupported platform '$(uname -s)'. Use build.bat on Windows." >&2
        exit 1
        ;;
esac

check_tool() {
    local tool="$1" hint="$2"
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: $tool not found in PATH." >&2
        echo "       $hint" >&2
        exit 1
    fi
}
check_tool go "Install Go 1.25+ from https://go.dev/dl/"
check_tool jq "Install jq: 'sudo apt install -y jq' on Linux, 'brew install jq' on macOS."

if [[ ! -f "$VERSIONS_FILE" ]]; then
    echo "ERROR: versions.json not found at $VERSIONS_FILE" >&2
    exit 1
fi

echo "========================================"
echo " Reading versions from $VERSIONS_FILE"
echo "========================================"
echo

# Mirror build.bat's --arg trick: component keys contain hyphens, which jq's
# bare-identifier syntax would parse as subtraction. Passing the key as a
# string variable sidesteps the ambiguity.
V_PRODUCT=$(jq -r '.product'                                   "$VERSIONS_FILE")
V_PROXY=$(  jq -r --arg k 'ollama-proxy'     '.components[$k]' "$VERSIONS_FILE")
V_LMPROXY=$(jq -r --arg k 'lmstudio-proxy'   '.components[$k]' "$VERSIONS_FILE")
V_LLAMAPROXY=$(jq -r --arg k 'llamacpp-proxy' '.components[$k]' "$VERSIONS_FILE")
V_NINFO=$(  jq -r --arg k 'nvpair-node-info'    '.components[$k]' "$VERSIONS_FILE")
V_NSCAN=$(  jq -r --arg k 'nvpair-node-scanner' '.components[$k]' "$VERSIONS_FILE")
V_MNODES=$( jq -r --arg k 'nvpair-manual-nodes' '.components[$k]' "$VERSIONS_FILE")
V_WLMGR=$(  jq -r --arg k 'nvpair-workload-manager' '.components[$k]' "$VERSIONS_FILE")
V_ERRORS=$( jq -r --arg k 'nvpair-errors'       '.components[$k]' "$VERSIONS_FILE")
V_ENGMGR=$( jq -r --arg k 'nvpair-engine-manager' '.components[$k]' "$VERSIONS_FILE")
V_NSETTINGS=$(jq -r --arg k 'nvpair-node-settings' '.components[$k]' "$VERSIONS_FILE")
V_BROKER=$( jq -r --arg k 'nvpair-ui-broker'    '.components[$k]' "$VERSIONS_FILE")
V_CLUMGR=$( jq -r --arg k 'nvpair-cluster-manager' '.components[$k]' "$VERSIONS_FILE")
V_SCHED=$(  jq -r --arg k 'nvpair-job-scheduler' '.components[$k]' "$VERSIONS_FILE")
V_TUI=$(    jq -r --arg k 'nvpair-tui'          '.components[$k]' "$VERSIONS_FILE")

if [[ -z "$V_PRODUCT" || "$V_PRODUCT" == "null" ]]; then
    echo "ERROR: failed to parse versions.json" >&2
    exit 1
fi

printf '  product           = %s\n' "$V_PRODUCT"
printf '  ollama-proxy      = %s\n' "$V_PROXY"
printf '  lmstudio-proxy    = %s\n' "$V_LMPROXY"
printf '  llamacpp-proxy    = %s\n' "$V_LLAMAPROXY"
printf '  nvpair-node-info     = %s\n' "$V_NINFO"
printf '  nvpair-node-scanner  = %s\n' "$V_NSCAN"
printf '  nvpair-manual-nodes  = %s\n' "$V_MNODES"
printf '  nvpair-workload-mgr  = %s\n' "$V_WLMGR"
printf '  nvpair-errors        = %s\n' "$V_ERRORS"
printf '  nvpair-engine-manager= %s\n' "$V_ENGMGR"
printf '  nvpair-node-settings = %s\n' "$V_NSETTINGS"
printf '  nvpair-ui-broker     = %s\n' "$V_BROKER"
printf '  nvpair-cluster-mgr   = %s\n' "$V_CLUMGR"
printf '  nvpair-job-scheduler = %s\n' "$V_SCHED"
printf '  nvpair-tui           = %s\n' "$V_TUI"
echo

echo "========================================"
echo " Building all components (platform: $PLATFORM)"
echo "========================================"
echo

build_subbinary() {
    local idx="$1" name="$2" version="$3"
    echo "[$idx/14] Building $name (v$version)..."
    (cd "$ROOT/$name" && go build -ldflags "-X main.Version=$version" -o "$name" .)
    echo "      OK"
}
build_subbinary 1 ollama-proxy         "$V_PROXY"
build_subbinary 2 lmstudio-proxy       "$V_LMPROXY"
build_subbinary 3 llamacpp-proxy       "$V_LLAMAPROXY"
build_subbinary 4 nvpair-node-info     "$V_NINFO"
build_subbinary 5 nvpair-node-scanner  "$V_NSCAN"
build_subbinary 6 nvpair-manual-nodes  "$V_MNODES"
build_subbinary 7 nvpair-workload-manager "$V_WLMGR"
build_subbinary 8 nvpair-errors        "$V_ERRORS"
build_subbinary 9 nvpair-engine-manager "$V_ENGMGR"
build_subbinary 10 nvpair-node-settings "$V_NSETTINGS"
build_subbinary 11 nvpair-ui-broker    "$V_BROKER"
build_subbinary 12 nvpair-cluster-manager "$V_CLUMGR"
build_subbinary 13 nvpair-job-scheduler "$V_SCHED"
build_subbinary 14 nvpair-tui          "$V_TUI"

BIN_OUT="$ROOT/build/bin"

echo
echo "========================================"
echo " Copying binaries to $BIN_OUT"
echo "========================================"
echo

# Start from an empty bin dir. In any reused build workspace, a binary built for
# a different revision -- for example, a component that doesn't exist here --
# could otherwise linger and ride into the bundle. Wipe and recreate the
# directory, then copy the fresh set so it contains exactly this revision's
# components.
rm -rf "$BIN_OUT"
mkdir -p "$BIN_OUT"
cp "$ROOT/ollama-proxy/ollama-proxy"         "$BIN_OUT/ollama-proxy"
cp "$ROOT/lmstudio-proxy/lmstudio-proxy"     "$BIN_OUT/lmstudio-proxy"
cp "$ROOT/llamacpp-proxy/llamacpp-proxy"     "$BIN_OUT/llamacpp-proxy"
cp "$ROOT/nvpair-node-info/nvpair-node-info"       "$BIN_OUT/nvpair-node-info"
cp "$ROOT/nvpair-node-scanner/nvpair-node-scanner" "$BIN_OUT/nvpair-node-scanner"
cp "$ROOT/nvpair-manual-nodes/nvpair-manual-nodes" "$BIN_OUT/nvpair-manual-nodes"
cp "$ROOT/nvpair-workload-manager/nvpair-workload-manager" "$BIN_OUT/nvpair-workload-manager"
cp "$ROOT/nvpair-errors/nvpair-errors"             "$BIN_OUT/nvpair-errors"
cp "$ROOT/nvpair-engine-manager/nvpair-engine-manager" "$BIN_OUT/nvpair-engine-manager"
cp "$ROOT/nvpair-node-settings/nvpair-node-settings" "$BIN_OUT/nvpair-node-settings"
cp "$ROOT/nvpair-ui-broker/nvpair-ui-broker"       "$BIN_OUT/nvpair-ui-broker"
cp "$ROOT/nvpair-cluster-manager/nvpair-cluster-manager" "$BIN_OUT/nvpair-cluster-manager"
cp "$ROOT/nvpair-job-scheduler/nvpair-job-scheduler" "$BIN_OUT/nvpair-job-scheduler"
cp "$ROOT/nvpair-tui/nvpair-tui"                   "$BIN_OUT/nvpair-tui"

echo
echo "========================================"
echo " Build complete (product v$V_PRODUCT)"
echo "========================================"
echo
printf '  Proxy:        %s\n' "$BIN_OUT/ollama-proxy"
printf '  LM Studio Proxy: %s\n' "$BIN_OUT/lmstudio-proxy"
printf '  Llama.cpp Proxy: %s\n' "$BIN_OUT/llamacpp-proxy"
printf '  Node Info:    %s\n' "$BIN_OUT/nvpair-node-info"
printf '  Node Scanner: %s\n' "$BIN_OUT/nvpair-node-scanner"
printf '  Manual Nodes: %s\n' "$BIN_OUT/nvpair-manual-nodes"
printf '  Workload Mgr: %s\n' "$BIN_OUT/nvpair-workload-manager"
printf '  Errors:       %s\n' "$BIN_OUT/nvpair-errors"
printf '  Engine Mgr:   %s\n' "$BIN_OUT/nvpair-engine-manager"
printf '  Node Settings:%s\n' " $BIN_OUT/nvpair-node-settings"
printf '  UI Broker:    %s\n' "$BIN_OUT/nvpair-ui-broker"
printf '  Cluster Mgr:  %s\n' "$BIN_OUT/nvpair-cluster-manager"
printf '  Job Scheduler:%s\n' " $BIN_OUT/nvpair-job-scheduler"
printf '  TUI:          %s\n' "$BIN_OUT/nvpair-tui"
echo
