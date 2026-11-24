// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssociationExport_PreservesEstablishedDerivation(t *testing.T) {
	association := &Association{Kz: []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	}}
	// Independently derived with Node's HKDF-SHA256 implementation. The info
	// bytes are 00 03, "app", 00 01: a two-byte label length followed by context.
	expected, err := hex.DecodeString("bbc4364268fc3b96f5ebf88486f1b181be5c3afc9e74cc9283acce70230383b57789c394c7faed49f01a")
	require.NoError(t, err)

	secret, err := association.Export("app", []byte{0x00, 0x01}, 42)

	require.NoError(t, err)
	assert.Equal(t, expected, secret)
}

func TestAssociationExport_ValidatesLengthBounds(t *testing.T) {
	association := &Association{Kz: bytes.Repeat([]byte{0x17}, 32)}
	accept := func(name, label string, length int) {
		t.Run(name, func(t *testing.T) {
			secret, err := association.Export(label, nil, length)
			require.NoError(t, err)
			assert.Len(t, secret, length)
		})
	}
	accept("zero output bytes", "app", 0)
	accept("maximum output length", "app", 255*sha256.Size)
	accept("largest label length", strings.Repeat("a", 65535), 32)

	reject := func(name, label string, length int) {
		t.Run(name, func(t *testing.T) {
			secret, err := association.Export(label, nil, length)
			assert.Error(t, err)
			assert.Empty(t, secret)
		})
	}
	reject("negative output length", "app", -1)
	reject("output exceeds HKDF limit", "app", 255*sha256.Size+1)
	reject("label exceeds encoded length", strings.Repeat("a", 65536), 32)
}
