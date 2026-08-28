# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

; NVIDIA Personal AI Router - NSIS Installer Script
; Requires NSIS 3.x with MUI2
;
; REFERENCE FILE. This is the NSIS definition NVIDIA builds the Windows services
; bundle from, kept here so the install layout, firewall rules, and uninstall
; behavior are inspectable. It is not a supported way to produce a distributable
; installer: signing lives outside this repository, so anything built from this
; file is unsigned. To run the services locally, use build.bat and launch the
; binaries from build\bin.

!include "MUI2.nsh"
!include "FileFunc.nsh"

;---------------------------------------
; General
;---------------------------------------
!define PRODUCT_NAME "NVIDIA Personal AI Router"
!define PRODUCT_PUBLISHER "NVIDIA"
!define PRODUCT_URL "https://github.com/NVIDIA/Personal-AI-Router"

; Version can be overridden from CLI: makensis /DPRODUCT_VERSION=1.2.3
!ifndef PRODUCT_VERSION
  !define PRODUCT_VERSION "0.1.0"
!endif

!define ARP_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"

; Build a Unicode (UTF-16) installer rather than the legacy ANSI target.
; Silences makensis warning 7998 and makes the installer behave correctly for
; users whose profile paths contain non-ASCII characters.
Unicode true

Name "${PRODUCT_NAME} ${PRODUCT_VERSION}"
OutFile "..\dist\NVIDIA-Personal-AI-Router-${PRODUCT_VERSION}-Setup.exe"
InstallDir "$PROGRAMFILES64\${PRODUCT_NAME}"
InstallDirRegKey HKLM "${ARP_KEY}" "InstallLocation"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

;---------------------------------------
; MUI2 Configuration
;---------------------------------------
!define MUI_ABORTWARNING
!define MUI_ICON "${NSISDIR}\Contrib\Graphics\Icons\modern-install.ico"
!define MUI_UNICON "${NSISDIR}\Contrib\Graphics\Icons\modern-uninstall.ico"

; Pages
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "EULA.txt"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

; Uninstaller pages
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

; Language
!insertmacro MUI_LANGUAGE "English"

;---------------------------------------
; .onInit — detect a previous install and offer to upgrade. We only SET A
; FLAG here; the actual uninstall is deferred until the user reaches the
; Install section so that cancelling on the Welcome/Directory pages does
; not leave them with neither the old nor the new version.
;---------------------------------------
Var PrevUninstaller
Var PrevInstallDir

Function .onInit
  ReadRegStr $PrevUninstaller HKLM "${ARP_KEY}" "UninstallString"
  ReadRegStr $PrevInstallDir  HKLM "${ARP_KEY}" "InstallLocation"
  StrCmp $PrevUninstaller "" done

  ; Strip surrounding quotes that WriteRegStr added so ExecWait can run it.
  StrCpy $R0 $PrevUninstaller 1
  StrCmp $R0 '"' 0 +3
    StrCpy $PrevUninstaller $PrevUninstaller "" 1
    StrCpy $PrevUninstaller $PrevUninstaller -1

  ; Inform the user up-front (but don't touch anything yet). Silent-mode
  ; installs just proceed.
  IfSilent done 0
  MessageBox MB_OKCANCEL|MB_ICONQUESTION \
    "${PRODUCT_NAME} is already installed.$\n$\nClick OK to continue - the previous version will be removed when you click Install on the next screen. Click Cancel to abort." \
    /SD IDOK IDOK done
  Abort

done:
FunctionEnd

;---------------------------------------
; CloseRunningInstance — terminates any NVPAIR process that might hold a
; file lock on a binary we are about to overwrite. Called before both
; the previous-version uninstall and the file-copy step so that neither
; can fail with a sharing violation.
;
; We use taskkill (always present on supported Windows) rather than a
; third-party plugin so the installer has no external dependencies.
; taskkill exits non-zero when the image is not found; that is fine —
; nsExec::ExecToLog routes its output into the installer log rather
; than surfacing it as an error, and we deliberately do not check the
; return code.
;---------------------------------------
!macro CloseRunningInstance
  DetailPrint "Checking for running ${PRODUCT_NAME} processes..."
  nsExec::ExecToLog 'taskkill /F /IM "ollama-proxy.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "lmstudio-proxy.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-node-info.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-node-scanner.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-manual-nodes.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-workload-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-errors.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-engine-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-node-settings.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-cluster-manager.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-job-scheduler.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-ui-broker.exe"'
  nsExec::ExecToLog 'taskkill /F /IM "nvpair-tui.exe"'
  ; Give Windows a moment to release the handles.
  Sleep 500
!macroend

;---------------------------------------
; Installer Section
;---------------------------------------
Section "Install"
  ; Make sure nothing is holding file locks on the binaries we are about
  ; to touch — otherwise File / the prior uninstaller's Delete will fail
  ; with a sharing violation and leave a half-installed mess.
  !insertmacro CloseRunningInstance

  ; If .onInit found a previous install, remove it now — at this point the
  ; user has clicked Install, so the destructive step is safe.
  StrCmp $PrevUninstaller "" skip_prev_uninstall
    DetailPrint "Removing previous version..."
    ; _?=$PrevInstallDir keeps the old uninstaller in its dir so ExecWait
    ; actually waits; we then clean up the stray uninstall.exe ourselves.
    ClearErrors
    ExecWait '"$PrevUninstaller" /S _?=$PrevInstallDir' $R3
    Delete "$PrevInstallDir\uninstall.exe"
  skip_prev_uninstall:

  SetOutPath "$INSTDIR"
  File "EULA.txt"

  SetOutPath "$INSTDIR\bin"
  ; ollama-proxy's listen port is now a dual-protocol endpoint: loopback
  ; plaintext for local clients, and pin-gated cluster mTLS for peers (it
  ; forwards a validated peer straight to the loopback engine). Plaintext
  ; requests from the LAN are rejected in-process, so only pinned cluster
  ; members can run inference — the firewall rule below still allows the
  ; executable because that same user-configurable port carries the mTLS ingress.
  File "..\build\bin\ollama-proxy.exe"
  ; lmstudio-proxy fronts the cluster's LM Studio engines (OpenAI API) with the
  ; same dual-protocol port (loopback plaintext + cluster mTLS ingress); it
  ; listens for clients and browses mDNS, so it gets firewall rules below.
  File "..\build\bin\lmstudio-proxy.exe"
  File "..\build\bin\nvpair-node-info.exe"
  File "..\build\bin\nvpair-node-scanner.exe"
  File "..\build\bin\nvpair-manual-nodes.exe"
  File "..\build\bin\nvpair-workload-manager.exe"
  ; nvpair-errors runs with --peer-sync (nvpair-ui-broker passes it), so it
  ; serves the cross-node ingest endpoint on TCP :14319 and advertises
  ; over mDNS — it gets firewall rules below like the other networked
  ; subprocesses. nvpair-node-settings is pure stdio (no listening port), so it
  ; needs no rule.
  File "..\build\bin\nvpair-errors.exe"
  File "..\build\bin\nvpair-node-settings.exe"
  ; nvpair-engine-manager serves the read-only model list on TCP :14322 (models-http)
  ; and, when clustered, the mTLS remote-control surface on TCP :14323 (ec), so it
  ; gets firewall rules below like the other networked binaries.
  File "..\build\bin\nvpair-engine-manager.exe"
  ; nvpair-cluster-manager advertises over mDNS and serves the inter-node pairing /
  ; mTLS channel on TCP :14321, so it gets firewall rules below.
  File "..\build\bin\nvpair-cluster-manager.exe"
  ; nvpair-job-scheduler is pure stdio (no listening port, no mDNS), so it needs no
  ; firewall rule — it runs under nvpair-ui-broker and only ranks nodes for the proxies.
  File "..\build\bin\nvpair-job-scheduler.exe"
  ; nvpair-ui-broker is the primary entry point: it talks JSON-RPC over stdio / a
  ; named pipe only, so it needs no inbound firewall rule. The graphical UI
  ; bundled alongside this backend launches it to drive the NVPAIR API, and it
  ; supervises the bundled workers.
  File "..\build\bin\nvpair-ui-broker.exe"
  ; nvpair-tui is a terminal client that spawns and supervises nvpair-ui-broker
  ; over stdio for headless / SSH operation. It has no listening port, so
  ; it needs no firewall rule.
  File "..\build\bin\nvpair-tui.exe"

  ; Write uninstaller
  WriteUninstaller "$INSTDIR\uninstall.exe"

  ; This backend installer owns only the uninstall shortcut. Product packaging
  ; places the graphical UI in the same installation directory and owns its app
  ; launcher.
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortcut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

  ; Add/Remove Programs registry
  WriteRegStr   HKLM "${ARP_KEY}" "DisplayName"     "${PRODUCT_NAME}"
  WriteRegStr   HKLM "${ARP_KEY}" "DisplayVersion"  "${PRODUCT_VERSION}"
  WriteRegStr   HKLM "${ARP_KEY}" "Publisher"        "${PRODUCT_PUBLISHER}"
  WriteRegStr   HKLM "${ARP_KEY}" "URLInfoAbout"     "${PRODUCT_URL}"
  WriteRegStr   HKLM "${ARP_KEY}" "InstallLocation"  "$INSTDIR"
  WriteRegStr   HKLM "${ARP_KEY}" "UninstallString"  '"$INSTDIR\uninstall.exe"'
  WriteRegStr   HKLM "${ARP_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKLM "${ARP_KEY}" "NoModify" 1
  WriteRegDWORD HKLM "${ARP_KEY}" "NoRepair" 1

  ; Compute installed size for ARP
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKLM "${ARP_KEY}" "EstimatedSize" $0

  ; Firewall exceptions.
  ;
  ; Rules use `profile=any remoteip=localsubnet` (NOT profile=private,domain).
  ; A LAN-discovery product must work even when Windows has classified the home
  ; network as Public (common on laptops, or when the user declined network
  ; discovery at the "make this PC discoverable?" prompt) — private,domain-scoped
  ; rules silently don't apply on a Public network, so inbound TCP 14318
  ; (node-info) is dropped and peers discover the node over mDNS but never
  ; complete the node-info handshake (no metrics, missing from "Available
  ; nodes"). Scoping to localsubnet keeps the ports closed to anything off the
  ; local link, so covering all profiles does not expose the node on untrusted
  ; public networks.
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Ollama Proxy" dir=in action=allow program="$INSTDIR\bin\ollama-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR LM Studio Proxy" dir=in action=allow program="$INSTDIR\bin\lmstudio-proxy.exe" enable=yes profile=any remoteip=localsubnet'

  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Node Info" dir=in action=allow program="$INSTDIR\bin\nvpair-node-info.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Node Scanner" dir=in action=allow program="$INSTDIR\bin\nvpair-node-scanner.exe" enable=yes profile=any remoteip=localsubnet'

  ; mDNS needs UDP 5353 inbound
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\ollama-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS LM Studio Proxy (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\lmstudio-proxy.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS Node Info (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\nvpair-node-info.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS Node Scanner (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\nvpair-node-scanner.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS Workload Manager (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\nvpair-workload-manager.exe" enable=yes profile=any remoteip=localsubnet'

  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Manual Nodes" dir=in action=allow program="$INSTDIR\bin\nvpair-manual-nodes.exe" enable=yes profile=any remoteip=localsubnet'

  ; Workload manager accepts peer lifecycle events over TCP 14320 inbound.
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Workload Manager (TCP 14320)" dir=in action=allow protocol=TCP localport=14320 program="$INSTDIR\bin\nvpair-workload-manager.exe" enable=yes profile=any remoteip=localsubnet'

  ; nvpair-errors cross-node sync: HTTP ingest on TCP :14319 + mDNS (UDP 5353)
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Errors" dir=in action=allow program="$INSTDIR\bin\nvpair-errors.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS Errors (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\nvpair-errors.exe" enable=yes profile=any remoteip=localsubnet'

  ; nvpair-cluster-manager: inter-node pairing + mTLS on TCP :14321 and mDNS (UDP 5353)
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Cluster Manager (TCP 14321)" dir=in action=allow protocol=TCP localport=14321 program="$INSTDIR\bin\nvpair-cluster-manager.exe" enable=yes profile=any remoteip=localsubnet'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR mDNS Cluster Manager (UDP 5353)" dir=in action=allow protocol=UDP localport=5353 program="$INSTDIR\bin\nvpair-cluster-manager.exe" enable=yes profile=any remoteip=localsubnet'

  ; nvpair-engine-manager serves the model list to peers on TCP :14322 (models-http).
  ; No mDNS rule: it doesn't advertise itself — the node-scanner daemon carries em= in the shared record.
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Engine Manager (TCP 14322)" dir=in action=allow protocol=TCP localport=14322 program="$INSTDIR\bin\nvpair-engine-manager.exe" enable=yes profile=any remoteip=localsubnet'

  ; nvpair-engine-manager also serves the cluster-scoped remote-control surface (ec)
  ; to paired peers over mutually-authenticated TLS on TCP :14323. It only binds
  ; when this node is clustered, but the rule is added unconditionally like the rest.
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="NVPAIR Engine Manager Control (TCP 14323)" dir=in action=allow protocol=TCP localport=14323 program="$INSTDIR\bin\nvpair-engine-manager.exe" enable=yes profile=any remoteip=localsubnet'
SectionEnd

;---------------------------------------
; Uninstaller Section
;---------------------------------------
Section "Uninstall"
  ; Kill any running instance so Delete does not hit a sharing violation.
  !insertmacro CloseRunningInstance

  ; Remove firewall exceptions
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Ollama Proxy"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Node Info"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Node Scanner"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS Node Info (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS Node Scanner (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Manual Nodes"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Workload Manager (TCP 14320)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS Workload Manager (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Errors"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS Errors (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Cluster Manager (TCP 14321)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR mDNS Cluster Manager (UDP 5353)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Engine Manager (TCP 14322)"'
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="NVPAIR Engine Manager Control (TCP 14323)"'

  ; Remove files
  Delete "$INSTDIR\bin\ollama-proxy.exe"
  Delete "$INSTDIR\bin\lmstudio-proxy.exe"
  Delete "$INSTDIR\bin\nvpair-node-info.exe"
  Delete "$INSTDIR\bin\nvpair-node-scanner.exe"
  Delete "$INSTDIR\bin\nvpair-manual-nodes.exe"
  Delete "$INSTDIR\bin\nvpair-workload-manager.exe"
  Delete "$INSTDIR\bin\nvpair-errors.exe"
  Delete "$INSTDIR\bin\nvpair-engine-manager.exe"
  Delete "$INSTDIR\bin\nvpair-node-settings.exe"
  Delete "$INSTDIR\bin\nvpair-cluster-manager.exe"
  Delete "$INSTDIR\bin\nvpair-job-scheduler.exe"
  Delete "$INSTDIR\bin\nvpair-ui-broker.exe"
  Delete "$INSTDIR\bin\nvpair-tui.exe"
  RMDir  "$INSTDIR\bin"
  Delete "$INSTDIR\EULA.txt"
  Delete "$INSTDIR\uninstall.exe"
  RMDir  "$INSTDIR"

  ; Remove Start Menu
  Delete "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk"
  RMDir  "$SMPROGRAMS\${PRODUCT_NAME}"

  ; Remove registry
  DeleteRegKey HKLM "${ARP_KEY}"
SectionEnd
