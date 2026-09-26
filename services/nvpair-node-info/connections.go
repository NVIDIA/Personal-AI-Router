// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sort"
	"time"
)

const (
	maxConnectionInterfaces = 128
	maxConnectionAddresses  = 32
)

type PhysicalPortInfo struct {
	Source   string `json:"source"`
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}

type ConnectionInterface struct {
	Name                 string            `json:"name"`
	Index                int               `json:"index"`
	MAC                  string            `json:"mac"`
	Physical             bool              `json:"physical"`
	Up                   bool              `json:"up"`
	Carrier              *bool             `json:"carrier,omitempty"`
	SpeedMbps            int64             `json:"speedMbps,omitempty"`
	Addresses            []string          `json:"addresses"`
	AddressesUnavailable bool              `json:"addressesUnavailable,omitempty"`
	PhysicalPort         *PhysicalPortInfo `json:"physicalPort,omitempty"`
	Driver               string            `json:"driver,omitempty"`
	RDMADevices          []string          `json:"rdmaDevices"`
	MTU                  int               `json:"mtu"`
}

// ConnectionInfo is a point-in-time host observation. It reports only native
// OS facts; cable partners, directness and RDMA usability require their own
// reviewed product operations.
type ConnectionInfo struct {
	Source     string                `json:"source"`
	Status     string                `json:"status"`
	ObservedAt int64                 `json:"observedAt"`
	Truncated  bool                  `json:"truncated"`
	Interfaces []ConnectionInterface `json:"interfaces"`
}

func connectionReport(now time.Time, interfaces []ConnectionInterface) *ConnectionInfo {
	rows := append([]ConnectionInterface(nil), interfaces...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Index < rows[j].Index })
	truncated := len(rows) > maxConnectionInterfaces
	if truncated {
		rows = rows[:maxConnectionInterfaces]
	}
	for i := range rows {
		if rows[i].Addresses == nil {
			rows[i].Addresses = []string{}
		}
		if len(rows[i].Addresses) > maxConnectionAddresses {
			rows[i].Addresses, rows[i].AddressesUnavailable = []string{}, true
		}
		if rows[i].RDMADevices == nil {
			rows[i].RDMADevices = []string{}
		}
		sort.Strings(rows[i].Addresses)
		sort.Strings(rows[i].RDMADevices)
	}
	return &ConnectionInfo{Source: "host-os", Status: "observed", ObservedAt: now.UnixMilli(), Truncated: truncated, Interfaces: rows}
}

func unavailableConnectionReport(now time.Time) *ConnectionInfo {
	return &ConnectionInfo{Source: "host-os", Status: "unavailable", ObservedAt: now.UnixMilli(), Interfaces: []ConnectionInterface{}}
}
