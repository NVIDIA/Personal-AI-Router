#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# installer_build.sh — NVIDIA Personal AI Router packaging script for Linux and macOS.
#
# REFERENCE SCRIPT. This shows how NVIDIA assembles the services bundle; it is not
# a supported way to produce a distributable build. Anything it emits is unsigned,
# because signing lives outside this repository. To build and run the services
# locally, use ./build.sh and run the binaries from build/bin.
#
# Mirrors installer_build.bat. Calls build.sh first (which itself fails fast if
# any required tool is missing), then stages the build output into a versioned
# directory and produces a distributable archive:
#
#   Linux:  dist/NVIDIA-Personal-AI-Router-<version>-linux-<arch>.tar.gz
#   macOS:  dist/NVIDIA-Personal-AI-Router-<version>-darwin-<arch>.tar.gz
#
# This script produces the backend bundle (the worker binaries staged under
# bin/). Product packaging places the graphical UI alongside this bundle in the
# same installation directory; the UI launches nvpair-ui-broker to drive the
# NVPAIR API and supervise the workers.
#
# Version precedence (same as installer_build.bat):
#   1. Explicit CLI arg:   installer_build.sh 1.2.3
#   2. versions.json "installer" field
#   3. versions.json "product" field (fallback)
#
# The tarball uses a nested layout — every entry lives under
# NVIDIA-Personal-AI-Router-<version>/ — so `tar xf` produces a single clean
# directory the user can move, rename, or delete as a unit.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
VERSIONS_FILE="$ROOT/versions.json"
DIST_DIR="$ROOT/dist"

case "$(uname -s)" in
    Linux)  PLATFORM="linux"  ;;
    Darwin) PLATFORM="darwin" ;;
    *)
        echo "ERROR: unsupported platform '$(uname -s)'. Use installer_build.bat on Windows." >&2
        exit 1
        ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) ARCH="amd64"  ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
        echo "ERROR: unsupported architecture '$ARCH'." >&2
        exit 1
        ;;
esac

echo " NOTE: installer_build.sh is a reference script. Its output is unsigned and"
echo " is not a supported distributable build. For local use, run ./build.sh."
echo

echo "========================================"
echo " Step 1: Building all components"
echo "========================================"
echo

# NVPAIR_SKIP_BUILD lets a caller package an already-built tree without
# re-running build.sh. This is useful when the staged binaries have undergone
# post-build processing that a rebuild would overwrite.
if [[ "${NVPAIR_SKIP_BUILD:-}" == "1" ]]; then
    echo "  NVPAIR_SKIP_BUILD=1 set — skipping build.sh; packaging existing build output."
else
    "$ROOT/build.sh"
fi

echo
echo "========================================"
echo " Step 2: Resolving installer version"
echo "========================================"
echo

# build.sh has already verified jq is on PATH.
resolve_version() {
    if [[ $# -ge 1 && -n "$1" ]]; then
        echo "$1"
        return
    fi
    if [[ ! -f "$VERSIONS_FILE" ]]; then
        return
    fi
    # `//` returns the right-hand side when the left is null or false,
    # giving us the installer-then-product fallback in one filter.
    local v
    v=$(jq -r '.installer // .product' "$VERSIONS_FILE")
    if [[ "$v" != "null" && -n "$v" ]]; then
        echo "$v"
    fi
}

VERSION="$(resolve_version "${1:-}")"
if [[ -z "$VERSION" ]]; then
    echo "ERROR: could not determine installer version — no CLI arg and no versions.json." >&2
    exit 1
fi

echo "  installer version: $VERSION"
echo "  platform:          $PLATFORM/$ARCH"
echo

mkdir -p "$DIST_DIR"

# --- Backend bundle tarball (Linux and macOS) ---
#
# Both platforms use the same backend artifact shape: a .tar.gz whose top-level
# directory holds the worker binaries under bin/ plus the platform INSTALL.md.
# Product packaging adds the bundled UI alongside this backend directory.

echo "========================================"
echo " Step 3: Staging tarball contents"
echo "========================================"
echo

STAGE_PARENT="$(mktemp -d -t nvpair-stage-XXXXXX)"
trap 'rm -rf "$STAGE_PARENT"' EXIT

# The nested layout: everything goes inside a single top-level directory
# named with the version. `tar xf` produces a clean dir the user can
# move/delete as a unit — no tarbomb risk, two versions can coexist.
STAGE_NAME="NVIDIA-Personal-AI-Router-$VERSION"
STAGE="$STAGE_PARENT/$STAGE_NAME"
mkdir -p "$STAGE/bin"

BIN_SRC="$ROOT/build/bin"

if [[ ! -x "$BIN_SRC/nvpair-ui-broker" ]]; then
    echo "ERROR: expected worker binaries at $BIN_SRC after build.sh — was the build interrupted?" >&2
    exit 1
fi

cp "$BIN_SRC/ollama-proxy"     "$STAGE/bin/"
cp "$BIN_SRC/lmstudio-proxy"   "$STAGE/bin/"
cp "$BIN_SRC/nvpair-node-info"    "$STAGE/bin/"
cp "$BIN_SRC/nvpair-node-scanner" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-manual-nodes" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-workload-manager" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-errors"       "$STAGE/bin/"
cp "$BIN_SRC/nvpair-engine-manager" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-node-settings" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-cluster-manager" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-job-scheduler" "$STAGE/bin/"
cp "$BIN_SRC/nvpair-ui-broker"    "$STAGE/bin/"
cp "$BIN_SRC/nvpair-tui"          "$STAGE/bin/"

if [[ "$PLATFORM" == "darwin" ]]; then
    cp "$ROOT/installer/macos/INSTALL.md" "$STAGE/"
else
    cp "$ROOT/installer/linux/INSTALL.md" "$STAGE/"
fi

# Make sure execute bits are preserved on every binary that needs them.
# `cp` does the right thing but a defensive chmod costs nothing and
# protects against future cp flag changes.
chmod 0755 "$STAGE"/bin/*

echo "  staged at: $STAGE"
echo "  contents:"
# Portable listing (BSD find on macOS has no -printf).
(cd "$STAGE_PARENT" && find "$STAGE_NAME" -mindepth 1 | sort | sed 's/^/    /')
echo

echo "========================================"
echo " Step 4: Creating tarball"
echo "========================================"
echo

ARCHIVE_NAME="NVIDIA-Personal-AI-Router-$VERSION-$PLATFORM-$ARCH.tar.gz"
ARCHIVE_PATH="$DIST_DIR/$ARCHIVE_NAME"

# GNU tar (Linux) supports reproducibility flags: sort entries, zero out
# owner/group, and pin mtimes so two builds of identical sources produce
# byte-identical tarballs. macOS ships BSD tar, which lacks those long
# options, so it gets a plain archive. --mtime uses the versions.json mtime
# as a stable reference so a version bump naturally produces a new hash.
if [[ "$PLATFORM" == "linux" ]]; then
    TAR_MTIME="@$(stat -c %Y "$VERSIONS_FILE")"
    tar --sort=name \
        --owner=0 --group=0 --numeric-owner \
        --mtime="$TAR_MTIME" \
        -C "$STAGE_PARENT" \
        -czf "$ARCHIVE_PATH" \
        "$STAGE_NAME"
else
    tar -C "$STAGE_PARENT" -czf "$ARCHIVE_PATH" "$STAGE_NAME"
fi

echo
echo "========================================"
echo " Installer ready"
echo "========================================"
echo
ls -lh "$ARCHIVE_PATH" | awk '{printf "  %s  (%s)\n", $NF, $5}'
echo
echo "  Extract with:  tar xf $ARCHIVE_NAME"
echo "  Then see:      $STAGE_NAME/INSTALL.md"
echo
