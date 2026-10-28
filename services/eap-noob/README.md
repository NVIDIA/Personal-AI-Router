<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# eapnoob

Pairs two PAIR nodes using the EAP-NOOB protocol (RFC 9140). The inviter is the
`Server`; the joiner is the `Peer`. The user carries a six-digit PIN from the
inviter to the joiner.

Each side supplies a [`PairingInfo`](info.go) value containing its identity and
public certificate. Pairing verifies that both sides used the same PIN and saw
the same identity information. After success (`Registered`), the cluster manager
saves the verified certificates for future connections.

This component processes JSON messages. The cluster manager handles networking,
PIN validation and encoding, certificate storage, and cluster membership.

## Terms used in the code and specification

| Term | Meaning |
| --- | --- |
| Cryptosuite | A set of algorithms used together. Suite 1 uses Curve25519 and SHA-256; suite 2 uses P-256 and SHA-256. |
| OOB (out of band) | A separate channel, such as the user copying the PIN between nodes. |
| NOOB | The 16-byte pairing secret, derived from the PIN in the pairing flow. |
| HOOB | A fingerprint of the pairing messages and NOOB, checked when transferring a full pairing code. The PIN flow does not use it. |
| NAI (Network Access Identifier) | The peer's protocol identity string, separate from its PAIR node UUID. |
| MAC (message authentication code) | A check made with a secret key to detect changed pairing data. |
| KDF (key derivation function) | Produces temporary authentication keys from shared secret material. |

## Usage

```go
serverInfo := eapnoob.PairingInfo{
    NodeID:              "inviter-node",
    Name:                "Office workstation",
    //...
}
peerInfo := eapnoob.PairingInfo{
    NodeID:              "joiner-node",
    Name:                "Laptop",
    //...
}
srv := eapnoob.NewServer(serverInfo)
peer := eapnoob.NewPeer(peerInfo)

// 1. Exchange identities and temporary public keys: call srv.Start() and relay
//    each side's Outcome.Send to the other until both are waiting for the PIN.

// 2. Each side locally encodes the user-carried PIN as the same 16-byte NOOB.
//    This example uses PIN 000123; production code generates a fresh PIN.
noob := make([]byte, 16)
noob[15] = 123
if err := srv.OOBOutputWith(noob); err != nil {
    return err
}
if err := peer.OOBInputNoob(noob); err != nil {
    return err
}

// 3. Verify the PIN and finish pairing: call srv.Start() again and relay
//    messages until both reach Registered.

// 4. After successful pairing, read srv.PeerInfo() and peer.ServerInfo().
//    The cluster manager saves their verified certificates for future connections.
```

See [`example_test.go`](example_test.go) for a complete, runnable message loop.
The shortened identities above omit fields; PAIR supplies the full `PairingInfo`,
including each node's public certificate.

### Processing messages

Pass each incoming message to `Receive` and relay any response to the other node:

```go
out, err := peer.Receive(msg) // or srv.Receive(msg)
// err         -> a processing error
// out.Send    -> response bytes to deliver (nil if none)
// out.State   -> current pairing state
// out.Done    -> this round of messages ended
// out.Success -> pairing completed successfully
// out.Err     -> a protocol error reported by either side
```

`Done` alone does not mean pairing succeeded: the first round ends while waiting
for the PIN. Keep both objects alive, then call `srv.Start()` again after the PIN
has been supplied to finish pairing.

## Implementation scope

Protocol settings are fixed to version 1 and inviter-to-joiner PIN transfer.
PAIR-to-PAIR pairing selects P-256 (suite 2). X25519 (suite 1) is retained because
[RFC 9140 §5.1](https://www.rfc-editor.org/rfc/rfc9140.html#section-5.1) requires
implementations to support it. It remains available when negotiating with other
peers. Reconnection and key renewal are not implemented.

Go's standard library supplies the cryptographic operations. A small local
[KDF implementation](kdf.go) derives the authentication keys without another
runtime dependency.

`Server.OOBOutput()` and `Peer.OOBInput(OOBMessage)` also support transferring a
random NOOB with its HOOB for planned pairing codes. Today's PIN flow uses
`Server.OOBOutputWith` and `Peer.OOBInputNoob` instead. See the
[cluster-manager specification](../nvpair-cluster-manager/spec.md) for protocol
details and PIN encoding.

## Testing

Run from this directory:

```sh
go test ./...
```

Tests cover successful pairing with both cryptosuites, incorrect secrets, changed
identity information, and invalid fingerprints.
