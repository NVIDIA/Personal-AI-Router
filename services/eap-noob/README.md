<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# eapnoob

A Go implementation of **EAP-NOOB** (RFC 9140, "Nimble Out-of-Band Authentication
for EAP"), exposed as a transport-agnostic library. Two consumers, a `Server` and
a `Peer`, perform an authenticated key exchange bootstrapped by a single
user-assisted out-of-band (OOB) message, and afterwards derive a **shared secret**.

In PAIR, the inviter is the Server and the joiner is the Peer. The user carries
a PIN from the inviter to the joiner; the cluster manager encodes it as the
16-byte secret `Noob` used by this library.

## What it does

EAP-NOOB performs an Ephemeral ECDH key exchange that is authenticated by a short
OOB message (`PeerId`, `Noob`, `Hoob`) carried over a user-assisted channel
(QR code, NFC, blinking LED, typed code, etc.). Once both sides confirm the
exchange they reach the `Registered` state and share the association key `Kz`,
from which `Export` derives application secrets of up to 8160 bytes.

The library is **byte-in / byte-out**: it produces and consumes the EAP-NOOB JSON
messages but does not implement EAP/RADIUS framing or networking. You wire up the
in-band transport and relay the OOB message yourself.

## Scope

Implemented:

- Common handshake, Initial Exchange, OOB Step (both directions), Waiting
  Exchange, Completion Exchange.
- Cryptosuite 1 (Curve25519 / SHA-256, mandatory) and cryptosuite 2
  (NIST P-256 / SHA-256, recommended).
- NIST SP 800-56C one-step KDF and the
  `MSK`/`EMSK`/`AMSK`/`MethodID`/`Kms`/`Kmp`/`Kz` outputs (RFC 9140, Table 5).
- Persistent association store (`Store` interface + in-memory implementation).
- Secret export via standard-library HKDF-SHA256 over `Kz`.
- Error notifications (Type=0) and the RFC error codes.

Protocol settings are fixed to version 1 and server cryptosuite priority `[2, 1]`.
`ServerConfig.Dirs` and `PeerConfig.PreferDir` select the OOB direction. Each
configuration accepts a typed [`PairingInfo`](info.go) input containing the node
identity and public certificate authenticated by Completion.

Not implemented (reserved for a future phase): the Reconnect / rekeying exchange
(message types 7-9, KeyingMode 1-3) and AAA roaming.

## Usage

```go
srv := eapnoob.NewServer(eapnoob.ServerConfig{
    ServerInfo: eapnoob.PairingInfo{Name: "inviter"},
    Dirs: eapnoob.OOBServerToPeer,
}, nil)
peer := eapnoob.NewPeer(eapnoob.PeerConfig{
    PeerInfo: eapnoob.PairingInfo{Name: "joiner"},
    PreferDir: eapnoob.OOBServerToPeer,
}, nil)

// 1. Initial Exchange: relay each side's Outcome.Send to the other until done.
//    (srv.Start() produces the first message.)

// 2. OOB Step: relay the OOB message over a user-assisted channel.
oob, err := srv.OOBOutput()
if err != nil {
    return err
}
if err := peer.OOBInput(oob); err != nil {
    return err
}

// 3. Completion Exchange: relay messages again until both reach Registered.

// 4. Derive a shared secret.
secret, err := srv.Export("my-application", nil, 48) // == peer.Export(...)
if err != nil {
    return err
}
_ = secret
```

See [`example_test.go`](example_test.go) for a complete, runnable driver loop
using PAIR's caller-supplied PIN-derived `Noob`. The shortened identities above
omit fields; the cluster manager supplies the full `PairingInfo`, including each
node's public certificate.

### Driving the message loop

Each role processes one inbound message at a time:

```go
out, err := peer.Receive(msg) // or srv.Receive(msg)
// out.Send    -> bytes to deliver to the other party (nil if none)
// out.State   -> current association state
// out.Done    -> this EAP conversation ended
// out.Success -> Completion succeeded; association is Registered
// out.Err     -> an EAP-NOOB error notification was sent/received
```

EAP-NOOB spans two EAP conversations with the OOB step in between: run one
conversation for the Initial Exchange, perform the OOB relay, then run a second
conversation for the Completion Exchange (call `srv.Start()` again to begin it).
`Done` alone does not mean pairing succeeded.

### OOB direction

`ServerConfig.Dirs` selects the supported OOB directions using `OOBPeerToServer`
(`1`), `OOBServerToPeer` (`2`), or `OOBBoth` (`3`, the default). These constants
have type `OOBDirection`. When both are offered, `PeerConfig.PreferDir` selects
the direction, defaulting to `OOBServerToPeer`.
The sending side calls `OOBOutput()` and the receiving side calls `OOBInput()`
to transfer a random `Noob` and verify its `Hoob` fingerprint. Both methods
require the corresponding direction to have been negotiated.

PAIR's PIN flow instead supplies the same 16-byte `Noob` locally through
`Server.OOBOutputWith` and `Peer.OOBInputNoob`, without constructing an OOB
envelope or computing Hoob. The Completion MACs authenticate the secret and
transcript. See the [cluster-manager specification](../nvpair-cluster-manager/spec.md)
for PIN encoding.

### Shared application secrets and storage

The completion key derivation function (KDF) produces authentication keys and
the association key `Kz`. Export uses HKDF, the HMAC-based extract-and-expand
key derivation function, to derive application secrets from `Kz`. These are
different algorithms. Export uses Go's standard `crypto/hkdf` package; the
completion KDF retains its local implementation because it is a different
algorithm required by RFC 9140.

Exports return `ErrNotRegistered` until a registered association is available.
Labels are separated from context with a two-byte label-length prefix and may
contain at most 65535 bytes. Export lengths range from 0 to 8160 bytes, the
HKDF-SHA-256 limit. Use distinct labels or contexts for distinct purposes.

Both constructors accept a `Store`. Passing `nil` uses a `MemoryStore` that
copies keys on save and load. Supply a persistent implementation to retain
associations across process restarts. Each role saves its association before
reporting success; a storage error prevents local registration. A loaded
`Association` can also export secrets directly.

## Testing

```sh
go test ./...
```

The suite runs full pairings for both cryptosuites and both OOB directions,
verifies that the server and peer derive identical exported secrets, and checks
the KDF, JWK round-trip, OOB fingerprint rejection, MAC verification, and
association storage paths.
