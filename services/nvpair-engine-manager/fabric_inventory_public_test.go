// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func observedFabricInterface(index int, switchID, portName string, eligible bool) fabricObservedInterface {
	physical, carrier := true, eligible
	return fabricObservedInterface{
		fabricInterface: fabricInterface{
			Name:         fmt.Sprintf("enp%d%s", index, portName),
			Index:        index,
			MAC:          fmt.Sprintf("02:00:00:00:00:%02x", index),
			PhysicalPort: fabricPhysicalPort{Source: "linux-sysfs", SwitchID: switchID, PortName: portName},
			Addresses:    []string{},
			Driver:       "mlx5_core",
			RDMADevices:  []string{fmt.Sprintf("roce%d", index)},
			MTU:          1500,
		},
		Up:        eligible,
		Physical:  &physical,
		Carrier:   &carrier,
		SpeedMbps: 200000,
	}
}

func TestSummarizeFabricInventoryCollapsesFunctionsAndClassifiesPorts(t *testing.T) {
	facts := fabricInventory{NodeID: "node-a", Principal: "principal-a", ObservedAt: 123, Interfaces: []fabricObservedInterface{
		observedFabricInterface(1, "switch-a", "p0", true),
		observedFabricInterface(2, "switch-a", "p0", true),
		observedFabricInterface(3, "switch-a", "p1", false),
		observedFabricInterface(4, "switch-a", "p1", false),
	}}
	got := summarizeFabricInventory(facts)
	if got.Status != "observed" || got.NodeID != facts.NodeID || got.ObservedAt != facts.ObservedAt || len(got.Ports) != 2 {
		t.Fatalf("unexpected public inventory: %+v", got)
	}
	if got.Ports[0].PortName != "p0" || !got.Ports[0].Eligible || got.Ports[0].Reason != "" {
		t.Fatalf("eligible port was not retained: %+v", got.Ports[0])
	}
	if got.Ports[1].PortName != "p1" || got.Ports[1].Eligible || got.Ports[1].Reason == "" {
		t.Fatalf("ineligible port was not explained: %+v", got.Ports[1])
	}
	encoded, err := json.Marshal(fabricInventorySnapshot{Nodes: []fabricNodeObservation{got}})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"principal-a", "enp1p0", "02:00:00", "roce1", "routes", "digest", "addresses"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public inventory leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestTopologyInventoryRejectsDuplicateOrOversizedSelections(t *testing.T) {
	s := &fabricService{}
	for _, ids := range [][]string{{}, {"node-a", "node-a"}, {"a", "b", "c", "d"}} {
		if _, err := s.topologyInventory(t.Context(), ids); err == nil {
			t.Fatalf("selection %v unexpectedly accepted", ids)
		}
	}
}
