#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <signed-app.app>" >&2
    exit 2
fi

DESKTOP_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
VERIFY_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pair-helper-signatures.XXXXXX")
trap 'rm -rf "$VERIFY_DIR"' EXIT
trap 'exit 1' HUP INT TERM

/usr/bin/xcrun --sdk macosx swiftc \
    -module-cache-path "$VERIFY_DIR/module-cache" \
    "$DESKTOP_DIR/native/Shared/HelperProtocol.swift" \
    "$DESKTOP_DIR/native/VerifyHelperSignatures/main.swift" \
    -o "$VERIFY_DIR/verify-helper-signatures"

"$VERIFY_DIR/verify-helper-signatures" "$1"
