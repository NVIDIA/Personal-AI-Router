#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Personal AI Router custom Debian post-remove. This script REPLACES electron-builder's default
# after-remove.tpl, so it must undo what after-install created (PATH symlink,
# AppArmor profile) in addition to the Personal AI Router-specific cache refresh and the
# purge-time per-user data cleanup. ${executable}/${sanitizedProductName} are
# expanded at build time; other $VARs are shell vars (left unbraced -- the macro
# expander only rewrites ${[A-Za-z]+}). No `set -e`, and a trailing `exit 0`, so a
# best-effort step never fails the removal.

# 1. Remove the PATH launcher (the dispatch wrapper after-install writes).
# Also best-effort clear any update-alternatives entry left by an older package
# layout that registered the launcher as an alternative rather than a wrapper --
# 'path' must be the registered alternative binary, not the generic symlink, see
# https://man7.org/linux/man-pages/man1/update-alternatives.1.html
if type update-alternatives >/dev/null 2>&1; then
    update-alternatives --remove '${executable}' '/opt/${sanitizedProductName}/${executable}' >/dev/null 2>&1 || true
fi
rm -f '/usr/bin/${executable}'

# 2. Unload + remove the AppArmor profile so the policy is not left enforced until
#    the next reboot. Mirror the chroot guard used in after-install -- live
#    AppArmor operations are not meaningful inside a chroot.
#    https://wiki.debian.org/AppArmor/HowToUse
APPARMOR_PROFILE_DEST='/etc/apparmor.d/${executable}'
if [ -f "$APPARMOR_PROFILE_DEST" ]; then
    if apparmor_status --enabled >/dev/null 2>&1; then
        if ! { [ -x '/usr/bin/ischroot' ] && /usr/bin/ischroot; } && hash apparmor_parser 2>/dev/null; then
            apparmor_parser --remove "$APPARMOR_PROFILE_DEST" || true
        fi
    fi
    rm -f "$APPARMOR_PROFILE_DEST"
fi

# 3. Refresh desktop/icon caches. Harmless on both remove and upgrade.
if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database /usr/share/applications >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
fi

# 4. Remove per-user data only on `purge`, never on a plain `remove`. Per-user
# data here means settings, logs, cluster identity and certificates, and engines
# Personal AI Router installed; downloaded model weights live outside these roots
# (e.g. ~/.ollama) and are never touched. This is the standard Debian choice:
# `apt remove` uninstalls the app but keeps data; `apt purge` (or
# `apt remove --purge` / `dpkg -P`) also wipes it. dpkg/apt --
# and electron-updater's DebUpdater installing a new .deb -- call postrm with
# "upgrade" during an update, so user data is preserved in that case too.
case "${1:-}" in
  purge)
    # postrm runs as root, so resolve the user who actually ran the app to find
    # their per-user data. Best-effort: a multi-user box or a custom
    # XDG_CONFIG_HOME may leave another user's data behind.
    real_user="${SUDO_USER:-}"
    if [ -z "$real_user" ] && command -v logname >/dev/null 2>&1; then
      real_user="$(logname 2>/dev/null || true)"
    fi

    if [ -n "$real_user" ]; then
      user_home="$(getent passwd "$real_user" 2>/dev/null | cut -d: -f6 || true)"
      [ -n "$user_home" ] || user_home="/home/$real_user"

      # Per-user data roots. Keep these names in sync with APP_ORG/APP_DATA_DIR_NAME in
      # src/shared/constants/app.ts and the Go appdir "Nvidia Corporation/Personal
      # AI Router" (backend base = $XDG_CONFIG_HOME or ~/.config). The living,
      # append-only inventory is scripts/wipe-app-data.sh — do not silently diverge.
      # Every delete is best-effort so a locked or missing path never aborts removal.
      rm -rf "$user_home/.config/Nvidia Corporation/Personal AI Router" 2>/dev/null || true
      rm -rf "$user_home/.config/NVIDIA Corporation/PAIR" 2>/dev/null || true
      # Remove the current and previous parents only when empty so other NVIDIA
      # applications survive.
      rmdir "$user_home/.config/Nvidia Corporation" 2>/dev/null || true
      rmdir "$user_home/.config/NVIDIA Corporation" 2>/dev/null || true
    fi
    ;;
esac

exit 0
