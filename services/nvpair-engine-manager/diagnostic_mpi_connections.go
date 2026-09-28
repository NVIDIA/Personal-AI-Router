// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/cableprobe"
)

// Selected native interface identity comes from the existing cable selection.
// Its address comes only from the participant's current node-info response.
type diagnosticMPIInterfaceSelection struct {
	Network   string               `json:"network"` // Explicit management Socket baseline or selected fabric.
	NodeID    string               `json:"nodeId"`
	Principal string               `json:"principal"`
	SwitchID  string               `json:"switchId"`
	PortName  string               `json:"portName"`
	Interface cableprobe.Interface `json:"interface"`
}
type diagnosticMPIConnections struct {
	HostUUID    string `json:"hostUuid"`
	Connections *struct {
		Source     string `json:"source"`
		Status     string `json:"status"`
		ObservedAt int64  `json:"observedAt"`
		Truncated  bool   `json:"truncated"`
		Interfaces []struct {
			Name                 string   `json:"name"`
			Index                int      `json:"index"`
			MAC                  string   `json:"mac"`
			Up                   bool     `json:"up"`
			Physical             bool     `json:"physical"`
			AddressesUnavailable bool     `json:"addressesUnavailable"`
			Addresses            []string `json:"addresses"`
			PhysicalPort         *struct {
				Source   string `json:"source"`
				SwitchID string `json:"switchId"`
				PortName string `json:"portName"`
			} `json:"physicalPort"`
		} `json:"interfaces"`
	} `json:"connections"`
}
type diagnosticMPICollectiveAddress struct {
	Selection diagnosticMPIInterfaceSelection `json:"selection"`
	Address   string                          `json:"address"`
	Subnet    string                          `json:"subnet"`
	AgeMs     int64                           `json:"ageMs"`
}

func projectDiagnosticMPIAddress(info diagnosticMPIConnections, selected diagnosticMPIInterfaceSelection, now time.Time) (diagnosticMPICollectiveAddress, error) {
	result := diagnosticMPICollectiveAddress{Selection: selected}
	c := info.Connections
	if (selected.Network != "management" && selected.Network != "fabric") || !cableInterfaceValid(selected.Interface) || !diagnosticToken.MatchString(selected.NodeID) || !diagnosticToken.MatchString(selected.Principal) ||
		info.HostUUID != selected.NodeID || c == nil || c.Source != "host-os" || c.Status != "observed" || c.Truncated ||
		c.ObservedAt <= 0 || c.ObservedAt > now.UnixMilli() || now.UnixMilli()-c.ObservedAt >= 2000 || len(c.Interfaces) == 0 || len(c.Interfaces) > 128 {
		return result, errors.New("current complete PAIR connection facts are required for the selected interface")
	}
	var subnet *net.IPNet
	selectedCount := 0
	for _, row := range c.Interfaces {
		if row.Name != selected.Interface.Name {
			continue
		}
		selectedCount++
		p := row.PhysicalPort
		if row.Index != selected.Interface.Index || !strings.EqualFold(row.MAC, selected.Interface.MAC) || !row.Up || !row.Physical || row.AddressesUnavailable || len(row.Addresses) > 32 ||
			(selected.Network == "fabric" && (p == nil || p.Source != "linux-sysfs" || p.SwitchID != selected.SwitchID || p.PortName != selected.PortName)) ||
			(selected.Network == "management" && (selected.SwitchID != "" || selected.PortName != "")) {
			return result, errors.New("selected native interface identity or availability changed")
		}
		for _, value := range row.Addresses {
			ip, network, err := net.ParseCIDR(value)
			if err != nil {
				return result, errors.New("selected interface address is unknown")
			}
			if ip.To4() == nil {
				continue
			}
			ones, bits := network.Mask.Size()
			// A routed ring cable is one point-to-point /31 whose two addresses are both hosts.
			pointToPoint := selected.Network == "fabric" && ones == 31
			if result.Address != "" || bits != 32 || ones < 1 || ones > 30 && !pointToPoint || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || !pointToPoint && ip.Equal(network.IP) {
				return result, errors.New("selected interface needs one unambiguous IPv4 host address and subnet")
			}
			broadcast := append(net.IP(nil), network.IP.To4()...)
			for i := range broadcast {
				broadcast[i] |= ^network.Mask[i]
			}
			if !pointToPoint && ip.Equal(broadcast) {
				return result, errors.New("selected interface address is not a unicast host")
			}
			result.Address, result.Subnet, subnet = ip.String(), network.String(), network
		}
	}
	if selectedCount != 1 || subnet == nil {
		return result, errors.New("selected physical interface has no unique current IPv4 address")
	}
	// OpenMPI CIDR inclusion can otherwise choose another alias or fall back.
	for _, row := range c.Interfaces {
		if !row.Up {
			continue
		}
		if row.AddressesUnavailable || len(row.Addresses) > 32 {
			return result, errors.New("complete interface addresses are required to bind the MPI subnet")
		}
		for _, value := range row.Addresses {
			ip, _, err := net.ParseCIDR(value)
			if err != nil {
				return result, errors.New("participant interface address is unknown")
			}
			if ip.To4() != nil && subnet.Contains(ip) && row.Name != selected.Interface.Name {
				return result, errors.New("collective subnet also selects a different native interface")
			}
		}
	}
	result.AgeMs = now.UnixMilli() - c.ObservedAt
	return result, nil
}

func (d *diagnosticService) localMPIAddress(ctx context.Context, selected diagnosticMPIInterfaceSelection) (diagnosticMPICollectiveAddress, error) {
	f := d.m.cableLocal
	if f == nil || f.nodeID != selected.NodeID || f.port <= 0 || f.port > 65535 {
		return diagnosticMPICollectiveAddress{}, errors.New("parent-owned PAIR node-info service is unavailable")
	}
	d.m.mesh.Refresh()
	if selected.Principal != d.m.mesh.NodeUUID() || !d.m.mesh.Clustered() {
		return diagnosticMPICollectiveAddress{}, errors.New("selected participant is no longer the admitted local identity")
	}
	certificate := cableSelfCertificate(d.m.mesh, selected.Principal)
	var info diagnosticMPIConnections
	if err := readCableJSON(ctx, f.http, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))+"/v1/node-info", &info); err != nil {
		return diagnosticMPICollectiveAddress{}, err
	}
	result, err := projectDiagnosticMPIAddress(info, selected, time.Now())
	d.m.mesh.Refresh()
	if !d.m.mesh.Clustered() || d.m.mesh.NodeUUID() != selected.Principal || len(certificate) == 0 || !bytes.Equal(certificate, cableSelfCertificate(d.m.mesh, selected.Principal)) {
		return diagnosticMPICollectiveAddress{}, errors.New("PAIR identity changed during the connection read")
	}
	return result, err
}

func (d *diagnosticService) callMPIAddress(ctx context.Context, member diagnosticMember, selected diagnosticMPIInterfaceSelection) (diagnosticMPICollectiveAddress, error) {
	if selected.NodeID != member.NodeID || selected.Principal != member.Principal {
		return diagnosticMPICollectiveAddress{}, errors.New("collective interface belongs to a different participant")
	}
	d.m.mesh.Refresh()
	if !d.m.mesh.Clustered() || !d.m.mesh.HasPin(member.Principal) {
		return diagnosticMPICollectiveAddress{}, errors.New("collective participant is no longer admitted")
	}
	if selected.Principal == d.m.mesh.NodeUUID() {
		return d.localMPIAddress(ctx, selected)
	}
	client, err := d.client(ctx, member)
	if err != nil {
		return diagnosticMPICollectiveAddress{}, err
	}
	began := time.Now()
	raw, err := client.postJSON(ctx, diagnosticControlPath, "", diagnosticControlRequest{Method: "mpi-address", MPISelection: &selected})
	if err != nil {
		return diagnosticMPICollectiveAddress{}, err
	}
	result := diagnosticMPICollectiveAddress{AgeMs: -1}
	if strictDiagnosticJSON(raw, &result) != nil || result.Selection != selected || result.AgeMs < 0 || result.AgeMs+time.Since(began).Milliseconds() >= 2000 {
		return diagnosticMPICollectiveAddress{}, errors.New("collective address observation changed or expired")
	}
	ip, network, err := net.ParseCIDR(result.Subnet)
	address := net.ParseIP(result.Address)
	if err != nil || ip.To4() == nil || address.To4() == nil || !network.Contains(address) {
		return diagnosticMPICollectiveAddress{}, errors.New("collective address observation has an invalid subnet")
	}
	result.AgeMs += time.Since(began).Milliseconds()
	return result, nil
}
