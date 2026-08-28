<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# eapnoob

A Go implementation of **EAP-NOOB** (RFC 9140, "Nimble Out-of-Band Authentication
for EAP"), exposed as a transport-agnostic library. Two consumers, a `Server` and
a `Peer`, perform an authenticated key exchange bootstrapped by a single
user-assisted out-of-band (OOB) message, and afterwards derive a **shared secret
of any length**.

## What it does

EAP-NOOB performs an Ephemeral ECDH key exchange that is authenticated by a short
OOB message (`PeerId`, `Noob`, `Hoob`) carried over a user-assisted channel
(QR code, NFC, blinking LED, typed code, etc.). Once both sides confirm the
exchange they reach the `Registered` state and share the association key `Kz`,
from which `Export` derives arbitrary-length secrets.

The library is **byte-in / byte-out**: it produces and consumes the EAP-NOOB JSON
messages but does not implement EAP/RADIUS framing or networking. You wire up the
in-band transport and relay the OOB message yourself.

## Scope

Implemented:

- Common handshake, Initial Exchange, OOB Step (both directions), Waiting
  Exchange, Completion Exchange.
- Cryptosuite 1 (Curve25519 / SHA-256, mandatory) and cryptosuite 2
  (NIST P-256 / SHA-256, recommended).
- NIST SP 800-56C one-step KDF and the `MSK`/`EMSK`/`AMSK`/`Kms`/`Kmp`/`Kz`
  outputs (RFC 9140, Table 5).
- Persistent association store (`Store` interface + in-memory implementation).
- Arbitrary-length secret export via HKDF-SHA256 over `Kz`.
- Error notifications (Type=0) and the RFC error codes.

Not implemented (reserved for a future phase): the Reconnect / rekeying exchange
(message types 7-9, KeyingMode 1-3) and AAA roaming.

## Usage

```go
srv := eapnoob.NewServer(eapnoob.ServerConfig{Dirs: 1}, nil)
peer := eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 1}, nil)

// 1. Initial Exchange: relay each side's Outcome.Send to the other until done.
//    (server.Start() produces the first message.)

// 2. OOB Step: relay the OOB message over a user-assisted channel.
oob, _ := peer.OOBOutput()
_ = srv.OOBInput(oob)

// 3. Completion Exchange: relay messages again until both reach Registered.

// 4. Derive a shared secret of any length.
secret, _ := srv.Export("my-application", nil, 48) // == peer.Export(...)
```

See [`example_test.go`](example_test.go) for a complete, runnable driver loop.

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

### OOB direction

`Dirs`/`PreferDir` select the OOB direction (`1` = peer-to-server, `2` =
server-to-peer). The sending side calls `OOBOutput()` and the receiving side
calls `OOBInput()`.

## Testing

```sh
go test ./...
```

The suite runs full pairings for both cryptosuites and both OOB directions,
verifies that the server and peer derive identical exported secrets, and checks
the KDF, JWK round-trip, OOB fingerprint rejection, and MAC verification paths.
