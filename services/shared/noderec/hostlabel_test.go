// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostLabel_DerivesStableNameFromNodeUUID(t *testing.T) {
	label, err := HostLabel("f0e1d2c3-4b5a-4678-9abc-def012345678", "indigo")
	require.NoError(t, err)
	assert.Equal(t, "nvpair-f0e1d2c34b5a", label, "the first twelve hex digits, dashes removed, lowercase")
	assert.Equal(t, 19, len(label), "the seven-byte prefix plus twelve hex digits")

	again, err := HostLabel("f0e1d2c3-4b5a-4678-9abc-def012345678", "indigo")
	require.NoError(t, err)
	assert.Equal(t, label, again, "the same node UUID must always give the same label")

	// The digits are what identify the host, not the spelling they arrive in: the
	// node UUID reaches this call from a JSON file another process wrote, so
	// upper-case hex and surrounding decoration must still yield the label of the
	// identity they spell rather than quietly dropping the host onto the
	// hostname-derived fallback path.
	cases := []struct {
		name     string
		nodeUUID string
	}{
		{"upper-case hex", "F0E1D2C3-4B5A-4678-9ABC-DEF012345678"},
		{"wrapped in decoration", "{f0e1d2c3-4b5a-4678-9abc-def012345678}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spelledLabel, err := HostLabel(tc.nodeUUID, "indigo")
			require.NoError(t, err)
			assert.Equal(t, "nvpair-f0e1d2c34b5a", spelledLabel, "node UUID %q spells the same identity", tc.nodeUUID)
		})
	}
}

func TestHostLabel_TwoNodeUUIDsYieldDifferentLabels(t *testing.T) {
	// Two hosts must never advertise the same name, so a difference anywhere
	// inside the twelve hex digits the label is built from has to show up in it.
	test := func(name, firstUUID, secondUUID string) {
		t.Run(name, func(t *testing.T) {
			firstLabel, err := HostLabel(firstUUID, "indigo")
			require.NoError(t, err)
			secondLabel, err := HostLabel(secondUUID, "indigo")
			require.NoError(t, err)
			assert.NotEqual(t, firstLabel, secondLabel, "node UUIDs that differ inside the label's window must not share a host label")
		})
	}
	test("first hex digit differs",
		"f0e1d2c3-4b5a-4678-9abc-def012345678",
		"a0e1d2c3-4b5a-4678-9abc-def012345678")
	test("twelfth hex digit differs",
		"f0e1d2c3-4b5a-4678-9abc-def012345678",
		"f0e1d2c3-4b5b-4678-9abc-def012345678")
}

func TestHostLabel_WindowIsTheFirstTwelveHexDigits(t *testing.T) {
	// The label spends exactly the first twelve hex digits of the node UUID — a
	// 48-bit window, about 16.7 million hosts before a collision becomes likely —
	// and nothing more. Two UUIDs that agree there share a label on purpose:
	// widening the window later would rename every host already advertising
	// itself, and narrowing it would give up the collision guard for free.
	// Recorded here so the trade-off is visible to whoever is tempted to resize it.
	test := func(name, otherUUID string) {
		t.Run(name, func(t *testing.T) {
			label, err := HostLabel("f0e1d2c3-4b5a-4678-9abc-def012345678", "indigo")
			require.NoError(t, err)
			require.Equal(t, "nvpair-f0e1d2c34b5a", label, "the window starts at the first hex digit")

			otherLabel, err := HostLabel(otherUUID, "indigo")
			require.NoError(t, err)
			assert.Equal(t, label, otherLabel, "hex digits past the twelfth are not part of the label")
		})
	}
	test("thirteenth hex digit differs", "f0e1d2c3-4b5a-4679-9abc-def012345678")
	test("last hex digit differs", "f0e1d2c3-4b5a-4678-9abc-def012345679")
}

func TestHostLabel_UsableNodeUUIDAdvertisesWithoutAHostname(t *testing.T) {
	// The ladder is tried in the order the doc states it: the node UUID first,
	// the hostname only when there is no usable identity. That ordering is the
	// whole subject here, because moving the "no hostname" error above the node
	// UUID branch is an easy refactor that makes a node whose hostname is unknown
	// advertise nothing — the exact opposite of what the identity branch is for.
	label, err := HostLabel("f0e1d2c3-4b5a-4678-9abc-def012345678", "")
	require.NoError(t, err, "a usable node UUID is enough on its own, with no hostname to fall back on")
	assert.Equal(t, "nvpair-f0e1d2c34b5a", label)
	assert.Equal(t, 19, len(label))

	// Once the identity is usable the hostname plays no part, so a host that does
	// know its name does not advertise a different label because of it.
	withHostname, err := HostLabel("f0e1d2c3-4b5a-4678-9abc-def012345678", "indigo")
	require.NoError(t, err)
	assert.Equal(t, label, withHostname, "the node UUID alone decides the label")
}

func TestHostLabel_UnusableNodeUUIDFallsBackToHashedHostname(t *testing.T) {
	// Twelve hex digits is the rule, and it belongs to this function rather than
	// to whoever produced the value: anything shorter is not a node UUID, so the
	// caller's bad input cannot narrow the label below the collision guard. The
	// expectation is the same call with no node UUID at all, which pins "treat it
	// as absent" without recomputing the hash here.
	test := func(name, nodeUUID string) {
		t.Run(name, func(t *testing.T) {
			label, err := HostLabel(nodeUUID, "indigo")
			require.NoError(t, err)
			fallbackLabel, err := HostLabel("", "indigo")
			require.NoError(t, err)
			assert.Equal(t, fallbackLabel, label, "node UUID %q holds no usable identity and must yield the hostname-derived label", nodeUUID)
		})
	}
	test("two hex digits", "ab")
	test("no node UUID shape at all", "not-a-uuid-at-all")
	test("eleven hex digits, one short of the width", "f0e1d2c34b5")

	// Control: twelve digits is usable, so the test above cannot be satisfied by
	// falling back for everything.
	t.Run("exactly twelve hex digits still uses the node UUID", func(t *testing.T) {
		label, err := HostLabel("f0e1d2c3-4b5a", "indigo")
		require.NoError(t, err)
		assert.Equal(t, "nvpair-f0e1d2c34b5a", label)
	})
}

func TestHostLabel_EmptyUUIDFallsBackToHashedHostname(t *testing.T) {
	label, err := HostLabel("", "indigo")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(label, hostLabelPrefix), "fallback label %q must carry the PAIR prefix", label)
	assert.Equal(t, len(hostLabelPrefix)+12, len(label), "the fallback keeps the same twelve-hex-digit width as the node UUID path")

	// The derivation itself is part of the contract, so this is a known-answer
	// vector rather than a recomputation of the rule: "indigo" is hashed whole,
	// and its first twelve hex digits are aecdef998727 (printf 'indigo' |
	// shasum -a 256). Prefix, width and stability are all satisfied by
	// sha256(hostname[:1]) or sha256(salt+hostname), and either change renames
	// every host that has no node identity — the same regression the twelve-digit
	// width exists to prevent for hosts that do. Changing this literal is a
	// renaming event on the network, so it has to fail loudly here.
	assert.Equal(t, "nvpair-aecdef998727", label,
		"the fallback is hostLabelPrefix plus the first twelve hex digits of sha256(hostname); changing the derivation renames every fallback host already advertised")

	again, err := HostLabel("", "indigo")
	require.NoError(t, err)
	assert.Equal(t, label, again, "one hostname must always give the same fallback label")
}

func TestHostLabel_FallbackLabelsDifferAcrossHostnames(t *testing.T) {
	// The fallback has to be derived from the hostname, not merely look like it:
	// prefix, width and stability are all satisfied by one fixed label, and a
	// fixed label gives every host without a node identity the same name — the
	// contention this rule exists to remove. The hash itself is not recomputed
	// here; that would just re-run the production rule.
	indigoLabel, err := HostLabel("", "indigo")
	require.NoError(t, err)
	vegaLabel, err := HostLabel("", "vega")
	require.NoError(t, err)
	iglooLabel, err := HostLabel("", "igloo")
	require.NoError(t, err)

	assert.NotEqual(t, indigoLabel, vegaLabel, "two hostnames must not share one fallback host label")
	assert.NotEqual(t, vegaLabel, iglooLabel, "two hostnames must not share one fallback host label")
	// indigo and igloo share their first character on purpose. Hashing part of
	// the hostname, e.g. sha256(hostname[:1]), passes the two assertions above —
	// those pairs differ early anyway — and gives these two hosts one name.
	assert.NotEqual(t, indigoLabel, iglooLabel,
		"hostnames that agree only on their first character must not share one fallback host label, so hashing part of the hostname cannot pass")
}

func TestHostLabel_NeverProducesTheOperatingSystemHostname(t *testing.T) {
	// The defect being fixed is PAIR claiming the machine's own name, which makes
	// macOS append "-2" to the computer's Local hostname. Whatever the machine
	// calls itself, the name PAIR advertises stays its own: one label, lowercase,
	// within the 63-byte DNS label limit.
	cases := []struct {
		name     string
		hostname string
	}{
		{"lowercase", "indigo"},
		{"capitalised", "Indigo"},
		{"carries the mDNS suffix", "indigo.local"},
		{"carries a DNS search domain", "indigo.lab"},
		// What os.Hostname() actually returns on a Mac: macOS re-derived the
		// hostname from the Local hostname, itself its rendering of the computer
		// name "Home Mac". Upper case, a hyphen and a search domain in one
		// string — embedding rather than hashing this would advertise a name that
		// is not a single DNS label, and not one the host has claimed.
		{"real macOS name with uppercase, hyphen and search domain", "Home-Server.lab"},
		{"contains spaces", "a host with spaces"},
		{"longer than a DNS label", strings.Repeat("h", 70)}, // 70 bytes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, err := HostLabel("", tc.hostname)
			require.NoError(t, err)

			assert.NotEqual(t, tc.hostname, label, "the host label must never be the operating system's hostname")
			assert.NotContains(t, label, ".", "a host label is one DNS label, so no suffix separator")
			assert.NotContains(t, label, " ", "a host label is one DNS label, so no spaces")
			assert.Equal(t, strings.ToLower(label), label, "a host label is lowercase")
			assert.Less(t, len(label), 64, "a DNS label is at most 63 bytes")
		})
	}
}

func TestHostLabel_NoUsableInputReturnsError(t *testing.T) {
	// With no usable node UUID and no hostname there is no entropy to derive a
	// name from, and a fixed label would give every host that reached this path
	// the same name. An error and no label lets the caller advertise nothing
	// instead.
	test := func(name, nodeUUID string) {
		t.Run(name, func(t *testing.T) {
			label, err := HostLabel(nodeUUID, "")
			assert.Error(t, err, "no identity and no hostname must not yield a label")
			assert.Equal(t, "", label)
			assert.ErrorContains(t, err, "no usable node UUID")
			assert.ErrorContains(t, err, "no hostname")
		})
	}
	test("no node UUID at all", "")
	// The wording has to be true here too: the node UUID was not empty, it just
	// held no usable identity, and telling the operator otherwise would send them
	// looking at the wrong input.
	test("node UUID too short to use", "ab")
}
