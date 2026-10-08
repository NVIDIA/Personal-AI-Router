// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.Len(t, got, 2, "observed")
	require.True(t, got["10.172.54.70"], "observed (%v)", got)
	require.True(t, got["10.0.0.5"], "observed (%v)", got)

	d.setObservedAddresses([]string{"10.0.0.5"})
	got = d.observedAddresses()
	require.Len(t, got, 1, "observed")
	require.True(t, got["10.0.0.5"], "observed (%v)", got)

	d.setObservedAddresses(nil)
	{
		got = d.observedAddresses()
		require.Empty(t, got, "observed")
	}
}

// The relay arrives as a JSON-RPC request, so the daemon's dispatch must accept
// it and apply it.
func TestHandleSetObservedAddresses(t *testing.T) {
	d := newSelfTestDaemon("host-a", "10.172.54.70")
	params, err := json.Marshal(noderec.ObservedAddressesParams{Addresses: []string{"10.172.54.70"}})
	require.NoError(t, err, "marshal")

	require.True(t, d.handle(&Message{Method: noderec.MethodSetObservedAddresses, Params: params}), "daemon did not claim the observed-addresses method")
	{
		got := d.observedAddresses()
		require.True(t, got["10.172.54.70"], "observed (%v)", got)
	}
}
