// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Zero selects the only supported policy: retain until explicit rollback or
// reboot. Kernel "forever" is runtime state, never persistent network config.
const fabricNativeLeaseSeconds = 0
const fabricNativeForever = ^uint32(0)
const fabricNativeMaxBytes = 1 << 20

type fabricNativeRoute struct {
	Dst      string          `json:"dst"`
	Dev      string          `json:"dev"`
	Gateway  string          `json:"gateway"`
	Table    json.RawMessage `json:"table"`
	Scope    json.RawMessage `json:"scope"`
	Protocol json.RawMessage `json:"protocol"`
	Type     string          `json:"type"`
	Nexthops []struct {
		Dev     string `json:"dev"`
		Gateway string `json:"gateway"`
	} `json:"nexthops"`
}

func fabricParseRoutes(data []byte) ([]fabricNativeRoute, []string, error) {
	var rows []fabricNativeRoute
	if len(data) > fabricNativeMaxBytes || json.Unmarshal(data, &rows) != nil || rows == nil || len(rows) > 8192 {
		return nil, nil, errors.New("complete native route table is unavailable")
	}
	var prefixes []string
	for _, row := range rows {
		if row.Dst == "default" {
			continue
		}
		p, ok := fabricRouteDestination(row.Dst)
		if !ok {
			return nil, nil, errors.New("native route destination is ambiguous")
		}
		if p.Bits() != 0 {
			prefixes = append(prefixes, p.Masked().String())
		}
	}
	return rows, fabricSorted(prefixes), nil
}

// iproute2 prints a host route's destination without its prefix length.
func fabricRouteDestination(dst string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(dst); err == nil {
		return p, true
	}
	a, err := netip.ParseAddr(dst)
	if err != nil {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}

func fabricSorted(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

func fabricNativeHasLinkDependency(names []string) bool {
	for _, name := range names {
		if name == "master" || strings.HasPrefix(name, "upper_") || strings.HasPrefix(name, "lower_") {
			return true
		}
	}
	return false
}

func fabricNativeIdentity(want, got fabricInterface) error {
	if want.Index <= 0 || want.Name == "" || len(want.Name) > 15 || strings.ContainsAny(want.Name, "/\\ :\t\r\n\x00") || want.Name == "." || want.Name == ".." {
		return errors.New("invalid reviewed native interface identity")
	}
	mac, err := net.ParseMAC(want.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || want.MAC == "00:00:00:00:00:00" ||
		want.PhysicalPort.Source != "linux-sysfs" || want.PhysicalPort.SwitchID == "" || want.PhysicalPort.PortName == "" ||
		want.Driver == "" || len(want.RDMADevices) == 0 || want.MTU <= 0 ||
		want.Index != got.Index || want.Name != got.Name || !strings.EqualFold(want.MAC, got.MAC) ||
		!strings.EqualFold(want.PhysicalPort.SwitchID, got.PhysicalPort.SwitchID) || want.PhysicalPort.PortName != got.PhysicalPort.PortName ||
		want.PhysicalPort.Source != got.PhysicalPort.Source || want.Driver != got.Driver || want.MTU != got.MTU ||
		!slices.Equal(fabricSorted(want.RDMADevices), fabricSorted(got.RDMADevices)) {
		return errors.New("reviewed interface, physical port, driver, RDMA device, or MTU changed")
	}
	return nil
}

func fabricNativePrefix(iface fabricInterface) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(iface.Address)
	if err != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() < 8 || p.Bits() > 31 || (p.Bits() <= 30 && p.Addr() == p.Masked().Addr()) {
		return netip.Prefix{}, errors.New("fabric address must be a private IPv4 host CIDR with a bounded prefix")
	}
	a := p.Addr().As4()
	u := binary.BigEndian.Uint32(a[:])
	if p.Bits() <= 30 && u|(^uint32(0)>>p.Bits()) == u {
		return netip.Prefix{}, errors.New("fabric address cannot be a subnet broadcast")
	}
	return p, nil
}

// An empty gateway requires the peer on-link; a routed proof names the peer
// address on the shared cable that the kernel must choose as next hop.
func fabricRouteSelectionQualified(data []byte, iface fabricInterface, peer, gateway string) bool {
	var rows []struct {
		Destination string `json:"dst"`
		From        string `json:"from"`
		Device      string `json:"dev"`
		Preferred   string `json:"prefsrc"`
		Source      string `json:"src"`
		Gateway     string `json:"gateway"`
		Type        string `json:"type"`
	}
	prefix, err := fabricNativePrefix(iface)
	if err != nil || (prefix.Bits() != 30 && prefix.Bits() != 31) || json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
		return false
	}
	row := rows[0]
	// A "route get ... from" reply carries the accepted local source as
	// "from" and omits prefsrc (iproute2 6.1).
	source := row.From
	if source == "" {
		source = row.Preferred
	}
	if source == "" {
		source = row.Source
	}
	return row.Destination == peer && row.Device == iface.Name && source == prefix.Addr().String() && row.Gateway == gateway && (row.Type == "" || row.Type == "unicast")
}

func fabricNativeAddressesMatch(target, actual fabricInterface, afterGeneratedPause bool) bool {
	if afterGeneratedPause {
		if !fabricLinkLocalBaseline(append([]string{}, actual.Addresses...)) {
			return false
		}
		for _, address := range actual.Addresses {
			if !slices.Contains(target.Addresses, address) {
				return false
			}
		}
		return true
	}
	if validFabricGeneratedDefault(target.GeneratedDefault, target) &&
		fabricLinkLocalBaseline(append([]string{}, target.Addresses...)) &&
		fabricLinkLocalBaseline(append([]string{}, actual.Addresses...)) {
		return true
	}
	return slices.Equal(fabricSorted(actual.Addresses), fabricSorted(target.Addresses))
}

func fabricRouteBlockers(rows []fabricNativeRoute, targets []fabricInterface) []string {
	var blockers []string
	for _, row := range rows {
		for _, target := range targets {
			selected := row.Dev == target.Name
			for _, nh := range row.Nexthops {
				selected = selected || nh.Dev == target.Name
			}
			if row.Dst == "default" || row.Dst == "0.0.0.0/0" || row.Dst == "::/0" {
				if selected {
					blockers = append(blockers, "selected fabric interface carries a management default route: "+target.Name)
				}
				continue
			}
			if target.Address == "" {
				continue
			}
			proposed, err := fabricNativePrefix(target)
			if err != nil {
				blockers = append(blockers, err.Error())
				continue
			}
			existing, ok := fabricRouteDestination(row.Dst)
			if ok && existing.Overlaps(proposed) {
				// An exact already-present target address is for the coordinator to
				// adopt; the native writer never takes ownership of it.
				if selected && slices.Contains(target.Addresses, target.Address) && proposed.Contains(existing.Addr()) && existing.Bits() >= proposed.Bits() {
					continue
				}
				blockers = append(blockers, "proposed fabric subnet overlaps an existing route: "+row.Dst)
			}
			for _, route := range target.Routes {
				destination, err := netip.ParsePrefix(route.Destination)
				if ok && err == nil && existing.Overlaps(destination) {
					blockers = append(blockers, "proposed fabric host route overlaps an existing route: "+row.Dst)
				}
			}
		}
	}
	return fabricSorted(blockers)
}

// A raw netlink IFA_LABEL is independent of the device name. The kernel
// validates its 15-byte bound and DELADDR matches the exact label atomically.
// iproute2's interface-name prefix restriction is a userspace compatibility
// rule. See Linux v6.8 net/ipv4/devinet.c, ifa_ipv4_policy/inet_rtm_deladdr/
// inet_rtm_to_ifa. No ioctl, alias rename, shell, flush, or replace is used.
func fabricNativeAddressMessage(iface fabricInterface, label string, seconds int, remove bool) ([]byte, error) {
	p, err := fabricNativePrefix(iface)
	if err != nil {
		return nil, err
	}
	if iface.Index <= 0 || uint64(iface.Index) > 0x7fffffff || len(label) != 15 || strings.ContainsAny(label, "\x00:/\\ \t\r\n") || (!remove && seconds != fabricNativeLeaseSeconds) {
		return nil, errors.New("invalid exact owned address request")
	}
	msg := make([]byte, 24)
	typ, flags := uint16(20), uint16(1|4|0x400|0x200) // NEWADDR, REQUEST|ACK|CREATE|EXCL
	if remove {
		typ, flags = 21, 1|4
	} // DELADDR never uses a wildcard.
	binary.NativeEndian.PutUint16(msg[4:6], typ)
	binary.NativeEndian.PutUint16(msg[6:8], flags)
	binary.NativeEndian.PutUint32(msg[8:12], 1)
	msg[16], msg[17] = 2, byte(p.Bits()) // AF_INET, prefix; global scope.
	binary.NativeEndian.PutUint32(msg[20:24], uint32(iface.Index))
	attr := func(kind uint16, data []byte) {
		n := 4 + len(data)
		a := make([]byte, (n+3)&^3)
		binary.NativeEndian.PutUint16(a[:2], uint16(n))
		binary.NativeEndian.PutUint16(a[2:4], kind)
		copy(a[4:], data)
		msg = append(msg, a...)
	}
	ip := p.Addr().As4()
	attr(1, ip[:])
	attr(2, ip[:])
	attr(3, append([]byte(label), 0)) // ADDRESS, LOCAL, LABEL
	if !remove {
		lifetime := make([]byte, 16)
		binary.NativeEndian.PutUint32(lifetime[:4], fabricNativeForever)
		binary.NativeEndian.PutUint32(lifetime[4:8], fabricNativeForever)
		attr(6, lifetime) // IFA_CACHEINFO; kernel owns birth/update timestamps.
	}
	binary.NativeEndian.PutUint32(msg[:4], uint32(len(msg)))
	return msg, nil
}

func fabricNativeHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Match inputs come from current, identity-bound netlink/udev observations.
// Known includes the complete primary/alternative name set and known kind absence.
type fabricConfigLink struct {
	Names          []string
	Type           string
	Kind           string
	Known          bool
	SystemdVersion int
}

// A missing DEVTYPE is meaningful only in a complete record bound to this link.
func fabricConfigDeviceType(data []byte, name string, index int) (string, error) {
	invalid := errors.New("selected udev identity is unavailable")
	if len(data) == 0 || len(data) > fabricNativeMaxBytes {
		return "", invalid
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return "", invalid
		}
		if key != "INTERFACE" && key != "IFINDEX" && key != "DEVTYPE" {
			continue
		}
		if _, duplicate := fields[key]; duplicate || value == "" || len(value) > 128 || strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return "", invalid
		}
		fields[key] = value
	}
	actual, err := strconv.Atoi(fields["IFINDEX"])
	if err != nil || actual != index || fields["INTERFACE"] != name {
		return "", invalid
	}
	return fields["DEVTYPE"], nil
}

func fabricConfigWords(value string) ([]string, bool) {
	words := strings.Fields(value)
	if len(words) == 0 || len(words) > 32 {
		return nil, false
	}
	for i, word := range words {
		if word[0] == '\'' || word[0] == '"' {
			if len(word) < 3 || word[len(word)-1] != word[0] {
				return nil, false
			}
			word = word[1 : len(word)-1]
		}
		if strings.ContainsAny(word, "\\%!^#;\"'[]\x00\r\n") || strings.IndexFunc(word, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return nil, false
		}
		if _, err := path.Match(word, ""); err != nil {
			return nil, false
		}
		words[i] = word
	}
	return words, true
}

func fabricConfigMatches(patterns, values []string) bool {
	for _, pattern := range patterns {
		for _, value := range values {
			if matched, _ := path.Match(pattern, value); matched {
				return true
			}
		}
	}
	return false
}

// Support only short interface-name terms that systemd will retain. An invalid
// Name term may be discarded while another Match clause remains effective.
func fabricConfigNameSupported(word string) bool {
	if word == "" || len(word) > 15 || word == "." || word == ".." || word == "all" || word == "default" {
		return false
	}
	numeric := true
	for _, c := range word {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-*?", c)) {
			return false
		}
		numeric = numeric && c >= '0' && c <= '9'
	}
	return !numeric
}

// Directory/symlink holds precede format filters, including linked drop-ins.
func fabricConfigEntryDisposition(dir, name string, directory, symlink bool) string {
	if directory {
		return "nested"
	}
	if symlink {
		return "linked"
	}
	if strings.Contains(dir, "/netplan") && !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
		return "skip"
	}
	if strings.Contains(dir, "/systemd/network") && !strings.HasSuffix(name, ".network") {
		return "skip"
	}
	return "read"
}

func fabricNetworkdUnrelated(data []byte, targets []fabricInterface, links map[string]fabricConfigLink) bool {
	section, matchedSection := "", false
	clauses := map[string][]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.ContainsAny(line, "\\\x00") {
			return false // Continuations/escapes require broader systemd syntax.
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return false
			}
			section = line[1 : len(line)-1]
			if section == "Match" {
				if matchedSection {
					return false
				}
				matchedSection = true
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return false
		}
		if section != "Match" {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "Name" && key != "Type" && key != "Kind" && key != "WLANInterfaceType" && key != "Virtualization" {
			return false
		}
		if _, duplicate := clauses[key]; duplicate {
			return false // Includes assignments that would reset an earlier match.
		}
		if key == "Name" && strings.ContainsAny(value, "\"'") {
			return false // Match.Name uses a distinct systemd word parser.
		}
		if key == "Virtualization" && strings.TrimSpace(value) != "container" {
			return false
		}
		words, valid := fabricConfigWords(strings.TrimSpace(value))
		if !valid || (key != "Name" && strings.ContainsAny(strings.Join(words, ""), "*?")) {
			return false
		}
		if key == "Name" {
			for _, word := range words {
				if !fabricConfigNameSupported(word) {
					return false
				}
			}
		}
		clauses[key] = words
	}
	if len(clauses) == 0 {
		return false
	}
	for _, target := range targets {
		link, ok := links[target.Name]
		if !ok || !link.Known || link.Type == "" || len(link.Names) == 0 || !slices.Contains(link.Names, target.Name) || link.SystemdVersion < 251 {
			return false
		}
		// Match clauses are ANDed. One known nonmatch excludes this target.
		disjoint := false
		for key, patterns := range clauses {
			var values []string
			switch key {
			case "Name":
				values = link.Names
			case "Type":
				values = []string{link.Type}
			case "Kind":
				if link.Kind != "" {
					values = []string{link.Kind}
				}
			case "WLANInterfaceType", "Virtualization":
				// These additional conditions cannot broaden a valid Name/Type/Kind
				// match. Without their own evidence, they never prove exclusion.
				continue
			}
			disjoint = disjoint || !fabricConfigMatches(patterns, values)
		}
		if !disjoint {
			return false
		}
	}
	return true
}

// Files remain untouched and hashed. Only a supported, positively disjoint
// binding is admitted; unknown schema/matches and drop-ins remain held.
func fabricNativeConfigUnrelated(filename string, data []byte, targets []fabricInterface, current ...map[string]fabricConfigLink) bool {
	if len(data) > fabricNativeMaxBytes || bytes.Contains(data, []byte{0}) || len(targets) == 0 || len(targets) > 8 || len(current) > 1 {
		return false
	}
	var links map[string]fabricConfigLink
	if len(current) == 1 {
		links = current[0]
	}
	if strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml") {
		return fabricNetplanUnrelated(data, targets, links)
	}
	if strings.HasSuffix(filename, ".network") {
		return fabricNetworkdUnrelated(data, targets, links)
	}
	if !strings.HasSuffix(filename, ".nmconnection") {
		return len(bytes.TrimSpace(data)) == 0
	}
	section, name := "", ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return false
			}
			section = line[1 : len(line)-1]
			if section == "match" {
				return false
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok {
			return false
		}
		if section == "connection" && key == "interface-name" {
			if name != "" {
				return false
			}
			name = value
		}
	}
	if name == "" || strings.ContainsAny(name, "*?[]\\; ") {
		return false
	}
	for _, target := range targets {
		link, known := links[target.Name]
		if !known || !link.Known || len(link.Names) == 0 || !slices.Contains(link.Names, target.Name) || slices.Contains(link.Names, name) || bytes.Contains(bytes.ToLower(data), []byte(strings.ToLower(target.MAC))) {
			return false
		}
	}
	return true
}

func fabricNativeDNSBlockers(data []byte, targets []fabricInterface) []string {
	var blockers []string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(parts) == 0 || parts[0] != "nameserver" {
			continue
		}
		if len(parts) != 2 {
			return []string{"DNS resolver configuration is ambiguous"}
		}
		a, err := netip.ParseAddr(parts[1])
		if err != nil {
			return []string{"DNS resolver address is ambiguous"}
		}
		for _, target := range targets {
			for _, address := range append(slices.Clone(target.Addresses), target.Address) {
				p, err := netip.ParsePrefix(address)
				if err == nil && p.Contains(a.WithZone("")) {
					blockers = append(blockers, "selected fabric subnet is used by a DNS resolver")
				}
			}
			if a.Zone() == target.Name || a.Zone() == strconv.Itoa(target.Index) {
				blockers = append(blockers, "selected fabric interface is used by a DNS resolver")
			}
		}
	}
	return fabricSorted(blockers)
}

func fabricNativeRulesSafe(data []byte) bool {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(data, &rows) != nil || len(rows) < 2 || len(rows) > 3 {
		return false
	}
	seen := map[int]bool{}
	for _, row := range rows {
		for key := range row {
			if key != "priority" && key != "src" && key != "table" && key != "protocol" {
				return false
			}
		}
		var priority int
		var src string
		if json.Unmarshal(row["priority"], &priority) != nil || json.Unmarshal(row["src"], &src) != nil || src != "all" {
			return false
		}
		if seen[priority] {
			return false
		}
		seen[priority] = true
		table := strings.Trim(string(row["table"]), "\"")
		if !((priority == 0 && (table == "local" || table == "255")) || (priority == 32766 && (table == "main" || table == "254")) || (priority == 32767 && (table == "default" || table == "253"))) {
			return false
		}
	}
	return seen[0] && seen[32766]
}

func fabricNativeCurrentAddresses(data []byte, iface fabricInterface) ([]string, map[string]fabricNativeAddress, error) {
	var rows []struct {
		Index     int                   `json:"ifindex"`
		Name      string                `json:"ifname"`
		MAC       string                `json:"address"`
		Addresses []fabricNativeAddress `json:"addr_info"`
	}
	if len(data) > fabricNativeMaxBytes || json.Unmarshal(data, &rows) != nil || len(rows) != 1 || rows[0].Index != iface.Index || rows[0].Name != iface.Name || !strings.EqualFold(rows[0].MAC, iface.MAC) {
		return nil, nil, errors.New("current native address identity is unavailable")
	}
	addresses := map[string]fabricNativeAddress{}
	var cidrs []string
	for _, addr := range rows[0].Addresses {
		a, err := netip.ParseAddr(addr.Local)
		if err != nil || addr.PrefixLen <= 0 || addr.PrefixLen > a.BitLen() {
			return nil, nil, errors.New("native address is ambiguous")
		}
		cidr := netip.PrefixFrom(a, addr.PrefixLen).String()
		if _, duplicate := addresses[cidr]; duplicate {
			return nil, nil, errors.New("duplicate native address record")
		}
		addresses[cidr] = addr
		cidrs = append(cidrs, cidr)
	}
	return fabricSorted(cidrs), addresses, nil
}

type fabricNativeAddress struct {
	Local     string          `json:"local"`
	PrefixLen int             `json:"prefixlen"`
	Label     string          `json:"label"`
	Valid     json.RawMessage `json:"valid_life_time"`
	Preferred json.RawMessage `json:"preferred_life_time"`
	Family    string          `json:"family"`
	Scope     string          `json:"scope"`
	Flags     []string        `json:"flags,omitempty"`
	Tentative bool            `json:"tentative,omitempty"`
	DADFailed bool            `json:"dadfailed,omitempty"`
	Temporary bool            `json:"temporary,omitempty"`
}

// Only this fixed, native-read baseline may be interrupted by the separately
// approved generated-default mode. It never makes a foreign address owned.
func fabricNativeLinkLocalOnly(addresses map[string]fabricNativeAddress) bool {
	cidrs := make([]string, 0, len(addresses))
	for cidr, address := range addresses {
		var valid, preferred uint32
		if address.Family != "inet6" || address.Scope != "link" || len(address.Flags) != 0 || address.Tentative || address.DADFailed || address.Temporary ||
			json.Unmarshal(address.Valid, &valid) != nil || json.Unmarshal(address.Preferred, &preferred) != nil || valid != fabricNativeForever || preferred != fabricNativeForever {
			return false
		}
		cidrs = append(cidrs, cidr)
	}
	return addresses != nil && fabricLinkLocalBaseline(cidrs)
}

func fabricNativeOwnedAddress(address fabricNativeAddress, label string) bool {
	var valid, preferred uint32
	return address.Label == label && json.Unmarshal(address.Valid, &valid) == nil && json.Unmarshal(address.Preferred, &preferred) == nil &&
		valid == fabricNativeForever && preferred == fabricNativeForever
}

func fabricNativeRemovalSafe(iface fabricInterface, current map[string]fabricNativeAddress, label string) (bool, error) {
	address, found := current[iface.Address]
	if !found {
		return false, nil
	}
	if !fabricNativeOwnedAddress(address, label) {
		return false, errors.New("same CIDR has foreign or changed ownership; preserved")
	}
	p, err := fabricNativePrefix(iface)
	if err != nil {
		return false, err
	}
	for other := range current {
		if other == iface.Address {
			continue
		}
		q, err := netip.ParsePrefix(other)
		if err != nil || q.Overlaps(p) {
			return false, fmt.Errorf("owned address shares a subnet with another address; cleanup held to preserve %s", other)
		}
	}
	return true, nil
}
