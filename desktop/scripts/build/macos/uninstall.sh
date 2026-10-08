#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

# Manual, user-run full uninstaller for NVIDIA PAIR. Shipped inside
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
# logs, cluster identity and certificates, and engines NVIDIA PAIR
# installed — unless --purge is passed. Downloaded model weights live outside
# these roots (~/.ollama, ~/.llamacpp, ~/.lmstudio/models) and are never touched
# either way: every engine declares its model store as models_dir, and a test in
# services/nvpair-engine-manager fails if one resolves inside the app data root.

PURGE_DATA=0
case "${1:-}" in
  '') ;;
  --purge) PURGE_DATA=1 ;;
  *)
    echo "usage: $(basename "$0") [--purge]" >&2
    echo "  --purge  also remove settings, logs, cluster identity, and PAIR-installed engines" >&2
    echo "           (your downloaded models are kept)" >&2
    exit 2
    ;;
esac

# The bundle this script ships in, three levels above Contents/Resources/
# installer-tools. It is found rather than named because installs exist under
# two names: the bundle is `NVIDIA PAIR.app`, and one installed while it was
# still `PAIR.app` keeps that name, since Squirrel.Mac updates a bundle in place.
APP_PATH="$(cd "$(dirname "$0")/../../.." && pwd -P)"
case "$APP_PATH" in
  *.app) ;;
  *)
    echo "run this from inside the app bundle: <app>.app/Contents/Resources/installer-tools/$(basename "$0")" >&2
    exit 2
    ;;
esac
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

echo "Stopping NVIDIA PAIR processes..."
# The app process is named by the bundle's own executable, read rather than
# assumed for the same reason APP_PATH is.
APP_EXECUTABLE="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$APP_PATH/Contents/Info.plist" 2>/dev/null || true)"
if [ -n "$APP_EXECUTABLE" ]; then
  pkill -TERM -x "$APP_EXECUTABLE" 2>/dev/null || true
fi
# Keep this list in sync with MODULAR_RUNTIME_BINARIES and
# MODULAR_BUNDLED_BINARIES in src/shared/constants/modular-binaries.ts.
for proc in \
  "nvpair-tui" \
  "nvpair-proxy" \
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
  # ollama-proxy and lmstudio-proxy are pre-unification names, kept so an
  # upgrade's leftover firewall entries are removed too; --remove on a path
  # that was never added is a no-op.
  for bin in nvpair-proxy ollama-proxy lmstudio-proxy nvpair-node-info nvpair-node-scanner \
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

# Remove the engines PAIR installed, before the bundle that carries the binary
# doing it and before the data root holding the records of which installs were
# ours. Downloaded models are preserved: engine-manager always skips each
# engine's model store. An engine the user installed themselves is left alone.
if [ "$PURGE_DATA" = "1" ]; then
  ENGINE_MANAGER="$APP_PATH/Contents/Resources/cli-bin/nvpair-engine-manager"
  if [ -x "$ENGINE_MANAGER" ]; then
    echo "Removing PAIR-installed engines..."
    if [ -n "$real_user" ] && [ "$real_user" != "root" ]; then
      # -H and an explicit HOME, because engine-manager derives both the data
      # directory holding the ownership records and every "~" in a manifest
      # from $HOME. macOS sudo keeps HOME by default, so without these it
      # reads root's home, finds no records, and removes nothing.
      sudo -H -u "$real_user" env HOME="$target_home" "$ENGINE_MANAGER" --uninstall-managed || true
    else
      "$ENGINE_MANAGER" --uninstall-managed || true
    fi
  fi
fi

echo "Removing $APP_PATH ..."
rm -rf "$APP_PATH" 2>/dev/null || true

# The generated `nvpair` launcher points into the bundle we just deleted, so it
# goes whether or not data is kept. See src/electron/nvpair-command.ts.
rm -f /usr/local/bin/nvpair 2>/dev/null || true

if [ "$PURGE_DATA" != "1" ]; then
  echo "User data preserved. Re-run with --purge to remove it."
  echo "NVIDIA PAIR has been removed."
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

echo "NVIDIA PAIR has been fully removed."
exit 0
