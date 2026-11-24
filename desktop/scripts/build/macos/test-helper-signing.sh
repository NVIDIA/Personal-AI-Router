#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

DESKTOP_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/pair-helper-signing.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT
trap 'exit 1' HUP INT TERM

APP="$TEST_DIR/Fixture.app"
DAEMON="$APP/Contents/MacOS/com.nvidia.nvpair.helper"
mkdir -p "$APP/Contents/MacOS"
cp "$DESKTOP_DIR/native/build/com.nvidia.nvpair.helper" "$DAEMON"
cp /usr/bin/true "$APP/Contents/MacOS/Fixture"
/usr/bin/plutil -create xml1 "$APP/Contents/Info.plist"
/usr/bin/plutil -insert CFBundleIdentifier -string com.nvidia.nvpair "$APP/Contents/Info.plist"
/usr/bin/plutil -insert CFBundleExecutable -string Fixture "$APP/Contents/Info.plist"
/usr/bin/plutil -insert CFBundlePackageType -string APPL "$APP/Contents/Info.plist"

echo 'Checking daemon identifier after standalone signing'
/usr/bin/codesign --force --sign - "$DAEMON"
/usr/bin/codesign --verify --strict --all-architectures \
    '-R=identifier "com.nvidia.nvpair.helper"' "$DAEMON"

echo 'Checking daemon identifier after recursive bundle signing'
/usr/bin/codesign --force --deep --sign - "$APP"
/usr/bin/codesign --verify --deep --strict "$APP"
/usr/bin/codesign --verify --strict --all-architectures \
    '-R=identifier "com.nvidia.nvpair.helper"' "$DAEMON"
