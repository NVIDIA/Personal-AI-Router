// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"reflect"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// TestForwardNodeInfoObservedAddressesReachesTheScanner exercises the middle of
// the observed-address pipeline: node-info reports the addresses peers actually
// reached it on, and the scanner — which owns what this node advertises — is the
// consumer that needs them. Nothing else connects the two, so a broker that drops
// the notification leaves address ranking with no peer evidence at all while every
// component's own unit tests still pass.
func TestForwardNodeInfoObservedAddressesReachesTheScanner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		from  string
		addrs []string
	}{
		{"a reported set", `{"addresses":["10.172.54.70","10.0.0.5"]}`, []string{"10.172.54.70", "10.0.0.5"}},
		// An empty set is a withdrawal, not a no-op: it retires evidence whose TTL
		// expired, and suppressing it would leave the scanner ranking on a link
		// that has gone away.
		{"a withdrawal", `{"addresses":[]}`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brokerSide, scannerSide := net.Pipe()
			t.Cleanup(func() {
				_ = brokerSide.Close()
				_ = scannerSide.Close()
			})

			sp := &scannerProcess{peer: NewPeer(NewCodec(brokerSide))}
			go sp.peer.Serve(nil, nil)
			b := &Broker{}
			b.setScanner(sp)

			relayed := make(chan *Message, 1)
			go func() {
				codec := NewCodec(scannerSide)
				msg, err := codec.Read()
				if err != nil {
					return
				}
				relayed <- msg
				_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
			}()

			b.forwardNodeInfoNotification(noderec.NotifyObservedAddresses, json.RawMessage(tc.from))

			select {
			case msg := <-relayed:
				if msg.Method != noderec.MethodSetObservedAddresses {
					t.Fatalf("relayed method = %q, want %q", msg.Method, noderec.MethodSetObservedAddresses)
				}
				var got noderec.ObservedAddressesParams
				if err := json.Unmarshal(msg.Params, &got); err != nil {
					t.Fatalf("decode relayed params %s: %v", msg.Params, err)
				}
				if len(got.Addresses) != len(tc.addrs) || (len(tc.addrs) > 0 && !reflect.DeepEqual(got.Addresses, tc.addrs)) {
					t.Fatalf("relayed addresses = %v, want %v", got.Addresses, tc.addrs)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("observed addresses never reached the scanner")
			}
		})
	}
}
