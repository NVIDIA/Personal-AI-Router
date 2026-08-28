// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/noderec"
)

// The peer-observed set is what an address ranking treats as proof rather than
// inference, so the daemon must hold exactly what node-info reported: replaced
// wholesale (an address peers stopped reaching stops counting) and free of the
// empty entries a hand-built payload can carry.
func TestSetObservedAddressesReplacesTheSet(t *testing.T) {
	d := newSelfTestDaemon("host-a", "10.172.54.70")

	d.setObservedAddresses([]string{"10.172.54.70", "", "10.0.0.5"})
	got := d.observedAddresses()
	if len(got) != 2 || !got["10.172.54.70"] || !got["10.0.0.5"] {
		t.Fatalf("observed = %v, want both reported addresses and no empty entry", got)
	}

	d.setObservedAddresses([]string{"10.0.0.5"})
	got = d.observedAddresses()
	if len(got) != 1 || !got["10.0.0.5"] {
		t.Fatalf("observed = %v, want only the still-reported address", got)
	}

	d.setObservedAddresses(nil)
	if got = d.observedAddresses(); len(got) != 0 {
		t.Fatalf("observed = %v, want empty", got)
	}
}

// The relay arrives as a JSON-RPC request, so the daemon's dispatch must accept
// it and apply it.
func TestHandleSetObservedAddresses(t *testing.T) {
	d := newSelfTestDaemon("host-a", "10.172.54.70")
	params, err := json.Marshal(noderec.ObservedAddressesParams{Addresses: []string{"10.172.54.70"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !d.handle(&Message{Method: noderec.MethodSetObservedAddresses, Params: params}) {
		t.Fatal("daemon did not claim the observed-addresses method")
	}
	if got := d.observedAddresses(); !got["10.172.54.70"] {
		t.Fatalf("observed = %v, want the relayed address", got)
	}
}
