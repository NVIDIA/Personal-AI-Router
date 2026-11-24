// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestAvailableNode_DecodesBrokerIdentityFields(t *testing.T) {
	const body = `{"hostUuid":"host-a","discoveryRecordId":"observation:candidate-a","identityVerification":"unverified"}`
	var node availableNode
	require.NoError(t, json.Unmarshal([]byte(body), &node))
	assert.Equal(t, "observation:candidate-a", node.DiscoveryRecordID)
	assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
}
