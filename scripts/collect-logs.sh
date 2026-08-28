#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Collect Personal AI Router logs and rewrite the identifiers in them so the
# result can be shared.
#
# Runnable from anywhere (absolute or relative path):
#   ./scripts/collect-logs.sh
#   ./scripts/collect-logs.sh -in /path/to/nvpair-logs-2026-07-21.txt -out ./bundle
#   ./scripts/collect-logs.sh -raw          # local debugging, NOT shareable
#   ./scripts/collect-logs.sh -h
#
# With no arguments the local log directory for the current user is read and a
# sanitized bundle is written to ./pair-logs-<timestamp>.
#
# A packaged install ships a compiled `collectlogs` beside this script, and that
# is used when present. Running from a source checkout there is no such binary,
# so the collector is built on demand and go is required — the same toolchain
# services/build.sh already needs. No Node either way.
#
# The logic lives in scripts/collectlogs (Go) rather than in this script.
# Identifiers arrive at several JSON escape depths, so replacing them correctly
# means decoding, substituting, and re-encoding; text substitution over the raw
# file would need a separate pattern per depth and would miss cases. Keeping one
# implementation also keeps this script and its Windows twin from drifting.
#
# Keep in sync with:
#   - scripts/collect-logs.ps1 (Windows twin — update both in the same change)
#   - desktop/src/shared/constants/app.ts (app data directory names)
#   - desktop/src/shared/utils/log.ts (log directory and file names)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$SCRIPT_DIR/collectlogs"
PREBUILT="$SCRIPT_DIR/collectlogs"

if [ -x "$PREBUILT" ] && [ ! -d "$PREBUILT" ]; then
    # Packaged install: the compiled collector sits beside this script.
    BINARY="$PREBUILT"
else
    if [ ! -f "$MODULE_DIR/go.mod" ]; then
        echo "collect-logs: no collector binary beside this script, and no module" >&2
        echo "              source at $MODULE_DIR to build one from." >&2
        exit 2
    fi

    if ! command -v go >/dev/null 2>&1; then
        echo "collect-logs: this checkout has no compiled collector, so go is" >&2
        echo "              required to build one, and it was not found on PATH." >&2
        echo "              Install Go 1.24.3 or newer, or use the copy shipped" >&2
        echo "              with an installed PAIR." >&2
        exit 2
    fi

    BUILD_DIR="$(mktemp -d)"
    cleanup() { rm -rf "$BUILD_DIR"; }
    trap cleanup EXIT

    BINARY="$BUILD_DIR/collectlogs"
    (cd "$MODULE_DIR" && go build -o "$BINARY" .)
fi

# Run from the caller's directory so that a relative -out path means what the
# caller expects.
status=0
"$BINARY" "$@" || status=$?
exit "$status"
