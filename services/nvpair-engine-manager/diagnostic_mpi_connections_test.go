// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
)

func TestDiagnosticMPIUsesOnlySelectedNativeInterfaceAddress(t *testing.T) {
	now := time.UnixMilli(1800000000000)
	selected := diagnosticMPIInterfaceSelection{Network: "fabric", NodeID: "spark-b", Principal: "spark-b", SwitchID: "0011223344556677", PortName: "p0", Interface: cableprobe.Interface{Name: "fabric0", Index: 3, MAC: "02:00:00:00:00:01"}}
	fixture := `{"hostUuid":"spark-b","connections":{"source":"host-os","status":"observed","observedAt":1799999999900,"interfaces":[{"name":"fabric0","index":3,"mac":"02:00:00:00:00:01","physical":true,"up":true,"addresses":["10.60.0.1/30"],"physicalPort":{"source":"linux-sysfs","switchId":"0011223344556677","portName":"p0"}},{"name":"control0","index":7,"mac":"02:00:00:00:00:07","physical":true,"up":true,"addresses":["192.0.2.10/24"]}]}}`
	parse := func(text string) diagnosticMPIConnections {
		var info diagnosticMPIConnections
		if err := json.Unmarshal([]byte(text), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	got, err := projectDiagnosticMPIAddress(parse(fixture), selected, now)
	if err != nil || got.Address != "10.60.0.1" || got.Subnet != "10.60.0.0/30" || got.AgeMs != 100 {
		t.Fatalf("wrong selected binding: %+v %v", got, err)
	}
	management := diagnosticMPIInterfaceSelection{Network: "management", NodeID: "spark-b", Principal: "spark-b", Interface: cableprobe.Interface{Name: "control0", Index: 7, MAC: "02:00:00:00:00:07"}}
	withoutFabric := parse(strings.Replace(fixture, `["10.60.0.1/30"]`, `[]`, 1))
	baseline, err := projectDiagnosticMPIAddress(withoutFabric, management, now)
	if err != nil || baseline.Address != "192.0.2.10" || baseline.Selection.Network != "management" {
		t.Fatalf("explicit management Socket baseline was incorrectly gated by empty fabric: %v", err)
	}
	management.Network = "fabric"
	if _, err := projectDiagnosticMPIAddress(withoutFabric, management, now); err == nil {
		t.Fatal("management interface silently became an approved fabric selection")
	}
	for _, test := range []struct{ name, from, to string }{
		{"empty fabric is not control LAN", `["10.60.0.1/30"]`, `[]`},
		{"renamed interface", `"name":"fabric0"`, `"name":"fabric1"`},
		{"reused index", `"index":3`, `"index":4`},
		{"different port", `"portName":"p0"`, `"portName":"p1"`},
		{"stale snapshot", `1799999999900`, `1799999997000`},
		{"unknown addresses", `"physical":true`, `"physical":true,"addressesUnavailable":true`},
		{"two addresses", `["10.60.0.1/30"]`, `["10.60.0.1/30","10.70.0.1/30"]`},
		{"overlapping alias", `["192.0.2.10/24"]`, `["10.60.0.2/30"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := projectDiagnosticMPIAddress(parse(strings.Replace(fixture, test.from, test.to, 1)), selected, now); err == nil {
				t.Fatal("ambiguous or stale interface admitted")
			}
		})
	}
}
