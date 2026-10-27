<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# eapnoob

PAIR's pairing component implements the Initial, Waiting, and Completion
Exchanges of **EAP-NOOB** (RFC 9140, "Nimble Out-of-Band Authentication for EAP").
The cluster-manager drives a `Server` and a `Peer` to authenticate the node
identity and certificate information exchanged during pairing.

## What it does

EAP-NOOB authenticates an ephemeral ECDH exchange using an out-of-band (OOB)
secret, `Noob`. PAIR currently obtains that secret from a six-digit PIN carried
by the user from the inviter to the joiner. The Completion MACs bind the secret
to the exchanged `ServerInfo` and `PeerInfo`, including both nodes' certificates.
After successful Completion, both roles reach `Registered` and the
cluster-manager pins the authenticated certificates in its own trusted-node
store.

The component consumes and produces EAP-NOOB JSON messages. The cluster-manager
owns the transport, PIN validation and encoding, durable trust, and membership.
This module serves PAIR; it is not a general-purpose authentication or
application-secret export library.

## Scope

Implemented:

- Common handshake, Initial Exchange, OOB Step (both directions), Waiting
  Exchange, Completion Exchange.
- Cryptosuite 1 (Curve25519 / SHA-256, mandatory) and cryptosuite 2
  (NIST P-256 / SHA-256, recommended).
- NIST SP 800-56C one-step KDF, retaining the Completion MAC keys `Kms` and
  `Kmp` at their RFC 9140 Table 5 offsets.
- Caller-supplied `Noob` for PAIR's PIN flow, plus the full server-to-peer
  `OOBMessage` path reserved for PAIR's planned high-entropy pairing codes.
- Error notifications (Type=0) and the RFC error codes.

Reconnect / rekeying exchanges (message types 7-9, KeyingMode 1-3) and AAA
roaming are not implemented. Completed associations and application keys are
not persisted by this module.

## Usage

```go
srv := eapnoob.NewServer(eapnoob.ServerConfig{Dirs: 2, ServerInfo: serverInfo})
peer := eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 2, PeerInfo: peerInfo})

// 1. Initial Exchange: relay each side's Outcome.Send to the other until done.
//    (srv.Start() produces the first message.)

// 2. The cluster-manager validates and encodes the user-carried PIN as the
//    same 16-byte noob on both sides. No OOBMessage is sent in the PIN flow.
if err := srv.OOBOutputWith(noob); err != nil {
    return err
}
if err := peer.OOBInputNoob(noob); err != nil {
    return err
}

// 3. Completion Exchange: relay messages again until both reach Registered.

// 4. After successful Completion, the cluster-manager reads srv.PeerInfo()
//    and peer.ServerInfo() and pins the authenticated certificates.
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

PAIR uses direction 2. Its PIN flow calls `Server.OOBOutputWith(noob)` to
register the inviter's secret while staying in `Waiting`, and
`Peer.OOBInputNoob(noob)` to move the joiner to `OOBReceived`. Neither method
constructs or verifies a Hoob fingerprint; transcript agreement is proved by
the Completion MACs.

`Server.OOBOutput()` and `Peer.OOBInput(OOBMessage)` retain random `Noob`
generation and Hoob verification for the planned high-entropy OOB flow. That
flow carries the full `OOBMessage{PeerId, Noob, Hoob}` through the user-assisted
channel; it is not used by today's PIN pairing.

## Testing

```sh
go test ./...
```

The suite exercises both cryptosuites, PAIR's caller-supplied `Noob` flow,
Completion authentication, KDF outputs, JWK round-trips, and OOB fingerprint
rejection. The cluster-manager tests cover PIN encoding and the durable trust
boundary.
