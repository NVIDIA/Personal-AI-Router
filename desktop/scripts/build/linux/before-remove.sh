#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Personal AI Router Debian pre-remove. Releases the PATH entries this user's
# engines own. No `set -e`, and a trailing `exit 0`, so a best-effort step never
# fails the removal.
#
# Unlike after-install.sh and after-remove.sh, this one reaches fpm through the
# raw passthrough in electron-builder.config.ts, so ${macro} is NOT expanded
# here. It finds its own paths at runtime instead.
#
# WHY prerm AND NOT postrm. Engine-manager records what it added to PATH under
# the user's data root (engine-bin/engine-path/), while the entries themselves
# live in the login shell's profiles, which no maintainer script touches.
# after-remove handles the data root on `purge` -- but dpkg deletes the
# package's files before postrm runs, so by then the binary that understands
# those records is gone. prerm is the last point at which both still exist.
#
# This runs on `apt remove` as well as `apt purge`, so a plain remove gives up
# its PATH entries even though it keeps the engines. That is the recoverable
# direction: the drain releases each entry but keeps the record's note that PAIR
# installed the engine, so a reinstall re-adopts it and republishes. Deleting
# the record outright is what made this unrecoverable for an engine whose CLI
# lives outside PAIR's install directory -- nothing else tells it apart from an
# engine the user installed. The alternative -- entries no tool can identify,
# pointing into a directory a later purge deletes -- is worse either way.
#
# dpkg calls prerm with "upgrade" during an update, and "failed-upgrade" when
# recovering from one. Those keep the installation, so leave PATH alone.
case "${1:-}" in
  upgrade|failed-upgrade)
    exit 0
    ;;
esac

# Ask dpkg where it put the binary rather than rebuilding /opt/<product>/...
# from a name this script cannot be told at build time.
command -v dpkg-query >/dev/null 2>&1 || exit 0
engine_manager="$(dpkg-query -L "${DPKG_MAINTSCRIPT_PACKAGE:-}" 2>/dev/null \
  | grep -E '/cli-bin/nvpair-engine-manager$' | head -n 1)"
[ -n "$engine_manager" ] && [ -x "$engine_manager" ] || exit 0

# prerm runs as root, while the records, the dotfiles, and $XDG_CONFIG_HOME all
# belong to the user who ran the app. Resolve that user the same way
# after-remove resolves the data root on purge, and run as them so the binary
# reads the environment it wrote under. Best-effort: on a multi-user box, other
# users' entries are left for their own reinstall to reclaim.
real_user="${SUDO_USER:-}"
if [ -z "$real_user" ] && command -v logname >/dev/null 2>&1; then
  real_user="$(logname 2>/dev/null || true)"
fi
[ -n "$real_user" ] && [ "$real_user" != root ] || exit 0

if command -v runuser >/dev/null 2>&1; then
  runuser -u "$real_user" -- "$engine_manager" --remove-user-path >/dev/null 2>&1 || true
else
  su -s /bin/sh -c "'$engine_manager' --remove-user-path" "$real_user" >/dev/null 2>&1 || true
fi

exit 0
