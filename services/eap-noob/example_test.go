// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob_test

import (
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

// Example shows PAIR's server-to-peer pairing with a caller-supplied Noob.
// The cluster manager validates and encodes the user-carried PIN before calling
// these methods, then pins the authenticated identity after Completion.
func Example() {
	srv := eapnoob.NewServer(eapnoob.ServerConfig{Dirs: 2, ServerInfo: map[string]any{"name": "inviter"}})
	peer := eapnoob.NewPeer(eapnoob.PeerConfig{PreferDir: 2, PeerInfo: map[string]any{"name": "joiner"}})

	// Initial Exchange.
	if err := run(srv, peer); err != nil {
		panic(err)
	}

	// A fixture representing PIN 000123, already validated and encoded by PAIR.
	noob := make([]byte, 16)
	noob[15] = 123
	if err := srv.OOBOutputWith(noob); err != nil {
		panic(err)
	}
	if err := peer.OOBInputNoob(noob); err != nil {
		panic(err)
	}

	// Completion Exchange.
	if err := run(srv, peer); err != nil {
		panic(err)
	}

	fmt.Println("registered:", srv.State() == eapnoob.StateRegistered && peer.State() == eapnoob.StateRegistered)
	fmt.Println("authenticated inviter:", string(peer.ServerInfo()))
	fmt.Println("authenticated joiner:", string(srv.PeerInfo()))
	// Output:
	// registered: true
	// authenticated inviter: {"name":"inviter"}
	// authenticated joiner: {"name":"joiner"}
}
