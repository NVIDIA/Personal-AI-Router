// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func fabricNativeTestInterface() fabricInterface {
	return fabricInterface{Name: "enP2p1s0f0np012", Index: 42, MAC: "02:00:00:00:00:42", PhysicalPort: fabricPhysicalPort{Source: "linux-sysfs", SwitchID: "001122", PortName: "p0"}, Address: "172.31.240.1/30", Driver: "mlx5_core", RDMADevices: []string{"mlx5_4"}, MTU: 1500}
}

func TestFabricNativeRoutesAllTablesAndDefaults(t *testing.T) {
	data := []byte(`[{"dst":"default","dev":"mgmt0","table":"main","gateway":"10.0.0.1"},{"dst":"172.31.240.0/30","dev":"vpn0","table":120,"scope":"link"},{"dst":"192.168.2.8","dev":"lo","table":"local","type":"local"}]`)
	rows, prefixes, err := fabricParseRoutes(data)
	if err != nil || !slices.Equal(prefixes, []string{"172.31.240.0/30", "192.168.2.8/32"}) || string(rows[1].Table) != "120" {
		t.Fatalf("all-table route evidence lost: %+v %v %v", rows, prefixes, err)
	}
	if len(fabricRouteBlockers(rows, []fabricInterface{fabricNativeTestInterface()})) != 1 {
		t.Fatal("non-main route conflict not held")
	}
	for _, malformed := range []string{`null`, `{}`, `[{}]`, `[{"dst":"not-a-route"}]`, `[] trailing`} {
		if _, _, err := fabricParseRoutes([]byte(malformed)); err == nil {
			t.Fatalf("ambiguous routes accepted: %s", malformed)
		}
	}
}

func TestFabricNativeManagementDefaultMultipathAndDNS(t *testing.T) {
	iface := fabricNativeTestInterface()
	rows, _, err := fabricParseRoutes([]byte(`[{"dst":"default","nexthops":[{"dev":"` + iface.Name + `","gateway":"10.1.1.1"}]}]`))
	if err != nil || len(fabricRouteBlockers(rows, []fabricInterface{iface})) != 1 {
		t.Fatal("multipath management route not protected")
	}
	for _, data := range []string{"nameserver 172.31.240.2\n", "nameserver fe80::1%" + iface.Name, "nameserver unknown"} {
		if len(fabricNativeDNSBlockers([]byte(data), []fabricInterface{iface})) == 0 {
			t.Fatalf("DNS use not held: %s", data)
		}
	}
	if len(fabricNativeDNSBlockers([]byte("nameserver 127.0.0.53\n"), []fabricInterface{iface})) != 0 {
		t.Fatal("unrelated resolver falsely conflicts")
	}
}

func TestFabricNativeIdentityAllReviewedBindings(t *testing.T) {
	base := fabricNativeTestInterface()
	if len(base.Name) != 15 || fabricNativeIdentity(base, base) != nil {
		t.Fatal("15-character device name unsupported")
	}
	for name, change := range map[string]func(*fabricInterface){
		"index": func(i *fabricInterface) { i.Index++ }, "name": func(i *fabricInterface) { i.Name = "other0" },
		"mac": func(i *fabricInterface) { i.MAC = "02:00:00:00:00:43" }, "switch": func(i *fabricInterface) { i.PhysicalPort.SwitchID = "other" },
		"port": func(i *fabricInterface) { i.PhysicalPort.PortName = "p1" }, "source": func(i *fabricInterface) { i.PhysicalPort.Source = "cached" },
		"driver": func(i *fabricInterface) { i.Driver = "other" }, "rdma": func(i *fabricInterface) { i.RDMADevices = []string{"mlx5_5"} },
		"mtu": func(i *fabricInterface) { i.MTU++ },
	} {
		t.Run(name, func(t *testing.T) {
			got := base
			change(&got)
			if fabricNativeIdentity(base, got) == nil {
				t.Fatal("changed identity accepted")
			}
		})
	}
	for _, name := range []string{"../net", ".", "-bad name", "foo/bar", strings.Repeat("x", 16)} {
		bad := base
		bad.Name = name
		if fabricNativeIdentity(bad, bad) == nil {
			t.Fatal("unsafe name accepted")
		}
	}
}

func fabricNativeTestAttrs(t *testing.T, message []byte) map[uint16][]byte {
	t.Helper()
	if len(message) < 24 || binary.NativeEndian.Uint32(message[:4]) != uint32(len(message)) {
		t.Fatal("netlink framing")
	}
	attrs := map[uint16][]byte{}
	for pos := 24; pos < len(message); {
		if len(message)-pos < 4 {
			t.Fatal("truncated attribute")
		}
		n := int(binary.NativeEndian.Uint16(message[pos : pos+2]))
		typ := binary.NativeEndian.Uint16(message[pos+2 : pos+4])
		if n < 4 || pos+n > len(message) || attrs[typ] != nil {
			t.Fatal("invalid attribute")
		}
		attrs[typ] = message[pos+4 : pos+n]
		pos += (n + 3) &^ 3
	}
	return attrs
}

func TestFabricNativeNetlinkExactLabelWithoutTimedExpiry(t *testing.T) {
	iface, label := fabricNativeTestInterface(), "nP7b4TrU_l81zQ9"
	add, err := fabricNativeAddressMessage(iface, label, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	attrs := fabricNativeTestAttrs(t, add)
	if binary.NativeEndian.Uint16(add[4:6]) != 20 || binary.NativeEndian.Uint16(add[6:8]) != 0x605 || add[16] != 2 || add[17] != 30 || binary.NativeEndian.Uint32(add[20:24]) != 42 {
		t.Fatal("add must be exact IPv4 CREATE|EXCL, never replace")
	}
	if string(attrs[3]) != label+"\x00" || len(attrs[1]) != 4 || !slices.Equal(attrs[1], attrs[2]) || len(attrs[6]) != 16 || binary.NativeEndian.Uint32(attrs[6][:4]) != 0xffffffff || binary.NativeEndian.Uint32(attrs[6][4:8]) != 0xffffffff {
		t.Fatal("exact label, address, or until-reboot lifetime lost")
	}
	del, err := fabricNativeAddressMessage(iface, label, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	removed := fabricNativeTestAttrs(t, del)
	if binary.NativeEndian.Uint16(del[4:6]) != 21 || binary.NativeEndian.Uint16(del[6:8]) != 5 || string(removed[3]) != label+"\x00" || len(removed) != 3 || !slices.Equal(removed[2], attrs[2]) {
		t.Fatal("delete must match exact CIDR, index, and operation label")
	}
	for _, lease := range []int{-1, 1, 1799, 1800, 1801, 2147483647} {
		if _, err := fabricNativeAddressMessage(iface, label, lease, false); err == nil {
			t.Fatal("timed/nonrecipe lifetime accepted")
		}
	}
	for _, cidr := range []string{"172.31.240.0/30", "172.31.240.3/30", "8.8.8.8/24", "fe80::1/64", "172.31.240.1/32", "-flag"} {
		bad := iface
		bad.Address = cidr
		if _, err := fabricNativeAddressMessage(bad, label, 0, false); err == nil {
			t.Fatalf("unsafe address accepted: %s", cidr)
		}
	}
}

func TestFabricNativeOwnedCleanupPreservesForeignAndSecondary(t *testing.T) {
	iface, label := fabricNativeTestInterface(), "nP7b4TrU_l81zQ9"
	owned := fabricNativeAddress{Local: "172.31.240.1", PrefixLen: 30, Label: label, Valid: json.RawMessage(`4294967295`), Preferred: json.RawMessage(`4294967295`)}
	current := map[string]fabricNativeAddress{iface.Address: owned}
	if yes, err := fabricNativeRemovalSafe(iface, current, label); err != nil || !yes {
		t.Fatalf("exact owned cleanup rejected: %v", err)
	}
	foreign := owned
	foreign.Label = iface.Name
	current[iface.Address] = foreign
	if _, err := fabricNativeRemovalSafe(iface, current, label); err == nil {
		t.Fatal("foreign replacement selected for deletion")
	}
	changed := owned
	changed.Valid = json.RawMessage(`1800`)
	current[iface.Address] = changed
	if _, err := fabricNativeRemovalSafe(iface, current, label); err == nil {
		t.Fatal("foreign timed lifetime selected for deletion")
	}
	current[iface.Address] = owned
	current["172.31.240.2/30"] = foreign
	if _, err := fabricNativeRemovalSafe(iface, current, label); err == nil {
		t.Fatal("primary deletion could cascade to foreign secondary")
	}
	if yes, err := fabricNativeRemovalSafe(iface, map[string]fabricNativeAddress{}, label); yes || err != nil {
		t.Fatal("absence must not issue deletion")
	}
}

func TestFabricNativeCleanupRequiresExactUntimedLifetime(t *testing.T) {
	label := "nP7b4TrU_l81zQ9"
	for _, pair := range [][2]string{{`1800`, `1800`}, {`4294967295`, `1800`}, {`1800`, `4294967295`}, {`0`, `0`}, {`"forever"`, `"forever"`}, {`null`, `null`}} {
		address := fabricNativeAddress{Label: label, Valid: json.RawMessage(pair[0]), Preferred: json.RawMessage(pair[1])}
		if fabricNativeOwnedAddress(address, label) {
			t.Fatalf("changed or ambiguous lifetime accepted: %v", pair)
		}
	}
}

func TestFabricNativeAddressSnapshotMustMatchReviewedIdentity(t *testing.T) {
	iface := fabricNativeTestInterface()
	data := []byte(`[{"ifindex":42,"ifname":"` + iface.Name + `","address":"` + iface.MAC + `","addr_info":[{"local":"172.31.240.1","prefixlen":30,"label":"nP7b4TrU_l81zQ9","valid_life_time":4294967295,"preferred_life_time":4294967295}]}]`)
	addresses, current, err := fabricNativeCurrentAddresses(data, iface)
	if err != nil || !slices.Equal(addresses, []string{iface.Address}) || !fabricNativeOwnedAddress(current[iface.Address], "nP7b4TrU_l81zQ9") {
		t.Fatal("mocked kernel address snapshot rejected")
	}
	bad := iface
	bad.Index++
	if _, _, err := fabricNativeCurrentAddresses(data, bad); err == nil {
		t.Fatal("reused/changed index accepted")
	}
}

func TestFabricNativeConfigurationFailClosed(t *testing.T) {
	targets := []fabricInterface{fabricNativeTestInterface()}
	links := map[string]fabricConfigLink{targets[0].Name: {Names: []string{targets[0].Name, "alt-selected0"}, Type: "ether", Known: true, SystemdVersion: 255}}
	for _, test := range []struct {
		name, data string
		unrelated  bool
	}{
		{"/etc/netplan/01-network-manager-all.yaml", "network:\n  version: 2\n  renderer: NetworkManager\n", true},
		{"/etc/netplan/99-nvidia-sync-cluster.yaml", "network:\n  version: 2\n  ethernets:\n    enp1s0f0np0:\n      addresses: [192.168.0.1/24]\n", false},
		{"/etc/netplan/unknown.yaml", "network: {version: 2, ethernets: {}}", false},
		{"/etc/netplan/malformed.yaml", "network:\nversion: 2\nrenderer: NetworkManager\n", false},
		{"/etc/NetworkManager/system-connections/management.nmconnection", "[connection]\ninterface-name=mgmt0\n[ipv4]\nmethod=auto\n", true},
		{"/etc/NetworkManager/system-connections/selected.nmconnection", "[connection]\ninterface-name=" + targets[0].Name + "\n", false},
		{"/etc/NetworkManager/system-connections/wildcard.nmconnection", "[connection]\ninterface-name=mgmt0\n[match]\ninterface-name=*&!mgmt0\n", false},
		{"/etc/NetworkManager/system-connections/unknown.nmconnection", "[connection]\n[ipv4]\nmethod=auto\n", false},
		{"/etc/NetworkManager/system-connections/duplicate.nmconnection", "[connection]\ninterface-name=mgmt0\ninterface-name = " + targets[0].Name + "\n", false},
		{"/usr/lib/systemd/network/management.network", "[Match]\nName=mgmt0 mgmt1\n[Network]\nDHCP=yes\n", true},
		{"/usr/lib/systemd/network/wildcard.network", "[Match]\nName=en*\n[Network]\nDHCP=yes\n", false},
		{"/usr/lib/systemd/network/duplicate.network", "[Match]\nName=mgmt0\nName = " + targets[0].Name + "\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := fabricNativeConfigUnrelated(test.name, []byte(test.data), targets, links); got != test.unrelated {
				t.Fatalf("config disposition = %v", got)
			}
		})
	}
}

func TestFabricNativeConfigurationVirtualizationConjunct(t *testing.T) {
	// Match and configuration shape from the observed stock systemd template;
	// no hostname, filename exception, or virtualization-state assumption.
	const stock = "[Match]\nKind=veth\nName=host0\nVirtualization=container\n[Network]\nDHCP=yes\nLinkLocalAddressing=yes\nLLDP=yes\nEmitLLDP=customer-bridge\n[DHCP]\nUseTimezone=yes\n"
	target := fabricNativeTestInterface()
	known := fabricConfigLink{Names: []string{target.Name}, Type: "ether", Kind: "", Known: true, SystemdVersion: 255}
	for _, test := range []struct {
		name, data string
		applicable bool
		unrelated  bool
	}{
		{"stock virtual interface template", stock, false, true},
		{"applicable alias and kind", stock, true, false},
		{"virtualization alone cannot exclude", "[Match]\nVirtualization=container\n", false, false},
		{"unknown virtualization value", strings.Replace(stock, "=container", "=unknown", 1), false, false},
		{"negated virtualization", strings.Replace(stock, "=container", "=!container", 1), false, false},
		{"empty reset", strings.Replace(stock, "=container", "=", 1), false, false},
		{"duplicate condition", strings.Replace(stock, "Virtualization=container", "Virtualization=container\nVirtualization=container", 1), false, false},
		{"unknown selector", strings.Replace(stock, "Virtualization=container", "UnknownCondition=container", 1), false, false},
		{"invalid Name cannot exclude", "[Match]\nName=invalid:name\nType=ether\nVirtualization=container\n", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			link := known
			if test.applicable {
				link.Names = append([]string{target.Name}, "host0")
				link.Kind = "veth"
			}
			for _, filename := range []string{"/usr/lib/systemd/network/80-container-host0.network", "/etc/systemd/network/renamed.network"} {
				if got := fabricNativeConfigUnrelated(filename, []byte(test.data), []fabricInterface{target}, map[string]fabricConfigLink{target.Name: link}); got != test.unrelated {
					t.Fatalf("conjunct disposition=%v, want %v", got, test.unrelated)
				}
			}
		})
	}
}

func TestFabricNativeConfigurationRelatednessEvidence(t *testing.T) {
	t.Run("linked drop-ins are held before suffix filtering", func(t *testing.T) {
		for _, name := range []string{"80-container-vb.network.d", "network.d", "80-.network.d"} {
			if got := fabricConfigEntryDisposition("/etc/systemd/network", name, false, true); got != "linked" {
				t.Fatalf("linked drop-in was filtered out: %s", name)
			}
			if got := fabricConfigEntryDisposition("/etc/systemd/network", name, true, false); got != "nested" {
				t.Fatalf("drop-in directory was filtered out: %s", name)
			}
		}
		if fabricConfigEntryDisposition("/etc/systemd/network", "test.network", false, false) != "read" || fabricConfigEntryDisposition("/etc/systemd/network", "README", false, false) != "skip" {
			t.Fatal("ordinary file format selection changed")
		}
	})
	t.Run("udev absence requires bound identity", func(t *testing.T) {
		for _, data := range []string{"", "DEVTYPE=ether\n", "INTERFACE=wrong\nIFINDEX=3\n", "INTERFACE=enp1s0f0np0\nIFINDEX=4\n", "INTERFACE=enp1s0f0np0\nIFINDEX=3\nDEVTYPE=\n", "INTERFACE=enp1s0f0np0\nIFINDEX=3\nIFINDEX=3\n", "INTERFACE=enp1s0f0np0\nIFINDEX=3\nbroken\n"} {
			if _, err := fabricConfigDeviceType([]byte(data), "enp1s0f0np0", 3); err == nil {
				t.Fatal("unbound or malformed udev metadata established a type")
			}
		}
		for _, suffix := range []string{"", "DEVTYPE=wlan\n"} {
			got, err := fabricConfigDeviceType([]byte("INTERFACE=enp1s0f0np0\nIFINDEX=3\n"+suffix), "enp1s0f0np0", 3)
			if err != nil || (suffix == "" && got != "") || (suffix != "" && got != "wlan") {
				t.Fatal("bound udev type or explicit absence lost")
			}
		}
	})
	target := fabricNativeTestInterface()
	targets := []fabricInterface{target}
	known := fabricConfigLink{Names: []string{target.Name, "alt-selected0"}, Type: "ether", Kind: "", Known: true, SystemdVersion: 255}
	for _, test := range []struct {
		name, data, metadata string
		unrelated            bool
	}{
		{"6rd sit", "[Match]\nKind=sit\nName=6rd-*\n[Network]\nDHCP=no\n", "", true},
		{"container vb", "[Match]\nKind=veth\nName=vb-*\n[Network]\nDHCP=yes\n", "", true},
		{"container ve", "[Match]\nKind=veth\nName=ve-*\n[Network]\nDHCP=yes\n", "", true},
		{"container vz", "[Match]\nKind=bridge\nName=vz-*\n[Network]\nDHCP=yes\n", "", true},
		{"container vt", "[Match]\nKind=tun\nName=vt-*\n[Network]\nDHCP=yes\n", "", true},
		{"wireless ad-hoc", "[Match]\nType=wlan\nWLANInterfaceType=ad-hoc\n[Network]\nDHCP=yes\n", "", true},
		{"quoted name syntax held", "[Match]\nName=\"mgmt0\" 'mgmt1'\nType=ether\n", "", false},
		{"selected primary", "[Match]\nName=" + target.Name + "\n", "", false},
		{"selected alternative", "[Match]\nName=alt-*\n", "", false},
		{"selected type", "[Match]\nType=ether\nName=" + target.Name + "\n", "", false},
		{"discarded colon name plus matching type", "[Match]\nName=invalid:name\nType=ether\n", "", false},
		{"discarded slash name plus matching type", "[Match]\nName=invalid/name\nType=ether\n", "", false},
		{"discarded numeric name plus matching type", "[Match]\nName=123\nType=ether\n", "", false},
		{"discarded dot name plus matching type", "[Match]\nName=..\nType=ether\n", "", false},
		{"unsupported long name plus matching type", "[Match]\nName=" + strings.Repeat("x", 128) + "\nType=ether\n", "", false},
		{"wireless mode alone", "[Match]\nWLANInterfaceType=ad-hoc\n", "", false},
		{"unknown Match key", "[Match]\nName=mgmt0\nProperty=ID_NET_DRIVER=wireless\n", "", false},
		{"negated name", "[Match]\nName=!mgmt0\n", "", false},
		{"empty reset", "[Match]\nName=\n", "", false},
		{"duplicate key", "[Match]\nName=mgmt0\nName=mgmt1\n", "", false},
		{"concatenated reset", "[Match]\nName=mgmt0\nName=\n", "", false},
		{"second Match section", "[Match]\nName=mgmt0\n[Network]\nDHCP=yes\n[Match]\nName=mgmt1\n", "", false},
		{"escaped name", "[Match]\nName=mgmt\\x30\n", "", false},
		{"malformed section", "[Match\nName=mgmt0\n", "", false},
		{"malformed pattern", "[Match]\nName=mgmt[\n", "", false},
		{"POSIX character class held", "[Match]\nName=[[:alpha:]]*\n", "", false},
		{"kind wildcard held", "[Match]\nKind=*\n", "", false},
		{"inline text is not a comment", "[Match]\nName=mgmt0 # " + target.Name + "\n", "", false},
		{"missing metadata", "[Match]\nName=mgmt0\n", "missing", false},
		{"unknown metadata", "[Match]\nName=mgmt0\n", "unknown", false},
		{"missing names", "[Match]\nName=mgmt0\n", "names", false},
		{"old systemd semantics", "[Match]\nName=mgmt0\n", "old-version", false},
	} {
		t.Run("networkd/"+test.name, func(t *testing.T) {
			link := known
			switch test.metadata {
			case "unknown":
				link.Known = false
			case "names":
				link.Names = nil
			case "old-version":
				link.SystemdVersion = 250
			}
			links := map[string]fabricConfigLink{target.Name: link}
			if test.metadata == "missing" {
				links = nil
			}
			if got := fabricNativeConfigUnrelated("/usr/lib/systemd/network/example.network", []byte(test.data), targets, links); got != test.unrelated {
				t.Fatalf("networkd unrelated=%v, want %v", got, test.unrelated)
			}
		})
	}

	// Synthetic, redacted equivalent of the observed management Wi-Fi schema.
	// No real SSID, credential, profile name, or UUID is included.
	const wifi = `network:
  version: 2
  wifis:
    management:
      renderer: NetworkManager
      match:
        name: wlP9s9
      dhcp4: true
      dhcp6: true
      networkmanager:
        name: REDACTED-definition
        uuid: 00000000-0000-4000-8000-000000000001
      access-points:
        REDACTED:
          auth:
            key-management: psk
            password: REDACTED
          networkmanager:
            name: REDACTED-access-point
            uuid: 00000000-0000-4000-8000-000000000002
            passthrough:
              connection.timestamp: "1788725827"
              wifi.powersave: "2"
              ipv6.addr-gen-mode: "1"
              ipv6.ip6-privacy: "0"
              proxy._: ""
              wifi-security.auth-alg: open
`
	// Synthetic, redacted equivalent of the observed NetworkManager Ethernet
	// profile. The address is private, non-secret test data outside the fabric.
	const ethernet = `network:
  version: 2
  ethernets:
    NM-00000000-0000-4000-8000-000000000003:
      renderer: NetworkManager
      match:
        name: enP7s7
      addresses:
      - "10.44.3.1/30"
      mtu: 1500
      wakeonlan: true
      networkmanager:
        uuid: 00000000-0000-4000-8000-000000000003
        name: REDACTED-ethernet
        passthrough:
          ipv4.never-default: "true"
          ipv6.addr-gen-mode: "default"
          ipv6.method: "disabled"
          ipv6.ip6-privacy: "-1"
          proxy._: ""
`
	definitionOverride := func(key string) string {
		value := target.Name
		if key == "connection.type" {
			value = "802-3-ethernet"
		}
		return strings.Replace(wifi, "      access-points:", "        passthrough:\n          "+key+": "+value+"\n      access-points:", 1)
	}
	apOverride := func(key string) string {
		value := target.Name
		if key == "connection.type" {
			value = "802-3-ethernet"
		}
		return strings.Replace(wifi, `              proxy._: ""`, "              "+key+": "+value, 1)
	}
	for _, test := range []struct {
		name, data string
		unrelated  bool
	}{
		{"redacted management Wi-Fi", wifi, true},
		{"redacted management Ethernet", ethernet, true},
		{"selected primary", strings.Replace(wifi, "name: wlP9s9", "name: "+target.Name, 1), false},
		{"selected alternative", strings.Replace(wifi, "name: wlP9s9", "name: alt-selected0", 1), false},
		{"Ethernet selected primary", strings.Replace(ethernet, "name: enP7s7", "name: "+target.Name, 1), false},
		{"Ethernet selected alternative", strings.Replace(ethernet, "name: enP7s7", "name: alt-selected0", 1), false},
		{"definition interface override", definitionOverride("connection.interface-name"), false},
		{"definition type override", definitionOverride("connection.type"), false},
		{"definition match override", definitionOverride("match.interface-name"), false},
		{"AP interface override", apOverride("connection.interface-name"), false},
		{"AP type override", apOverride("connection.type"), false},
		{"AP match override", apOverride("match.interface-name"), false},
		{"unknown passthrough", apOverride("unknown.setting"), false},
		{"invalid connection timestamp", strings.Replace(wifi, `connection.timestamp: "1788725827"`, `connection.timestamp: "not-a-time"`, 1), false},
		{"invalid Wi-Fi powersave", strings.Replace(wifi, `wifi.powersave: "2"`, `wifi.powersave: "4"`, 1), false},
		{"interface rename", strings.Replace(wifi, "      dhcp4:", "      set-name: "+target.Name+"\n      dhcp4:", 1), false},
		{"Ethernet interface rename", strings.Replace(ethernet, "      addresses:", "      set-name: "+target.Name+"\n      addresses:", 1), false},
		{"Ethernet overlapping address", strings.Replace(ethernet, "10.44.3.1/30", "172.31.240.2/30", 1), false},
		{"Ethernet unknown passthrough", strings.Replace(ethernet, `          proxy._: ""`, "          unknown.setting: value", 1), false},
		{"Wi-Fi fields under ethernets", strings.Replace(wifi, "  wifis:", "  ethernets:", 1), false},
		{"unknown network schema", strings.Replace(wifi, "  wifis:", "  bridges: {}\n  wifis:", 1), false},
		{"bond schema", strings.Replace(wifi, "  wifis:", "  bonds: {}\n  wifis:", 1), false},
		{"wildcard match", strings.Replace(wifi, "name: wlP9s9", "name: '*'", 1), false},
		{"Ethernet wildcard match", strings.Replace(ethernet, "name: enP7s7", "name: '*'", 1), false},
		{"duplicate key", strings.Replace(wifi, "  version: 2", "  version: 2\n  version: 2", 1), false},
		{"alias", "network: {version: 2, renderer: NetworkManager, wifis: {first: &wifi {match: {name: wlP9s9}}, second: *wifi}}", false},
		{"merge", "network: {version: 2, renderer: NetworkManager, wifis: {first: &wifi {match: {name: wlP9s9}}, second: {<<: *wifi}}}", false},
		{"custom auth tag", strings.Replace(wifi, "password: REDACTED", "password: !opaque REDACTED", 1), false},
		{"multiple documents", wifi + "---\nnetwork: {version: 2, renderer: NetworkManager}\n", false},
		{"malformed YAML", "network: [\n", false},
	} {
		t.Run("netplan/"+test.name, func(t *testing.T) {
			links := map[string]fabricConfigLink{target.Name: known}
			if got := fabricNativeConfigUnrelated("/etc/netplan/redacted-management.yaml", []byte(test.data), targets, links); got != test.unrelated {
				t.Fatalf("Netplan unrelated=%v, want %v", got, test.unrelated)
			}
		})
	}
}

func TestFabricNativePolicyRoutingMustBeCompleteAndConventional(t *testing.T) {
	if !fabricNativeRulesSafe([]byte(`[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main"},{"priority":32767,"src":"all","table":"default"}]`)) {
		t.Fatal("conventional full rules rejected")
	}
	for _, rules := range []string{`[]`, `null`, `[{"priority":0,"src":"all","table":"local"}]`, `[{"priority":0,"src":"all","table":"local"},{"priority":0,"src":"all","table":"local"}]`, `[{"priority":100,"src":"all","table":120}]`, `[{"priority":0,"src":"all","table":"local","fwmark":1}]`, `[{"priority":0,"src":"172.31.240.0/30","table":"local"}]`} {
		if fabricNativeRulesSafe([]byte(rules)) {
			t.Fatalf("ambiguous routing accepted: %s", rules)
		}
	}
}
