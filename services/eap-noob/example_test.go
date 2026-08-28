// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob_test

import (
	"bytes"
	"fmt"

	"eapnoob"
)

// run relays one EAP conversation between the server and peer until a side has
// nothing more to send.
func run(srv *eapnoob.Server, peer *eapnoob.Peer) error {
	msg, err := srv.Start()
	if err != nil {
		return err
	}
	peerTurn := true
	for {
		var out eapnoob.Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = srv.Receive(msg)
		}
		if err != nil {
			return err
		}
		if out.Err != nil {
			return out.Err
		}
		if len(out.Send) == 0 {
			return nil
		}
		msg = out.Send
		peerTurn = !peerTurn
	}
}

// Example shows a full EAP-NOOB pairing followed by exporting a shared secret of
// arbitrary length. Here the OOB message travels peer-to-server; the caller is
// responsible for relaying it over a user-assisted channel (QR code, etc.).
func Example() {
	srv := eapnoob.NewServer(eapnoob.ServerConfig{Dirs: 1}, nil)
	peer := eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 1}, nil)

	// Initial Exchange.
	if err := run(srv, peer); err != nil {
		panic(err)
	}

	// OOB Step: relay (PeerId, Noob, Hoob) from peer to server out of band.
	oob, err := peer.OOBOutput()
	if err != nil {
		panic(err)
	}
	if err := srv.OOBInput(oob); err != nil {
		panic(err)
	}

	// Completion Exchange.
	if err := run(srv, peer); err != nil {
		panic(err)
	}

	// Both sides now share an association and can derive a secret of any length.
	const secretLen = 48
	serverSecret, _ := srv.Export("my-application", nil, secretLen)
	peerSecret, _ := peer.Export("my-application", nil, secretLen)

	fmt.Println("registered:", srv.State() == eapnoob.StateRegistered)
	fmt.Println("secret length:", len(serverSecret))
	fmt.Println("secrets match:", bytes.Equal(serverSecret, peerSecret))
	// Output:
	// registered: true
	// secret length: 48
	// secrets match: true
}
