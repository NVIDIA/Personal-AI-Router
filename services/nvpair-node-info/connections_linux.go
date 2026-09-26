// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const connectionAttributeLimit = 4096

func readConnectionAttribute(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, connectionAttributeLimit+1))
	if err != nil || len(data) > connectionAttributeLimit {
		return "", os.ErrInvalid
	}
	return strings.TrimSpace(string(data)), nil
}

func connectionInterface(iface net.Interface) ConnectionInterface {
	row := ConnectionInterface{
		Name: iface.Name, Index: iface.Index, MAC: strings.ToLower(iface.HardwareAddr.String()),
		Up: iface.Flags&net.FlagUp != 0, Addresses: []string{}, RDMADevices: []string{}, MTU: iface.MTU,
	}
	addresses, err := iface.Addrs()
	if err != nil {
		row.AddressesUnavailable = true
	} else {
		for _, address := range addresses {
			row.Addresses = append(row.Addresses, address.String())
		}
	}

	base := filepath.Join("/sys/class/net", iface.Name)
	if _, err := os.Stat(filepath.Join(base, "device")); err == nil {
		row.Physical = true
	}
	if value, err := readConnectionAttribute(filepath.Join(base, "carrier")); err == nil && (value == "0" || value == "1") {
		carrier := value == "1"
		row.Carrier = &carrier
	}
	if value, err := readConnectionAttribute(filepath.Join(base, "speed")); err == nil {
		if speed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && speed > 0 {
			row.SpeedMbps = speed
		}
	}
	switchID, switchErr := readConnectionAttribute(filepath.Join(base, "phys_switch_id"))
	portName, portErr := readConnectionAttribute(filepath.Join(base, "phys_port_name"))
	if switchErr == nil && portErr == nil && switchID != "" && portName != "" {
		row.PhysicalPort = &PhysicalPortInfo{Source: "linux-sysfs", SwitchID: switchID, PortName: portName}
	}
	if driver, err := os.Readlink(filepath.Join(base, "device", "driver")); err == nil {
		row.Driver = filepath.Base(driver)
	}
	if devices, err := os.ReadDir(filepath.Join(base, "device", "infiniband")); err == nil && len(devices) <= 16 {
		for _, device := range devices {
			row.RDMADevices = append(row.RDMADevices, device.Name())
		}
	}
	sort.Strings(row.Addresses)
	sort.Strings(row.RDMADevices)
	return row
}

func observeConnections() *ConnectionInfo {
	observedAt := time.Now()
	interfaces, err := net.Interfaces()
	if err != nil {
		return unavailableConnectionReport(observedAt)
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Index < interfaces[j].Index })
	if len(interfaces) > maxConnectionInterfaces+1 {
		interfaces = interfaces[:maxConnectionInterfaces+1]
	}
	rows := make([]ConnectionInterface, 0, min(len(interfaces), maxConnectionInterfaces+1))
	for _, iface := range interfaces {
		rows = append(rows, connectionInterface(iface))
	}
	return connectionReport(observedAt, rows)
}
