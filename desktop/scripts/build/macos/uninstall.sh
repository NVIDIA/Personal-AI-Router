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

# Release the PATH entries this user's engines own, while the binary that owns
# the ownership records still exists.
#
# Engine-manager records what it added under the data root
# (engine-bin/engine-path/), while the entries themselves live in the login
# shell's profiles — which the purge below never touches. Removing the records
# first would strand those entries with no way left to identify them, pointing
# at engine directories this script is about to delete. Only needed when data is
# going away: keeping it keeps the engines, the records, and a reinstall's
# ability to clean up later.
#
# Runs as the invoking user for the same reason as the helper above: the
# profiles and the records are theirs, not root's.
#
# Unlike every other step here, a failure is not shrugged off. The binary
# removes what it can and reports the rest, and the records that could still
# identify whatever it left are inside the data root the purge is about to
# delete — so a failed release cancels the purge rather than making those
# entries unidentifiable. The app bundle still goes; a reinstall retries.
if [ "$PURGE_DATA" = "1" ]; then
  EM="$APP_PATH/Contents/Resources/cli-bin/nvpair-engine-manager"
  if [ -x "$EM" ]; then
    echo "Releasing engine PATH entries..."
    # `|| released=$?` rather than a bare call: set -e is on, so a failure would
    # otherwise abort before the check below could keep the data.
    released=0
    if [ -n "$real_user" ] && [ "$real_user" != "root" ]; then
      sudo -u "$real_user" "$EM" --remove-user-path >/dev/null 2>&1 || released=$?
    else
      "$EM" --remove-user-path >/dev/null 2>&1 || released=$?
    fi
    if [ "$released" -ne 0 ]; then
      echo "Warning: could not release every engine PATH entry." >&2
      echo "Keeping user data so a reinstall can finish the cleanup; re-run with --purge afterwards." >&2
      PURGE_DATA=0
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
