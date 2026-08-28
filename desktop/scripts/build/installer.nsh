# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

; REFERENCE FILE. These are the NSIS hooks electron-builder runs when NVIDIA
; builds the Windows installer, kept here so the process teardown, firewall
; rules, and user-data handling are inspectable. Building an installer yourself
; produces an unsigned artifact — signing lives outside this repository — so this
; is not a supported distribution path. For local use, `npm run build` and
; `npm start` need none of this.

!macro pairCloseRunningProcesses
  DetailPrint "Checking for running Personal AI Router processes..."
  ; taskkill exits non-zero when the image isn't running; nsExec routes that
  ; into the log and we never check the return code, so a missing process is a
  ; no-op and never aborts the (un)installer. Keep this list in sync with
  ; MODULAR_RUNTIME_BINARIES and MODULAR_BUNDLED_BINARIES in
  ; src/shared/constants/modular-binaries.ts.
  nsExec::ExecToLog 'taskkill /F /T /IM "${APP_EXECUTABLE_FILENAME}"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-tui.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "ollama-proxy.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "lmstudio-proxy.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-node-info.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-node-scanner.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-manual-nodes.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-node-settings.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-engine-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-workload-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-cluster-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-job-scheduler.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-errors.exe"'
  nsExec::ExecToLog 'taskkill /F /T /IM "nvpair-ui-broker.exe"'
  Sleep 500
!macroend

; Best-effort removal of per-user data on a genuine uninstall. Runs from
; customUnInstall (inside the main uninstall section, BEFORE the template's
; RMDir /r $INSTDIR and quitSuccess) — a one-click uninstaller Quits at the end
; of that section, so an appended customUnInstallSection would never execute.
;
; Per-user data here means settings, logs, cluster identity and certificates,
; and engines Personal AI Router installed. Downloaded model weights live
; outside these roots and are never touched.
;
; UNINSTALLER SAFETY: only ever delete the three per-user AppData roots below.
; Never touch $INSTDIR (C:\Program Files\...). The uninstaller itself lives at
; $INSTDIR\Uninstall ${PRODUCT_FILENAME}.exe and is removed by the template's
; own RMDir /r $INSTDIR + DeleteRegKey; deleting it here would orphan the
; Add/Remove-Programs entry. Likewise never Abort/Quit from these hooks, or the
; template never reaches DeleteRegKey.
;
; RMDir /r is best-effort: locked files (e.g. a running engine binary) are
; skipped, the error flag is set, and the uninstall continues — it never aborts.
;
; Uses ReadEnvStr for LOCALAPPDATA so elevated per-machine uninstall
; targets the invoking user's profile, not the admin shell context.
;
; Per-user data roots (kept in sync with src/shared/constants/app.ts
; APP_ORG/APP_DATA_DIR_NAME and the Go appdir
; "Nvidia Corporation/Personal AI Router"):
;   - Shared app data:    %LOCALAPPDATA%\Nvidia Corporation\Personal AI Router
;   - Previous app data:  %LOCALAPPDATA%\NVIDIA Corporation\PAIR
;   - electron-updater:   %LOCALAPPDATA%\nvpair-updater
; On Windows the "NVIDIA Corporation" / "Nvidia Corporation" casing resolves to
; the same case-insensitive folder.
; Per-user data roots. Keep in sync with src/shared/constants/app.ts and the
; append-only inventory in repo-root scripts/wipe-app-data.ps1 / wipe-app-data.sh.
!macro pairRemoveUserData
  DetailPrint "Removing Personal AI Router user data..."
  ClearErrors
  ReadEnvStr $0 LOCALAPPDATA
  RMDir /r "$0\Nvidia Corporation\Personal AI Router"
  RMDir /r "$0\NVIDIA Corporation\PAIR"
  RMDir "$0\NVIDIA Corporation"
  RMDir /r "$0\nvpair-updater"
  ClearErrors
!macroend

; Best-effort: when the user opts to remove data, stop any process whose
; executable lives UNDER one of the data roots (e.g. an engine like Ollama
; running from %LOCALAPPDATA%\Nvidia Corporation\Personal AI Router\engine-bin\)
; so RMDir /r can then delete it. Scoped by ExecutablePath, so an external
; Ollama installed elsewhere is never touched. nsExec::ExecToLog never aborts the (un)installer,
; PowerShell swallows its own errors (-ErrorAction SilentlyContinue), and a
; missing powershell.exe just logs a failure — every failure mode lets the
; uninstall continue. PowerShell reads $env:LOCALAPPDATA itself so usernames
; containing spaces need no extra quoting.
!macro pairKillProcessesInDataDirs
  DetailPrint "Stopping any processes running from Personal AI Router data folders..."
  nsExec::ExecToLog 'powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "Get-CimInstance Win32_Process | Where-Object { $$_.ExecutablePath -and ($$_.ExecutablePath -like \"$$env:LOCALAPPDATA\NVIDIA Corporation\PAIR\*\" -or $$_.ExecutablePath -like \"$$env:LOCALAPPDATA\Nvidia Corporation\Personal AI Router\*\") } | ForEach-Object { Stop-Process -Id $$_.ProcessId -Force -ErrorAction SilentlyContinue }"'
  ; Give the OS a moment to release the file handles before RMDir /r.
  Sleep 1000
!macroend

; Non-fatal fallback: if something still held a binary open under a data root
; (e.g. %LOCALAPPDATA%\Nvidia Corporation\Personal AI Router\engine-bin\) and the kill above
; didn't catch it in time, RMDir /r leaves it behind. List whatever survived and
; continue — the uninstall must never abort over locked user data.
!macro pairWarnIfDataRemains
  ReadEnvStr $0 LOCALAPPDATA
  StrCpy $9 ""
  IfFileExists "$0\Nvidia Corporation\Personal AI Router\*.*" 0 +2
    StrCpy $9 "$9$\n$0\Nvidia Corporation\Personal AI Router"
  IfFileExists "$0\NVIDIA Corporation\PAIR\*.*" 0 +2
    StrCpy $9 "$9$\n$0\NVIDIA Corporation\PAIR"
  IfFileExists "$0\nvpair-updater\*.*" 0 +2
    StrCpy $9 "$9$\n$0\nvpair-updater"
  StrCmp $9 "" pairNoLeftover
    MessageBox MB_OK|MB_ICONEXCLAMATION "Some Personal AI Router data could not be removed because files were still in use (for example, a running engine such as Ollama).$\n$\nClose those programs, then delete these folders manually:$9"
  pairNoLeftover:
!macroend

; Rules use `profile=any remoteip=localsubnet` (NOT profile=private,domain).
; A LAN-discovery product must work even when Windows has classified the home
; network as Public (common on laptops / when the user declined network
; discovery) — private,domain-scoped rules silently don't apply on a Public
; network, so inbound TCP 14318 (node-info) is dropped and peers can discover
; the node over mDNS but never complete the node-info handshake (no metrics,
; missing from "Available nodes"). Scoping to localsubnet keeps the ports
; closed to anything off the local link, so covering all profiles does not
; expose the node on untrusted public networks.
!macro pairAddFirewallRules
  DetailPrint "Adding Personal AI Router firewall rules..."
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Ollama Proxy" dir=in action=allow program="$INSTDIR\resources\cli-bin\ollama-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router LM Studio Proxy" dir=in action=allow program="$INSTDIR\resources\cli-bin\lmstudio-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Node Info" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-node-info.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Node Scanner" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-node-scanner.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Workload Manager" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-workload-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Errors" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-errors.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Cluster Manager" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-cluster-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Engine Manager" dir=in action=allow program="$INSTDIR\resources\cli-bin\nvpair-engine-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\ollama-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS LM Studio Proxy (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\lmstudio-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS Node Info (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\nvpair-node-info.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS Node Scanner (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\nvpair-node-scanner.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS Workload Manager (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\nvpair-workload-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router mDNS Errors (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\resources\cli-bin\nvpair-errors.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Workload Manager (TCP 14320)" dir=in action=allow protocol=TCP localport=14320 program="$INSTDIR\resources\cli-bin\nvpair-workload-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Cluster Manager (TCP 14321)" dir=in action=allow protocol=TCP localport=14321 program="$INSTDIR\resources\cli-bin\nvpair-cluster-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="Personal AI Router Engine Manager (TCP 14322)" dir=in action=allow protocol=TCP localport=14322 program="$INSTDIR\resources\cli-bin\nvpair-engine-manager.exe" enable=yes profile=any remoteip=localsubnet'
!macroend

!macro pairRemoveFirewallRules
  DetailPrint "Removing Personal AI Router firewall rules..."
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Ollama Proxy"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router LM Studio Proxy"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Node Info"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Node Scanner"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Workload Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Errors"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Cluster Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Engine Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS LM Studio Proxy (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS Node Info (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS Node Scanner (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS Workload Manager (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router mDNS Errors (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Workload Manager (TCP 14320)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Cluster Manager (TCP 14321)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="Personal AI Router Engine Manager (TCP 14322)"'
  ; Remove pre-rebrand rules left by older installations.
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Ollama Proxy"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR LM Studio Proxy"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Node Info"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Node Scanner"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Workload Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Errors"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Cluster Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Engine Manager"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS LM Studio Proxy (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS Node Info (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS Node Scanner (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS Workload Manager (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR mDNS Errors (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Workload Manager (TCP 14320)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Cluster Manager (TCP 14321)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="PAIR Engine Manager (TCP 14322)"'
!macroend

!macro customHeader
  ; "1" => a real user-initiated uninstall (prompt before removing data).
  ; "0" => a machine-driven /S run (auto-update or scripted) — never prompt.
  ;
  ; electron-builder inserts customHeader in BOTH makensis passes, but the
  ; uninstaller code that sets/reads this var (customUnInit / customUnInstall in
  ; uninstaller.nsh) is only compiled when BUILD_UNINSTALLER is defined. Declaring
  ; the var unconditionally makes the main installer pass emit "warning 6001:
  ; Variable not referenced or never set", which electron-builder treats as a
  ; fatal error. Guard the declaration to the uninstaller pass where it's used.
  !ifdef BUILD_UNINSTALLER
    Var /GLOBAL PairInteractiveUninstall
  !endif
!macroend

!macro customInit
  ; Pin the install root to 64-bit Program Files on every architecture.
  ;
  ; electron-builder's multiUser.nsh starts from $PROGRAMFILES — which is
  ; "C:\Program Files (x86)", because the NSIS stub is a 32-bit x86 executable —
  ; and only upgrades to $PROGRAMFILES64 inside `!ifdef APP_64`. That define
  ; exists only when x64 is one of the architectures packed into the installer.
  ; These builds are one installer per architecture (electron-builder.config.ts
  ; narrows the target archs from --x64/--arm64), so the arm64 installer defines
  ; APP_ARM64 alone, the upgrade branch is never compiled in, and an ARM64
  ; machine installs a 64-bit app under "Program Files (x86)". The !ifdef is
  ; compile-time, so no runtime CPU check can correct it from the template side.
  ;
  ; customInit runs after initMultiUser, so a path from the registry or /D has
  ; already been applied and only the untouched default still matches. An
  ; install already registered at the x86 path matches the same string; the
  ; template's uninstallOldVersion runs the old uninstaller against its
  ; registered location first, so that directory is removed rather than orphaned.
  ${if} $INSTDIR == "$PROGRAMFILES\${APP_FILENAME}"
    StrCpy $INSTDIR "$PROGRAMFILES64\${APP_FILENAME}"
  ${endif}
  !insertmacro pairCloseRunningProcesses
!macroend

; Refuse to report success when the payload's executables are not on disk.
;
; electron-builder's extractUsing7za retries CopyFiles five times, then falls
; back to running Nsis7z::Extract straight into $INSTDIR and ignores the result.
; Anything that blocks writing the .exe files while the rest of the tree copies
; fine — a delete-pending lock left by the previous version's uninstaller, or an
; endpoint-protection agent removing binaries as they land — therefore finishes
; as a "successful" install: registry entries written, shortcuts created, and no
; application behind them. That is what a user sees as "PAIR.exe has been
; changed or moved" from a shortcut that never had a target.
;
; Checking the app executable and the broker is enough: they come from the same
; archive, so if those two arrived the extraction wrote executables.
;
; Runs before the firewall rules, which are pointless without the binaries they
; name. The shortcuts are removed so no broken launcher is left behind, but the
; uninstaller and its Add/Remove entry are deliberately kept — they are the only
; way for the user to clear the partial install.
;
; /SD IDOK, unlike the uninstall data prompt below, which deliberately omits it.
; A MessageBox with no /SD is still displayed in NSIS silent mode, and the two
; hooks need opposite behavior there: a one-click uninstaller forces SetSilent
; silent even for a human, so its prompt must render, whereas the installer is
; only silent when /S was passed — that is, driven by electron-updater or a
; script with nobody to click OK. Without a silent default this modal would hold
; an auto-update's installer open indefinitely instead of letting SetErrorLevel
; report the failure to its caller.
!macro pairAssertPayloadInstalled
  StrCpy $9 ""
  ${ifNot} ${FileExists} "$INSTDIR\${APP_EXECUTABLE_FILENAME}"
    StrCpy $9 "$9$\n$INSTDIR\${APP_EXECUTABLE_FILENAME}"
  ${endif}
  ${ifNot} ${FileExists} "$INSTDIR\resources\cli-bin\nvpair-ui-broker.exe"
    StrCpy $9 "$9$\n$INSTDIR\resources\cli-bin\nvpair-ui-broker.exe"
  ${endif}
  ${if} $9 != ""
    DetailPrint "Installation incomplete: executables missing from $INSTDIR"
    Delete "$newDesktopLink"
    Delete "$newStartMenuLink"
    ClearErrors
    MessageBox MB_OK|MB_ICONSTOP "Personal AI Router could not be installed.$\n$\nThe installer unpacked its files but these executables are missing:$9$\n$\nThis usually means security software removed them, or a previous version was still running and held them open. Close Personal AI Router, allow it in your security software, and run the installer again.$\n$\nUninstall the partial installation from Add/Remove Programs first if the problem repeats." /SD IDOK
    SetErrorLevel 2
    Quit
  ${endif}
!macroend

!macro customInstall
  !insertmacro pairAssertPayloadInstalled
  !insertmacro pairAddFirewallRules
!macroend

; Detect a genuine silent run on the ORIGINAL command line. A one-click
; uninstaller forces SetSilent silent right after its "are you sure?" prompt
; (uninstaller.nsh), so ${Silent} is true even when a human clicked Uninstall;
; only the presence of /S on the command line distinguishes machine-driven runs
; (electron-updater passes /S --updated). GetParameters/GetOptions are already
; used by the template's un.onInit, so FileFunc is available here.
!macro customUnInit
  StrCpy $PairInteractiveUninstall "1"
  ClearErrors
  ${GetParameters} $R0
  ${GetOptions} $R0 "/S" $R1
  ${ifNot} ${Errors}
    StrCpy $PairInteractiveUninstall "0"
  ${endif}
!macroend

; Runs inside the main uninstall section, BEFORE the template removes $INSTDIR
; and before quitSuccess. Stops the app's own processes, removes firewall rules,
; and (on a genuine, non-update uninstall) optionally removes per-user data.
; Never touch $INSTDIR here, and never Abort/Quit — see pairRemoveUserData.
!macro customUnInstall
  !insertmacro pairCloseRunningProcesses
  !insertmacro pairRemoveFirewallRules

  ; Skip all data handling during an auto-update reinstall.
  ${ifNot} ${isUpdated}
    StrCpy $8 "0" ; default: KEEP data (also the machine-driven default)
    ${if} $PairInteractiveUninstall == "1"
      ; A plain MessageBox (no /SD) still displays in NSIS silent mode, so the
      ; one-click interactive uninstall shows this prompt. Default button = No.
      MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also remove all Personal AI Router data?$\n$\nThis permanently deletes your settings, logs, cluster identity and certificates, and any engines Personal AI Router installed. Downloaded models are not removed. Click No to keep your data for a future reinstall." IDYES pairDataYes IDNO pairDataDone
      pairDataYes:
        StrCpy $8 "1"
      pairDataDone:
    ${endif}
    ${if} $8 == "1"
      !insertmacro pairKillProcessesInDataDirs
      !insertmacro pairRemoveUserData
      !insertmacro pairWarnIfDataRemains
    ${endif}
  ${endif}
!macroend
