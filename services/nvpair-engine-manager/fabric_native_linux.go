// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func fabricNativeRead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, fabricNativeMaxBytes+1))
	if err != nil || len(data) > fabricNativeMaxBytes {
		return nil, errors.New("native file exceeds safe read bound")
	}
	return data, nil
}

func fabricNativeTool(name string) (string, error) {
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || st.Uid != 0 {
			return "", errors.New("native tool is not a trusted root-owned executable")
		}
		return path, nil
	}
	return "", os.ErrNotExist
}

func fabricNativeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := fabricNativeTool(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := diagnosticProcess(ctx, path, args, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "TERM=dumb", "PAGER="}, nil)
	if err != nil {
		return nil, fmt.Errorf("bounded native %s query failed: %w", name, err)
	}
	return data, nil
}

func fabricNativeRoutes(ctx context.Context) ([]string, error) {
	data, err := fabricNativeCommand(ctx, "ip", "-j", "-4", "route", "show", "table", "all")
	if err != nil {
		return nil, err
	}
	_, prefixes, err := fabricParseRoutes(data)
	return prefixes, err
}

func fabricNativeRouteQualified(ctx context.Context, iface fabricInterface, peer string) (fabricRDMABinding, error) {
	prefix, err := fabricNativePrefix(iface)
	peerAddr, peerErr := netip.ParseAddr(peer)
	if err != nil || peerErr != nil || (prefix.Bits() != 30 && prefix.Bits() != 31) || !prefix.Contains(peerAddr) || peerAddr == prefix.Addr() {
		return fabricRDMABinding{}, fabricQualificationFailure("route-unavailable", errors.New("invalid sealed fabric route proof"))
	}
	current, err := fabricNativeInterfaceCurrent(iface, true)
	if err != nil || !slices.Contains(current.Addresses, iface.Address) {
		return fabricRDMABinding{}, fabricQualificationFailure("route-unavailable", errors.New("fabric interface identity or owned address changed"))
	}
	data, err := fabricNativeCommand(ctx, "ip", "-j", "-4", "route", "get", peer, "from", prefix.Addr().String(), "oif", iface.Name)
	if err != nil || !fabricRouteSelectionQualified(data, iface, peer, "") {
		return fabricRDMABinding{}, fabricQualificationFailure("route-unavailable", errors.New("source/interface-constrained fabric route proof failed"))
	}
	binding, err := fabricNativeRDMABinding(iface, prefix.Addr())
	if err != nil {
		return fabricRDMABinding{}, fabricQualificationFailure("gid-unavailable", err)
	}
	return binding, nil
}

// The query names no output interface: the kernel's own selection for this
// source must be the reviewed gateway on this interface, not another link.
func fabricNativeRoutedQualified(ctx context.Context, iface fabricInterface, route fabricRoute) error {
	prefix, err := fabricNativePrefix(iface)
	destination, destinationErr := netip.ParsePrefix(route.Destination)
	if err != nil || destinationErr != nil || !validFabricInterfaceRoutes(iface) || !slices.Contains(iface.Routes, route) {
		return fabricQualificationFailure("route-unavailable", errors.New("invalid sealed fabric routed proof"))
	}
	current, err := fabricNativeInterfaceCurrent(iface, true)
	if err != nil || !slices.Contains(current.Addresses, iface.Address) {
		return fabricQualificationFailure("route-unavailable", errors.New("fabric interface identity or owned address changed"))
	}
	peer := destination.Addr().String()
	data, err := fabricNativeCommand(ctx, "ip", "-j", "-4", "route", "get", peer, "from", prefix.Addr().String())
	if err != nil || !fabricRouteSelectionQualified(data, iface, peer, route.Gateway) {
		return fabricQualificationFailure("route-unavailable", errors.New("source-constrained fabric routed proof failed"))
	}
	return nil
}

func fabricNativeRDMABinding(iface fabricInterface, address netip.Addr) (fabricRDMABinding, error) {
	if len(iface.RDMADevices) != 1 || filepath.Base(iface.RDMADevices[0]) != iface.RDMADevices[0] {
		return fabricRDMABinding{}, errors.New("exact RDMA device binding is unavailable")
	}
	portsRoot := filepath.Join("/sys/class/infiniband", iface.RDMADevices[0], "ports")
	ports, err := os.ReadDir(portsRoot)
	if err != nil || len(ports) == 0 || len(ports) > 16 {
		return fabricRDMABinding{}, errors.New("RDMA port inventory is unavailable")
	}
	var matches []fabricRDMABinding
	for _, portEntry := range ports {
		port, err := strconv.Atoi(portEntry.Name())
		if err != nil || port < 1 || port > 255 || !portEntry.IsDir() {
			continue
		}
		attrs := filepath.Join(portsRoot, portEntry.Name(), "gid_attrs")
		indices, err := os.ReadDir(filepath.Join(attrs, "ndevs"))
		if err != nil || len(indices) > 256 {
			continue
		}
		for _, entry := range indices {
			index, err := strconv.Atoi(entry.Name())
			if err != nil || index < 0 || index > 255 || entry.IsDir() {
				continue
			}
			ndev, ndevErr := fabricNativeRead(filepath.Join(attrs, "ndevs", entry.Name()))
			if ndevErr != nil || strings.TrimSpace(string(ndev)) != iface.Name {
				continue
			}
			typeData, typeErr := fabricNativeRead(filepath.Join(attrs, "types", entry.Name()))
			gidData, gidErr := fabricNativeRead(filepath.Join(portsRoot, portEntry.Name(), "gids", entry.Name()))
			gid, parseErr := netip.ParseAddr(strings.TrimSpace(string(gidData)))
			if typeErr == nil && gidErr == nil && parseErr == nil && strings.TrimSpace(string(typeData)) == "RoCE v2" && gid.Unmap() == address {
				matches = append(matches, fabricRDMABinding{Port: port, GIDIndex: index, GIDType: "RoCE v2"})
			}
		}
	}
	if len(matches) != 1 {
		return fabricRDMABinding{}, errors.New("exact RoCE v2 GID binding is unavailable or ambiguous")
	}
	return matches[0], nil
}

func fabricNativeInterfaceCurrent(want fabricInterface, requireUp bool) (fabricInterface, error) {
	if err := fabricNativeIdentity(want, want); err != nil {
		return fabricInterface{}, err
	}
	actual, err := net.InterfaceByIndex(want.Index)
	if err != nil {
		return fabricInterface{}, errors.New("reviewed native interface is unavailable")
	}
	got := fabricInterface{Name: actual.Name, Index: actual.Index, MAC: actual.HardwareAddr.String(), MTU: actual.MTU}
	if got.Name != want.Name {
		return got, errors.New("native interface name changed")
	}
	base := filepath.Join("/sys/class/net", got.Name)
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) > 256 {
		return got, errors.New("native interface dependency inventory unavailable")
	}
	var entryNames []string
	for _, entry := range entries {
		entryNames = append(entryNames, entry.Name())
	}
	if fabricNativeHasLinkDependency(entryNames) {
		return got, errors.New("selected native interface has an existing master or dependent link; preserved")
	}
	switchID, port, err := nativeCableProbePhysicalPort(got.Name)
	if err != nil {
		return got, err
	}
	got.PhysicalPort = fabricPhysicalPort{Source: "linux-sysfs", SwitchID: switchID, PortName: port}
	driver, err := os.Readlink(filepath.Join(base, "device", "driver"))
	if err != nil {
		return got, errors.New("native driver identity unavailable")
	}
	got.Driver = filepath.Base(driver)
	devices, err := os.ReadDir(filepath.Join(base, "device", "infiniband"))
	if err != nil || len(devices) == 0 || len(devices) > 16 {
		return got, errors.New("native RDMA identity unavailable")
	}
	for _, device := range devices {
		got.RDMADevices = append(got.RDMADevices, device.Name())
	}
	addresses, err := actual.Addrs()
	if err != nil {
		return got, err
	}
	for _, address := range addresses {
		got.Addresses = append(got.Addresses, address.String())
	}
	got.Addresses = fabricSorted(got.Addresses)
	if requireUp {
		carrier, err := fabricNativeRead(filepath.Join(base, "carrier"))
		if err != nil || strings.TrimSpace(string(carrier)) != "1" || actual.Flags&net.FlagUp == 0 || actual.Flags&net.FlagRunning == 0 {
			return got, errors.New("selected native interface is not up with carrier")
		}
	}
	if err := fabricNativeIdentity(want, got); err != nil {
		return got, err
	}
	after, err := net.InterfaceByIndex(want.Index)
	if err != nil || after.Name != actual.Name || after.HardwareAddr.String() != actual.HardwareAddr.String() || after.MTU != actual.MTU || after.Flags != actual.Flags {
		return got, errors.New("native interface changed during identity inspection")
	}
	return got, nil
}

// Resolve systemd's Type from udev DEVTYPE, falling back to the native link type.
// The ip detailed response supplies the complete alternate names and netlink kind.
func fabricNativeConfigLinks(ctx context.Context, targets []fabricInterface) (map[string]fabricConfigLink, error) {
	invalid := errors.New("selected configuration match metadata is unavailable")
	versionData, err := fabricNativeCommand(ctx, "systemd", "--version")
	var version int
	if err != nil {
		return nil, invalid
	}
	if n, err := fmt.Sscanf(string(versionData), "systemd %d", &version); err != nil || n != 1 || version < 251 {
		return nil, invalid
	}
	links := map[string]fabricConfigLink{}
	for _, target := range targets {
		data, err := fabricNativeCommand(ctx, "ip", "-j", "-d", "link", "show", "dev", target.Name)
		var rows []struct {
			Index    int      `json:"ifindex"`
			Name     string   `json:"ifname"`
			MAC      string   `json:"address"`
			Type     string   `json:"link_type"`
			AltNames []string `json:"altnames"`
			Info     *struct {
				Kind string `json:"info_kind"`
			} `json:"linkinfo"`
		}
		if err != nil || json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
			return nil, invalid
		}
		row := rows[0]
		if row.Index != target.Index || row.Name != target.Name || !strings.EqualFold(row.MAC, target.MAC) || row.Type != "ether" || len(row.AltNames) > 32 {
			return nil, invalid
		}
		link := fabricConfigLink{Names: append([]string{row.Name}, row.AltNames...), Type: row.Type, SystemdVersion: version}
		seen := map[string]bool{}
		for _, name := range link.Names {
			if name == "" || len(name) > 128 || seen[name] || strings.IndexFunc(name, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
				return nil, invalid
			}
			seen[name] = true
		}
		if row.Info != nil {
			if row.Info.Kind == "" || strings.ContainsAny(row.Info.Kind, " \t\r\n\x00") {
				return nil, invalid
			}
			link.Kind = row.Info.Kind
		}
		properties, err := fabricNativeCommand(ctx, "udevadm", "info", "--query=property", "--path=/sys/class/net/"+target.Name)
		if err != nil {
			return nil, invalid
		}
		deviceType, err := fabricConfigDeviceType(properties, row.Name, row.Index)
		if err != nil {
			return nil, invalid
		}
		if deviceType != "" {
			link.Type = deviceType
		}
		link.Known = true
		links[target.Name] = link
	}
	return links, nil
}

func fabricNativeConfigSnapshot(targets []fabricInterface, admittedNMFiles map[string]bool, links map[string]fabricConfigLink) (map[string]string, []string, error) {
	digests := map[string]string{}
	var blockers []string
	total := 0
	for _, dir := range []string{"/etc/netplan", "/lib/netplan", "/run/netplan", "/etc/NetworkManager/system-connections", "/run/NetworkManager/system-connections", "/usr/lib/NetworkManager/system-connections", "/etc/systemd/network", "/run/systemd/network", "/usr/lib/systemd/network", "/etc/network/interfaces.d"} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			digests[dir] = "absent"
			continue
		}
		if err != nil || len(entries) > 256 {
			return nil, nil, errors.New("complete native network configuration inventory unavailable")
		}
		digests[dir] = "present"
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(dir, name)
			disposition := fabricConfigEntryDisposition(dir, name, entry.IsDir(), entry.Type()&os.ModeSymlink != 0)
			if disposition == "nested" || disposition == "linked" {
				blockers = append(blockers, disposition+" network configuration requires review: "+path)
			}
			if disposition != "read" {
				continue
			}
			data, err := fabricNativeRead(path)
			if err != nil {
				return nil, nil, errors.New("native network configuration is unreadable")
			}
			total += len(data)
			if total > fabricNativeMaxBytes {
				return nil, nil, errors.New("native network configuration exceeds snapshot bound")
			}
			digests[path] = fabricNativeHash(data)
			if !admittedNMFiles[path] && !fabricNativeConfigUnrelated(path, data, targets, links) {
				blockers = append(blockers, "existing or ambiguous network configuration requires review: "+path)
			}
		}
	}
	// ifupdown can source other arbitrary files; any active declaration holds.
	if data, err := fabricNativeRead("/etc/network/interfaces"); err == nil {
		digests["/etc/network/interfaces"] = fabricNativeHash(data)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			if line != "" && line != "auto lo" && line != "iface lo inet loopback" {
				blockers = append(blockers, "existing ifupdown configuration requires review")
				break
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	return digests, fabricSorted(blockers), nil
}

func fabricNativeInspect(ctx context.Context, targets []fabricInterface) (fabricNativeFacts, error) {
	return fabricNativeInspectAt(ctx, targets, false, false)
}

func fabricNativeInspectRebind(ctx context.Context, targets []fabricInterface, generatedRebindApproved bool) (fabricNativeFacts, error) {
	return fabricNativeInspectAt(ctx, targets, false, generatedRebindApproved)
}

// The post-pause read is only used by nativeAdd after its durable generated
// pause. It permits the approved LL baseline to disappear, never new IP state.
func fabricNativeInspectAt(ctx context.Context, targets []fabricInterface, afterGeneratedPause, generatedRebindApproved bool) (fabricNativeFacts, error) {
	var facts fabricNativeFacts
	if len(targets) == 0 || len(targets) > 8 {
		return facts, fabricInspectionError("native-inspect-failed", errors.New("invalid native fabric target count"))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	targets = slices.Clone(targets)
	snapshot := map[string]any{}
	admittedNMFiles := map[string]bool{}
	nmObserved := false
	if _, err := fabricNativeTool("nmcli"); err == nil {
		nm, err := fabricNMReadSettled(ctx, fabricNativeCommand, targets)
		if err != nil {
			return facts, fabricInspectionError("network-manager-query-failed", err)
		}
		nmObserved = true
		for i := range targets {
			iface := &targets[i]
			if afterGeneratedPause {
				device := nm.Devices[iface.Name]
				if !fabricNMGeneratedDevice(nm, *iface) || device.Autoconnect || device.State != 30 || device.UUID != "" || device.ActivePath != "" {
					return facts, fabricInspectionError("network-manager-device-unavailable", errors.New("generated pause is not confirmed before owned activation"))
				}
			} else {
				d, err := fabricNMGeneratedCandidate(nm, *iface)
				if err != nil || (iface.GeneratedDefault != nil && !fabricNMGeneratedSame(iface.GeneratedDefault, d) && (!generatedRebindApproved || !fabricNMGeneratedStableSame(iface.GeneratedDefault, d))) {
					return facts, fabricInspectionError("network-manager-generation-changed", errors.New("reviewed generated default is unavailable or changed"))
				}
				if iface.GeneratedDefault == nil || generatedRebindApproved && fabricNMGeneratedStableSame(iface.GeneratedDefault, d) {
					iface.GeneratedDefault = d
				}
				if iface.GeneratedDefault != nil {
					facts.GeneratedDefaults = append(facts.GeneratedDefaults, *iface.GeneratedDefault)
				}
			}
			if iface.GeneratedDefault != nil {
				if err := fabricNMGeneratedProof(ctx, fabricNativeCommand, nm, *iface, afterGeneratedPause); err != nil {
					return facts, fabricInspectionError("network-manager-query-failed", err)
				}
			} else if len(iface.Addresses) != 0 {
				return facts, fabricInspectionError("interface-changed", errors.New("link-local interruption requires a qualified generated default"))
			}
		}
		snapshot["networkManager"] = fabricNativeHash(nm)
		facts.Blockers = append(facts.Blockers, fabricNMPreflight(nm, targets, "")...)
		for _, profile := range nm.Profiles {
			for _, iface := range targets {
				if (fabricNMCompatibleProfile(profile, iface, nm.Devices[iface.Name]) || fabricNMGeneratedPending(nm, iface, profile)) && profile.Filename != "" {
					admittedNMFiles[profile.Filename] = true
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return facts, fabricInspectionError("network-manager-unavailable", err)
	} else if _, statErr := os.Stat("/run/NetworkManager"); !errors.Is(statErr, os.ErrNotExist) {
		return facts, fabricInspectionError("network-manager-unavailable", errors.New("NetworkManager state exists but its ownership query is unavailable"))
	} else {
		for _, iface := range targets {
			if iface.GeneratedDefault != nil || len(iface.Addresses) != 0 {
				return facts, fabricInspectionError("network-manager-unavailable", errors.New("generated-default scope requires NetworkManager"))
			}
		}
	}
	for _, target := range targets {
		if target.GeneratedDefault != nil && !nmObserved {
			return facts, fabricInspectionError("network-manager-unavailable", errors.New("reviewed generated-default manager is unavailable"))
		}
	}
	for _, family := range []string{"-4", "-6"} {
		data, err := fabricNativeCommand(ctx, "ip", "-j", family, "route", "show", "table", "all")
		if err != nil {
			return facts, fabricInspectionError("route-query-failed", err)
		}
		rows, prefixes, err := fabricParseRoutes(data)
		if err != nil {
			return facts, fabricInspectionError("route-data-invalid", err)
		}
		snapshot[family+"routes"] = rows
		facts.Routes = append(facts.Routes, prefixes...)
		facts.Blockers = append(facts.Blockers, fabricRouteBlockers(rows, targets)...)
		rules, err := fabricNativeCommand(ctx, "ip", "-j", family, "rule", "show")
		if err != nil {
			return facts, fabricInspectionError("rule-query-failed", err)
		}
		snapshot[family+"rules"] = json.RawMessage(rules)
		if !fabricNativeRulesSafe(rules) {
			facts.Blockers = append(facts.Blockers, "policy routing is outside the supported fabric address profile")
		}
	}
	all, err := net.Interfaces()
	if err != nil {
		return facts, fabricInspectionError("address-inventory-unavailable", err)
	}
	if len(all) > 1024 {
		return facts, fabricInspectionError("address-inventory-unavailable", errors.New("full native address inventory unavailable"))
	}
	for _, iface := range all {
		addresses, err := iface.Addrs()
		if err != nil {
			return facts, fabricInspectionError("address-inventory-unavailable", err)
		}
		var cidrs []string
		for _, address := range addresses {
			cidrs = append(cidrs, address.String())
			facts.Routes = append(facts.Routes, address.String())
		}
		snapshot["addresses:"+strconv.Itoa(iface.Index)] = fabricSorted(cidrs)
	}
	matchLinks, err := fabricNativeConfigLinks(ctx, targets)
	if err != nil {
		return facts, fabricInspectionError("configuration-unavailable", err)
	}
	snapshot["config-match"] = matchLinks
	config, blockers, err := fabricNativeConfigSnapshot(targets, admittedNMFiles, matchLinks)
	if err != nil {
		return facts, fabricInspectionError("configuration-unavailable", err)
	}
	snapshot["config"] = config
	facts.Blockers = append(facts.Blockers, blockers...)
	dns, err := fabricNativeRead("/etc/resolv.conf")
	if err != nil {
		return facts, fabricInspectionError("dns-unavailable", err)
	}
	snapshot["dns"] = fabricNativeHash(dns)
	facts.Blockers = append(facts.Blockers, fabricNativeDNSBlockers(dns, targets)...)
	for _, target := range targets {
		actual, err := fabricNativeInterfaceCurrent(target, true)
		if err != nil {
			return facts, fabricInspectionError("interface-changed", err)
		}
		if !fabricNativeAddressesMatch(target, actual, afterGeneratedPause) {
			return facts, fabricInspectionError("interface-changed", errors.New("reviewed native address set changed"))
		}
		if target.GeneratedDefault != nil {
			addresses, err := fabricNativeAddressState(ctx, target)
			if err != nil || !fabricNativeLinkLocalOnly(addresses) {
				return facts, fabricInspectionError("address-inventory-unavailable", errors.New("generated-default native baseline is not stable link-local-only"))
			}
			var cidrs []string
			for cidr := range addresses {
				cidrs = append(cidrs, cidr)
			}
			if !slices.Equal(fabricSorted(cidrs), fabricSorted(actual.Addresses)) {
				return facts, fabricInspectionError("interface-changed", errors.New("native address baseline changed during inspection"))
			}
		}
		snapshot["target:"+target.Name] = actual
		if _, err := fabricNativeTool("resolvectl"); err == nil {
			data, err := fabricNativeCommand(ctx, "resolvectl", "dns", target.Name)
			if err != nil {
				return facts, fabricInspectionError("dns-unavailable", err)
			}
			snapshot["resolved:"+target.Name] = fabricNativeHash(data)
			prefix := fmt.Sprintf("Link %d (%s):", target.Index, target.Name)
			if strings.TrimSpace(string(data)) != prefix {
				facts.Blockers = append(facts.Blockers, "selected interface has DNS state or ambiguous resolver ownership: "+target.Name)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return facts, fabricInspectionError("dns-unavailable", err)
		} else if _, statErr := os.Stat("/run/systemd/resolve"); !errors.Is(statErr, os.ErrNotExist) {
			return facts, fabricInspectionError("dns-unavailable", errors.New("system resolver state exists but its per-link query is unavailable"))
		}
		if _, err := fabricNativeInterfaceCurrent(target, true); err != nil {
			return facts, fabricInspectionError("interface-changed", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return facts, fabricInspectionError("native-inspect-failed", err)
	}
	facts.Routes, facts.Blockers = fabricSorted(facts.Routes), fabricSorted(facts.Blockers)
	facts.Digest = fabricNativeHash(snapshot)
	return facts, nil
}

// This private, boot-scoped receipt exists before NEWADDR. A crash after the
// kernel ACK still leaves the exact random deletion label recoverable. A user
// profile's journal alone is never accepted as permission to delete an address.
func fabricNativeJournal() (*os.File, *os.File, error) {
	if os.Geteuid() != 0 {
		return nil, nil, errors.New("fabric address mutation requires the explicitly approved root worker")
	}
	if err := os.Mkdir("/run/nvpair-fabric", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, err
	}
	fd, err := unix.Open("/run/nvpair-fabric", unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	dir := os.NewFile(uintptr(fd), "nvpair-fabric")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != 0 || stat.Mode&0777 != 0700 {
		dir.Close()
		return nil, nil, errors.New("fabric journal directory ownership is unsafe")
	}
	lockFD, err := unix.Openat(fd, "lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		dir.Close()
		return nil, nil, err
	}
	lock := os.NewFile(uintptr(lockFD), "lock")
	if err := unix.Fstat(lockFD, &stat); err != nil || stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Size != 0 {
		lock.Close()
		dir.Close()
		return nil, nil, errors.New("fabric journal lock ownership is unsafe")
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		dir.Close()
		return nil, nil, errors.New("another native fabric address operation is active")
	}
	return dir, lock, nil
}

func fabricNativeReceiptFile(dir *os.File, iface fabricInterface, operation string, create bool, nm ...*fabricNMReceipt) (fabricNativeReceipt, error) {
	var receipt fabricNativeReceipt
	if len(operation) < 16 || len(operation) > 128 || strings.ContainsAny(operation, "\x00\r\n/\\") {
		return receipt, errors.New("invalid fabric operation identity")
	}
	name := fabricNativeHash([]any{operation, iface.Index, iface.Address}) + ".json"
	flags := unix.O_RDONLY
	if create {
		flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return receipt, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Size > 16384 {
		return receipt, errors.New("native fabric receipt ownership is unsafe")
	}
	boot, err := fabricNativeRead("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return receipt, err
	}
	if create {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return receipt, err
		}
		receipt = fabricNativeReceipt{Operation: operation, Interface: iface, Label: base64.RawURLEncoding.EncodeToString(nonce[:])[:15], BootID: strings.TrimSpace(string(boot))}
		if len(nm) == 1 {
			receipt.NM = nm[0]
			if !fabricNMReceiptValid(receipt) {
				return receipt, errors.New("invalid NetworkManager creation receipt")
			}
		}
		if err := json.NewEncoder(f).Encode(receipt); err != nil {
			return receipt, err
		}
		if err := f.Sync(); err != nil {
			return receipt, err
		}
		if err := dir.Sync(); err != nil {
			return receipt, err
		}
	} else {
		decoder := json.NewDecoder(io.LimitReader(f, 16385))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&receipt); err != nil {
			return receipt, err
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return receipt, errors.New("native fabric receipt has trailing data")
		}
		if receipt.Operation != operation || receipt.BootID == "" || receipt.BootID != strings.TrimSpace(string(boot)) || receipt.Interface.Address != iface.Address || len(receipt.Label) != 15 {
			return receipt, errors.New("native fabric receipt does not match this operation and boot")
		}
		if err := fabricNativeIdentity(iface, receipt.Interface); err != nil {
			return receipt, err
		}
		if receipt.NM != nil && !fabricNMReceiptValid(receipt) {
			return receipt, errors.New("invalid NetworkManager ownership receipt")
		}
	}
	return receipt, nil
}

func fabricNativeUpdateReceipt(dir *os.File, receipt fabricNativeReceipt) error {
	if !fabricNMReceiptValid(receipt) {
		return errors.New("invalid NetworkManager receipt update")
	}
	name := fabricNativeHash([]any{receipt.Operation, receipt.Interface.Index, receipt.Interface.Address}) + ".json"
	file, err := os.CreateTemp(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), ".fabric-receipt-")
	if err != nil {
		return err
	}
	temporary := filepath.Base(file.Name())
	defer func() { file.Close(); _ = unix.Unlinkat(int(dir.Fd()), temporary, 0) }()
	if err := json.NewEncoder(file).Encode(receipt); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), temporary, int(dir.Fd()), name); err != nil {
		return err
	}
	return dir.Sync()
}

func fabricNativeNMIO(dir *os.File) fabricNMIO {
	return fabricNMIO{run: fabricNativeCommand, identity: fabricNativeInterfaceCurrent, addresses: fabricNativeAddressState, save: func(receipt fabricNativeReceipt) error { return fabricNativeUpdateReceipt(dir, receipt) }, preflight: func(ctx context.Context, iface fabricInterface) error {
		facts, err := fabricNativeInspectAt(ctx, []fabricInterface{iface}, iface.GeneratedDefault != nil, false)
		if err != nil {
			return err
		}
		if len(facts.Blockers) != 0 {
			return errors.New(strings.Join(facts.Blockers, "; "))
		}
		return nil
	}}
}

func fabricNativeAddressState(ctx context.Context, iface fabricInterface) (map[string]fabricNativeAddress, error) {
	data, err := fabricNativeCommand(ctx, "ip", "-j", "address", "show", "dev", iface.Name)
	if err != nil {
		return nil, err
	}
	_, addresses, err := fabricNativeCurrentAddresses(data, iface)
	return addresses, err
}

func fabricNativeAdd(ctx context.Context, iface fabricInterface, operationID string, leaseSeconds int, generatedRebindApproved bool) error {
	if len(iface.Addresses) != 0 && iface.GeneratedDefault == nil || iface.GeneratedDefault != nil && !validFabricGeneratedDefault(iface.GeneratedDefault, iface) {
		return errors.New("native link-local interruption requires the approved generated-default descriptor")
	}
	if iface.GeneratedDefault != nil && !generatedRebindApproved {
		return errors.New("native generated-default interruption requires explicit selected-port approval")
	}
	if leaseSeconds != fabricNativeLeaseSeconds {
		return errors.New("fabric address lifetime must select explicit rollback or reboot; timed expiry is unsupported")
	}
	if _, err := fabricNativePrefix(iface); err != nil {
		return err
	}
	if !validFabricInterfaceRoutes(iface) {
		return errors.New("fabric host routes must be one reviewed /32 route via the p0 cable peer")
	}
	dir, lock, err := fabricNativeJournal()
	if err != nil {
		return err
	}
	defer dir.Close()
	defer lock.Close()
	facts, err := fabricNativeInspectRebind(ctx, []fabricInterface{iface}, generatedRebindApproved)
	if err != nil {
		return err
	}
	target := fabricTarget{Interfaces: []fabricInterface{iface}}
	if _, err := rebindFabricGeneratedDefaults(&target, facts, generatedRebindApproved); err != nil {
		return fabricInspectionError("network-manager-generation-changed", err)
	}
	iface = target.Interfaces[0]
	if len(facts.Blockers) != 0 {
		return fabricInspectionError("configuration-blocked", fmt.Errorf("native fabric preflight blocked: %s", strings.Join(facts.Blockers, "; ")))
	}
	current, err := fabricNativeAddressState(ctx, iface)
	if err != nil {
		return err
	}
	if _, present := current[iface.Address]; present {
		return errors.New("exact address already exists and will not be adopted as operation-owned")
	}
	var nmReceipt *fabricNMReceipt
	if _, err := fabricNativeTool("nmcli"); err == nil {
		nm, err := fabricNMReadSettled(ctx, fabricNativeCommand, []fabricInterface{iface})
		if err != nil {
			return err
		}
		if blockers := fabricNMPreflight(nm, []fabricInterface{iface}, ""); len(blockers) != 0 {
			return fabricInspectionError("configuration-blocked", errors.New(strings.Join(blockers, "; ")))
		}
		device := nm.Devices[iface.Name]
		if device.Managed {
			baseline, err := fabricNMBaseline(nm, iface, "")
			if err != nil {
				return fabricInspectionError("network-manager-profiles-unavailable", err)
			}
			original := device.Autoconnect
			nmReceipt = &fabricNMReceipt{UUID: fabricNMUUID(operationID, iface), Owner: nm.Owner, BusID: nm.BusID, DevicePath: device.Path, PermanentMAC: device.PermanentMAC, OriginalAutoconnect: &original, BaselineProfiles: baseline}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if iface.GeneratedDefault != nil && nmReceipt == nil {
		return errors.New("reviewed generated-default manager is unavailable; no unmanaged fallback")
	}
	if len(iface.Routes) != 0 && nmReceipt == nil {
		return errors.New("reviewed fabric host routes require an operation-owned NetworkManager profile; no unmanaged fallback")
	}
	var receipt fabricNativeReceipt
	if nmReceipt != nil {
		receipt, err = fabricNativeReceiptFile(dir, iface, operationID, true, nmReceipt)
	} else {
		receipt, err = fabricNativeReceiptFile(dir, iface, operationID, true)
	}
	if err != nil {
		return err
	}
	if receipt.NM != nil {
		return fabricNMAdd(ctx, iface, receipt, fabricNativeNMIO(dir), generatedRebindApproved)
	}
	if _, err := fabricNativeInterfaceCurrent(iface, true); err != nil {
		return err
	}
	msg, err := fabricNativeAddressMessage(iface, receipt.Label, leaseSeconds, false)
	if err != nil {
		return err
	}
	if err := fabricNativeNetlink(ctx, msg); err != nil {
		return err
	}
	if _, err := fabricNativeInterfaceCurrent(iface, true); err != nil {
		return err
	}
	current, err = fabricNativeAddressState(ctx, iface)
	if err != nil {
		return err
	}
	if !fabricNativeOwnedAddress(current[iface.Address], receipt.Label) {
		return errors.New("kernel did not confirm the exact labelled address without timed expiry; cleanup receipt retained")
	}
	return nil
}

func fabricNativeRemove(ctx context.Context, iface fabricInterface, operationID string) error {
	if _, err := fabricNativePrefix(iface); err != nil {
		return err
	}
	dir, lock, err := fabricNativeJournal()
	if err != nil {
		return err
	}
	defer dir.Close()
	defer lock.Close()
	if _, err := fabricNativeInterfaceCurrent(iface, false); err != nil {
		return err
	}
	current, err := fabricNativeAddressState(ctx, iface)
	if err != nil {
		return err
	}
	receipt, err := fabricNativeReceiptFile(dir, iface, operationID, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, present := current[iface.Address]; !present {
				if _, toolErr := fabricNativeTool("nmcli"); toolErr == nil {
					profiles, err := fabricNMProfiles(ctx, fabricNativeCommand)
					if err != nil {
						return err
					}
					for _, profile := range profiles {
						if profile.UUID == fabricNMUUID(operationID, iface) {
							return errors.New("matching NetworkManager profile has no private receipt; preserved")
						}
					}
				} else if !errors.Is(toolErr, os.ErrNotExist) {
					return toolErr
				}
				return nil
			}
		}
		return errors.New("operation-owned native address receipt unavailable; address preserved")
	}
	if receipt.NM != nil {
		return fabricNMRemove(ctx, iface, receipt, fabricNativeNMIO(dir))
	}
	if _, present := current[iface.Address]; !present {
		return nil
	}
	remove, err := fabricNativeRemovalSafe(iface, current, receipt.Label)
	if err != nil || !remove {
		return err
	}
	if _, err := fabricNativeInterfaceCurrent(iface, false); err != nil {
		return err
	}
	msg, err := fabricNativeAddressMessage(iface, receipt.Label, 0, true)
	if err != nil {
		return err
	}
	if err := fabricNativeNetlink(ctx, msg); err != nil {
		return err
	}
	current, err = fabricNativeAddressState(ctx, iface)
	if err != nil {
		return err
	}
	if _, present := current[iface.Address]; present {
		return errors.New("address remains or was replaced after labelled cleanup; preserved")
	}
	return nil
}

func fabricNativeNetlink(ctx context.Context, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if os.Geteuid() != 0 {
		return errors.New("native address mutation requires root")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	data := make([]byte, 65536)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("native address ACK unavailable; receipt retained: %w", err)
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 50); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return errors.New("native address ACK socket failed")
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, _, flags, from, err := unix.Recvmsg(fd, data, nil, 0)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		peer, ok := from.(*unix.SockaddrNetlink)
		if !ok || peer.Pid != 0 || flags&unix.MSG_TRUNC != 0 {
			return errors.New("native address ACK sender or bounds invalid")
		}
		messages, err := syscall.ParseNetlinkMessage(data[:n])
		if err != nil {
			return err
		}
		for _, message := range messages {
			if message.Header.Seq != 1 {
				return errors.New("native address ACK sequence mismatch")
			}
			if message.Header.Type != unix.NLMSG_ERROR || len(message.Data) < 4 {
				return errors.New("native address ACK has unexpected type")
			}
			code := int32(binary.NativeEndian.Uint32(message.Data[:4]))
			if code == 0 {
				return nil
			}
			if code > 0 {
				return errors.New("native address ACK error invalid")
			}
			return syscall.Errno(-code)
		}
	}
}
