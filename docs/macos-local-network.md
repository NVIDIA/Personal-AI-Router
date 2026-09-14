<!--
SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
SPDX-License-Identifier: Apache-2.0
-->

# Local network access on macOS 15+

Node discovery is mDNS. macOS 15 (Sequoia) put every local-network operation
behind a per-app grant — unicast to your own subnet, and *all* multicast, which
is what mDNS is. Without that grant the sends do not merely go unanswered, they
fail outright:

```
mdns send: query did not leave any interface service=_nvpair-node._tcp
  errors="en0 write: write udp4 192.168.1.36:56003->224.0.0.251:5353: sendto: no route to host"
```

`sendto: no route to host` is `EHOSTUNREACH`, which reads like a routing fault
and is not one. The tell is that traffic *through* the router keeps working
while anything *on* the subnet fails, and that the same send succeeds from a
shell on the same machine.

## Two things have to be true

**1. The app must have a usage string.** macOS only offers the grant to a bundle
whose `Info.plist` carries `NSLocalNetworkUsageDescription`. With no string
there is nothing to put in the prompt, so no prompt appears, the app never
enters *System Settings → Privacy & Security → Local Network*, and every send
fails silently. The packaged app declares it via `mac.extendInfo` in
`desktop/electron-builder.config.ts`, alongside `NSBonjourServices` listing the
service types the scanner registers and browses.

**2. The grant follows the *responsible* app, not the process holding the
socket.** macOS walks up to the application that is responsible for the process
tree. A helper's traffic is credited to the app that launched it. This is the
part that surprises people.

## Why a dev run looks broken

`npm start` from an editor's integrated terminal produces this tree:

```
Visual Studio Code → Code Helper → bash → make → npm → node → Electron → nvpair-node-scanner
```

The responsible app is **Visual Studio Code**. Every discovery attempt is
attributed to it — the unified log says so directly:

```bash
log show --last 1h --style compact \
  --predicate 'eventMessage CONTAINS[c] "LocalNetwork: found bundle"' \
  | grep -oE 'bundle id [a-zA-Z0-9.-]+' | sort | uniq -c | sort -rn
```

On an affected machine that prints thousands of `com.microsoft.VSCode` and zero
Electron. Patching Electron's `Info.plist` changes nothing while this is true,
because Electron is not the bundle being judged.

## Fixing it

Pick one:

**Grant the responsible app.** Enable **Visual Studio Code** (or whichever app
launched the tree) in *System Settings → Privacy & Security → Local Network*,
quit it completely, and relaunch. Fastest, and correct as far as it goes — but
the grant is the editor's, so it covers anything else that editor spawns too.

**Launch from Terminal.** Apple exempts command-line tools run from Terminal or
over SSH, including the processes they spawn, so `npm start` from Terminal.app
sidesteps the question entirely. Good for a quick discovery test.

**Give Electron its own identity.** To have *Electron* prompt and appear in the
list under its own name it needs both the usage string and a launch through
Launch Services, so that it — not the editor — is the responsible app:

```bash
make macos-dev-local-network      # adds the keys, re-signs the ad-hoc bundle
open -n desktop/node_modules/electron/dist/Electron.app --args .
```

`npm ci` replaces `node_modules`, so re-run the make target after installing.

Confirm which way it went by re-running the log query above and checking whether
the bundle id is now Electron's.

## Verifying

The cheapest check that separates authorization from a real network fault —
this send succeeds from a shell even when the app cannot make it:

```bash
python3 - <<'EOF'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, socket.inet_aton('<your en0 address>'))
print(s.sendto(b'probe', ('224.0.0.251', 5353)), 'bytes')
EOF
```

Succeeds from a shell, fails from the app ⇒ authorization, not networking.

## Notes

- There is no API to raise the prompt on demand. It appears when the app
  performs a local-network operation while frontmost and the grant is still
  undetermined — so trigger discovery from a user action, and if it is denied,
  say so and point at the Settings pane rather than failing silently.
- There is no supported way to reset a Local Network decision; `tccutil` does
  not cover it. Testing a first-run prompt needs a fresh user account or a VM
  snapshot.
- `AllowedWiFiLocalNetworkAddresses` / `AllowedEthernetLocalNetworkAddresses`
  under the `com.apple.network.local-network` domain exempt whole CIDR ranges,
  but they arrived in **macOS 15.5** and are unavailable on earlier 15.x. They
  are also machine-wide and apply to every process, so they are a lab
  convenience, not a fix to ship behind.
- An ad-hoc, linker-signed bundle with no Team ID has no stable identity for
  macOS to hang a grant on, so a dev grant can be forgotten across reinstalls.
  The shipped app avoids this by being Developer ID signed and notarized.
- macOS needs no multicast entitlement (that requirement is iOS-only), and the
  `com.apple.security.network.*` entitlements matter only under App Sandbox.
