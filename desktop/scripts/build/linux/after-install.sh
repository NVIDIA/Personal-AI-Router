#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Personal AI Router custom Debian post-install. This script REPLACES electron-builder's default
# after-install.tpl, so it must re-create everything that template provides (PATH
# launcher, Chromium sandbox permissions, AppArmor profile) and THEN run the
# Personal AI Router-specific desktop/icon cache refresh. ${executable} and ${sanitizedProductName}
# are expanded by electron-builder at build time; every other $VAR is a shell
# variable and is intentionally left unbraced -- the macro expander only rewrites
# ${[A-Za-z]+} and throws on an unknown all-letter macro, so braced shell vars are
# unsafe here. No `set -e`: a Debian maintainer script must never abort the install
# on a best-effort step.

# 1. Put a launcher on PATH so the `${executable}` command resolves to the bundled
#    terminal UI (nvpair-tui, which spawns its own broker) — the terminal command
#    is the TUI on every platform. The desktop entry's Exec uses the full /opt
#    path, so the graphical app is unaffected by this wrapper.
#    First clear any prior alternative/symlink from an older package layout.
if type update-alternatives >/dev/null 2>&1; then
    update-alternatives --remove '${executable}' '/opt/${sanitizedProductName}/${executable}' >/dev/null 2>&1 || true
fi
rm -f '/usr/bin/${executable}'
# Quoted heredoc: $@ stays literal in the written file, while electron-builder
# still expands the ${sanitizedProductName} / ${executable} build-time macros
# before this script ships.
cat > '/usr/bin/${executable}' <<'NVPAIR_WRAP'
#!/bin/sh
# Personal AI Router terminal UI launcher.
exec '/opt/${sanitizedProductName}/resources/cli-bin/nvpair-tui' "$@"
NVPAIR_WRAP
chmod 0755 '/usr/bin/${executable}'

# 2. Force the SUID Chromium sandbox helper. The default template only sets 4755
#    when unprivileged user namespaces look unavailable, but that probe runs as
#    root in the postinst (root can always unshare) so it resolves to 0755 and
#    then relies entirely on the AppArmor profile below to grant userns to the
#    unprivileged user. On Ubuntu 24.04
#    (kernel.apparmor_restrict_unprivileged_userns=1) that leaves the sandbox
#    broken unless the profile loads -- exactly the "must launch with
#    --no-sandbox / gray window" failure. Setting SUID unconditionally makes the
#    setuid sandbox work regardless of userns/AppArmor state; the bundled profile
#    is unconfined, so the two sandbox paths do not conflict.
chmod 4755 '/opt/${sanitizedProductName}/chrome-sandbox' || true

# 3. Refresh MIME + desktop databases (from the default template).
if hash update-mime-database 2>/dev/null; then
    update-mime-database /usr/share/mime || true
fi
if hash update-desktop-database 2>/dev/null; then
    update-desktop-database /usr/share/applications || true
fi

# 4. Install the AppArmor profile (Ubuntu 24+) so the unprivileged-userns path
#    also works and the app is registered with AppArmor. Self-guarding: if the
#    bundled profile is missing or this AppArmor version cannot parse it (e.g.
#    Ubuntu 22.04 lacks abi/4.0), the dry run fails and we skip it -- the app
#    still runs via the SUID sandbox set above.
if apparmor_status --enabled >/dev/null 2>&1; then
    APPARMOR_PROFILE_SOURCE='/opt/${sanitizedProductName}/resources/apparmor-profile'
    APPARMOR_PROFILE_TARGET='/etc/apparmor.d/${executable}'
    if [ -f "$APPARMOR_PROFILE_SOURCE" ] && apparmor_parser --skip-kernel-load --debug "$APPARMOR_PROFILE_SOURCE" >/dev/null 2>&1; then
        cp -f "$APPARMOR_PROFILE_SOURCE" "$APPARMOR_PROFILE_TARGET" || true
        # Skip live load inside a chroot (image-build environments); mirror dh_apparmor's -W -T.
        if ! { [ -x '/usr/bin/ischroot' ] && /usr/bin/ischroot; } && hash apparmor_parser 2>/dev/null; then
            apparmor_parser --replace --write-cache --skip-read-cache "$APPARMOR_PROFILE_TARGET" || true
        fi
    else
        echo "Skipping AppArmor profile install (missing or unsupported by this AppArmor version)"
    fi
fi

# 5. Personal AI Router desktop/icon cache refresh.
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
fi

exit 0
