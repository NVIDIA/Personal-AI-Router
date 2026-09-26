// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
)

type fabricObservedInterface struct {
	fabricInterface
	Up                   bool  `json:"up"`
	Physical             *bool `json:"physical"`
	Carrier              *bool `json:"carrier"`
	SpeedMbps            int64 `json:"speedMbps"`
	AddressesUnavailable bool  `json:"addressesUnavailable"`
}
type fabricInventory struct {
	NodeID     string                    `json:"nodeId"`
	Principal  string                    `json:"principal"`
	ObservedAt int64                     `json:"observedAt"`
	Interfaces []fabricObservedInterface `json:"interfaces"`
	Routes     []string                  `json:"routes"`
	Digest     string                    `json:"digest"`
}

// Inventory is the existing PAIR node-info host observation, augmented with a
// bounded read of routes. Discovery and carrier do not establish a peer cable.
func readFabricInventory(ctx context.Context, local *cableLocalFacts, mesh *clustertrust.Mesh) (fabricInventory, error) {
	var out fabricInventory
	if local == nil || local.port < 1 || !cableIdentifier(local.nodeID, 128) {
		return out, errors.New("parent-owned node-info inventory unavailable")
	}
	mesh.Refresh()
	principal := mesh.NodeUUID()
	if !mesh.Clustered() || !mesh.HasPin(principal) {
		return out, errors.New("paired identity unavailable")
	}
	var info struct {
		HostUUID    string `json:"hostUuid"`
		Connections *struct {
			Source     string                    `json:"source"`
			Status     string                    `json:"status"`
			ObservedAt int64                     `json:"observedAt"`
			Truncated  bool                      `json:"truncated"`
			Interfaces []fabricObservedInterface `json:"interfaces"`
		} `json:"connections"`
	}
	if err := readCableJSON(ctx, local.http, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(local.port))+"/v1/node-info", &info); err != nil {
		return out, err
	}
	c := info.Connections
	if info.HostUUID != local.nodeID || c == nil || c.Source != "host-os" || c.Status != "observed" || c.Truncated || c.Interfaces == nil || len(c.Interfaces) > 128 || c.ObservedAt <= 0 || c.ObservedAt > time.Now().UnixMilli() || time.Now().UnixMilli()-c.ObservedAt > 2000 {
		return out, errors.New("current complete PAIR interface inventory unavailable")
	}
	routes, err := fabricNativeRoutes(ctx)
	if err != nil {
		return out, errors.New("current complete route inventory unavailable")
	}
	mesh.Refresh()
	if !mesh.Clustered() || mesh.NodeUUID() != principal {
		return out, errors.New("paired identity changed during inventory")
	}
	out = fabricInventory{NodeID: local.nodeID, Principal: principal, ObservedAt: c.ObservedAt, Interfaces: c.Interfaces, Routes: routes}
	sort.Slice(out.Interfaces, func(i, j int) bool { return out.Interfaces[i].Index < out.Interfaces[j].Index })
	sort.Strings(out.Routes)
	body, _ := json.Marshal([]any{out.NodeID, out.Principal, out.Interfaces, out.Routes})
	out.Digest = fmt.Sprintf("%x", sha256.Sum256(body))
	return out, nil
}

func fabricSelectedTarget(facts fabricInventory, ref cableprobe.PortRef) (fabricTarget, error) {
	out := fabricTarget{NodeID: facts.NodeID, Principal: facts.Principal, SwitchID: ref.SwitchID, PortName: ref.PortName, Interfaces: []fabricInterface{}}
	if facts.NodeID != ref.NodeID || !cableIdentifier(facts.Principal, 256) {
		return out, errors.New("selected participant identity changed")
	}
	indexes, names := map[int]bool{}, map[string]bool{}
	for _, iface := range facts.Interfaces {
		if iface.Index <= 0 || indexes[iface.Index] || names[iface.Name] || iface.AddressesUnavailable || iface.Addresses == nil {
			return out, errors.New("ambiguous interface or address inventory")
		}
		indexes[iface.Index], names[iface.Name] = true, true
		p := iface.PhysicalPort
		if p.SwitchID != ref.SwitchID || p.PortName != ref.PortName {
			continue
		}
		if p.Source != "linux-sysfs" || !iface.Up || iface.Physical == nil || !*iface.Physical || iface.Carrier == nil || !*iface.Carrier || iface.SpeedMbps != 200000 || iface.Driver != "mlx5_core" || len(iface.RDMADevices) != 1 || !fabricLinkLocalBaseline(iface.Addresses) || !cableInterfaceValid(cableprobe.Interface{Name: iface.Name, Index: iface.Index, MAC: iface.MAC}) {
			return out, errors.New("selected port requires two unconfigured active 200-Gbit ConnectX functions with complete native identity")
		}
		selected := iface.fabricInterface
		selected.GeneratedDefault = nil // Only authenticated native INSPECT supplies pause bindings.
		out.Interfaces = append(out.Interfaces, selected)
	}
	if len(out.Interfaces) != 2 || out.Interfaces[0].RDMADevices[0] == out.Interfaces[1].RDMADevices[0] || out.Interfaces[0].MAC == out.Interfaces[1].MAC {
		return out, errors.New("selected physical port must expose exactly two independent RDMA functions")
	}
	return out, nil
}

// A ring uses one function from each physical port. The lowest ifindex is the
// primary function; the sibling remains untouched and collision-visible.
func fabricSelectedRingTarget(facts fabricInventory, refs []cableprobe.PortRef) (fabricTarget, error) {
	if len(refs) != 2 || refs[0].NodeID != facts.NodeID || refs[1].NodeID != facts.NodeID || refs[0] == refs[1] {
		return fabricTarget{}, errors.New("ring participants require two distinct physical ports")
	}
	out := fabricTarget{NodeID: facts.NodeID, Principal: facts.Principal, Ports: []fabricTargetPort{}, Interfaces: []fabricInterface{}}
	for _, ref := range refs {
		selected, err := fabricSelectedTarget(facts, ref)
		if err != nil {
			return fabricTarget{}, err
		}
		iface := selected.Interfaces[0]
		if selected.Interfaces[1].Index < iface.Index {
			iface = selected.Interfaces[1]
		}
		out.Ports = append(out.Ports, fabricTargetPort{SwitchID: ref.SwitchID, PortName: ref.PortName})
		out.Interfaces = append(out.Interfaces, iface)
	}
	if out.Ports[0].PortName > out.Ports[1].PortName {
		out.Ports[0], out.Ports[1] = out.Ports[1], out.Ports[0]
		out.Interfaces[0], out.Interfaces[1] = out.Interfaces[1], out.Interfaces[0]
	}
	if out.Ports[0].SwitchID != out.Ports[1].SwitchID || out.Ports[0].PortName != "p0" || out.Ports[1].PortName != "p1" || out.Interfaces[0].Index == out.Interfaces[1].Index {
		return fabricTarget{}, errors.New("ring participants require exact p0 and p1 primary functions on one ConnectX switch")
	}
	return out, nil
}

// Empty or a bounded canonical /64 link-local set is only a review candidate.
// Native generated-profile inspection and separate pause consent are still
// required before a nonempty baseline can reach an address operation.
func fabricLinkLocalBaseline(addresses []string) bool {
	if addresses == nil || len(addresses) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, address := range addresses {
		p, err := netip.ParsePrefix(address)
		if err != nil || !p.Addr().Is6() || !p.Addr().IsLinkLocalUnicast() || p.Bits() != 64 || p.String() != address || seen[address] {
			return false
		}
		seen[address] = true
	}
	return true
}

func fabricAllocate(targets []fabricTarget, inventories []fabricInventory) error {
	if len(targets) != 2 || len(inventories) != 2 {
		return errors.New("exactly two selected participants required")
	}
	used := []netip.Prefix{}
	for _, inventory := range inventories {
		cidrs := append([]string{}, inventory.Routes...)
		for _, iface := range inventory.Interfaces {
			cidrs = append(cidrs, iface.Addresses...)
		}
		for _, cidr := range cidrs {
			p, e := netip.ParsePrefix(cidr)
			if e != nil {
				return errors.New("invalid route or address inventory")
			}
			if p.Addr().Is4() && p.Bits() != 0 {
				used = append(used, p.Masked())
			}
		}
	}
	// ponytail: two private /30 lanes only; arbitrary rings require their own recipe.
	lanes := []netip.Prefix{}
	for n := 0; n < 4096 && len(lanes) < 2; n++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 31, byte(240 + n/64), byte((n % 64) * 4)}), 30)
		if n >= 1024 {
			p = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 253, byte((n - 1024) / 64), byte((n % 64) * 4)}), 30)
		}
		conflict := false
		for _, other := range used {
			if p.Overlaps(other) {
				conflict = true
				break
			}
		}
		if !conflict {
			lanes = append(lanes, p)
			used = append(used, p)
		}
	}
	if len(lanes) != 2 {
		return errors.New("no collision-free bounded private address lanes available")
	}
	for node := range targets {
		for lane := range targets[node].Interfaces {
			a := lanes[lane].Addr().Next()
			if node == 1 {
				a = a.Next()
			}
			targets[node].Interfaces[lane].Address = netip.PrefixFrom(a, 30).String()
		}
	}
	return nil
}

func fabricAllocateRing(targets []fabricTarget, inventories []fabricInventory) error {
	if len(targets) != 3 || len(inventories) != 3 {
		return errors.New("exactly three ring participants required")
	}
	pool := netip.MustParsePrefix("10.253.0.0/29")
	for _, inventory := range inventories {
		cidrs := append([]string{}, inventory.Routes...)
		for _, iface := range inventory.Interfaces {
			cidrs = append(cidrs, iface.Addresses...)
		}
		for _, cidr := range cidrs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return errors.New("invalid route or address inventory")
			}
			if prefix.Addr().Is4() && prefix.Overlaps(pool) {
				return errors.New("sealed ring address pool conflicts with current routes or addresses")
			}
		}
	}
	addresses := map[int]map[string]string{
		0: {"p0": "10.253.0.0/31", "p1": "10.253.0.2/31"},
		1: {"p0": "10.253.0.4/31", "p1": "10.253.0.1/31"},
		2: {"p0": "10.253.0.3/31", "p1": "10.253.0.5/31"},
	}
	for role := range targets {
		if len(targets[role].Interfaces) != 2 {
			return errors.New("ring participant primary interfaces are incomplete")
		}
		for i := range targets[role].Interfaces {
			iface := &targets[role].Interfaces[i]
			address, ok := addresses[role][iface.PhysicalPort.PortName]
			if !ok {
				return errors.New("ring participant port role is invalid")
			}
			iface.Address = address
		}
	}
	return nil
}

func fabricTargetPortRefs(target fabricTarget) []cableprobe.PortRef {
	if len(target.Ports) != 0 {
		refs := make([]cableprobe.PortRef, 0, len(target.Ports))
		for _, port := range target.Ports {
			refs = append(refs, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
		}
		return refs
	}
	return []cableprobe.PortRef{{NodeID: target.NodeID, SwitchID: target.SwitchID, PortName: target.PortName}}
}

func fabricTargetFromInventory(want fabricTarget, facts fabricInventory) (fabricTarget, error) {
	refs := fabricTargetPortRefs(want)
	if len(refs) == 2 {
		return fabricSelectedRingTarget(facts, refs)
	}
	if len(refs) != 1 {
		return fabricTarget{}, errors.New("invalid fabric target port binding")
	}
	return fabricSelectedTarget(facts, refs[0])
}

func fabricSameTarget(want fabricTarget, facts fabricInventory) bool {
	actual, err := fabricTargetFromInventory(want, facts)
	if err != nil {
		return false
	}
	for i := range actual.Interfaces {
		actual.Interfaces[i].Address = want.Interfaces[i].Address
		// Node inventory cannot supply privileged profile evidence. The native
		// worker independently rechecks this reviewed binding before effects.
		actual.Interfaces[i].GeneratedDefault = want.Interfaces[i].GeneratedDefault
		// A consented generated default gains and loses its link-local address
		// with every DHCP attempt; anything beyond link-local still differs.
		if want.Interfaces[i].GeneratedDefault != nil && fabricLinkLocalBaseline(append([]string{}, actual.Interfaces[i].Addresses...)) {
			actual.Interfaces[i].Addresses = want.Interfaces[i].Addresses
		}
	}
	return reflect.DeepEqual(want, actual)
}

func fabricSameIdentity(want fabricTarget, facts fabricInventory) bool {
	if want.NodeID != facts.NodeID || want.Principal != facts.Principal || len(want.Interfaces) != 2 {
		return false
	}
	for _, expected := range want.Interfaces {
		found := false
		for _, actual := range facts.Interfaces {
			if actual.Index == expected.Index {
				found = fabricNativeIdentity(expected, actual.fabricInterface) == nil
			}
		}
		if !found {
			return false
		}
	}
	return true
}
