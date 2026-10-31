// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPIN_EncodesExactNoobBytes(t *testing.T) {
	test := func(name, pin string, want [16]byte) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want[:], noobFromPIN(pin))
		})
	}
	test("all zeroes", "000000", [16]byte{})
	test("leading zeroes", "000123", [16]byte{15: 0x7b})
	test("ordinary PIN", "402199", [16]byte{13: 0x06, 14: 0x23, 15: 0x17})
	test("maximum PIN", "999999", [16]byte{13: 0x0f, 14: 0x42, 15: 0x3f})
}

func TestGeneratePIN_ReturnsMatchingNoob(t *testing.T) {
	pin, noob, err := generatePIN()
	require.NoError(t, err, "generatePIN")
	assert.Regexp(t, pinPattern, pin)
	assert.Equal(t, noobFromPIN(pin), noob)
}
