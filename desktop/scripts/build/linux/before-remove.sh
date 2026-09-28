#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Personal AI Router Debian pre-remove. Keeps a copy of nvpair-engine-manager so
# that after-remove can release the user's PATH entries on `purge`. No `set -e`,
# and a trailing `exit 0`, so a best-effort step never fails the removal.
#
# Unlike after-install.sh and after-remove.sh, this one reaches fpm through the
# raw passthrough in electron-builder.config.ts, so ${macro} is NOT expanded
# here. It finds its own paths at runtime instead.
#
# WHY A COPY. Engine-manager records what it added to PATH under the user's data
# root (engine-bin/engine-path/), while the entries themselves live in the login
# shell's profiles, which no maintainer script touches. Only a purge deletes the
# data root, and only postrm can tell a purge from a plain remove -- prerm sees
# "remove" either way. But dpkg deletes the package's files before postrm runs,
# and a purge can come long after the remove, so the binary that understands the
# records has to outlive the package. It is kept under /var/lib/<package>, which
# after-remove deletes on purge.
#
# Releasing here instead would give up the entries on a plain `apt remove`, which
# keeps the engines: a reinstalled app would then have nothing that republishes
# them short of reinstalling each engine.
#
# dpkg calls prerm with "upgrade" during an update, and "failed-upgrade" when
# recovering from one. Those keep the installation, so there is nothing to keep.
case "${1:-}" in
  upgrade|failed-upgrade)
    exit 0
    ;;
esac

# Ask dpkg where it put the binary rather than rebuilding /opt/<product>/...
# from a name this script cannot be told at build time.
package="${DPKG_MAINTSCRIPT_PACKAGE:-}"
[ -n "$package" ] || exit 0
command -v dpkg-query >/dev/null 2>&1 || exit 0
engine_manager="$(dpkg-query -L "$package" 2>/dev/null \
  | grep -E '/cli-bin/nvpair-engine-manager$' | head -n 1)"
[ -n "$engine_manager" ] && [ -x "$engine_manager" ] || exit 0

mkdir -p "/var/lib/$package" 2>/dev/null \
  && install -m 0755 "$engine_manager" "/var/lib/$package/nvpair-engine-manager" 2>/dev/null \
  || true

exit 0
