#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Wipe all PAIR-owned application data (clean-only; does not relaunch the app).
#
# Runnable from anywhere (absolute or relative path):
#   ./scripts/wipe-app-data.sh --dry-run
#   ./scripts/wipe-app-data.sh --confirm
#
# No Node required. The desktop app spawns this script after shutting down the
# service tree.
#
# ---------------------------------------------------------------------------
# APPEND-ONLY wipe inventory
#
# This list GROWS and is NEVER cut down. When storage moves (e.g. version 1
# writes under /a/b/c and version 2 under /a/b/d), BOTH paths stay listed so
# upgrades from any prior release remain recoverable.
#
# Keep names in sync with:
#   - desktop/src/shared/constants/app.ts
#   - services/shared/appdir/appdir.go
#   - desktop/scripts/build/{installer.nsh,linux/after-remove.sh,macos/uninstall.sh}
#   - scripts/wipe-app-data.ps1 (Windows twin — update both in the same change)
#
# Explicit exclusions (never add): ~/.ollama, ~/.lmstudio, external engine
# installs, and the application install tree (Program Files / /opt/PAIR /
# PAIR.app).
# ---------------------------------------------------------------------------
set -uo pipefail

CONFIRM_PHRASE=wipe
DRY_RUN=0
CONFIRM=0
FORCE_KILL=0
WAIT_PID=""
RELAUNCH_EXEC=""
WAIT_TIMEOUT_SECONDS=60

usage() {
  cat <<EOF
Usage: scripts/wipe-app-data.sh [options]

Delete all Personal AI Router-owned application data (settings, logs, cluster
identity, chat history, PAIR-managed engines under the app data root).

Does NOT delete third-party model libraries (e.g. ~/.ollama, ~/.lmstudio).
Does NOT uninstall the application binary.

Options:
  --dry-run          List paths that would be deleted; no confirmation required
  --confirm          Skip interactive prompt (required for non-TTY / app-invoked use)
  --force-kill       Stop PAIR processes before wiping
  --wait-pid=<pid>   Wait for that process to exit before wiping (app-invoked)
  --wait-timeout=<s> Give up waiting after this many seconds (default ${WAIT_TIMEOUT_SECONDS})
  --relaunch=<path>  Start this executable after a successful wipe (app-invoked)
  --help             Show this help

Interactive mode requires typing "${CONFIRM_PHRASE}" to proceed.
Quit Personal AI Router before running unless --force-kill is set.
EOF
}

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --confirm|--yes) CONFIRM=1 ;;
    --force-kill) FORCE_KILL=1 ;;
    --wait-pid=*) WAIT_PID="${arg#*=}" ;;
    --wait-timeout=*) WAIT_TIMEOUT_SECONDS="${arg#*=}" ;;
    --relaunch=*) RELAUNCH_EXEC="${arg#*=}" ;;
    --help|-h) usage; exit 0 ;;
    -*)
      echo "Unknown option: $arg" >&2
      usage >&2
      exit 2
      ;;
  esac
done

APP_ID=com.nvidia.nvpair
APP_ORG="Nvidia Corporation"
APP_DATA_DIR_NAME="Personal AI Router"
APP_PREVIOUS_ORG="NVIDIA Corporation"
APP_PREVIOUS_DATA_DIR_NAME=PAIR

uname_s="$(uname -s 2>/dev/null || echo unknown)"
case "$uname_s" in
  Darwin) PLATFORM=darwin ;;
  Linux) PLATFORM=linux ;;
  MINGW*|MSYS*|CYGWIN*) PLATFORM=win32 ;;
  *) PLATFORM=linux ;;
esac

HOME_DIR="${HOME:-}"
if [[ -z "$HOME_DIR" ]]; then
  echo "error: HOME is unset" >&2
  exit 1
fi

if [[ "$PLATFORM" == "darwin" ]]; then
  CONFIG_BASE="$HOME_DIR/Library/Application Support"
  CACHE_HOME="$HOME_DIR/Library/Caches"
elif [[ "$PLATFORM" == "win32" ]]; then
  echo "error: on Windows use scripts/wipe-app-data.cmd (or wipe-app-data.ps1)" >&2
  exit 1
else
  CONFIG_BASE="${XDG_CONFIG_HOME:-$HOME_DIR/.config}"
  CACHE_HOME="${XDG_CACHE_HOME:-$HOME_DIR/.cache}"
fi

CURRENT_ROOT="$CONFIG_BASE/$APP_ORG/$APP_DATA_DIR_NAME"
LEGACY_ROOT="$CONFIG_BASE/$APP_PREVIOUS_ORG/$APP_PREVIOUS_DATA_DIR_NAME"

USERNAME="${USER:-user}"
if command -v sha256sum >/dev/null 2>&1; then
  SCOPE="$(printf '%s' "${APP_ID}:${USERNAME}" | sha256sum | awk '{print substr($1,1,16)}')"
elif command -v shasum >/dev/null 2>&1; then
  SCOPE="$(printf '%s' "${APP_ID}:${USERNAME}" | shasum -a 256 | awk '{print substr($1,1,16)}')"
else
  SCOPE=unknown
fi
RUNTIME_BASE="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
RUNTIME_BASE="${RUNTIME_BASE%/}"
CONTROL_DIR="$RUNTIME_BASE/nvpair-$SCOPE"

# Append-only target list (path|reason). Never remove entries — only append.
TARGETS=()
TARGETS+=("$CURRENT_ROOT|Current shared Electron + backend app data root")
TARGETS+=("$LEGACY_ROOT|Legacy pre-rename app data root (NVIDIA Corporation/PAIR)")
TARGETS+=("$CACHE_HOME/nvpair-updater|electron-updater download cache")
TARGETS+=("$CONTROL_DIR|Removed CLI control socket directory and auth token")
TARGETS+=("$HOME_DIR/.local/bin/nvpair|Generated Linux nvpair launcher")
TARGETS+=("/usr/local/bin/nvpair|Generated macOS/Linux nvpair launcher (best-effort)")

if [[ "$PLATFORM" == "darwin" ]]; then
  TARGETS+=("$HOME_DIR/Library/Preferences/${APP_ID}.plist|macOS preferences")
  TARGETS+=("$HOME_DIR/Library/Saved Application State/${APP_ID}.savedState|macOS saved application state")
  TARGETS+=("$HOME_DIR/Library/Caches/${APP_ID}|macOS framework cache")
  TARGETS+=("$HOME_DIR/Library/HTTPStorages/${APP_ID}|macOS HTTP storage")
  TARGETS+=("$HOME_DIR/Library/HTTPStorages/${APP_ID}.binarycookies|macOS HTTP cookies")
  TARGETS+=("$HOME_DIR/Library/WebKit/${APP_ID}|macOS WebKit storage")
fi

PAIR_PROCS=(
  PAIR
  nvpair-ui-broker
  ollama-proxy
  lmstudio-proxy
  nvpair-node-info
  nvpair-node-scanner
  nvpair-manual-nodes
  nvpair-node-settings
  nvpair-engine-manager
  nvpair-workload-manager
  nvpair-cluster-manager
  nvpair-job-scheduler
  nvpair-errors
  nvpair-tui
)

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "[dry-run] Would remove ${#TARGETS[@]} path(s):"
  for entry in "${TARGETS[@]}"; do
    path="${entry%%|*}"
    reason="${entry#*|}"
    echo "  $path  ($reason)"
  done
  exit 0
fi

# The app spawns this script detached and then exits, so the wipe must happen
# after its process is gone. Otherwise Chromium flushes session/cache files back
# into the directory we just deleted and the "clean" relaunch is not clean.
if [[ -n "$WAIT_PID" ]]; then
  waited=0
  while kill -0 "$WAIT_PID" 2>/dev/null; do
    if [[ "$waited" -ge "$WAIT_TIMEOUT_SECONDS" ]]; then
      echo "error: process $WAIT_PID did not exit within ${WAIT_TIMEOUT_SECONDS}s; aborting wipe" >&2
      exit 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
fi

running_procs=()
for proc in "${PAIR_PROCS[@]}"; do
  if command -v pgrep >/dev/null 2>&1 && pgrep -x "$proc" >/dev/null 2>&1; then
    running_procs+=("$proc")
  fi
done

if [[ ${#running_procs[@]} -gt 0 && "$FORCE_KILL" -eq 0 ]]; then
  echo "Personal AI Router appears to be running (${running_procs[*]})." >&2
  echo "Quit the app first, or pass --force-kill to stop processes before wiping." >&2
  exit 1
fi

if [[ ${#running_procs[@]} -gt 0 && "$FORCE_KILL" -eq 1 ]]; then
  echo "Stopping PAIR processes (--force-kill)..."
  for proc in "${PAIR_PROCS[@]}"; do
    pkill -TERM -x "$proc" 2>/dev/null || true
  done
  sleep 1
fi

if [[ "$CONFIRM" -eq 0 ]]; then
  if [[ ! -t 0 || ! -t 1 ]]; then
    echo "Refusing to wipe without confirmation. Pass --confirm for non-interactive use, or run in a TTY." >&2
    exit 2
  fi
  echo ""
  echo "WARNING: This permanently deletes all Personal AI Router app data."
  echo "Third-party model libraries (e.g. ~/.ollama, ~/.lmstudio) are NOT removed."
  echo ""
  echo "Paths to remove:"
  for entry in "${TARGETS[@]}"; do
    echo "  - ${entry%%|*}"
  done
  echo ""
  echo "Type \"${CONFIRM_PHRASE}\" to confirm:"
  read -r answer
  if [[ "$answer" != "$CONFIRM_PHRASE" ]]; then
    echo "Aborted."
    exit 130
  fi
fi

echo "Removing app data..."
failures=0
removed=0
for entry in "${TARGETS[@]}"; do
  path="${entry%%|*}"
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    continue
  fi
  if rm -rf "$path" 2>/dev/null; then
    echo "  removed $path"
    removed=$((removed + 1))
  else
    echo "  FAILED  $path" >&2
    failures=$((failures + 1))
  fi
done

# Remove org parents only when empty (other NVIDIA apps may share them).
rmdir "$CONFIG_BASE/$APP_ORG" 2>/dev/null || true
rmdir "$CONFIG_BASE/$APP_PREVIOUS_ORG" 2>/dev/null || true

echo "Removed $removed path(s)."
if [[ "$failures" -gt 0 ]]; then
  echo "$failures path(s) could not be removed." >&2
  exit 1
fi

# Relaunch only after the wipe succeeded, so the new process creates the fresh
# first-run state instead of racing the deletes.
if [[ -n "$RELAUNCH_EXEC" ]]; then
  if [[ -x "$RELAUNCH_EXEC" ]]; then
    echo "Relaunching $RELAUNCH_EXEC"
    nohup "$RELAUNCH_EXEC" >/dev/null 2>&1 &
    disown 2>/dev/null || true
    exit 0
  fi
  echo "error: relaunch target is not executable: $RELAUNCH_EXEC" >&2
  exit 1
fi

echo "Done. Restart Personal AI Router to begin with a clean state."
exit 0
