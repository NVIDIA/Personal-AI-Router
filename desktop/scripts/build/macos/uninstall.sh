#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

# Manual, user-run full uninstaller for Personal AI Router. Shipped inside
# the app bundle at Contents/Resources/installer-tools/uninstall-macos.sh.
#
# macOS auto-update uses Squirrel.Mac (an in-place .app swap) and never runs this
# script or any pkg script, so there is no update-vs-uninstall gating here -- it
# is only ever invoked deliberately by the user. Every delete is best-effort: a
# locked or missing path is ignored and the script continues.
#
# Removing the app and removing your data are separate steps, as they are on the
# other platforms: the Windows uninstaller asks, and `apt remove` keeps data
# while `apt purge` also discards it. So this keeps per-user data — settings,
# logs, cluster identity and certificates, and engines Personal AI Router
# installed — unless --purge is passed. Downloaded model weights live outside
# these roots (e.g. ~/.ollama) and are never touched either way.

PURGE_DATA=0
case "${1:-}" in
  '') ;;
  --purge) PURGE_DATA=1 ;;
  *)
    echo "usage: $(basename "$0") [--purge]" >&2
    echo "  --purge  also remove settings, logs, cluster identity, and PAIR-installed engines" >&2
    exit 2
    ;;
esac

APP_PATH="/Applications/PAIR.app"
# Bundle id used to key the macOS framework state cleaned up below (the .dmg
# leaves no pkg receipt to forget).
PACKAGE_ID="com.nvidia.nvpair"

# Removing /Applications needs root, but user data lives in the real user's home.
# When invoked via sudo, $HOME is root's home, so resolve the invoking user's
# home from $SUDO_USER and target that instead.
real_user="${SUDO_USER:-}"
if [ -n "$real_user" ] && [ "$real_user" != "root" ]; then
  target_home="$(dscl . -read "/Users/$real_user" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "$target_home" ] || target_home="$(eval echo "~$real_user" 2>/dev/null || true)"
else
  target_home="$HOME"
fi
[ -n "$target_home" ] || target_home="$HOME"

APP_SUPPORT="$target_home/Library/Application Support"

echo "Stopping Personal AI Router processes..."
# Keep this list in sync with MODULAR_RUNTIME_BINARIES and
# MODULAR_BUNDLED_BINARIES in src/shared/constants/modular-binaries.ts, plus the
# Electron app process.
for proc in \
  "PAIR" \
  "nvpair-tui" \
  "ollama-proxy" \
  "lmstudio-proxy" \
  "nvpair-node-info" \
  "nvpair-node-scanner" \
  "nvpair-manual-nodes" \
  "nvpair-node-settings" \
  "nvpair-engine-manager" \
  "nvpair-workload-manager" \
  "nvpair-cluster-manager" \
  "nvpair-job-scheduler" \
  "nvpair-errors" \
  "nvpair-ui-broker"; do
  pkill -TERM -x "$proc" 2>/dev/null || true
done
sleep 1

FW=/usr/libexec/ApplicationFirewall/socketfilterfw
if [ -x "$FW" ]; then
  for bin in ollama-proxy lmstudio-proxy nvpair-node-info nvpair-node-scanner \
             nvpair-workload-manager nvpair-errors nvpair-cluster-manager nvpair-engine-manager; do
    "$FW" --remove "$APP_PATH/Contents/Resources/cli-bin/$bin" >/dev/null 2>&1 || true
  done
fi

# Unregister the SMAppService privileged helper (LaunchDaemon) before the app is
# removed, while the bundled control tool still exists. SMAppService state is
# tied to the real user's session, so when invoked via sudo we run the tool as
# the invoking user. Best-effort: a missing/unregistered daemon is ignored.
CTL="$APP_PATH/Contents/MacOS/nvpair-helper-ctl"
if [ -x "$CTL" ]; then
  echo "Unregistering privileged helper..."
  if [ -n "$real_user" ] && [ "$real_user" != "root" ]; then
    sudo -u "$real_user" "$CTL" uninstall >/dev/null 2>&1 || true
  else
    "$CTL" uninstall >/dev/null 2>&1 || true
  fi
fi

echo "Removing $APP_PATH ..."
rm -rf "$APP_PATH" 2>/dev/null || true

# The generated `nvpair` launcher points into the bundle we just deleted, so it
# goes whether or not data is kept. See src/electron/nvpair-command.ts.
rm -f /usr/local/bin/nvpair 2>/dev/null || true

if [ "$PURGE_DATA" != "1" ]; then
  echo "User data preserved. Re-run with --purge to remove it."
  echo "Personal AI Router has been removed."
  exit 0
fi

echo "Removing user data..."
# Per-user data roots. Keep these names in sync with APP_ORG/APP_DATA_DIR_NAME in
# src/shared/constants/app.ts and the Go appdir "Nvidia Corporation/Personal AI
# Router" under Application Support. The living, append-only inventory is
# scripts/wipe-app-data.sh — do not silently diverge.
rm -rf "$APP_SUPPORT/Nvidia Corporation/Personal AI Router" 2>/dev/null || true
rm -rf "$APP_SUPPORT/NVIDIA Corporation/PAIR" 2>/dev/null || true
# Remove the current and previous parents only when empty so other NVIDIA
# applications survive.
rmdir "$APP_SUPPORT/Nvidia Corporation" 2>/dev/null || true
rmdir "$APP_SUPPORT/NVIDIA Corporation" 2>/dev/null || true

# Best-effort removal of macOS framework state keyed on the bundle id.
rm -rf "$target_home/Library/Preferences/$PACKAGE_ID.plist" 2>/dev/null || true
rm -rf "$target_home/Library/Saved Application State/$PACKAGE_ID.savedState" 2>/dev/null || true
rm -rf "$target_home/Library/Caches/$PACKAGE_ID" 2>/dev/null || true
rm -rf "$target_home/Library/HTTPStorages/$PACKAGE_ID" 2>/dev/null || true
rm -rf "$target_home/Library/HTTPStorages/$PACKAGE_ID.binarycookies" 2>/dev/null || true
rm -rf "$target_home/Library/WebKit/$PACKAGE_ID" 2>/dev/null || true

echo "Personal AI Router has been fully removed."
exit 0
