// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every external effect is represented in this fake; unexpected commands fail
// instead of falling through to the host's network or command runner.
type fabricNMTestHost struct {
	t                      *testing.T
	iface                  fabricInterface
	receipt                fabricNativeReceipt
	owner, busID, flags    string
	profiles               []fabricNMProfile
	deviceUUID, activePath string
	state                  int
	native                 map[string]fabricNativeAddress
	calls                  [][]string
	saved                  []fabricNativeReceipt
	createErr              error
	activateErr            error
	createdFields          map[string]string
	pendingReads, dadReads int
	activating             bool
	autoconnect            bool
	resumeForeign          bool
	resumeAddress          string
	settleForeign          bool
	foreignPendingReads    int
	foreignBecomesUsable   bool
	retirePlaceholders     bool
	regenerateAt           string
	regeneratedPlaceholder *fabricNMProfile
	malformed              map[string]string
	kernelEdit             func([]map[string]any) []map[string]any
}

func fabricNMTestNew(t *testing.T) *fabricNMTestHost {
	t.Helper()
	iface := fabricNativeTestInterface()
	originalAutoconnect := true
	h := &fabricNMTestHost{t: t, iface: iface, owner: ":1.42", busID: "0123456789abcdef0123456789abcdef", flags: "b true\nu 1\n", state: 30, autoconnect: true,
		native: map[string]fabricNativeAddress{}, malformed: map[string]string{}}
	h.receipt = fabricNativeReceipt{Operation: "mock-fabric-operation", Interface: iface, Label: "nP7b4TrU_l81zQ9", BootID: "mock-boot",
		NM: &fabricNMReceipt{UUID: fabricNMUUID("mock-fabric-operation", iface), Owner: h.owner, BusID: h.busID, DevicePath: fabricNMRoot + "/Devices/42", PermanentMAC: iface.MAC, OriginalAutoconnect: &originalAutoconnect, BaselineProfiles: []fabricNMProfileBinding{}}}
	return h
}

func fabricNMTestEscape(value string) string {
	return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(value)
}

func fabricNMTestFields(fields map[string]string) []byte {
	var lines []string
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		lines = append(lines, key+":"+fields[key])
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestFabricNMMultilineRawFieldsAndTabularEscapes(t *testing.T) {
	t.Run("captured device MAC and empty optional fields", func(t *testing.T) {
		// nmcli stdout for a selected device readback.
		const captured = `GENERAL.DEVICE:enp1s0f0np0
GENERAL.HWADDR:02:00:00:0B:00:00
GENERAL.NM-MANAGED:yes
GENERAL.STATE:30 (disconnected)
GENERAL.CON-UUID:
GENERAL.CON-PATH:
IP4.GATEWAY:
IP6.GATEWAY:
`
		want := map[string]string{
			"GENERAL.DEVICE": "enp1s0f0np0", "GENERAL.HWADDR": "02:00:00:0B:00:00",
			"GENERAL.NM-MANAGED": "yes", "GENERAL.STATE": "30 (disconnected)",
			"GENERAL.CON-UUID": "", "GENERAL.CON-PATH": "", "IP4.GATEWAY": "", "IP6.GATEWAY": "",
		}
		got, err := fabricNMFields([]byte(captured))
		if err != nil || !maps.Equal(got, want) {
			t.Fatalf("native multiline fields changed: %+v %v", got, err)
		}
	})
	t.Run("IPv6 and literal backslash colon values remain raw", func(t *testing.T) {
		const raw = `IP6.ADDRESS[1]:fe80::f133:56c6:a27f:8690/64
connection.id:literal\:name\\tail
`
		want := map[string]string{"IP6.ADDRESS[1]": "fe80::f133:56c6:a27f:8690/64", "connection.id": `literal\:name\\tail`}
		got, err := fabricNMFields([]byte(raw))
		if err != nil || !maps.Equal(got, want) {
			t.Fatalf("multiline values were split or unescaped: %+v %v", got, err)
		}
		if string(fabricNMTestFields(want)) != raw {
			t.Fatal("multiline fixture added tabular escaping")
		}
	})
	var tooMany strings.Builder
	for i := 0; i < 513; i++ {
		fmt.Fprintf(&tooMany, "FIELD%d:value\n", i)
	}
	for name, data := range map[string]string{
		"duplicate field": "FIELD:a\nFIELD:b\n", "missing separator": "FIELD\n",
		"empty field name": ":value\n", "control in value": "FIELD:bad\x00\n",
		"control in name": "FI\tELD:value\n", "empty output": "",
		"output bound": strings.Repeat("x", fabricNativeMaxBytes+1), "field count bound": tooMany.String(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fabricNMFields([]byte(data)); err == nil {
				t.Fatal("malformed or unbounded multiline output was accepted")
			}
		})
	}
	t.Run("tabular escaping stays strict", func(t *testing.T) {
		got, err := fabricNMSplit(`profile\:name:path\\name:`)
		if err != nil || !slices.Equal(got, []string{"profile:name", `path\name`, ""}) {
			t.Fatalf("valid tabular escapes changed: %v %v", got, err)
		}
		for _, data := range []string{`bad\`, `bad\n`, "bad\x00"} {
			if _, err := fabricNMSplit(data); err == nil {
				t.Fatal("ambiguous tabular escape or control was accepted")
			}
		}
	})
}

func (h *fabricNMTestHost) ownedProfile() fabricNMProfile {
	return fabricNMProfile{UUID: h.receipt.NM.UUID, Type: "802-3-ethernet", Path: fabricNMRoot + "/Settings/17",
		Filename: "/run/NetworkManager/system-connections/nvpair-fabric.nmconnection",
		Fields: map[string]string{
			"connection.id": "nvpair-fabric-" + h.receipt.Label, "connection.uuid": h.receipt.NM.UUID,
			"connection.type": "802-3-ethernet", "connection.interface-name": h.iface.Name,
			"connection.autoconnect": "no", "connection.multi-connect": "1 (single)", "connection.timestamp": "0",
			"connection.llmnr": "0 (no)", "connection.mdns": "0 (no)", "connection.lldp": "disable",
			"802-3-ethernet.mac-address": h.iface.MAC, "802-3-ethernet.cloned-mac-address": "preserve", "802-3-ethernet.mtu": "1500",
			"802-3-ethernet.auto-negotiate": "no", "802-3-ethernet.speed": "0", "802-3-ethernet.duplex": "", "802-3-ethernet.wake-on-lan": "ignore",
			"ipv4.method": "manual", "ipv4.addresses": h.iface.Address, "ipv4.never-default": "yes", "ipv4.ignore-auto-dns": "yes", "ipv4.ignore-auto-routes": "yes",
			"ipv4.gateway": "", "ipv4.dns": "", "ipv4.dns-search": "", "ipv4.routes": "", "ipv4.route-table": "254 (main)", "ipv4.dad-timeout": "1000", "ipv4.may-fail": "no",
			"ipv4.link-local": "disabled",
			"ipv6.method":     "disabled", "ipv6.never-default": "yes", "ipv6.ignore-auto-dns": "yes",
			"proxy.method": "none", "proxy.browser-only": "no", "proxy.pac-url": "", "proxy.pac-script": "",
		}}
}

func (h *fabricNMTestHost) completeActivation() {
	h.activating = false
	h.state = 100
	prefix := netip.MustParsePrefix(h.iface.Address)
	h.native[h.iface.Address] = fabricNativeAddress{Local: prefix.Addr().String(), PrefixLen: prefix.Bits(), Label: h.iface.Name,
		Valid: json.RawMessage(`4294967295`), Preferred: json.RawMessage(`4294967295`)}
}

// The kernel view follows the mocked addresses and the activated owned
// profile's configured routes, the way NetworkManager programs them.
func (h *fabricNMTestHost) kernelRoutes() []byte {
	rows := []map[string]any{{"dst": "default", "gateway": "192.0.2.1", "dev": "wlP9s9", "protocol": "dhcp", "flags": []string{}}}
	for _, cidr := range slices.Sorted(maps.Keys(h.native)) {
		prefix := netip.MustParsePrefix(cidr)
		rows = append(rows,
			map[string]any{"dst": prefix.Masked().String(), "dev": h.iface.Name, "protocol": "kernel", "scope": "link", "prefsrc": prefix.Addr().String(), "flags": []string{}},
			map[string]any{"type": "local", "dst": prefix.Addr().String(), "table": "local", "dev": h.iface.Name, "protocol": "kernel", "scope": "host", "prefsrc": prefix.Addr().String(), "flags": []string{}})
	}
	for _, p := range h.profiles {
		if p.UUID != h.receipt.NM.UUID || !p.Active || h.state != 100 || p.Fields["ipv4.routes"] == "" {
			continue
		}
		for _, route := range strings.Split(p.Fields["ipv4.routes"], ", ") {
			destination, gateway, _ := strings.Cut(route, " ")
			rows = append(rows, map[string]any{"dst": strings.TrimSuffix(destination, "/32"), "gateway": gateway, "dev": h.iface.Name, "protocol": "static", "flags": []string{}})
		}
	}
	if h.kernelEdit != nil {
		rows = h.kernelEdit(rows)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		h.t.Fatal(err)
	}
	return data
}

func (h *fabricNMTestHost) run(_ context.Context, command string, args ...string) ([]byte, error) {
	h.t.Helper()
	h.calls = append(h.calls, append([]string{command}, args...))
	if slices.Contains(args, "--show-secrets") || slices.Contains(args, "-s") {
		h.t.Fatal("secret-bearing command requested")
	}
	text := func(value string) ([]byte, error) { return []byte(value), nil }
	match := func(want ...string) bool { return slices.Equal(args, want) }
	if command == "busctl" {
		switch {
		case match("--system", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId"):
			if value, ok := h.malformed["bus"]; ok {
				return text(value)
			}
			return text("s " + strconv.Quote(h.busID) + "\n")
		case match("--system", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetNameOwner", "s", fabricNMService):
			return text("s " + strconv.Quote(h.owner) + "\n")
		case match("--system", "call", h.owner, fabricNMRoot, fabricNMService, "GetDeviceByIpIface", "s", h.iface.Name):
			return text("o " + strconv.Quote(h.receipt.NM.DevicePath) + "\n")
		case match("--system", "get-property", h.owner, h.receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device.Wired", "PermHwAddress"):
			return text("s " + strconv.Quote(h.iface.MAC) + "\n")
		case match("--system", "get-property", h.owner, h.receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device", "Autoconnect"):
			return text("b " + strconv.FormatBool(h.autoconnect) + "\n")
		case match("--system", "set-property", h.owner, h.receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device", "Autoconnect", "b", "false"),
			match("--system", "set-property", h.owner, h.receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device", "Autoconnect", "b", "true"):
			if len(h.saved) == 0 {
				h.t.Fatal("policy setter preceded receipt persistence")
			}
			h.autoconnect = args[len(args)-1] == "true"
			if h.autoconnect {
				h.regeneratePlaceholder("restore")
			}
			if h.autoconnect && h.resumeForeign && h.deviceUUID == "" {
				for i := range h.profiles {
					p := &h.profiles[i]
					if p.UUID != h.receipt.NM.UUID && p.Fields["connection.interface-name"] == h.iface.Name {
						p.Active, p.Device, p.ActivePath = true, h.iface.Name, fabricNMRoot+"/ActiveConnection/20"
						h.deviceUUID, h.activePath, h.state = p.UUID, p.ActivePath, 100
						address := h.resumeAddress
						if address == "" {
							address = "192.168.50.10/24"
						}
						local, bits, _ := strings.Cut(address, "/")
						prefix, _ := strconv.Atoi(bits)
						h.native[address] = fabricNativeAddress{Local: local, PrefixLen: prefix, Label: h.iface.Name,
							Valid: json.RawMessage(`4294967295`), Preferred: json.RawMessage(`4294967295`)}
					}
				}
			}
			return text("")
		}
		for i := range h.profiles {
			p := &h.profiles[i]
			if match("--system", "get-property", h.owner, p.Path, fabricNMSettings, "Flags") {
				flags := p.Flags
				if p.UUID == h.receipt.NM.UUID {
					flags = 1
				}
				return text(fmt.Sprintf("u %d\n", flags))
			}
			if match("--system", "get-property", h.owner, p.Path, fabricNMSettings, "Unsaved", "Flags") {
				return text(h.flags)
			}
			if match("--system", "call", h.owner, fabricNMRoot, fabricNMService, "ActivateConnection", "ooo", p.Path, h.receipt.NM.DevicePath, "/") {
				if len(h.saved) == 0 {
					h.t.Fatal("activation preceded owned receipt persistence")
				}
				h.deviceUUID, h.activePath, h.state = p.UUID, fabricNMRoot+"/ActiveConnection/9", 70
				p.Active, p.Device, p.ActivePath = true, h.iface.Name, h.activePath
				// Manual activation may re-enable the device's autoconnect gate.
				h.autoconnect = true
				h.activating = true
				return []byte("o " + strconv.Quote(h.activePath) + "\n"), h.activateErr
			}
			if match("--system", "call", h.owner, p.Path, fabricNMSettings, "Delete") {
				if p.Flags&2 != 0 {
					h.t.Fatal("generated placeholder was manually deleted")
				}
				h.profiles = slices.Delete(h.profiles, i, i+1)
				h.deviceUUID, h.activePath, h.state = "", "", 30
				delete(h.native, h.iface.Address)
				h.regeneratePlaceholder("delete")
				return text("")
			}
		}
	}
	if command == "ip" && match("-j", "-4", "route", "show", "table", "all") {
		return h.kernelRoutes(), nil
	}
	if command == "nmcli" {
		if len(args) >= 4 && slices.Equal(args[:4], []string{"--wait", "2", "connection", "add"}) {
			if h.retirePlaceholders {
				h.profiles = slices.DeleteFunc(h.profiles, func(p fabricNMProfile) bool { return p.Flags == 3 })
			}
			profile := h.ownedProfile()
			if i := slices.Index(args, "ipv4.routes"); i >= 0 && i+1 < len(args) {
				profile.Fields["ipv4.routes"] = args[i+1]
			}
			maps.Copy(profile.Fields, h.createdFields)
			h.profiles = append(h.profiles, profile)
			return []byte("Connection successfully added\n"), h.createErr
		}
		if len(args) >= 3 && slices.Equal(args[len(args)-3:], []string{"device", "show", h.iface.Name}) {
			if value, ok := h.malformed["device"]; ok {
				return text(value)
			}
			if h.activating {
				if h.pendingReads == 0 {
					h.completeActivation()
				} else {
					h.pendingReads--
					h.dadReads++
				}
			}
			if h.settleForeign && !h.autoconnect && h.state == 70 && h.deviceUUID != h.receipt.NM.UUID {
				if h.foreignPendingReads > 0 {
					h.foreignPendingReads--
				} else if h.foreignBecomesUsable {
					h.state = 100
					h.native["192.168.50.10/24"] = fabricNativeAddress{Local: "192.168.50.10", PrefixLen: 24, Label: h.iface.Name}
				} else {
					h.deviceUUID, h.activePath, h.state = "", "", 30
					for i := range h.profiles {
						h.profiles[i].Active, h.profiles[i].Device, h.profiles[i].ActivePath = false, "", ""
					}
				}
			}
			fields := map[string]string{"GENERAL.DEVICE": h.iface.Name, "GENERAL.HWADDR": h.iface.MAC, "GENERAL.NM-MANAGED": "yes",
				"GENERAL.STATE": fmt.Sprintf("%d (mock state)", h.state), "GENERAL.CON-UUID": h.deviceUUID, "GENERAL.CON-PATH": h.activePath,
				"IP4.GATEWAY": "--", "IP6.GATEWAY": "--"}
			for i, address := range slices.Sorted(maps.Keys(h.native)) {
				fields[fmt.Sprintf("IP4.ADDRESS[%d]", i+1)] = address
			}
			return fabricNMTestFields(fields), nil
		}
		if len(args) >= 2 && slices.Equal(args[len(args)-2:], []string{"connection", "show"}) {
			if value, ok := h.malformed["inventory"]; ok {
				return text(value)
			}
			var lines []string
			for _, p := range h.profiles {
				active := "no"
				if p.Active {
					active = "yes"
				}
				row := []string{p.UUID, p.Type, p.Path, active, p.Device, p.ActivePath, p.Filename}
				for i := range row {
					row[i] = fabricNMTestEscape(row[i])
				}
				lines = append(lines, strings.Join(row, ":"))
			}
			return text(strings.Join(lines, "\n"))
		}
		for _, p := range h.profiles {
			if len(args) >= 4 && slices.Equal(args[len(args)-4:], []string{"connection", "show", "path", p.Path}) {
				if value, ok := h.malformed["profile"]; ok {
					return text(value)
				}
				return fabricNMTestFields(p.Fields), nil
			}
		}
	}
	h.t.Fatalf("unexpected command: %s %q", command, args)
	return nil, errors.New("unexpected mocked command")
}

func (h *fabricNMTestHost) io() fabricNMIO {
	return fabricNMIO{run: h.run,
		identity: func(iface fabricInterface, _ bool) (fabricInterface, error) {
			return h.iface, fabricNativeIdentity(iface, h.iface)
		},
		addresses: func(context.Context, fabricInterface) (map[string]fabricNativeAddress, error) {
			return maps.Clone(h.native), nil
		},
		save: func(receipt fabricNativeReceipt) error {
			copy := *receipt.NM
			receipt.NM = &copy
			h.saved = append(h.saved, receipt)
			return nil
		},
	}
}

func (h *fabricNMTestHost) mutations() (creates, activates, deletes int) {
	for _, call := range h.calls {
		if call[0] == "nmcli" && slices.Contains(call, "add") {
			creates++
		}
		if slices.Contains(call, "ActivateConnection") {
			activates++
		}
		if slices.Contains(call, "Delete") {
			deletes++
		}
	}
	return
}

func (h *fabricNMTestHost) policyChanges() int {
	n := 0
	for _, call := range h.calls {
		if slices.Contains(call, "set-property") {
			n++
		}
	}
	return n
}

func (h *fabricNMTestHost) add(t *testing.T) fabricNativeReceipt {
	t.Helper()
	if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if len(h.saved) == 0 {
		t.Fatalf("saved receipts = %d", len(h.saved))
	}
	return h.latestReceipt()
}

func (h *fabricNMTestHost) latestReceipt() fabricNativeReceipt {
	h.t.Helper()
	if len(h.saved) == 0 {
		h.t.Fatal("no durable receipt")
	}
	receipt := h.saved[len(h.saved)-1]
	nm := *receipt.NM
	receipt.NM = &nm
	return receipt
}

func TestFabricNMMockedLifecyclePreservesExactRuntimePolicy(t *testing.T) {
	h := fabricNMTestNew(t)
	receipt := h.add(t)
	var add []string
	for _, call := range h.calls {
		if call[0] == "nmcli" && slices.Contains(call, "add") {
			add = call
		}
	}
	for key, want := range map[string]string{"save": "no", "ifname": h.iface.Name, "connection.uuid": receipt.NM.UUID,
		"connection.autoconnect": "no", "802-3-ethernet.mac-address": h.iface.MAC, "802-3-ethernet.cloned-mac-address": "preserve",
		"802-3-ethernet.mtu": "1500", "connection.lldp": "disable", "ipv4.route-table": "254", "ipv4.dad-timeout": "1000", "ipv4.addresses": h.iface.Address, "ipv4.routes": ""} {
		i := slices.Index(add, key)
		if i < 0 || i+1 >= len(add) || add[i+1] != want {
			t.Fatalf("creation option %s = %q; wanted %q", key, add, want)
		}
	}
	if receipt.NM.SettingsPath != h.profiles[0].Path || receipt.NM.ProfileDigest == "" || !fabricNMReceiptValid(receipt) {
		t.Fatal("ownership receipt incomplete")
	}
	// A timestamp update is expected daemon bookkeeping, not profile adoption.
	h.profiles[0].Fields["connection.timestamp"] = "1800000000"
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if c, a, d := h.mutations(); c != 1 || a != 1 || d != 1 {
		t.Fatalf("mutations = create:%d activate:%d delete:%d", c, a, d)
	}
	if len(h.profiles) != 0 || len(h.native) != 0 || h.state != 30 {
		t.Fatal("owned profile or address survived cleanup")
	}
}

// fabricNMTestNewRouted is a routed ring member's p0 interface.
func fabricNMTestNewRouted(t *testing.T) *fabricNMTestHost {
	t.Helper()
	h := fabricNMTestNew(t)
	h.iface.Address = "10.253.0.0/31"
	h.iface.Routes = []fabricRoute{{Destination: "10.253.0.4/32", Gateway: "10.253.0.1"}}
	h.receipt.Interface = h.iface
	h.receipt.NM.UUID = fabricNMUUID(h.receipt.Operation, h.iface)
	return h
}

func (h *fabricNMTestHost) createArg(key string) (string, bool) {
	for _, call := range h.calls {
		if call[0] == "nmcli" && slices.Contains(call, "add") {
			if i := slices.Index(call, key); i >= 0 && i+1 < len(call) {
				return call[i+1], true
			}
		}
	}
	return "", false
}

func (h *fabricNMTestHost) routeReads() int {
	n := 0
	for _, call := range h.calls {
		if call[0] == "ip" {
			n++
		}
	}
	return n
}

func TestFabricNMRoutedProfileCarriesAndProvesExactlyItsHostRoute(t *testing.T) {
	h := fabricNMTestNewRouted(t)
	receipt := h.add(t)
	if routes, ok := h.createArg("ipv4.routes"); !ok || routes != "10.253.0.4/32 10.253.0.1" {
		t.Fatalf("routed profile creation wrote ipv4.routes=%q", routes)
	}
	if !receipt.NM.PolicyEstablished || !fabricNMReceiptValid(receipt) || h.routeReads() == 0 {
		t.Fatal("routed activation did not prove its kernel routes before establishing policy")
	}
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	completed := h.latestReceipt()
	if !completed.NM.CleanupConfirmed || len(h.profiles) != 0 || len(h.native) != 0 {
		t.Fatal("routed profile, address or host route survived cleanup")
	}
	if err := fabricNMRemove(context.Background(), h.iface, completed, h.io()); err != nil {
		t.Fatalf("second routed cleanup: %v", err)
	}
	if c, a, d := h.mutations(); c != 1 || a != 1 || d != 1 {
		t.Fatalf("routed mutations = create:%d activate:%d delete:%d", c, a, d)
	}
}

func TestFabricNMUnroutedProfileNeverReadsRoutesDuringRollback(t *testing.T) {
	h := fabricNMTestNew(t)
	h.iface.Address = "10.253.0.2/31"
	h.receipt.Interface = h.iface
	h.receipt.NM.UUID = fabricNMUUID(h.receipt.Operation, h.iface)
	receipt := h.add(t)
	if routes, ok := h.createArg("ipv4.routes"); !ok || routes != "" {
		t.Fatalf("unrouted profile creation wrote ipv4.routes=%q", routes)
	}
	reads := h.routeReads()
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if !h.latestReceipt().NM.CleanupConfirmed || h.routeReads() != reads {
		t.Fatal("unrouted rollback changed its exact cleanup evidence")
	}
}

func TestFabricNMRoutedProfileReadbackRejectsRouteDrift(t *testing.T) {
	for name, routes := range map[string]string{
		"missing":       "",
		"extra":         "10.253.0.4/32 10.253.0.1, 10.253.0.5/32 10.253.0.1",
		"wrong gateway": "10.253.0.4/32 10.253.0.9",
		"with metric":   "10.253.0.4/32 10.253.0.1 100",
	} {
		t.Run(name, func(t *testing.T) {
			h := fabricNMTestNewRouted(t)
			h.createdFields = map[string]string{"ipv4.routes": routes}
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("drifted owned profile routes were accepted")
			}
			if c, a, d := h.mutations(); c != 1 || a != 0 || d != 0 {
				t.Fatal("drifted routed profile was activated or deleted")
			}
		})
	}
}

func TestFabricNMRoutedKernelReadbackRejectsRouteDrift(t *testing.T) {
	iface := fabricNMTestNewRouted(t).iface.Name
	for name, edit := range map[string]func([]map[string]any) []map[string]any{
		"missing": func(rows []map[string]any) []map[string]any {
			return slices.DeleteFunc(rows, func(row map[string]any) bool { return row["gateway"] == "10.253.0.1" })
		},
		"extra": func(rows []map[string]any) []map[string]any {
			return append(rows, map[string]any{"dst": "10.253.0.5", "gateway": "10.253.0.1", "dev": iface})
		},
		"wrong gateway": func(rows []map[string]any) []map[string]any {
			for _, row := range rows {
				if row["gateway"] == "10.253.0.1" {
					row["gateway"] = "10.253.0.9"
				}
			}
			return rows
		},
		"another table": func(rows []map[string]any) []map[string]any {
			return append(rows, map[string]any{"dst": "10.253.0.4", "gateway": "10.253.0.1", "dev": iface, "table": "120"})
		},
		"multipath": func(rows []map[string]any) []map[string]any {
			return append(rows, map[string]any{"dst": "10.253.0.5", "nexthops": []map[string]string{{"dev": iface, "gateway": "10.253.0.1"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := fabricNMTestNewRouted(t)
			h.kernelEdit = edit
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("kernel route drift was accepted")
			}
			if len(h.saved) == 0 || h.latestReceipt().NM.PolicyEstablished {
				t.Fatal("kernel route drift established owned policy")
			}
		})
	}
}

func TestFabricNMRoutedCleanupHoldsWhileTheHostRouteRemains(t *testing.T) {
	h := fabricNMTestNewRouted(t)
	receipt := h.add(t)
	h.kernelEdit = func(rows []map[string]any) []map[string]any {
		return append(rows, map[string]any{"dst": "10.253.0.4", "gateway": "10.253.0.1", "dev": h.iface.Name, "protocol": "static"})
	}
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err == nil {
		t.Fatal("cleanup confirmed while the owned host route remained")
	}
	if h.latestReceipt().NM.CleanupConfirmed {
		t.Fatal("remaining host route was recorded as confirmed cleanup")
	}
	h.kernelEdit = nil
	if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
		t.Fatal(err)
	}
	if !h.latestReceipt().NM.CleanupConfirmed {
		t.Fatal("routed cleanup was not confirmed once the host route disappeared")
	}
}

func TestFabricNMMockedActivationWaitsForDAD(t *testing.T) {
	h := fabricNMTestNew(t)
	h.pendingReads = 1
	h.add(t)
	if h.dadReads != 1 || h.state != 100 || len(h.native) != 1 {
		t.Fatal("success did not require post-DAD address evidence")
	}
}

func TestFabricNMMockedColdAndParentProfilesBlockCreation(t *testing.T) {
	for _, kind := range []string{"active ownership", "match selector", "parent dependency"} {
		t.Run(kind, func(t *testing.T) {
			h := fabricNMTestNew(t)
			p := fabricNMProfile{UUID: "00000000-0000-4000-8000-000000000001", Type: "802-3-ethernet", Path: fabricNMRoot + "/Settings/2", Filename: "/etc/NetworkManager/system-connections/foreign.nmconnection",
				Fields: map[string]string{"connection.uuid": "00000000-0000-4000-8000-000000000001", "connection.id": "foreign"}}
			switch kind {
			case "active ownership":
				p.Fields["connection.interface-name"] = h.iface.Name
				p.Active = true
				p.Device = h.iface.Name
				p.ActivePath = fabricNMRoot + "/ActiveConnection/2"
			case "match selector":
				p.Fields["match.interface-name"] = "en*"
			case "parent dependency":
				p.Type = "vlan"
				p.Fields["connection.interface-name"] = "vlan42"
				p.Fields["vlan.parent"] = h.iface.Name
			}
			h.profiles = []fabricNMProfile{p}
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("foreign cold profile accepted")
			}
			if c, a, d := h.mutations(); c+a+d != 0 {
				t.Fatal("preflight rejection caused a mutation")
			}
		})
	}
}

func TestFabricNMMockedCleanupPreservesForeignState(t *testing.T) {
	for name, change := range map[string]func(*fabricNMTestHost){
		"UUID": func(h *fabricNMTestHost) {
			h.profiles[0].UUID = "00000000-0000-4000-8000-000000000099"
			h.profiles[0].Fields["connection.uuid"] = h.profiles[0].UUID
		},
		"name":             func(h *fabricNMTestHost) { h.profiles[0].Fields["connection.id"] = "adopted-by-user" },
		"settings object":  func(h *fabricNMTestHost) { h.profiles[0].Path = fabricNMRoot + "/Settings/99" },
		"settings content": func(h *fabricNMTestHost) { h.profiles[0].Fields["connection.zone"] = "foreign-zone" },
		"persistent file": func(h *fabricNMTestHost) {
			h.profiles[0].Filename = "/etc/NetworkManager/system-connections/adopted.nmconnection"
		},
		"saved flag":          func(h *fabricNMTestHost) { h.flags = "b false\nu 0\n" },
		"foreign flags":       func(h *fabricNMTestHost) { h.flags = "b true\nu 5\n" },
		"foreign active UUID": func(h *fabricNMTestHost) { h.deviceUUID = "00000000-0000-4000-8000-000000000099" },
		"extra address": func(h *fabricNMTestHost) {
			h.native["172.31.240.2/30"] = fabricNativeAddress{Local: "172.31.240.2", PrefixLen: 30}
		},
		"same owner different bus": func(h *fabricNMTestHost) { h.busID = "fedcba9876543210fedcba9876543210" },
	} {
		t.Run(name, func(t *testing.T) {
			h := fabricNMTestNew(t)
			receipt := h.add(t)
			change(h)
			if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err == nil {
				t.Fatal("changed ownership allowed cleanup")
			}
			if _, _, d := h.mutations(); d != 0 {
				t.Fatal("foreign state was deleted")
			}
			if len(h.profiles) != 1 || len(h.native) == 0 {
				t.Fatal("foreign state was not preserved")
			}
		})
	}
}

func TestFabricNMMockedUncertainCreationHasExactPendingCleanup(t *testing.T) {
	h := fabricNMTestNew(t)
	h.createErr = errors.New("mock response lost after daemon created the profile")
	if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
		t.Fatal("uncertain creation reported success")
	}
	if len(h.saved) == 0 || h.receipt.NM.SettingsPath != "" || len(h.profiles) != 1 || h.autoconnect {
		t.Fatal("pending creation fixture or receipt state incorrect")
	}
	if err := fabricNMRemove(context.Background(), h.iface, h.receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
		t.Fatal(err)
	}
	if c, a, d := h.mutations(); c != 1 || a != 0 || d != 1 {
		t.Fatalf("pending cleanup mutations = %d/%d/%d", c, a, d)
	}
	if !h.autoconnect {
		t.Fatal("pending cleanup did not restore original autoconnect")
	}
}

func (h *fabricNMTestHost) seedCompatibleProfile() fabricNMProfile {
	p := fabricNMProfile{UUID: "00000000-0000-4000-8000-000000000002", Type: "802-3-ethernet", Path: fabricNMRoot + "/Settings/2",
		Filename: "/etc/NetworkManager/system-connections/ethernet.nmconnection", Fields: map[string]string{
			"connection.id": "Ethernet fabric", "connection.uuid": "00000000-0000-4000-8000-000000000002", "connection.type": "802-3-ethernet",
			"connection.interface-name": h.iface.Name, "connection.autoconnect": "yes", "connection.timestamp": "0", "connection.master": "", "connection.slave-type": "",
			"802-3-ethernet.mac-address": "", "ipv4.method": "auto", "ipv6.method": "auto",
		}}
	h.profiles = append(h.profiles, p)
	h.receipt.NM.BaselineProfiles = []fabricNMProfileBinding{{UUID: p.UUID, Path: p.Path, Filename: p.Filename, Digest: fabricNMProfileDigest(p.Fields)}}
	return p
}

func TestFabricNMMockedPreservesDormantProfileAndForeignResumption(t *testing.T) {
	h := fabricNMTestNew(t)
	baseline := h.seedCompatibleProfile()
	before := maps.Clone(baseline.Fields)
	h.resumeForeign = true
	receipt := h.add(t)
	if h.autoconnect {
		t.Fatal("autoconnect was not paused after manual activation")
	}
	if !maps.Equal(h.profiles[0].Fields, before) {
		t.Fatal("existing profile settings were modified")
	}
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if !h.autoconnect || h.deviceUUID != baseline.UUID || len(h.profiles) != 1 || !h.profiles[0].Active || !maps.Equal(h.profiles[0].Fields, before) {
		t.Fatal("original policy or resumed foreign profile was not preserved")
	}
	if _, exists := h.native[h.iface.Address]; exists {
		t.Fatal("owned address survived rollback")
	}
	if _, exists := h.native["192.168.50.10/24"]; !exists {
		t.Fatal("foreign address removed")
	}
	sets := h.policyChanges()
	if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
		t.Fatal(err)
	}
	if c, a, d := h.mutations(); c != 1 || a != 1 || d != 1 || h.policyChanges() != sets {
		t.Fatal("idempotent cleanup changed the resumed connection or policy")
	}
}

func TestFabricNMMockedBaselineDriftAndMissingPolicyHold(t *testing.T) {
	for _, mode := range []string{"modified baseline", "missing original policy", "replaced baseline path", "generated without unsaved", "volatile profile", "external profile"} {
		t.Run(mode, func(t *testing.T) {
			h := fabricNMTestNew(t)
			h.seedCompatibleProfile()
			switch mode {
			case "modified baseline":
				h.profiles[0].Fields["connection.autoconnect"] = "no"
			case "missing original policy":
				h.receipt.NM.OriginalAutoconnect = nil
			case "replaced baseline path":
				h.profiles[0].Path = fabricNMRoot + "/Settings/90"
			case "generated without unsaved":
				h.profiles[0].Flags = 2
			case "volatile profile":
				h.profiles[0].Flags = 4
			case "external profile":
				h.profiles[0].Flags = 8
			}
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("unreviewed policy or baseline accepted")
			}
			if c, a, d := h.mutations(); c+a+d != 0 || h.policyChanges() != 0 {
				t.Fatal("admission hold mutated NetworkManager state")
			}
		})
	}
}

func (h *fabricNMTestHost) regeneratePlaceholder(stage string) {
	if h.regenerateAt != stage || h.regeneratedPlaceholder == nil {
		return
	}
	p := *h.regeneratedPlaceholder
	p.Fields = maps.Clone(p.Fields)
	h.profiles = append(h.profiles, p)
	h.regeneratedPlaceholder = nil
}

func (h *fabricNMTestHost) seedGeneratedPlaceholder() fabricNMProfile {
	h.seedCompatibleProfile()
	p := &h.profiles[0]
	p.Flags, p.FlagsKnown, p.Filename = 3, true, ""
	h.receipt.NM.BaselineProfiles = []fabricNMProfileBinding{{UUID: p.UUID, Path: p.Path, Filename: p.Filename, Digest: fabricNMProfileDigest(p.Fields), Ephemeral: true}}
	return *p
}

func TestFabricNMGeneratedCandidateAllowsOnlyTransientGenerationCycle(t *testing.T) {
	iface := fabricNativeTestInterface()
	profile := fabricNMProfile{Flags: 3, FlagsKnown: true, UUID: "00000000-0000-4000-8000-000000000002", Type: "802-3-ethernet",
		Path: fabricNMRoot + "/Settings/2", ActivePath: fabricNMRoot + "/ActiveConnection/2", Device: iface.Name, Active: true,
		Fields: map[string]string{"connection.id": "Generated Ethernet", "connection.uuid": "00000000-0000-4000-8000-000000000002", "connection.type": "802-3-ethernet", "connection.interface-name": iface.Name, "connection.autoconnect": "yes", "802-3-ethernet.mac-address": iface.MAC, "ipv4.method": "auto", "ipv6.method": "auto"}}
	device := fabricNMDevice{Autoconnect: true, Name: iface.Name, MAC: iface.MAC, PermanentMAC: iface.MAC, Path: fabricNMRoot + "/Devices/42", UUID: profile.UUID,
		ActivePath: profile.ActivePath, Managed: true, State: 70, Addresses: []string{"fe80::1/64"}, Gateways: []string{}, DNS: []string{}}
	snapshot := func() fabricNMSnapshot {
		copyProfile := profile
		copyProfile.Fields = maps.Clone(profile.Fields)
		copyDevice := device
		copyDevice.Addresses, copyDevice.Gateways, copyDevice.DNS = slices.Clone(device.Addresses), slices.Clone(device.Gateways), slices.Clone(device.DNS)
		return fabricNMSnapshot{Owner: ":1.42", BusID: strings.Repeat("a", 32), Devices: map[string]fabricNMDevice{iface.Name: copyDevice}, Profiles: []fabricNMProfile{copyProfile}}
	}
	reviewed, err := fabricNMGeneratedCandidate(snapshot(), iface)
	if err != nil || reviewed == nil {
		t.Fatalf("safe generated candidate unavailable: %+v %v", reviewed, err)
	}
	cycled := snapshot()
	cycledDevice := cycled.Devices[iface.Name]
	cycledDevice.ActivePath = fabricNMRoot + "/ActiveConnection/9"
	cycled.Devices[iface.Name] = cycledDevice
	cycled.Profiles[0].ActivePath = fabricNMRoot + "/ActiveConnection/9"
	current, err := fabricNMGeneratedCandidate(cycled, iface)
	if err != nil || !fabricNMGeneratedStableSame(reviewed, current) || fabricNMGeneratedSame(reviewed, current) {
		t.Fatal("transient ActiveConnection generation was not isolated from stable profile identity")
	}

	for name, mutate := range map[string]func(*fabricNMSnapshot){
		"DNS": func(s *fabricNMSnapshot) {
			value := s.Devices[iface.Name]
			value.DNS = []string{"192.0.2.53"}
			s.Devices[iface.Name] = value
		},
		"gateway": func(s *fabricNMSnapshot) {
			value := s.Devices[iface.Name]
			value.Gateways = []string{"192.0.2.1"}
			s.Devices[iface.Name] = value
		},
		"non-link-local address": func(s *fabricNMSnapshot) {
			value := s.Devices[iface.Name]
			value.Addresses = []string{"192.0.2.10/24"}
			s.Devices[iface.Name] = value
		},
		"usable state": func(s *fabricNMSnapshot) {
			value := s.Devices[iface.Name]
			value.State = 100
			s.Devices[iface.Name] = value
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := snapshot()
			mutate(&changed)
			if candidate, err := fabricNMGeneratedCandidate(changed, iface); err == nil || candidate != nil {
				t.Fatal("generated-default safety drift was admitted")
			}
		})
	}
}

func TestFabricNMMockedGeneratedPlaceholderRetiresAndRegenerates(t *testing.T) {
	for _, stage := range []string{"delete", "restore"} {
		t.Run(stage, func(t *testing.T) {
			h := fabricNMTestNew(t)
			original := h.seedGeneratedPlaceholder()
			snapshot, err := fabricNMRead(context.Background(), h.run, []fabricInterface{h.iface})
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := fabricNMBaseline(snapshot, h.iface, h.receipt.NM.UUID)
			if err != nil || len(bindings) != 1 || !bindings[0].Ephemeral {
				t.Fatalf("generated placeholder was not bound as ephemeral: %+v %v", bindings, err)
			}
			h.receipt.NM.BaselineProfiles = bindings
			regenerated := original
			regenerated.Fields = maps.Clone(original.Fields)
			regenerated.UUID, regenerated.Path = "00000000-0000-4000-8000-000000000003", fabricNMRoot+"/Settings/3"
			regenerated.Filename = "/run/NetworkManager/system-connections/default-ethernet.nmconnection"
			regenerated.Fields["connection.uuid"], regenerated.Fields["connection.id"] = regenerated.UUID, "Ethernet regenerated"
			h.retirePlaceholders, h.regenerateAt, h.regeneratedPlaceholder = true, stage, &regenerated
			receipt := h.add(t)
			if len(h.profiles) != 1 || h.profiles[0].UUID != receipt.NM.UUID {
				t.Fatal("daemon placeholder retirement was not tolerated")
			}
			if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
				t.Fatal(err)
			}
			if len(h.profiles) != 1 || h.profiles[0].UUID != regenerated.UUID || h.profiles[0].Flags != 3 || !h.autoconnect {
				t.Fatal("daemon-regenerated placeholder or original policy was not preserved")
			}
			if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
				t.Fatal(err)
			}
			if c, a, d := h.mutations(); c != 1 || a != 1 || d != 1 {
				t.Fatal("placeholder lifecycle triggered a manual mutation")
			}
		})
	}
}

func TestFabricNMMockedGeneratedPlaceholderRejectsForeignConfiguration(t *testing.T) {
	for name, change := range map[string]func(*fabricNMProfile){
		"manual method": func(p *fabricNMProfile) {
			p.Fields["ipv4.method"] = "manual"
			p.Fields["ipv4.addresses"] = "172.31.240.2/30"
		},
		"additional address":       func(p *fabricNMProfile) { p.Fields["ipv4.addresses"] = "172.31.240.2/30" },
		"DNS":                      func(p *fabricNMProfile) { p.Fields["ipv4.dns"] = "192.168.1.1" },
		"routes":                   func(p *fabricNMProfile) { p.Fields["ipv4.routes"] = "192.168.0.0/16 192.168.1.1" },
		"rules":                    func(p *fabricNMProfile) { p.Fields["ipv4.routing-rules"] = "priority 100 from all table 200" },
		"gateway":                  func(p *fabricNMProfile) { p.Fields["ipv4.gateway"] = "192.168.1.1" },
		"IPv6 address":             func(p *fabricNMProfile) { p.Fields["ipv6.addresses"] = "fd00::1/64" },
		"security":                 func(p *fabricNMProfile) { p.Fields["802-1x.eap"] = "tls" },
		"parent":                   func(p *fabricNMProfile) { p.Fields["vlan.parent"] = "parent0" },
		"master":                   func(p *fabricNMProfile) { p.Fields["connection.master"] = "bridge0" },
		"match":                    func(p *fabricNMProfile) { p.Fields["match.interface-name"] = "en*" },
		"persistent storage":       func(p *fabricNMProfile) { p.Filename = "/etc/NetworkManager/system-connections/generated.nmconnection" },
		"volatile flags":           func(p *fabricNMProfile) { p.Flags = 5 },
		"external flags":           func(p *fabricNMProfile) { p.Flags = 9 },
		"generated external flags": func(p *fabricNMProfile) { p.Flags = 11 },
		"user-adopted unsaved":     func(p *fabricNMProfile) { p.Flags = 1 },
		"user-adopted persistent": func(p *fabricNMProfile) {
			p.Flags = 0
			p.Filename = "/etc/NetworkManager/system-connections/adopted.nmconnection"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := fabricNMTestNew(t)
			h.seedGeneratedPlaceholder()
			change(&h.profiles[0])
			// Bind the supplied evidence itself so rejection proves the generated
			// profile constraint, rather than merely an ordinary digest mismatch.
			h.receipt.NM.BaselineProfiles[0].Filename = h.profiles[0].Filename
			h.receipt.NM.BaselineProfiles[0].Digest = fabricNMProfileDigest(h.profiles[0].Fields)
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("foreign configuration was admitted as an ephemeral placeholder")
			}
			if c, a, d := h.mutations(); c+a+d != 0 || h.policyChanges() != 0 {
				t.Fatal("generated-profile rejection changed network state")
			}
		})
	}
}

func TestFabricNMMockedRestoreRechecksExactAddressAbsence(t *testing.T) {
	h := fabricNMTestNew(t)
	h.seedCompatibleProfile()
	h.profiles[0].Fields["ipv4.method"] = "manual"
	h.profiles[0].Fields["ipv4.addresses"] = h.iface.Address
	h.receipt.NM.BaselineProfiles[0].Digest = fabricNMProfileDigest(h.profiles[0].Fields)
	h.resumeForeign, h.resumeAddress = true, h.iface.Address
	receipt := h.add(t)
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err == nil {
		t.Fatal("cleanup claimed address absence after original profile restored the exact CIDR")
	}
	if _, exists := h.native[h.iface.Address]; !exists || len(h.profiles) != 1 || !h.profiles[0].Active || !h.autoconnect {
		t.Fatal("restored foreign configuration was not preserved")
	}
	if _, _, deletes := h.mutations(); deletes != 1 {
		t.Fatal("restored foreign profile was selected for deletion")
	}
}

func TestFabricNMMockedOriginalAutoconnectFalseStaysFalse(t *testing.T) {
	h := fabricNMTestNew(t)
	original := false
	h.autoconnect = original
	h.receipt.NM.OriginalAutoconnect = &original
	receipt := h.add(t)
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	if h.autoconnect {
		t.Fatal("cleanup enabled autoconnect against original policy")
	}
}

func TestFabricNMMockedPendingDHCPSettlesOrPreservesUsableConnection(t *testing.T) {
	for _, usable := range []bool{false, true} {
		t.Run(fmt.Sprintf("becomes_usable_%t", usable), func(t *testing.T) {
			h := fabricNMTestNew(t)
			baseline := h.seedCompatibleProfile()
			h.profiles[0].Active, h.profiles[0].Device, h.profiles[0].ActivePath = true, h.iface.Name, fabricNMRoot+"/ActiveConnection/2"
			h.deviceUUID, h.activePath, h.state = baseline.UUID, h.profiles[0].ActivePath, 70
			h.settleForeign, h.foreignPendingReads, h.foreignBecomesUsable = true, 1, usable
			err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io())
			if !usable {
				if err != nil {
					t.Fatal(err)
				}
				if c, a, d := h.mutations(); c != 1 || a != 1 || d != 0 {
					t.Fatal("pending DHCP settle did not reach owned activation")
				}
				return
			}
			if err == nil {
				t.Fatal("usable foreign DHCP connection was overwritten")
			}
			if c, a, d := h.mutations(); c+a+d != 0 {
				t.Fatal("usable foreign activation caused a profile mutation")
			}
			if len(h.saved) == 0 {
				t.Fatal("pending policy intent was not journaled")
			}
			if err := fabricNMRemove(context.Background(), h.iface, h.saved[len(h.saved)-1], h.io()); err != nil {
				t.Fatal(err)
			}
			if !h.autoconnect || h.deviceUUID != baseline.UUID || len(h.native) != 1 || len(h.profiles) != 1 {
				t.Fatal("abort cleanup did not restore policy while preserving foreign connection")
			}
		})
	}
}

func TestFabricNMMockedMalformedFramingNeverMutates(t *testing.T) {
	for name, test := range map[string]struct{ stream, data string }{
		"duplicate field":         {"device", "GENERAL.DEVICE:x\nGENERAL.DEVICE:y\n"},
		"trailing tabular escape": {"inventory", "bad\\"},
		"unknown tabular escape":  {"inventory", "bad\\n\n"},
		"wrong device identity":   {"device", "GENERAL.DEVICE:bad:other\n"},
		"control character":       {"device", "GENERAL.DEVICE:bad\x00\n"},
		"truncated inventory":     {"inventory", "00000000-0000-4000-8000-000000000001:ethernet\n"},
		"invalid bus":             {"bus", "s \"not-a-bus-id\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			h := fabricNMTestNew(t)
			h.malformed[test.stream] = test.data
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("malformed output accepted")
			}
			if c, a, d := h.mutations(); c+a+d != 0 {
				t.Fatal("malformed evidence caused a mutation")
			}
		})
	}
}

func TestFabricNMMockedUnreviewedSettingsCannotBecomeOwned(t *testing.T) {
	for key, value := range map[string]string{
		"connection.secondaries": "00000000-0000-4000-8000-000000000002",
		"connection.master":      "bridge0", "vlan.parent": "parent0", "ipv4.routing-rules": "priority 100 from all table 200",
		"802-1x.eap": "tls", "connection.zone": "trusted", "proxy.method": "1 (auto)",
		"proxy.browser-only": "yes", "proxy.pac-url": "https://proxy.invalid/proxy.pac", "proxy.pac-script": "function FindProxyForURL() { return 'DIRECT'; }",
	} {
		t.Run(key, func(t *testing.T) {
			h := fabricNMTestNew(t)
			h.createdFields = map[string]string{key: value}
			if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
				t.Fatal("unreviewed setting was accepted into the owned fingerprint")
			}
			if c, a, d := h.mutations(); c != 1 || a != 0 || d != 0 {
				t.Fatal("unreviewed created profile was activated or deleted")
			}
			for _, saved := range h.saved {
				if saved.NM.ProfileDigest != "" || saved.NM.SettingsPath != "" {
					t.Fatal("unreviewed profile became an owned receipt")
				}
			}
		})
	}
}

func TestFabricNMMockedStockProxyDefaults(t *testing.T) {
	for _, browserOnly := range []string{"no", "false"} {
		t.Run(browserOnly, func(t *testing.T) {
			h := fabricNMTestNew(t)
			h.seedGeneratedPlaceholder()
			defaults := map[string]string{"proxy.method": "none", "proxy.browser-only": browserOnly, "proxy.pac-url": "", "proxy.pac-script": ""}
			maps.Copy(h.profiles[0].Fields, defaults)
			h.receipt.NM.BaselineProfiles[0].Digest = fabricNMProfileDigest(h.profiles[0].Fields)
			h.createdFields = defaults
			receipt := h.add(t)
			if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFabricNMMockedEstablishedPolicyDriftHolds(t *testing.T) {
	h := fabricNMTestNew(t)
	receipt := h.add(t)
	if !receipt.NM.PolicyEstablished || receipt.NM.CleanupConfirmed {
		t.Fatal("successful add did not durably establish its policy")
	}
	h.autoconnect = true
	setters := h.policyChanges()
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err == nil {
		t.Fatal("foreign policy drift after successful add was overwritten")
	}
	if _, _, deletes := h.mutations(); deletes != 0 || h.policyChanges() != setters || !h.autoconnect {
		t.Fatal("policy drift hold mutated state")
	}
	if len(h.profiles) != 1 || len(h.native) != 1 {
		t.Fatal("policy drift hold removed configuration")
	}
}

func TestFabricNMMockedCompletedCleanupDoesNotReclaimPolicy(t *testing.T) {
	h := fabricNMTestNew(t)
	receipt := h.add(t)
	if err := fabricNMRemove(context.Background(), h.iface, receipt, h.io()); err != nil {
		t.Fatal(err)
	}
	completed := h.latestReceipt()
	if !completed.NM.CleanupConfirmed {
		t.Fatal("completed cleanup was not durably journaled")
	}
	h.autoconnect = false
	setters := h.policyChanges()
	// Completed cleanup no longer owns later policy choices, regardless of
	// whether the adapter reports a read-only hold or an idempotent success.
	_ = fabricNMRemove(context.Background(), h.iface, completed, h.io())
	if _, _, deletes := h.mutations(); deletes != 1 || h.policyChanges() != setters || h.autoconnect {
		t.Fatal("completed cleanup reclaimed later foreign policy")
	}
}

func TestFabricNMMockedPartialActivationRecoversManualPolicyReset(t *testing.T) {
	h := fabricNMTestNew(t)
	h.activateErr = errors.New("mock activation response lost after applying the connection")
	if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err == nil {
		t.Fatal("uncertain activation reported success")
	}
	partial := h.latestReceipt()
	if partial.NM.PolicyEstablished || partial.NM.CleanupConfirmed || partial.NM.SettingsPath == "" || !h.autoconnect || h.deviceUUID != partial.NM.UUID {
		t.Fatal("partial activation journal or manual policy reset fixture incorrect")
	}
	h.completeActivation()
	if err := fabricNMRemove(context.Background(), h.iface, partial, h.io()); err != nil {
		t.Fatal(err)
	}
	if !h.latestReceipt().NM.CleanupConfirmed || !h.autoconnect || len(h.profiles) != 0 || len(h.native) != 0 {
		t.Fatal("partial activation did not restore original policy and confirm cleanup")
	}
	if c, a, d := h.mutations(); c != 1 || a != 1 || d != 1 {
		t.Fatal("partial activation cleanup affected the wrong operation")
	}
}
