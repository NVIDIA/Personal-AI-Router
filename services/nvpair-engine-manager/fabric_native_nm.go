// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

const fabricNMService = "org.freedesktop.NetworkManager"
const fabricNMRoot = "/org/freedesktop/NetworkManager"
const fabricNMSettings = "org.freedesktop.NetworkManager.Settings.Connection"

type fabricNMRun func(context.Context, string, ...string) ([]byte, error)

type fabricNativeReceipt struct {
	Operation string           `json:"operation"`
	Interface fabricInterface  `json:"interface"`
	Label     string           `json:"label"`
	BootID    string           `json:"bootId"`
	NM        *fabricNMReceipt `json:"networkManager,omitempty"`
}

type fabricNMReceipt struct {
	UUID                    string                   `json:"uuid"`
	Owner                   string                   `json:"owner"`
	BusID                   string                   `json:"busId"`
	DevicePath              string                   `json:"devicePath"`
	PermanentMAC            string                   `json:"permanentMAC"`
	SettingsPath            string                   `json:"settingsPath,omitempty"`
	ProfileDigest           string                   `json:"profileDigest,omitempty"`
	OriginalAutoconnect     *bool                    `json:"originalAutoconnect"`
	BaselineProfiles        []fabricNMProfileBinding `json:"baselineProfiles"`
	PolicyEstablished       bool                     `json:"policyEstablished,omitempty"`
	CleanupConfirmed        bool                     `json:"cleanupConfirmed,omitempty"`
	GeneratedPauseAttempted bool                     `json:"generatedPauseAttempted,omitempty"`
	GeneratedPausePath      string                   `json:"generatedPausePath,omitempty"`
	GeneratedPauseConfirmed bool                     `json:"generatedPauseConfirmed,omitempty"`
}

type fabricNMProfileBinding struct {
	UUID      string `json:"uuid"`
	Path      string `json:"path"`
	Filename  string `json:"filename"`
	Digest    string `json:"digest"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

type fabricNMDevice struct {
	Autoconnect                                     bool
	Name, MAC, PermanentMAC, Path, UUID, ActivePath string
	Managed                                         bool
	State                                           int
	Addresses, Gateways, DNS                        []string
}

type fabricNMProfile struct {
	Flags                                          uint32
	FlagsKnown                                     bool
	UUID, Type, Path, ActivePath, Device, Filename string
	Active                                         bool
	Fields                                         map[string]string
}

type fabricNMSnapshot struct {
	Owner    string
	BusID    string
	Devices  map[string]fabricNMDevice
	Profiles []fabricNMProfile
}

// nmcli's documented terse mode escapes only colon and backslash. Reject
// unexpected framing instead of confusing a profile name with another field.
func fabricNMSplit(line string) ([]string, error) {
	var fields []string
	var value strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' {
			i++
			if i == len(line) || (line[i] != ':' && line[i] != '\\') {
				return nil, errors.New("ambiguous NetworkManager escaping")
			}
			value.WriteByte(line[i])
		} else if c == ':' {
			fields = append(fields, value.String())
			value.Reset()
		} else if c < 32 || c == 127 {
			return nil, errors.New("invalid NetworkManager output framing")
		} else {
			value.WriteByte(c)
		}
	}
	return append(fields, value.String()), nil
}

func fabricNMFields(data []byte) (map[string]string, error) {
	if len(data) > fabricNativeMaxBytes {
		return nil, errors.New("NetworkManager output exceeds bound")
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		// --escape applies only to terse tabular output. Multiline output
		// prefixes each raw value with its property name and one colon.
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \\") {
			return nil, errors.New("NetworkManager fields are ambiguous")
		}
		for i := range line {
			if line[i] < 32 || line[i] == 127 {
				return nil, errors.New("invalid NetworkManager output framing")
			}
		}
		if _, duplicate := out[name]; duplicate {
			return nil, errors.New("duplicate NetworkManager field")
		}
		if value == "--" {
			value = ""
		}
		out[name] = value
	}
	if len(out) == 0 || len(out) > 512 {
		return nil, errors.New("NetworkManager fields unavailable")
	}
	return out, nil
}

func fabricNMString(data []byte, typ string) (string, error) {
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, typ+" ") {
		return "", errors.New("unexpected NetworkManager D-Bus value")
	}
	value, err := strconv.Unquote(strings.TrimPrefix(text, typ+" "))
	if err != nil || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("invalid NetworkManager D-Bus string")
	}
	return value, nil
}

func fabricNMObject(value, kind string) bool {
	prefix := fabricNMRoot + "/" + kind + "/"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 32)
	return err == nil && n > 0
}

func fabricNMOwner(ctx context.Context, run fabricNMRun) (string, error) {
	data, err := run(ctx, "busctl", "--system", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetNameOwner", "s", fabricNMService)
	if err != nil {
		return "", errors.New("NetworkManager daemon identity unavailable")
	}
	owner, err := fabricNMString(data, "s")
	if err != nil || !strings.HasPrefix(owner, ":") || strings.ContainsAny(owner, " /\\") {
		return "", errors.New("invalid NetworkManager daemon identity")
	}
	return owner, nil
}

func fabricNMUUID(operation string, iface fabricInterface) string {
	// Standard UUIDv5 under the URL namespace; ownership also requires the exact
	// full operation/target and private random nonce-name, not UUID alone.
	namespace, _ := hex.DecodeString("6ba7b8119dad11d180b400c04fd430c8")
	name := "https://nvidia.com/nvpair/fabric/v1/" + operation + "/" + fabricNativeHash(iface)
	sum := sha1.Sum(append(namespace, []byte(name)...))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func fabricNMValidUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func fabricNMDeviceRead(ctx context.Context, run fabricNMRun, owner string, iface fabricInterface) (fabricNMDevice, error) {
	var device fabricNMDevice
	data, err := run(ctx, "nmcli", "-t", "-e", "yes", "-c", "no", "-m", "multiline", "-f", "GENERAL.DEVICE,GENERAL.HWADDR,GENERAL.NM-MANAGED,GENERAL.STATE,GENERAL.CON-UUID,GENERAL.CON-PATH,IP4.ADDRESS,IP4.GATEWAY,IP4.DNS,IP6.ADDRESS,IP6.GATEWAY,IP6.DNS", "device", "show", iface.Name)
	if err != nil {
		return device, errors.New("NetworkManager device state unavailable")
	}
	fields, err := fabricNMFields(data)
	if err != nil {
		return device, err
	}
	for _, field := range []string{"GENERAL.DEVICE", "GENERAL.HWADDR", "GENERAL.NM-MANAGED", "GENERAL.STATE", "GENERAL.CON-UUID", "GENERAL.CON-PATH"} {
		if _, ok := fields[field]; !ok {
			return device, errors.New("NetworkManager device identity incomplete")
		}
	}
	device.Name, device.MAC, device.UUID, device.ActivePath = fields["GENERAL.DEVICE"], fields["GENERAL.HWADDR"], fields["GENERAL.CON-UUID"], fields["GENERAL.CON-PATH"]
	state := strings.Fields(fields["GENERAL.STATE"])
	if len(state) == 0 {
		return device, errors.New("NetworkManager state unavailable")
	}
	device.State, err = strconv.Atoi(state[0])
	if err != nil {
		return device, err
	}
	if fields["GENERAL.NM-MANAGED"] != "yes" && fields["GENERAL.NM-MANAGED"] != "no" {
		return device, errors.New("NetworkManager management state is unknown")
	}
	device.Managed = fields["GENERAL.NM-MANAGED"] == "yes"
	if device.Name != iface.Name || !strings.EqualFold(device.MAC, iface.MAC) {
		return device, errors.New("NetworkManager interface identity changed")
	}
	for key, value := range fields {
		if value == "" {
			continue
		}
		if strings.HasPrefix(key, "IP4.ADDRESS") || strings.HasPrefix(key, "IP6.ADDRESS") {
			device.Addresses = append(device.Addresses, value)
		}
		if strings.HasPrefix(key, "IP4.GATEWAY") || strings.HasPrefix(key, "IP6.GATEWAY") {
			device.Gateways = append(device.Gateways, value)
		}
		if strings.HasPrefix(key, "IP4.DNS") || strings.HasPrefix(key, "IP6.DNS") {
			device.DNS = append(device.DNS, value)
		}
	}
	data, err = run(ctx, "busctl", "--system", "call", owner, fabricNMRoot, fabricNMService, "GetDeviceByIpIface", "s", iface.Name)
	if err != nil {
		return device, errors.New("NetworkManager device object unavailable")
	}
	device.Path, err = fabricNMString(data, "o")
	if err != nil || !fabricNMObject(device.Path, "Devices") {
		return device, errors.New("NetworkManager device object is invalid")
	}
	data, err = run(ctx, "busctl", "--system", "get-property", owner, device.Path, "org.freedesktop.NetworkManager.Device.Wired", "PermHwAddress")
	if err != nil {
		return device, errors.New("NetworkManager permanent MAC unavailable")
	}
	device.PermanentMAC, err = fabricNMString(data, "s")
	if err != nil {
		return device, err
	}
	mac, err := net.ParseMAC(device.PermanentMAC)
	if err != nil || len(mac) != 6 || !strings.EqualFold(device.PermanentMAC, iface.MAC) {
		return device, errors.New("reviewed MAC differs from the permanent NetworkManager device identity")
	}
	data, err = run(ctx, "busctl", "--system", "get-property", owner, device.Path, "org.freedesktop.NetworkManager.Device", "Autoconnect")
	if err != nil {
		return device, errors.New("NetworkManager device autoconnect policy unavailable")
	}
	switch strings.TrimSpace(string(data)) {
	case "b true":
		device.Autoconnect = true
	case "b false":
		device.Autoconnect = false
	default:
		return device, errors.New("NetworkManager device autoconnect policy is ambiguous")
	}
	return device, nil
}

func fabricNMProfiles(ctx context.Context, run fabricNMRun) ([]fabricNMProfile, error) {
	data, err := run(ctx, "nmcli", "-t", "-e", "yes", "-c", "no", "-f", "UUID,TYPE,DBUS-PATH,ACTIVE,DEVICE,ACTIVE-PATH,FILENAME", "connection", "show")
	if err != nil || len(data) > fabricNativeMaxBytes {
		return nil, errors.New("complete NetworkManager profile inventory unavailable")
	}
	var profiles []fabricNMProfile
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		values, err := fabricNMSplit(line)
		if err != nil || len(values) != 7 || !fabricNMValidUUID(values[0]) || !fabricNMObject(values[2], "Settings") || seen[values[0]] || len(profiles) >= 32 {
			return nil, errors.New("ambiguous or oversized NetworkManager profile inventory")
		}
		seen[values[0]] = true
		for i := range values {
			if values[i] == "--" {
				values[i] = ""
			}
		}
		if values[3] != "yes" && values[3] != "no" {
			return nil, errors.New("unknown NetworkManager profile activity")
		}
		p := fabricNMProfile{UUID: values[0], Type: values[1], Path: values[2], Active: values[3] == "yes", Device: values[4], ActivePath: values[5], Filename: values[6]}
		body, err := run(ctx, "nmcli", "-t", "-e", "yes", "-c", "no", "-m", "multiline", "-f", "profile", "connection", "show", "path", p.Path)
		if err != nil {
			return nil, errors.New("cold NetworkManager profile settings unavailable")
		}
		p.Fields, err = fabricNMFields(body)
		if err != nil || p.Fields["connection.uuid"] != p.UUID {
			return nil, errors.New("NetworkManager profile identity changed")
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func fabricNMRead(ctx context.Context, run fabricNMRun, targets []fabricInterface) (fabricNMSnapshot, error) {
	snapshot := fabricNMSnapshot{Devices: map[string]fabricNMDevice{}}
	// The lower read helpers deliberately replace external command errors.
	// Retain the first fixed inspection category before that replacement while
	// passing their original results through unchanged.
	stage := "network-manager-bus-unavailable"
	var queryFailure error
	query := func(ctx context.Context, command string, args ...string) ([]byte, error) {
		data, err := run(ctx, command, args...)
		if err != nil && queryFailure == nil {
			queryFailure = fabricInspectionError(stage, err)
		}
		return data, err
	}
	failure := func(code string, err error) error {
		if queryFailure != nil {
			return queryFailure
		}
		return fabricInspectionError(code, err)
	}
	var err error
	snapshot.BusID, err = fabricNMBusID(ctx, query)
	if err != nil {
		return snapshot, failure(stage, err)
	}
	stage = "network-manager-owner-unavailable"
	snapshot.Owner, err = fabricNMOwner(ctx, query)
	if err != nil {
		return snapshot, failure(stage, err)
	}
	stage = "network-manager-device-unavailable"
	for _, iface := range targets {
		device, err := fabricNMDeviceRead(ctx, query, snapshot.Owner, iface)
		if err != nil {
			return snapshot, failure(stage, err)
		}
		snapshot.Devices[iface.Name] = device
	}
	stage = "network-manager-profiles-unavailable"
	snapshot.Profiles, err = fabricNMProfiles(ctx, query)
	if err != nil {
		return snapshot, failure(stage, err)
	}
	for i := range snapshot.Profiles {
		p := &snapshot.Profiles[i]
		if fabricNMProfileUnrelated(*p, targets) {
			continue
		}
		data, err := query(ctx, "busctl", "--system", "get-property", snapshot.Owner, p.Path, fabricNMSettings, "Flags")
		if err != nil {
			return snapshot, failure(stage, err)
		}
		value := strings.TrimSpace(string(data))
		if !strings.HasPrefix(value, "u ") {
			return snapshot, failure(stage, errors.New("matching NetworkManager profile flags invalid"))
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(value, "u "), 10, 32)
		if err != nil {
			return snapshot, failure(stage, err)
		}
		p.Flags, p.FlagsKnown = uint32(flags), true
	}
	stage = "network-manager-owner-unavailable"
	after, err := fabricNMOwner(ctx, query)
	if err != nil {
		return snapshot, failure(stage, err)
	}
	if after != snapshot.Owner {
		return snapshot, failure("network-manager-generation-changed", errors.New("NetworkManager restarted during inventory"))
	}
	stage = "network-manager-bus-unavailable"
	busAfter, err := fabricNMBusID(ctx, query)
	if err != nil {
		return snapshot, failure(stage, err)
	}
	if busAfter != snapshot.BusID {
		return snapshot, failure("network-manager-generation-changed", errors.New("system bus restarted during NetworkManager inventory"))
	}
	return snapshot, nil
}

// A snapshot takes many separate reads, and NetworkManager replaces a failed
// generated DHCP attempt on each unconfigured port about every 45 seconds, so
// one read can straddle a replacement. Re-read a bounded number of times until
// every target's generated binding is self-consistent; callers still refuse
// whatever the last snapshot shows.
func fabricNMReadSettled(ctx context.Context, run fabricNMRun, targets []fabricInterface) (fabricNMSnapshot, error) {
	for attempt := 1; ; attempt++ {
		snapshot, err := fabricNMRead(ctx, run, targets)
		if err != nil {
			return snapshot, err
		}
		settled := true
		for _, iface := range targets {
			if _, candidateErr := fabricNMGeneratedCandidate(snapshot, iface); candidateErr != nil {
				settled = false
			}
		}
		if settled || attempt == 5 {
			return snapshot, nil
		}
		if err := fabricNMWait(ctx); err != nil {
			return snapshot, err
		}
	}
}

func fabricNMProfileUnrelated(profile fabricNMProfile, targets []fabricInterface) bool {
	for _, target := range targets {
		for _, value := range profile.Fields {
			if strings.Contains(strings.ToLower(value), strings.ToLower(target.Name)) || strings.Contains(strings.ToLower(value), strings.ToLower(target.MAC)) {
				return false
			}
		}
		if profile.Device == target.Name {
			return false
		}
	}
	// A full literal interface binding proves disjointness even for unsupported
	// connection types. Otherwise only hardware-incompatible types are admitted.
	name := profile.Fields["connection.interface-name"]
	if name != "" && !strings.ContainsAny(name, "*?[]!&|\\ ") {
		return true
	}
	for key, value := range profile.Fields {
		if strings.HasPrefix(key, "match.") && value != "" {
			return false
		}
	}
	if profile.Type == "802-11-wireless" || profile.Type == "wifi" || profile.Type == "vpn" || profile.Type == "loopback" {
		return true
	}
	mac := profile.Fields["802-3-ethernet.mac-address"]
	parsed, err := net.ParseMAC(mac)
	return err == nil && len(parsed) == 6
}

func fabricNMPreflight(snapshot fabricNMSnapshot, targets []fabricInterface, ownedUUID string) []string {
	var blockers []string
	needed := 0
	for _, iface := range targets {
		device, exists := snapshot.Devices[iface.Name]
		if !exists || device.Name != iface.Name || !strings.EqualFold(device.MAC, iface.MAC) {
			blockers = append(blockers, "NetworkManager device identity unavailable")
			continue
		}
		pending := false
		for _, profile := range snapshot.Profiles {
			if profile.UUID == device.UUID && profile.Active && (fabricNMCompatibleProfile(profile, iface, device) || fabricNMGeneratedPending(snapshot, iface, profile)) {
				pending = device.State == 70
			}
		}
		generatedLL := validFabricGeneratedDefault(iface.GeneratedDefault, iface) && fabricNMGeneratedDevice(snapshot, iface) && fabricLinkLocalBaseline(append([]string{}, device.Addresses...))
		if (!pending && (device.UUID != "" || device.ActivePath != "")) || (len(device.Addresses) != 0 && !generatedLL) || len(device.DNS) != 0 || len(device.Gateways) != 0 {
			blockers = append(blockers, "selected interface has active NetworkManager addressing or ownership: "+iface.Name)
		}
		if (device.Managed && device.State != 30 && !pending) || (!device.Managed && device.State != 10) {
			blockers = append(blockers, "selected NetworkManager interface is not disconnected or explicitly unmanaged: "+iface.Name)
		}
		if device.Managed {
			needed++
		}
	}
	for _, profile := range snapshot.Profiles {
		if profile.UUID == ownedUUID {
			needed--
		}
	}
	if len(snapshot.Profiles)+max(needed, 0) > 32 {
		blockers = append(blockers, "NetworkManager profile inventory has no bounded capacity for every selected interface")
	}
	for _, profile := range snapshot.Profiles {
		if profile.UUID != ownedUUID && !fabricNMProfileUnrelated(profile, targets) {
			compatible := false
			for _, iface := range targets {
				compatible = compatible || fabricNMCompatibleProfile(profile, iface, snapshot.Devices[iface.Name]) || fabricNMGeneratedPending(snapshot, iface, profile)
			}
			if !compatible {
				blockers = append(blockers, "existing NetworkManager profile may own or depend on the selected interface: "+profile.UUID)
			}
		}
	}
	return fabricSorted(blockers)
}

func fabricNMGeneratedDevice(snapshot fabricNMSnapshot, iface fabricInterface) bool {
	d := iface.GeneratedDefault
	device, ok := snapshot.Devices[iface.Name]
	return validFabricGeneratedDefault(d, iface) && ok && device.Managed && snapshot.Owner == d.Owner && snapshot.BusID == d.BusID &&
		device.Path == d.DevicePath && device.Name == d.InterfaceName && strings.EqualFold(device.PermanentMAC, d.PermanentMAC) && strings.EqualFold(device.MAC, iface.MAC)
}

func fabricNMGeneratedPending(snapshot fabricNMSnapshot, iface fabricInterface, profile fabricNMProfile) bool {
	if !fabricNMGeneratedDevice(snapshot, iface) {
		return false
	}
	d, device := iface.GeneratedDefault, snapshot.Devices[iface.Name]
	return d.ActivePath != "" && device.State == 70 && device.UUID == d.UUID && device.ActivePath == d.ActivePath &&
		profile.UUID == d.UUID && profile.Path == d.SettingsPath && profile.Active && profile.ActivePath == d.ActivePath && profile.Device == iface.Name &&
		profile.Fields["connection.id"] == d.Name && fabricNMProfileDigest(profile.Fields) == d.ProfileDigest &&
		fabricNMEphemeralProfile(profile, iface, device) && fabricLinkLocalBaseline(append([]string{}, device.Addresses...)) && len(device.DNS) == 0 && len(device.Gateways) == 0
}

// This is discovery, not permission. Only the worker's separately approved
// request can carry the returned descriptor into nativeAdd.
func fabricNMGeneratedCandidate(snapshot fabricNMSnapshot, iface fabricInterface) (*fabricGeneratedDefault, error) {
	device, ok := snapshot.Devices[iface.Name]
	if !ok || !device.Managed {
		return nil, nil
	}
	var found *fabricNMProfile
	for i := range snapshot.Profiles {
		profile := &snapshot.Profiles[i]
		if !fabricNMEphemeralProfile(*profile, iface, device) {
			continue
		}
		if found != nil {
			return nil, errors.New("generated default profile is ambiguous")
		}
		found = profile
	}
	if found == nil {
		return nil, nil
	}
	if device.State != 30 && device.State != 70 || len(device.DNS) != 0 || len(device.Gateways) != 0 || !fabricLinkLocalBaseline(append([]string{}, device.Addresses...)) {
		return nil, errors.New("generated default has usable or unsupported configuration")
	}
	if device.State == 30 && (device.UUID != "" || device.ActivePath != "" || found.Active || found.ActivePath != "" || found.Device != "") ||
		device.State == 70 && (device.UUID != found.UUID || device.ActivePath == "" || !found.Active || found.ActivePath != device.ActivePath || found.Device != iface.Name) {
		return nil, errors.New("generated default activation binding is ambiguous")
	}
	original := device.Autoconnect
	d := &fabricGeneratedDefault{SchemaVersion: 1, InterfaceName: iface.Name, Index: iface.Index, Owner: snapshot.Owner, BusID: snapshot.BusID,
		DevicePath: device.Path, PermanentMAC: device.PermanentMAC, OriginalAutoconnect: &original, UUID: found.UUID, Name: found.Fields["connection.id"],
		SettingsPath: found.Path, ProfileDigest: fabricNMProfileDigest(found.Fields), ActivePath: device.ActivePath}
	if !validFabricGeneratedDefault(d, iface) {
		return nil, errors.New("generated default descriptor is incomplete")
	}
	return d, nil
}

func fabricNMGeneratedSame(want, got *fabricGeneratedDefault) bool {
	if !fabricNMGeneratedStableSame(want, got) {
		return false
	}
	return want.ActivePath == got.ActivePath || got.ActivePath == ""
}

func fabricNMGeneratedStableSame(want, got *fabricGeneratedDefault) bool {
	if want == nil || got == nil || want.OriginalAutoconnect == nil || got.OriginalAutoconnect == nil || *want.OriginalAutoconnect != *got.OriginalAutoconnect {
		return false
	}
	a, b := *want, *got
	a.OriginalAutoconnect = b.OriginalAutoconnect
	a.ActivePath, b.ActivePath = "", ""
	return a == b
}

func fabricNMGeneratedAttempt(iface fabricInterface, activePath string) fabricInterface {
	current := *iface.GeneratedDefault
	current.ActivePath = activePath
	iface.GeneratedDefault = &current
	return iface
}

func fabricNMBusJSON(data []byte, signature string, out any) error {
	var wire struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) > fabricNativeMaxBytes || !fabricNMUniqueJSON(data) || decoder.Decode(&wire) != nil || wire.Type != signature || len(wire.Data) == 0 || bytes.Equal(wire.Data, []byte("null")) || decoder.Decode(new(any)) != io.EOF || json.Unmarshal(wire.Data, out) != nil {
		return errors.New("bounded NetworkManager typed response is unavailable")
	}
	return nil
}

func fabricNMUniqueJSON(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	remaining := 65536
	var value func(int) bool
	value = func(depth int) bool {
		remaining--
		if depth > 16 || remaining < 0 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		opening, nested := token.(json.Delim)
		if !nested {
			return true
		}
		if opening != '{' && opening != '[' {
			return false
		}
		seen := map[string]bool{}
		for decoder.More() {
			if opening == '{' {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
			}
			if !value(depth + 1) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && (opening == '{' && closing == json.Delim('}') || opening == '[' && closing == json.Delim(']'))
	}
	return value(0) && decoder.Decode(new(any)) == io.EOF
}

func fabricNMProperty(ctx context.Context, run fabricNMRun, owner, object, api, property, signature string, out any) error {
	data, err := run(ctx, "busctl", "--json=short", "--system", "get-property", owner, object, api, property)
	if err != nil {
		return errors.New("NetworkManager object proof is unavailable")
	}
	return fabricNMBusJSON(data, signature, out)
}

type fabricNMBusVariant struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// busctl json_transform_message emits a signature and an array of method
// results; dictionary variants retain their type/data wrapper. No secret API
// is called and none of these settings are persisted or returned to the UI.
func fabricNMSettingsReply(data []byte, applied bool) (map[string]map[string]fabricNMBusVariant, uint64, error) {
	invalid := errors.New("generated applied-settings proof is incomplete")
	signature, count := "a{sa{sv}}", 1
	if applied {
		signature, count = "a{sa{sv}}t", 2
	}
	var values []json.RawMessage
	if fabricNMBusJSON(data, signature, &values) != nil || len(values) != count {
		return nil, 0, invalid
	}
	var version uint64
	if applied && (json.Unmarshal(values[1], &version) != nil || version == 0) {
		return nil, 0, invalid
	}
	var settings map[string]map[string]fabricNMBusVariant
	decoder := json.NewDecoder(bytes.NewReader(values[0]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&settings) != nil || len(settings) < 3 || len(settings) > 5 {
		return nil, 0, invalid
	}
	fields := 0
	for section, properties := range settings {
		if section != "connection" && section != "802-3-ethernet" && section != "ipv4" && section != "ipv6" && section != "proxy" {
			return nil, 0, invalid
		}
		if properties == nil {
			return nil, 0, invalid
		}
		for key, value := range properties {
			fields++
			if !cableIdentifier(key, 128) || value.Type == "" || len(value.Type) > 64 || len(value.Data) == 0 || bytes.Equal(value.Data, []byte("null")) {
				return nil, 0, invalid
			}
		}
	}
	if fields > 512 {
		return nil, 0, invalid
	}
	delete(settings["connection"], "timestamp") // NM's documented activation bookkeeping.
	return settings, version, nil
}

func fabricNMCanonicalSettings(settings map[string]map[string]fabricNMBusVariant) any {
	data, _ := json.Marshal(settings)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	return value
}

func fabricNMAppliedProof(ctx context.Context, run fabricNMRun, owner string, d *fabricGeneratedDefault) error {
	var prior any
	var priorVersion uint64
	for pass := 0; pass < 2; pass++ {
		savedData, err := run(ctx, "busctl", "--json=short", "--system", "call", owner, d.SettingsPath, fabricNMSettings, "GetSettings")
		if err != nil {
			return errors.New("generated settings snapshot is unavailable")
		}
		appliedData, err := run(ctx, "busctl", "--json=short", "--system", "call", owner, d.DevicePath, "org.freedesktop.NetworkManager.Device", "GetAppliedConnection", "u", "0")
		if err != nil {
			return errors.New("generated applied snapshot is unavailable")
		}
		saved, _, err := fabricNMSettingsReply(savedData, false)
		if err != nil {
			return err
		}
		applied, version, err := fabricNMSettingsReply(appliedData, true)
		if err != nil {
			return err
		}
		for key, want := range map[string]string{"uuid": d.UUID, "id": d.Name, "interface-name": d.InterfaceName} {
			field, ok := saved["connection"][key]
			var value string
			if !ok || field.Type != "s" || json.Unmarshal(field.Data, &value) != nil || value != want {
				return errors.New("generated settings identity changed")
			}
		}
		current := fabricNMCanonicalSettings(saved)
		if !reflect.DeepEqual(current, fabricNMCanonicalSettings(applied)) || pass != 0 && (!reflect.DeepEqual(prior, current) || version != priorVersion) {
			return errors.New("generated applied settings differ or changed during proof")
		}
		prior, priorVersion = current, version
	}
	data, err := run(ctx, "nmcli", "-t", "-e", "yes", "-c", "no", "-m", "multiline", "-f", "profile", "connection", "show", "path", d.SettingsPath)
	if err != nil {
		return errors.New("generated profile digest recheck unavailable")
	}
	fields, err := fabricNMFields(data)
	if err != nil || fabricNMProfileDigest(fields) != d.ProfileDigest {
		return errors.New("generated profile changed during applied proof")
	}
	return nil
}

func fabricNMGeneratedProof(ctx context.Context, run fabricNMRun, snapshot fabricNMSnapshot, iface fabricInterface, afterPause bool) error {
	if !fabricNMGeneratedDevice(snapshot, iface) {
		return errors.New("generated default proof identity changed")
	}
	d, device := iface.GeneratedDefault, snapshot.Devices[iface.Name]
	var paths []string
	if err := fabricNMProperty(ctx, run, snapshot.Owner, fabricNMRoot, fabricNMService, "ActiveConnections", "ao", &paths); err != nil {
		return err
	}
	if paths == nil || len(paths) > 32 {
		return errors.New("active connection coverage exceeds its bound")
	}
	api := "org.freedesktop.NetworkManager.Connection.Active"
	seen, matched, relevant := map[string]bool{}, 0, []string{}
	for _, active := range paths {
		if !fabricNMObject(active, "ActiveConnection") || seen[active] {
			return errors.New("active connection coverage is ambiguous")
		}
		seen[active] = true
		var devices []string
		var settings string
		if err := fabricNMProperty(ctx, run, snapshot.Owner, active, api, "Devices", "ao", &devices); err != nil {
			return err
		}
		if devices == nil || len(devices) > 128 {
			return errors.New("active connection device coverage is unavailable")
		}
		if err := fabricNMProperty(ctx, run, snapshot.Owner, active, api, "Connection", "o", &settings); err != nil {
			return err
		}
		if !slices.Contains(devices, d.DevicePath) && settings != d.SettingsPath {
			continue
		}
		matched++
		relevant = append(relevant, active)
		if afterPause || matched != 1 || len(devices) != 1 || devices[0] != d.DevicePath || settings != d.SettingsPath || active != d.ActivePath || device.State != 70 || device.UUID != d.UUID || device.ActivePath != active {
			return errors.New("another or replaced activation overlaps the approved generated default")
		}
		var uuid, master string
		var state uint32
		var default4, default6 bool
		for _, field := range []struct {
			name, signature string
			out             any
		}{{"Uuid", "s", &uuid}, {"Master", "o", &master}, {"State", "u", &state}, {"Default", "b", &default4}, {"Default6", "b", &default6}} {
			if err := fabricNMProperty(ctx, run, snapshot.Owner, active, api, field.name, field.signature, field.out); err != nil {
				return err
			}
		}
		if uuid != d.UUID || master != "/" || state != 1 || default4 || default6 {
			return errors.New("generated activation has usable or shared ownership")
		}
		if err := fabricNMAppliedProof(ctx, run, snapshot.Owner, d); err != nil {
			return err
		}
	}
	if device.State == 70 && matched != 1 || device.State == 30 && (matched != 0 || device.UUID != "" || device.ActivePath != "") || device.State != 30 && device.State != 70 {
		return errors.New("generated activation coverage does not match the native device")
	}
	var after []string
	var state uint32
	var active string
	if err := fabricNMProperty(ctx, run, snapshot.Owner, fabricNMRoot, fabricNMService, "ActiveConnections", "ao", &after); err != nil {
		return err
	}
	if after == nil || len(after) > 32 {
		return errors.New("active connection coverage recheck is unavailable")
	}
	afterSeen := map[string]bool{}
	for _, path := range after {
		if !fabricNMObject(path, "ActiveConnection") || afterSeen[path] {
			return errors.New("active connection coverage recheck is ambiguous")
		}
		afterSeen[path] = true
	}
	// Other ports' generated DHCP attempts come and go during this proof. Only a
	// change touching the approved device or profile invalidates it, and the
	// device recheck below binds the approved device itself.
	for _, path := range relevant {
		if !afterSeen[path] {
			return errors.New("active connection coverage changed during proof")
		}
	}
	for _, path := range after {
		if seen[path] {
			continue
		}
		var devices []string
		var settings string
		if fabricNMProperty(ctx, run, snapshot.Owner, path, api, "Devices", "ao", &devices) != nil || fabricNMProperty(ctx, run, snapshot.Owner, path, api, "Connection", "o", &settings) != nil {
			continue
		}
		if slices.Contains(devices, d.DevicePath) || settings == d.SettingsPath {
			return errors.New("active connection coverage changed during proof")
		}
	}
	if err := fabricNMProperty(ctx, run, snapshot.Owner, d.DevicePath, "org.freedesktop.NetworkManager.Device", "State", "u", &state); err != nil {
		return err
	}
	if err := fabricNMProperty(ctx, run, snapshot.Owner, d.DevicePath, "org.freedesktop.NetworkManager.Device", "ActiveConnection", "o", &active); err != nil {
		return err
	}
	wantActive := device.ActivePath
	if wantActive == "" {
		wantActive = "/"
	}
	if int(state) != device.State || active != wantActive {
		return errors.New("generated device changed during proof")
	}
	return fabricNMSameInstance(ctx, run, &fabricNMReceipt{Owner: snapshot.Owner, BusID: snapshot.BusID})
}

func fabricNMCompatibleProfile(profile fabricNMProfile, iface fabricInterface, device fabricNMDevice) bool {
	if !device.Managed || !profile.FlagsKnown || (profile.Flags > 1 && !fabricNMEphemeralProfile(profile, iface, device)) || (profile.Type != "802-3-ethernet" && profile.Type != "ethernet") || profile.Fields["connection.interface-name"] != iface.Name {
		return false
	}
	mac := profile.Fields["802-3-ethernet.mac-address"]
	if mac != "" && !strings.EqualFold(mac, device.PermanentMAC) {
		return false
	}
	cloned := profile.Fields["802-3-ethernet.cloned-mac-address"]
	if cloned != "" && cloned != "preserve" && cloned != "permanent" && !strings.EqualFold(cloned, iface.MAC) {
		return false
	}
	for key, value := range profile.Fields {
		if value == "" {
			continue
		}
		if !strings.HasPrefix(key, "connection.") && !strings.HasPrefix(key, "802-3-ethernet.") && !strings.HasPrefix(key, "ipv4.") && !strings.HasPrefix(key, "ipv6.") && !strings.HasPrefix(key, "proxy.") && !strings.HasPrefix(key, "match.") {
			return false
		}
		if strings.HasPrefix(key, "match.") || strings.HasSuffix(key, ".parent") || strings.HasSuffix(key, ".master") || strings.HasSuffix(key, ".controller") || key == "connection.port-type" || key == "connection.slave-type" || key == "connection.secondaries" {
			return false
		}
	}
	if profile.Active {
		return device.State == 70 && device.UUID == profile.UUID && device.ActivePath == profile.ActivePath && profile.Device == iface.Name && len(device.Addresses) == 0 && len(device.DNS) == 0 && len(device.Gateways) == 0
	}
	return profile.Device == "" && profile.ActivePath == ""
}

// NM-generated+unsaved is a daemon placeholder, not a user-owned connection.
// It may retire or regenerate when NM gains/loses a real profile. Do not claim
// its object/UUID survives and never explicitly modify or delete it.
func fabricNMEphemeralProfile(profile fabricNMProfile, iface fabricInterface, device fabricNMDevice) bool {
	if !profile.FlagsKnown || profile.Flags != 3 {
		return false
	}
	if profile.Filename != "" && (path.Dir(profile.Filename) != "/run/NetworkManager/system-connections" || path.Clean(profile.Filename) != profile.Filename) {
		return false
	}
	shape := profile
	shape.Flags = 1
	shape.Active = false
	shape.ActivePath, shape.Device = "", ""
	if !fabricNMCompatibleProfile(shape, iface, device) {
		return false
	}
	if profile.Fields["ipv4.method"] != "auto" {
		return false
	}
	switch profile.Fields["ipv6.method"] {
	case "auto", "disabled", "link-local":
	default:
		return false
	}
	for _, key := range []string{"ipv4.addresses", "ipv6.addresses", "ipv4.address-data", "ipv6.address-data", "ipv4.gateway", "ipv6.gateway", "ipv4.dns", "ipv6.dns", "ipv4.dns-search", "ipv6.dns-search", "ipv4.routes", "ipv6.routes", "ipv4.route-data", "ipv6.route-data", "ipv4.routing-rules", "ipv6.routing-rules", "connection.zone"} {
		if profile.Fields[key] != "" {
			return false
		}
	}
	return fabricNMProxyDisabled(profile.Fields)
}

func fabricNMProxyDisabled(fields map[string]string) bool {
	for key, value := range fields {
		if value == "" || !strings.HasPrefix(key, "proxy.") {
			continue
		}
		switch key {
		case "proxy.method":
			if value != "none" && fabricNMNumber(value) != "0" {
				return false
			}
		case "proxy.browser-only":
			if value != "no" && value != "false" {
				return false
			}
		default:
			return false // PAC content/URLs and unknown active settings hold.
		}
	}
	return true
}

func fabricNMBaseline(snapshot fabricNMSnapshot, iface fabricInterface, ownedUUID string) ([]fabricNMProfileBinding, error) {
	bindings := []fabricNMProfileBinding{}
	for _, profile := range snapshot.Profiles {
		if profile.UUID == ownedUUID || fabricNMProfileUnrelated(profile, []fabricInterface{iface}) {
			continue
		}
		if !fabricNMCompatibleProfile(profile, iface, snapshot.Devices[iface.Name]) && !fabricNMGeneratedPending(snapshot, iface, profile) {
			return nil, errors.New("matching NetworkManager profile is not an ordinary direct Ethernet configuration")
		}
		bindings = append(bindings, fabricNMProfileBinding{UUID: profile.UUID, Path: profile.Path, Filename: profile.Filename, Digest: fabricNMProfileDigest(profile.Fields), Ephemeral: fabricNMEphemeralProfile(profile, iface, snapshot.Devices[iface.Name])})
	}
	slices.SortFunc(bindings, func(a, b fabricNMProfileBinding) int { return strings.Compare(a.UUID, b.UUID) })
	return bindings, nil
}

func fabricNMBaselineCurrent(snapshot fabricNMSnapshot, iface fabricInterface, receipt fabricNativeReceipt) error {
	if receipt.NM == nil || receipt.NM.OriginalAutoconnect == nil || receipt.NM.BaselineProfiles == nil {
		return errors.New("original NetworkManager policy or profile bindings unavailable")
	}
	seen := map[string]bool{}
	ephemeralAdmitted, ordinaryCount := false, 0
	for _, binding := range receipt.NM.BaselineProfiles {
		if binding.Ephemeral {
			ephemeralAdmitted = true
		} else {
			ordinaryCount++
		}
	}
	for _, profile := range snapshot.Profiles {
		if profile.UUID == receipt.NM.UUID || fabricNMProfileUnrelated(profile, []fabricInterface{iface}) {
			continue
		}
		if ephemeralAdmitted && fabricNMEphemeralProfile(profile, iface, snapshot.Devices[iface.Name]) {
			continue
		}
		found := false
		for _, binding := range receipt.NM.BaselineProfiles {
			if !binding.Ephemeral && profile.UUID == binding.UUID {
				found = profile.Path == binding.Path && profile.Filename == binding.Filename && profile.FlagsKnown && profile.Flags <= 1 && fabricNMProfileDigest(profile.Fields) == binding.Digest
			}
		}
		if !found {
			return errors.New("preserved NetworkManager profile changed or a new matching profile appeared")
		}
		seen[profile.UUID] = true
	}
	if len(seen) != ordinaryCount {
		return errors.New("preserved NetworkManager profile disappeared")
	}
	return nil
}

func fabricNMAddArgs(iface fabricInterface, receipt fabricNativeReceipt) []string {
	return []string{"--wait", "2", "connection", "add", "save", "no", "type", "ethernet", "ifname", iface.Name,
		"con-name", "nvpair-fabric-" + receipt.Label, "connection.uuid", receipt.NM.UUID, "connection.autoconnect", "no", "connection.multi-connect", "1",
		"connection.llmnr", "no", "connection.mdns", "no", "connection.lldp", "disable",
		"802-3-ethernet.mac-address", receipt.NM.PermanentMAC, "802-3-ethernet.cloned-mac-address", "preserve", "802-3-ethernet.mtu", strconv.Itoa(iface.MTU),
		"802-3-ethernet.auto-negotiate", "no", "802-3-ethernet.speed", "0", "802-3-ethernet.duplex", "", "802-3-ethernet.wake-on-lan", "ignore",
		"ipv4.method", "manual", "ipv4.addresses", iface.Address, "ipv4.never-default", "yes", "ipv4.ignore-auto-dns", "yes", "ipv4.ignore-auto-routes", "yes",
		"ipv4.gateway", "", "ipv4.dns", "", "ipv4.dns-search", "", "ipv4.routes", fabricNMRoutes(iface), "ipv4.route-table", "254", "ipv4.dad-timeout", "1000", "ipv4.may-fail", "no", "ipv4.link-local", "disabled",
		"ipv6.method", "disabled", "ipv6.never-default", "yes", "ipv6.ignore-auto-dns", "yes"}
}

// nmcli's terse profile readback renders ipv4.routes in this same
// "destination gateway" form it accepts, joined by ", ".
func fabricNMRoutes(iface fabricInterface) string {
	routes := make([]string, 0, len(iface.Routes))
	for _, route := range iface.Routes {
		routes = append(routes, route.Destination+" "+route.Gateway)
	}
	return strings.Join(routes, ", ")
}

// The kernel's main-table IPv4 routes through one interface. Local-table rows
// are kernel-owned per-address state; any other table using it holds.
func fabricNMKernelRoutes(ctx context.Context, run fabricNMRun, iface fabricInterface) ([]fabricNativeRoute, error) {
	data, err := run(ctx, "ip", "-j", "-4", "route", "show", "table", "all")
	if err != nil {
		return nil, errors.New("kernel route readback unavailable")
	}
	rows, _, err := fabricParseRoutes(data)
	if err != nil {
		return nil, err
	}
	var selected []fabricNativeRoute
	for _, row := range rows {
		uses := row.Dev == iface.Name
		for _, hop := range row.Nexthops {
			uses = uses || hop.Dev == iface.Name
		}
		if !uses {
			continue
		}
		switch strings.Trim(string(row.Table), `"`) {
		case "local", "255":
		case "", "main", "254":
			selected = append(selected, row)
		default:
			return nil, errors.New("another routing table uses the owned fabric interface")
		}
	}
	return selected, nil
}

// Beyond its connected prefix, the owned interface carries exactly the
// reviewed host routes, each once and through its reviewed gateway.
func fabricNMRoutesExact(rows []fabricNativeRoute, iface fabricInterface) bool {
	prefix, err := fabricNativePrefix(iface)
	if err != nil {
		return false
	}
	pending := map[string]string{}
	for _, route := range iface.Routes {
		pending[route.Destination] = route.Gateway
	}
	for _, row := range rows {
		destination, ok := fabricRouteDestination(row.Dst)
		if !ok || len(row.Nexthops) != 0 || row.Type != "" && row.Type != "unicast" {
			return false
		}
		if row.Gateway == "" && destination == prefix.Masked() {
			continue
		}
		gateway, reviewed := pending[destination.String()]
		if !reviewed || gateway != row.Gateway {
			return false
		}
		delete(pending, destination.String())
	}
	return len(pending) == 0
}

func fabricNMRoutesAbsent(rows []fabricNativeRoute, iface fabricInterface) bool {
	for _, row := range rows {
		destination, ok := fabricRouteDestination(row.Dst)
		for _, route := range iface.Routes {
			if ok && destination.String() == route.Destination {
				return false
			}
		}
	}
	return true
}

// Profile deletion withdraws its routes with it. Only a routed profile needs
// the extra read; an unrouted interface's absence proof is its address.
func fabricNMOwnedRoutesGone(ctx context.Context, run fabricNMRun, iface fabricInterface) (bool, error) {
	if len(iface.Routes) == 0 {
		return true, nil
	}
	rows, err := fabricNMKernelRoutes(ctx, run, iface)
	if err != nil {
		return false, err
	}
	return fabricNMRoutesAbsent(rows, iface), nil
}

func fabricNMProfileDigest(fields map[string]string) string {
	copy := make(map[string]string, len(fields))
	for key, value := range fields {
		if key != "connection.timestamp" {
			copy[key] = value
		}
	}
	return fabricNativeHash(copy)
}

func fabricNMProfileOwned(ctx context.Context, run fabricNMRun, iface fabricInterface, receipt fabricNativeReceipt, snapshot fabricNMSnapshot) (fabricNMProfile, bool, error) {
	var empty fabricNMProfile
	if receipt.NM == nil || snapshot.Owner != receipt.NM.Owner || snapshot.BusID != receipt.NM.BusID || !fabricNMValidUUID(receipt.NM.UUID) || receipt.NM.UUID != fabricNMUUID(receipt.Operation, iface) {
		return empty, false, errors.New("operation NetworkManager daemon identity changed")
	}
	var profile *fabricNMProfile
	for i := range snapshot.Profiles {
		if snapshot.Profiles[i].UUID == receipt.NM.UUID {
			profile = &snapshot.Profiles[i]
		}
	}
	if profile == nil {
		return empty, false, nil
	}
	if receipt.NM.SettingsPath != "" && profile.Path != receipt.NM.SettingsPath {
		return empty, false, errors.New("owned NetworkManager settings object was replaced")
	}
	if profile.Filename != "" && (path.Dir(profile.Filename) != "/run/NetworkManager/system-connections" || path.Clean(profile.Filename) != profile.Filename) {
		return empty, false, errors.New("owned NetworkManager profile became persistent or has unknown storage")
	}
	data, err := run(ctx, "busctl", "--system", "get-property", snapshot.Owner, profile.Path, fabricNMSettings, "Unsaved", "Flags")
	if err != nil || strings.TrimSpace(string(data)) != "b true\nu 1" {
		return empty, false, errors.New("owned NetworkManager profile is not confirmed unsaved")
	}
	f := profile.Fields
	want := map[string]string{"connection.id": "nvpair-fabric-" + receipt.Label, "connection.uuid": receipt.NM.UUID, "connection.interface-name": iface.Name,
		"connection.autoconnect": "no", "802-3-ethernet.cloned-mac-address": "preserve", "802-3-ethernet.auto-negotiate": "no", "802-3-ethernet.duplex": "",
		"ipv4.method": "manual", "ipv4.addresses": iface.Address, "ipv4.never-default": "yes", "ipv4.ignore-auto-dns": "yes", "ipv4.ignore-auto-routes": "yes", "ipv4.gateway": "", "ipv4.dns": "", "ipv4.dns-search": "", "ipv4.routes": fabricNMRoutes(iface), "ipv4.may-fail": "no", "ipv6.method": "disabled", "ipv6.never-default": "yes", "ipv6.ignore-auto-dns": "yes"}
	for key, expected := range want {
		actual, exists := f[key]
		if !exists || actual != expected {
			return empty, false, errors.New("owned NetworkManager profile settings changed: " + key)
		}
	}
	for key, expected := range map[string]string{"connection.multi-connect": "1", "connection.llmnr": "0", "connection.mdns": "0", "connection.lldp": "0", "802-3-ethernet.mtu": strconv.Itoa(iface.MTU), "802-3-ethernet.speed": "0", "802-3-ethernet.wake-on-lan": "32768", "ipv4.route-table": "254", "ipv4.dad-timeout": "1000", "ipv4.link-local": "2"} {
		if key == "ipv4.link-local" && f[key] == "disabled" {
			continue
		}
		if fabricNMNumber(f[key]) != expected {
			return empty, false, errors.New("owned NetworkManager link policy changed")
		}
	}
	if (f["connection.type"] != "802-3-ethernet" && f["connection.type"] != "ethernet") || !strings.EqualFold(f["802-3-ethernet.mac-address"], receipt.NM.PermanentMAC) {
		return empty, false, errors.New("owned NetworkManager type or permanent MAC changed")
	}
	if receipt.NM.ProfileDigest == "" {
		if !fabricNMCompatibleProfile(*profile, iface, snapshot.Devices[iface.Name]) {
			return empty, false, errors.New("new NetworkManager profile has unreviewed dependencies or settings")
		}
		for key, value := range f {
			if value == "" {
				continue
			}
			if key == "ipv4.routing-rules" || key == "ipv6.routing-rules" || key == "connection.zone" {
				return empty, false, errors.New("new NetworkManager profile contains unreviewed routing, firewall, or proxy policy")
			}
		}
		if !fabricNMProxyDisabled(f) {
			return empty, false, errors.New("new NetworkManager profile contains active proxy policy")
		}
	}
	if receipt.NM.ProfileDigest != "" && fabricNMProfileDigest(f) != receipt.NM.ProfileDigest {
		return empty, false, errors.New("owned NetworkManager profile was modified")
	}
	return *profile, true, nil
}

type fabricNMIO struct {
	run       fabricNMRun
	identity  func(fabricInterface, bool) (fabricInterface, error)
	addresses func(context.Context, fabricInterface) (map[string]fabricNativeAddress, error)
	save      func(fabricNativeReceipt) error
	preflight func(context.Context, fabricInterface) error
}

func fabricNMAdd(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO, generatedRebindApproved ...bool) error {
	if !fabricNMReceiptValid(receipt) {
		return errors.New("complete native NetworkManager intent required before effects")
	}
	if len(generatedRebindApproved) > 1 {
		return errors.New("invalid generated-default rebind approval")
	}
	rebindApproved := len(generatedRebindApproved) == 1 && generatedRebindApproved[0]
	if receipt.NM.PolicyEstablished || receipt.NM.CleanupConfirmed || receipt.NM.GeneratedPauseAttempted {
		return errors.New("completed NetworkManager apply intent cannot be replayed")
	}
	current, err := fabricNMReadSettled(ctx, io.run, []fabricInterface{iface})
	if err != nil {
		return err
	}
	if receipt.NM == nil || current.Owner != receipt.NM.Owner || current.BusID != receipt.NM.BusID || current.Devices[iface.Name].Path != receipt.NM.DevicePath {
		return errors.New("reviewed NetworkManager daemon or device changed")
	}
	if iface.GeneratedDefault != nil {
		found, err := fabricNMGeneratedCandidate(current, iface)
		rebound := false
		if err != nil || !fabricNMGeneratedSame(iface.GeneratedDefault, found) {
			if err != nil || !rebindApproved || !fabricNMGeneratedStableSame(iface.GeneratedDefault, found) {
				return fabricInspectionError("network-manager-generation-changed", errors.New("approved generated default changed before native effects"))
			}
			copy := *found
			iface.GeneratedDefault = &copy
			receipt.NM.UUID = fabricNMUUID(receipt.Operation, iface)
			baseline, baselineErr := fabricNMBaseline(current, iface, receipt.NM.UUID)
			if baselineErr != nil || !slices.Equal(baseline, receipt.NM.BaselineProfiles) {
				return fabricInspectionError("network-manager-generation-changed", errors.New("generated default baseline changed before native effects"))
			}
			receipt.Interface = iface
			receipt.NM.BaselineProfiles = slices.Clone(baseline)
			rebound = true
		}
		if err := fabricNMGeneratedProof(ctx, io.run, current, iface, false); err != nil {
			if rebound {
				return fabricInspectionError("network-manager-generation-changed", err)
			}
			return err
		}
	}
	if blockers := fabricNMPreflight(current, []fabricInterface{iface}, receipt.NM.UUID); len(blockers) != 0 {
		return errors.New(strings.Join(blockers, "; "))
	}
	if err := fabricNMBaselineCurrent(current, iface, receipt); err != nil {
		return err
	}
	// This receipt is durable before the first device-policy effect, including
	// cases where a profile never gets created or activation later times out.
	if err := io.save(receipt); err != nil {
		return err
	}
	if err := fabricNMSetAutoconnect(ctx, iface, receipt, io, false); err != nil {
		return err
	}
	current, err = fabricNMQuiesce(ctx, iface, receipt, io)
	if err != nil {
		return err
	}
	if io.preflight != nil {
		if err := io.preflight(ctx, iface); err != nil {
			return err
		}
	}
	profile, found, err := fabricNMProfileOwned(ctx, io.run, iface, receipt, current)
	if err != nil {
		return err
	}
	if !found {
		if receipt.NM.SettingsPath != "" {
			return errors.New("owned profile disappeared; no automatic recreation")
		}
		if _, err := io.identity(iface, true); err != nil {
			return err
		}
		if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
			return err
		}
		if _, err := io.run(ctx, "nmcli", fabricNMAddArgs(iface, receipt)...); err != nil {
			return errors.New("NetworkManager profile creation is unconfirmed; retain the same operation for cleanup")
		}
		current, err = fabricNMRead(ctx, io.run, []fabricInterface{iface})
		if err != nil {
			return err
		}
		profile, found, err = fabricNMProfileOwned(ctx, io.run, iface, receipt, current)
		if err != nil || !found {
			return errors.New("new NetworkManager profile ownership was not confirmed")
		}
	}
	if blockers := fabricNMPreflight(current, []fabricInterface{iface}, receipt.NM.UUID); len(blockers) != 0 {
		return errors.New(strings.Join(blockers, "; "))
	}
	if current.Devices[iface.Name].Autoconnect || current.Devices[iface.Name].State != 30 || current.Devices[iface.Name].UUID != "" {
		return errors.New("selected NetworkManager interface did not remain paused and disconnected")
	}
	if err := fabricNMBaselineCurrent(current, iface, receipt); err != nil {
		return err
	}
	receipt.NM.SettingsPath, receipt.NM.ProfileDigest = profile.Path, fabricNMProfileDigest(profile.Fields)
	if err := io.save(receipt); err != nil {
		return err
	}
	if _, err := io.identity(iface, true); err != nil {
		return err
	}
	// Address activation and removal target the unique daemon owner, not a
	// reusable well-known name or settings path after a NetworkManager restart.
	if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
		return err
	}
	if _, err := io.run(ctx, "busctl", "--system", "call", receipt.NM.Owner, fabricNMRoot, fabricNMService, "ActivateConnection", "ooo", profile.Path, receipt.NM.DevicePath, "/"); err != nil {
		return errors.New("NetworkManager activation is unconfirmed; retain owned profile cleanup")
	}
	// Manual activation may unblock device autoconnect. Keep the explicitly
	// reviewed pause while the owned configuration is available to workloads.
	if err := fabricNMSetAutoconnect(ctx, iface, receipt, io, false); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		current, err = fabricNMRead(ctx, io.run, []fabricInterface{iface})
		if err != nil {
			return err
		}
		profile, found, err = fabricNMProfileOwned(ctx, io.run, iface, receipt, current)
		if err != nil || !found {
			return errors.New("activated NetworkManager ownership changed")
		}
		device := current.Devices[iface.Name]
		if err := fabricNMBaselineCurrent(current, iface, receipt); err != nil {
			return err
		}
		if device.Autoconnect {
			return errors.New("NetworkManager autoconnect pause changed during activation")
		}
		if device.UUID == receipt.NM.UUID && device.State == 100 && profile.Active && profile.Device == iface.Name && profile.ActivePath == device.ActivePath && slices.Equal(fabricSorted(device.Addresses), []string{iface.Address}) && len(device.Gateways) == 0 && len(device.DNS) == 0 {
			if _, err := io.identity(iface, true); err != nil {
				return err
			}
			addresses, err := io.addresses(ctx, iface)
			if err != nil {
				return err
			}
			if len(addresses) == 1 {
				if address, ok := addresses[iface.Address]; ok && fabricNativeOwnedAddress(address, iface.Name) {
					routes, err := fabricNMKernelRoutes(ctx, io.run, iface)
					if err != nil {
						return err
					}
					if !fabricNMRoutesExact(routes, iface) {
						return errors.New("exact NetworkManager routes were not confirmed by the kernel")
					}
					receipt.NM.PolicyEstablished = true
					return io.save(receipt)
				}
			}
			return errors.New("exact NetworkManager address was not confirmed by the kernel")
		}
		if device.UUID != "" && device.UUID != receipt.NM.UUID {
			return errors.New("foreign NetworkManager activation appeared; preserved")
		}
		if err := fabricNMWait(ctx); err != nil {
			return err
		}
	}
	return errors.New("NetworkManager activation did not complete within bounded readback")
}

func fabricNMRemove(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO) error {
	if !fabricNMReceiptValid(receipt) {
		return errors.New("complete original NetworkManager policy is required for rollback")
	}
	if _, err := io.identity(iface, false); err != nil {
		return err
	}
	current, err := fabricNMRead(ctx, io.run, []fabricInterface{iface})
	if err != nil {
		return err
	}
	if receipt.NM.GeneratedPauseAttempted && !receipt.NM.GeneratedPauseConfirmed {
		if err := fabricNMGeneratedProof(ctx, io.run, current, iface, true); err != nil {
			return errors.New("generated pause is still unconfirmed; policy restoration held")
		}
		receipt.NM.GeneratedPauseConfirmed = true
		if err := io.save(receipt); err != nil {
			return err
		}
	}
	profile, found, err := fabricNMProfileOwned(ctx, io.run, iface, receipt, current)
	if err != nil {
		return err
	}
	if receipt.NM.CleanupConfirmed {
		// The completed operation owns no device policy. Later changes are read
		// only evidence and can never cause this receipt to replay a setter.
		addresses, err := io.addresses(ctx, iface)
		if err != nil {
			return err
		}
		_, present := addresses[iface.Address]
		routesGone, err := fabricNMOwnedRoutesGone(ctx, io.run, iface)
		if err != nil {
			return err
		}
		if found || present || !routesGone {
			return errors.New("completed NetworkManager cleanup now has replacement profile, address or host route evidence; preserved")
		}
		return nil
	}
	if err := fabricNMBaselineCurrent(current, iface, receipt); err != nil {
		return err
	}
	addresses, err := io.addresses(ctx, iface)
	if err != nil {
		return err
	}
	if !found {
		if err := fabricNMRestoreAutoconnect(ctx, iface, receipt, io); err != nil {
			return err
		}
		routesGone, err := fabricNMOwnedRoutesGone(ctx, io.run, iface)
		if err != nil {
			return err
		}
		if _, exists := addresses[iface.Address]; (exists || !routesGone) && receipt.NM.SettingsPath != "" {
			return errors.New("address or host route remains without owned NetworkManager profile; preserved")
		}
		receipt.NM.CleanupConfirmed = true
		return io.save(receipt)
	}
	device := current.Devices[iface.Name]
	if receipt.NM.PolicyEstablished && device.Autoconnect {
		return errors.New("established NetworkManager autoconnect ownership changed; foreign policy preserved")
	}
	if device.Path != receipt.NM.DevicePath || (device.UUID != "" && device.UUID != receipt.NM.UUID) || (profile.Active && (profile.Device != iface.Name || profile.ActivePath != device.ActivePath || device.UUID != receipt.NM.UUID)) {
		return errors.New("foreign or changed NetworkManager activation is preserved")
	}
	for address := range addresses {
		if address != iface.Address {
			return errors.New("additional native addresses appeared; preserve NetworkManager device configuration")
		}
	}
	if err := fabricNMSetAutoconnect(ctx, iface, receipt, io, false); err != nil {
		return err
	}
	if _, err := io.identity(iface, false); err != nil {
		return err
	}
	if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
		return err
	}
	if _, err := io.run(ctx, "busctl", "--system", "call", receipt.NM.Owner, profile.Path, fabricNMSettings, "Delete"); err != nil {
		return errors.New("exact NetworkManager profile deletion is unconfirmed")
	}
	for attempt := 0; attempt < 5; attempt++ {
		current, err = fabricNMRead(ctx, io.run, []fabricInterface{iface})
		if err != nil {
			return err
		}
		if current.Owner != receipt.NM.Owner || current.BusID != receipt.NM.BusID {
			return errors.New("NetworkManager restarted during cleanup")
		}
		stillPresent := false
		for _, p := range current.Profiles {
			stillPresent = stillPresent || p.UUID == receipt.NM.UUID
		}
		addresses, err = io.addresses(ctx, iface)
		if err != nil {
			return err
		}
		_, addressPresent := addresses[iface.Address]
		routesGone, err := fabricNMOwnedRoutesGone(ctx, io.run, iface)
		if err != nil {
			return err
		}
		if !stillPresent && !addressPresent && routesGone {
			if err := fabricNMBaselineCurrent(current, iface, receipt); err != nil {
				return err
			}
			if err := fabricNMRestoreAutoconnect(ctx, iface, receipt, io); err != nil {
				return err
			}
			receipt.NM.CleanupConfirmed = true
			return io.save(receipt)
		}
		if err := fabricNMWait(ctx); err != nil {
			return err
		}
	}
	return errors.New("owned NetworkManager profile, address or host route cleanup remains unconfirmed")
}

func fabricNMReceiptValid(receipt fabricNativeReceipt) bool {
	if receipt.NM == nil {
		return false
	}
	r := receipt.NM
	if r.GeneratedPauseAttempted && !r.GeneratedPauseConfirmed && (r.PolicyEstablished || r.CleanupConfirmed) {
		return false
	}
	if d := receipt.Interface.GeneratedDefault; d != nil {
		if !validFabricGeneratedDefault(d, receipt.Interface) || d.Owner != r.Owner || d.BusID != r.BusID || d.DevicePath != r.DevicePath ||
			!strings.EqualFold(d.PermanentMAC, r.PermanentMAC) || r.OriginalAutoconnect == nil || *d.OriginalAutoconnect != *r.OriginalAutoconnect ||
			r.GeneratedPauseAttempted != fabricNMObject(r.GeneratedPausePath, "ActiveConnection") || (!r.GeneratedPauseAttempted && r.GeneratedPausePath != "") ||
			(r.GeneratedPauseConfirmed && !r.GeneratedPauseAttempted) {
			return false
		}
	} else if r.GeneratedPauseAttempted || r.GeneratedPauseConfirmed || r.GeneratedPausePath != "" {
		return false
	}
	if r.PolicyEstablished && (r.SettingsPath == "" || len(r.ProfileDigest) != 64) {
		return false
	}
	_, busErr := hex.DecodeString(r.BusID)
	return r.OriginalAutoconnect != nil && r.BaselineProfiles != nil && len(r.BaselineProfiles) <= 32 && r.UUID == fabricNMUUID(receipt.Operation, receipt.Interface) && len(r.BusID) == 32 && busErr == nil && strings.HasPrefix(r.Owner, ":") && !strings.ContainsAny(r.Owner, " /\\\r\n") && fabricNMObject(r.DevicePath, "Devices") && strings.EqualFold(r.PermanentMAC, receipt.Interface.MAC) &&
		(r.SettingsPath == "" || fabricNMObject(r.SettingsPath, "Settings")) && (r.ProfileDigest == "" || len(r.ProfileDigest) == 64)
}

func fabricNMSetAutoconnect(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO, enabled bool) error {
	if receipt.NM == nil || receipt.NM.OriginalAutoconnect == nil {
		return errors.New("original NetworkManager device policy unavailable")
	}
	if receipt.NM.CleanupConfirmed {
		return errors.New("completed cleanup cannot change NetworkManager device policy")
	}
	if _, err := io.identity(iface, false); err != nil {
		return err
	}
	if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
		return err
	}
	query := []string{"--system", "get-property", receipt.NM.Owner, receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device", "Autoconnect"}
	data, err := io.run(ctx, "busctl", query...)
	if err != nil {
		return errors.New("NetworkManager device policy readback unavailable")
	}
	value := strings.TrimSpace(string(data))
	if value != "b true" && value != "b false" {
		return errors.New("NetworkManager device policy is ambiguous")
	}
	wanted := "b " + strconv.FormatBool(enabled)
	if receipt.NM.PolicyEstablished && !enabled && value != "b false" {
		return errors.New("established NetworkManager autoconnect ownership changed; foreign policy preserved")
	}
	if value == wanted {
		return nil
	}
	if _, err := io.run(ctx, "busctl", "--system", "set-property", receipt.NM.Owner, receipt.NM.DevicePath, "org.freedesktop.NetworkManager.Device", "Autoconnect", "b", strconv.FormatBool(enabled)); err != nil {
		return errors.New("selected NetworkManager autoconnect policy change is unconfirmed")
	}
	data, err = io.run(ctx, "busctl", query...)
	if err != nil || strings.TrimSpace(string(data)) != wanted {
		return errors.New("selected NetworkManager autoconnect policy did not match readback")
	}
	return nil
}

func fabricNMRestoreAutoconnect(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO) error {
	// Owned profile/address absence was established before restoration. A
	// preserved foreign autoconnect profile may legitimately resume afterward.
	if err := fabricNMSetAutoconnect(ctx, iface, receipt, io, *receipt.NM.OriginalAutoconnect); err != nil {
		return err
	}
	if _, err := io.identity(iface, false); err != nil {
		return err
	}
	if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
		return err
	}
	profiles, err := fabricNMProfiles(ctx, io.run)
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if profile.UUID == receipt.NM.UUID {
			return errors.New("owned NetworkManager profile reappeared during policy restoration")
		}
	}
	addresses, err := io.addresses(ctx, iface)
	if err != nil {
		return err
	}
	if _, present := addresses[iface.Address]; present {
		return errors.New("the same address appeared after original policy resumed; foreign configuration was preserved")
	}
	return nil
}

func fabricNMQuiesce(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO) (fabricNMSnapshot, error) {
	if iface.GeneratedDefault != nil {
		return fabricNMGeneratedQuiesce(ctx, iface, receipt, io)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		snapshot, err := fabricNMRead(ctx, io.run, []fabricInterface{iface})
		if err != nil {
			return snapshot, err
		}
		if snapshot.Owner != receipt.NM.Owner || snapshot.BusID != receipt.NM.BusID || snapshot.Devices[iface.Name].Path != receipt.NM.DevicePath {
			return snapshot, errors.New("NetworkManager identity changed during pending activation wait")
		}
		if err := fabricNMBaselineCurrent(snapshot, iface, receipt); err != nil {
			return snapshot, err
		}
		device := snapshot.Devices[iface.Name]
		addresses, err := io.addresses(ctx, iface)
		if err != nil {
			return snapshot, err
		}
		if device.Autoconnect || len(device.Addresses) != 0 || len(device.DNS) != 0 || len(device.Gateways) != 0 || len(addresses) != 0 {
			return snapshot, errors.New("foreign activation became usable or autoconnect pause changed; configuration preserved")
		}
		if device.State == 30 && device.UUID == "" && device.ActivePath == "" {
			return snapshot, nil
		}
		if blockers := fabricNMPreflight(snapshot, []fabricInterface{iface}, receipt.NM.UUID); len(blockers) != 0 || device.State != 70 {
			return snapshot, errors.New("foreign NetworkManager activation is not an admitted addressless DHCP wait")
		}
		if err := fabricNMWait(ctx); err != nil {
			return snapshot, errors.New("pending NetworkManager activation did not settle within the bounded wait; no foreign disconnect was performed")
		}
	}
}

func fabricNMGeneratedQuiesce(ctx context.Context, iface fabricInterface, receipt fabricNativeReceipt, io fabricNMIO) (fabricNMSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		snapshot, err := fabricNMRead(ctx, io.run, []fabricInterface{iface})
		if err != nil {
			return snapshot, err
		}
		if !fabricNMGeneratedDevice(snapshot, iface) {
			return snapshot, errors.New("generated default device or daemon changed during pause")
		}
		if err := fabricNMBaselineCurrent(snapshot, iface, receipt); err != nil {
			return snapshot, err
		}
		device := snapshot.Devices[iface.Name]
		addresses, err := io.addresses(ctx, iface)
		if err != nil {
			return snapshot, err
		}
		if device.Autoconnect || len(device.DNS) != 0 || len(device.Gateways) != 0 || !fabricLinkLocalBaseline(append([]string{}, device.Addresses...)) || !fabricNativeLinkLocalOnly(addresses) {
			return snapshot, errors.New("generated pending activation became usable or selected policy changed; preserved")
		}
		// Every DHCP attempt of the consented generated default brings its own
		// link-local address. Pending NM IP objects can omit LL entries; they
		// cannot contradict the complete kernel read.
		for _, address := range device.Addresses {
			if _, present := addresses[address]; !present {
				return snapshot, errors.New("NetworkManager and kernel address observations disagree")
			}
		}
		if device.State == 30 && device.UUID == "" && device.ActivePath == "" {
			if receipt.NM.GeneratedPauseAttempted {
				if err := fabricNMGeneratedProof(ctx, io.run, snapshot, iface, true); err != nil {
					return snapshot, err
				}
				receipt.NM.GeneratedPauseConfirmed = true
				if err := io.save(receipt); err != nil {
					return snapshot, err
				}
			}
			return snapshot, nil
		}
		if receipt.NM.GeneratedPauseAttempted {
			if device.State != 110 || (device.ActivePath != "" && device.ActivePath != receipt.NM.GeneratedPausePath) || (device.UUID != "" && device.UUID != iface.GeneratedDefault.UUID) {
				return snapshot, errors.New("generated pause completion is unconfirmed")
			}
		} else {
			// NetworkManager replaces a failed attempt of the same generated profile
			// with a new active object. Autoconnect is already paused, so the attempt
			// recorded here is the last one it can start for this device.
			attempt := fabricNMGeneratedAttempt(iface, device.ActivePath)
			matched := false
			for _, profile := range snapshot.Profiles {
				matched = matched || fabricNMGeneratedPending(snapshot, attempt, profile)
			}
			if !matched {
				return snapshot, errors.New("approved generated pending activation changed")
			}
			intentAddresses := addresses
			receipt.NM.GeneratedPauseAttempted = true
			receipt.NM.GeneratedPausePath = device.ActivePath
			if err := io.save(receipt); err != nil {
				return snapshot, err
			}
			// Durable I/O can outlast pending eligibility. Keep its one-shot
			// intent on refusal and recheck current state after that write.
			snapshot, err = fabricNMRead(ctx, io.run, []fabricInterface{iface})
			if err != nil {
				return snapshot, err
			}
			if !fabricNMGeneratedDevice(snapshot, iface) {
				return snapshot, errors.New("generated default device changed during pause intent")
			}
			if err := fabricNMBaselineCurrent(snapshot, iface, receipt); err != nil {
				return snapshot, err
			}
			device = snapshot.Devices[iface.Name]
			addresses, err = io.addresses(ctx, iface)
			if err != nil {
				return snapshot, err
			}
			if device.Autoconnect || len(device.DNS) != 0 || len(device.Gateways) != 0 || !fabricLinkLocalBaseline(append([]string{}, device.Addresses...)) || !fabricNativeLinkLocalOnly(addresses) {
				return snapshot, errors.New("generated activation became usable during pause intent; preserved")
			}
			for address := range addresses {
				if _, present := intentAddresses[address]; !present {
					return snapshot, errors.New("native address changed during pause intent; preserved")
				}
			}
			for _, address := range device.Addresses {
				if _, present := addresses[address]; !present {
					return snapshot, errors.New("NetworkManager and kernel addressing changed during pause intent")
				}
			}
			if device.State == 30 && device.UUID == "" && device.ActivePath == "" {
				if err := fabricNMGeneratedProof(ctx, io.run, snapshot, iface, true); err != nil {
					return snapshot, err
				}
				receipt.NM.GeneratedPauseConfirmed = true
				return snapshot, io.save(receipt)
			}
			matched = false
			for _, profile := range snapshot.Profiles {
				matched = matched || fabricNMGeneratedPending(snapshot, attempt, profile)
			}
			if !matched {
				return snapshot, errors.New("approved pending activation changed during pause intent; preserved")
			}
			if err := fabricNMGeneratedProof(ctx, io.run, snapshot, attempt, false); err != nil {
				return snapshot, err
			}
			if err := fabricNMSameInstance(ctx, io.run, receipt.NM); err != nil {
				return snapshot, err
			}
			if _, err := io.run(ctx, "busctl", "--system", "call", receipt.NM.Owner, fabricNMRoot, fabricNMService, "DeactivateConnection", "o", receipt.NM.GeneratedPausePath); err != nil {
				return snapshot, errors.New("exact generated pending pause is unconfirmed; intent retained")
			}
		}
		if err := fabricNMWait(ctx); err != nil {
			return snapshot, errors.New("generated pending pause did not settle within its deadline")
		}
	}
}

func fabricNMWait(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func fabricNMNumber(value string) string {
	switch value {
	case "disable", "no":
		return "0"
	case "single":
		return "1"
	case "ignore":
		return "32768"
	}
	words := strings.Fields(value)
	if len(words) == 0 {
		return ""
	}
	base := 10
	if strings.HasPrefix(words[0], "0x") {
		base = 0
	}
	valueNumber, err := strconv.ParseInt(words[0], base, 32)
	if err != nil {
		return ""
	}
	if len(words) > 1 && (!strings.HasPrefix(strings.Join(words[1:], " "), "(") || !strings.HasSuffix(value, ")")) {
		return ""
	}
	return strconv.FormatInt(valueNumber, 10)
}

func fabricNMBusID(ctx context.Context, run fabricNMRun) (string, error) {
	data, err := run(ctx, "busctl", "--system", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId")
	if err != nil {
		return "", errors.New("system bus generation unavailable")
	}
	id, err := fabricNMString(data, "s")
	if err != nil || len(id) != 32 {
		return "", errors.New("system bus generation is invalid")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", errors.New("system bus generation is invalid")
	}
	return id, nil
}

func fabricNMSameInstance(ctx context.Context, run fabricNMRun, receipt *fabricNMReceipt) error {
	busID, err := fabricNMBusID(ctx, run)
	if err != nil || busID != receipt.BusID {
		return errors.New("system bus generation changed before NetworkManager mutation")
	}
	owner, err := fabricNMOwner(ctx, run)
	if err != nil || owner != receipt.Owner {
		return errors.New("NetworkManager daemon changed before mutation")
	}
	return nil
}
