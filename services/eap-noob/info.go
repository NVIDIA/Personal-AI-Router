// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

// PairingInfo identifies a node in ServerInfo (inviter) or PeerInfo (joiner).
// Completion authenticates these fields, including the public certificate.
// The cluster manager constructs and validates this identity and owns its
// durable certificate pins.
type PairingInfo struct {
	V                   int    `json:"v"`
	NodeUUID            string `json:"nodeUuid"`
	NodeID              string `json:"nodeId"`
	Name                string `json:"name"`
	ClusterID           string `json:"clusterId"`
	AdmissionEpoch      uint64 `json:"admissionEpoch,omitempty"`
	ClusterFriendlyName string `json:"clusterFriendlyName"`
	Addr                string `json:"addr,omitempty"`
	Cert                string `json:"cert"`
}
