#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
# SPDX-License-Identifier: Apache-2.0
#
# Make the DEVELOPMENT Electron promptable for macOS Local Network access.
#
# macOS 15 gates every local-network operation -- unicast to the subnet and all
# multicast, which includes the mDNS that node discovery is built on -- behind a
# per-app grant. macOS only offers that grant to an app whose Info.plist carries
# a usage string. The Electron that npm installs has none, so on macOS 15+ a dev
# run cannot discover nodes: every mDNS send returns EHOSTUNREACH and no prompt
# is ever shown, because there is nothing to show.
#
# The packaged app declares these keys through electron-builder (see
# desktop/electron-builder.config.ts). This script does the same to the throwaway
# Electron under node_modules so `npm start` behaves like the shipped app.
#
# IMPORTANT -- this is not sufficient on its own. macOS attributes a local
# network operation to the RESPONSIBLE process, which is the app that launched
# the tree, not necessarily the process holding the socket. Launch `npm start`
# from VS Code's integrated terminal and the responsible app is Visual Studio
# Code, so the grant that governs discovery is VS Code's and Electron never
# appears in the list no matter what this script writes. To be judged on its own
# identity the app has to be launched through Launch Services:
#
#     open -n <the patched Electron.app>
#
# See docs/macos-local-network.md for the full picture and the alternatives.
#
# npm rewrites node_modules, so re-run this after `npm ci` / `npm install`.
set -euo pipefail

APP="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/desktop/node_modules/electron/dist/Electron.app}"
PLIST="$APP/Contents/Info.plist"

if [ "$(uname -s)" != "Darwin" ]; then
    echo "macos-dev-local-network: not macOS, nothing to do" >&2
    exit 0
fi
if [ ! -f "$PLIST" ]; then
    echo "no Electron bundle at $APP (run npm ci in desktop/ first)" >&2
    exit 1
fi

usage='Personal AI Router finds other Personal AI Router nodes on your local network so they can share models and run inference together.'

# PlistBuddy has no upsert, so delete-then-add. The deletes are allowed to fail:
# on a fresh Electron neither key exists yet.
/usr/libexec/PlistBuddy -c "Delete :NSLocalNetworkUsageDescription" "$PLIST" >/dev/null 2>&1 || true
/usr/libexec/PlistBuddy -c "Add :NSLocalNetworkUsageDescription string $usage" "$PLIST" >/dev/null

/usr/libexec/PlistBuddy -c "Delete :NSBonjourServices" "$PLIST" >/dev/null 2>&1 || true
/usr/libexec/PlistBuddy -c "Add :NSBonjourServices array" "$PLIST" >/dev/null
for svc in _nvpair-node._tcp _nvpair-node-info._tcp _nvpair-ollama._tcp _nvpair-workload-manager._tcp; do
    /usr/libexec/PlistBuddy -c "Add :NSBonjourServices: string $svc" "$PLIST" >/dev/null
done

# Info.plist is sealed by the code signature, so editing it invalidates the
# ad-hoc signature Electron ships with. An app whose signature does not verify
# is not one macOS will hand a privacy grant to, so re-sign in place.
codesign --force --sign - --deep "$APP" >/dev/null 2>&1
codesign --verify --deep --strict "$APP" 2>/dev/null \
    && echo "signature verifies" \
    || echo "WARNING: signature did not verify; the grant may not stick" >&2

echo "patched $APP"
echo "  NSLocalNetworkUsageDescription: set"
echo "  NSBonjourServices: $(/usr/libexec/PlistBuddy -c 'Print :NSBonjourServices' "$PLIST" | grep -c '_nvpair') services"
echo
echo "The grant follows the RESPONSIBLE app. Launched from a VS Code terminal that"
echo "is VS Code, not Electron -- see docs/macos-local-network.md."
