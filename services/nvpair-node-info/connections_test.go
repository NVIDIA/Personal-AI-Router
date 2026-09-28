// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
)

func connectionFixture(index int, port string) ConnectionInterface {
	carrier := true
	return ConnectionInterface{
		Name: fmt.Sprintf("enp%ds0f%dnp%d", index, index%2, index%2), Index: index,
		MAC: "02:00:00:00:00:" + fmt.Sprintf("%02x", index), Physical: true, Up: true, Carrier: &carrier,
		SpeedMbps: 200000, Addresses: []string{}, PhysicalPort: &PhysicalPortInfo{Source: "linux-sysfs", SwitchID: "0011223344556677", PortName: port},
		Driver: "mlx5_core", RDMADevices: []string{fmt.Sprintf("roce%d", index)}, MTU: 1500,
	}
}

func TestNodeInfoHandlerPublishesConnectionsOnlyToLoopback(t *testing.T) {
	report := connectionReport(time.UnixMilli(1_800_000_000_000), []ConnectionInterface{connectionFixture(2, "p0")})
	handler := nodeInfoHandler(clustertrust.Open(t.TempDir()), func(loopback bool) []byte {
		var connections *ConnectionInfo
		if loopback {
			connections = report
		}
		return buildResponse(nil, nil, 0, statsSnapshot{}, "node-a", nil, connections)
	})
	for _, test := range []struct {
		name, remote string
		want         bool
	}{{"loopback", "127.0.0.1:1234", true}, {"lan", "192.168.0.10:1234", false}} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://node/v1/node-info", nil)
			request.RemoteAddr = test.remote
			response := httptest.NewRecorder()
			handler(response, request)
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			_, present := body["connections"]
			if present != test.want {
				t.Fatalf("connections present=%v want=%v body=%s", present, test.want, response.Body.String())
			}
		})
	}
}

func TestConnectionReportPublishesOneOrTwoPhysicalPorts(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	for _, ports := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d-port", ports), func(t *testing.T) {
			rows := []ConnectionInterface{}
			for port := ports - 1; port >= 0; port-- {
				rows = append(rows, connectionFixture(10+port, fmt.Sprintf("p%d", port)), connectionFixture(2+port, fmt.Sprintf("p%d", port)))
			}
			report := connectionReport(now, rows)
			body := buildResponseAt(nil, nil, 0, statsSnapshot{}, "node-a", nil, report, now)
			var response NodeInfoResponse
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			if response.Connections == nil || response.Connections.Source != "host-os" || response.Connections.Status != "observed" || response.Connections.ObservedAt != now.UnixMilli() || response.Connections.Truncated || len(response.Connections.Interfaces) != ports*2 {
				t.Fatalf("connections=%+v", response.Connections)
			}
			indexes := make([]int, 0, len(response.Connections.Interfaces))
			groups := map[string]int{}
			for _, iface := range response.Connections.Interfaces {
				indexes = append(indexes, iface.Index)
				if iface.PhysicalPort == nil || iface.PhysicalPort.Source != "linux-sysfs" || iface.Driver != "mlx5_core" || len(iface.RDMADevices) != 1 || iface.SpeedMbps != 200000 || iface.Carrier == nil || !*iface.Carrier {
					t.Fatalf("incomplete native interface: %+v", iface)
				}
				groups[iface.PhysicalPort.PortName]++
			}
			if !slices.IsSorted(indexes) || len(groups) != ports {
				t.Fatalf("indexes=%v groups=%v", indexes, groups)
			}
			for port := 0; port < ports; port++ {
				if groups[fmt.Sprintf("p%d", port)] != 2 {
					t.Fatalf("physical port aliases=%v", groups)
				}
			}
		})
	}
}

func TestNodeInfoConnectionsMatchSharedWireContract(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	rows := []ConnectionInterface{
		connectionFixture(11, "p1"), connectionFixture(3, "p1"),
		connectionFixture(10, "p0"), connectionFixture(2, "p0"),
	}
	got := buildResponseAt(nil, nil, 0, statsSnapshot{}, "host-owner", nil, connectionReport(now, rows), now)
	want, err := os.ReadFile(filepath.Join("..", "shared", "testdata", "node-info-connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gotJSON, wantJSON any
	if json.Unmarshal(got, &gotJSON) != nil || json.Unmarshal(want, &wantJSON) != nil {
		t.Fatal("decode node-info connections wire contract")
	}
	// The shared connection fixture is platform-neutral; OS is the one dynamic
	// top-level contract field and must match the binary running this test.
	wantJSON.(map[string]any)["os"] = runtime.GOOS
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("node-info connections wire contract changed\ngot:  %s\nwant: %s", got, want)
	}
}

func TestConnectionReportFailsClosedWhenIncompleteOrOversized(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	rows := make([]ConnectionInterface, maxConnectionInterfaces+1)
	for i := range rows {
		rows[i] = connectionFixture(i+1, "p0")
	}
	rows[0].Addresses = make([]string, maxConnectionAddresses+1)
	report := connectionReport(now, rows)
	if !report.Truncated || len(report.Interfaces) != maxConnectionInterfaces || !report.Interfaces[0].AddressesUnavailable || len(report.Interfaces[0].Addresses) != 0 {
		t.Fatalf("oversized report was not bounded: %+v", report)
	}
	unavailable := unavailableConnectionReport(now)
	if unavailable.Status != "unavailable" || unavailable.Interfaces == nil || len(unavailable.Interfaces) != 0 {
		t.Fatalf("unavailable report fabricated facts: %+v", unavailable)
	}
}
