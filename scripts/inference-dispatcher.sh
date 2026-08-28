#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Send synthetic inference traffic at an Ollama- or LM Studio-compatible
# endpoint. Pointed at a Personal AI Router proxy it behaves exactly like any
# other third-party client, so the request enters the cluster router.
#
# Runnable from anywhere (absolute or relative path):
#   ./scripts/inference-dispatcher.sh
#   ./scripts/inference-dispatcher.sh --backend lmstudio --count 5 --mode parallel
#   ./scripts/inference-dispatcher.sh --list-models
#   ./scripts/inference-dispatcher.sh --help
#
# With no arguments one prompt is sent to the default Ollama-compatible port
# using an automatically selected model.
#
# Requires go on PATH — the same toolchain services/build.sh already needs. No
# Node required, so this runs on a machine that only builds the backend.
#
# The client itself lives in scripts/inference-dispatcher (Go). The app ships a
# prebuilt copy for the Inference Demo (desktop/scripts/build-inference-dispatcher.ts);
# this wrapper exists so the same tool can be driven by hand without a build step.
#
# Keep in sync with:
#   - scripts/inference-dispatcher.ps1 (Windows twin — update both in the same change)
#   - docs/inference-dispatcher.md (documented flags and endpoints)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$SCRIPT_DIR/inference-dispatcher"

if [ ! -f "$MODULE_DIR/go.mod" ]; then
    echo "inference-dispatcher: cannot find the client module at $MODULE_DIR" >&2
    exit 2
fi

if ! command -v go >/dev/null 2>&1; then
    echo "inference-dispatcher: go is required and was not found on PATH." >&2
    echo "                      Install Go 1.24.3 or newer (see AGENTS.md)." >&2
    exit 2
fi

BUILD_DIR="$(mktemp -d)"
cleanup() { rm -rf "$BUILD_DIR"; }
trap cleanup EXIT

BINARY="$BUILD_DIR/inference-dispatcher"
(cd "$MODULE_DIR" && go build -o "$BINARY" .)

# Run from the caller's directory so that a relative --result-log or
# --debug-error-log path means what the caller expects.
status=0
"$BINARY" "$@" || status=$?
exit "$status"
