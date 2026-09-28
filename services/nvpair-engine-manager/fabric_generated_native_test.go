// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

// All commands below are in-memory fixtures. Unknown commands terminate the
// test through the existing fake; no native command, socket or helper can run.
type generatedNativeHost struct {
	*fabricNMTestHost
	pauses         int
	pauseErr       bool
	overlap        bool
	appliedChanged bool
}

func generatedNativeTestHost(t *testing.T, pending bool) *generatedNativeHost {
	h := fabricNMTestNew(t)
	p := h.seedGeneratedPlaceholder()
	if pending {
		h.state, h.deviceUUID, h.activePath = 70, p.UUID, fabricNMRoot+"/ActiveConnection/2"
		h.profiles[0].Active, h.profiles[0].Device, h.profiles[0].ActivePath = true, h.iface.Name, h.activePath
	}
	var ll fabricNativeAddress
	if json.Unmarshal([]byte(`{"family":"inet6","local":"fe80::42","prefixlen":64,"scope":"link","valid_life_time":4294967295,"preferred_life_time":4294967295}`), &ll) != nil {
		t.Fatal("fixture")
	}
	h.native["fe80::42/64"] = ll
	h.iface.Addresses = []string{"fe80::42/64"}
	original := h.autoconnect
	h.iface.GeneratedDefault = &fabricGeneratedDefault{SchemaVersion: 1, InterfaceName: h.iface.Name, Index: h.iface.Index, Owner: h.owner, BusID: h.busID,
		DevicePath: h.receipt.NM.DevicePath, PermanentMAC: h.iface.MAC, OriginalAutoconnect: &original, UUID: p.UUID, Name: p.Fields["connection.id"],
		SettingsPath: p.Path, ProfileDigest: fabricNMProfileDigest(p.Fields), ActivePath: h.activePath}
	h.receipt.Interface = h.iface
	h.receipt.NM.UUID = fabricNMUUID(h.receipt.Operation, h.iface)
	return &generatedNativeHost{fabricNMTestHost: h}
}

func generatedJSON(signature string, value any) ([]byte, error) {
	return json.Marshal(map[string]any{"type": signature, "data": value})
}

func (h *generatedNativeHost) run(ctx context.Context, command string, args ...string) ([]byte, error) {
	if command == "busctl" && len(args) >= 7 && args[0] == "--json=short" && args[1] == "--system" && args[2] == "call" && (args[6] == "GetSettings" || args[6] == "GetAppliedConnection") {
		h.calls = append(h.calls, append([]string{command}, args...))
		if args[3] != h.owner {
			return nil, errors.New("wrong fixture owner")
		}
		d := h.iface.GeneratedDefault
		variant := func(value string) any { return map[string]any{"type": "s", "data": value} }
		settings := map[string]any{"connection": map[string]any{"id": variant(d.Name), "uuid": variant(d.UUID), "interface-name": variant(d.InterfaceName)}, "802-3-ethernet": map[string]any{}, "ipv4": map[string]any{"method": variant("auto")}, "ipv6": map[string]any{"method": variant("auto")}}
		if args[6] == "GetSettings" {
			if args[4] != d.SettingsPath {
				h.t.Fatal("wrong settings proof path")
			}
			return generatedJSON("a{sa{sv}}", []any{settings})
		}
		if args[4] != d.DevicePath || len(args) != 9 || args[7] != "u" || args[8] != "0" {
			h.t.Fatal("wrong applied proof arguments")
		}
		if h.appliedChanged {
			settings["ipv4"].(map[string]any)["method"] = variant("manual")
		}
		return generatedJSON("a{sa{sv}}t", []any{settings, 17})
	}
	if command == "busctl" && len(args) == 7 && args[0] == "--json=short" && args[1] == "--system" && args[2] == "get-property" {
		h.calls = append(h.calls, append([]string{command}, args...))
		if args[3] != h.owner {
			return nil, errors.New("wrong fixture owner")
		}
		object, property := args[4], args[6]
		if object == fabricNMRoot && property == "ActiveConnections" {
			paths := []string{}
			for _, p := range h.profiles {
				if p.Active {
					paths = append(paths, p.ActivePath)
				}
			}
			if h.overlap {
				paths = append(paths, fabricNMRoot+"/ActiveConnection/99")
			}
			return generatedJSON("ao", paths)
		}
		if object == h.receipt.NM.DevicePath {
			switch property {
			case "State":
				return generatedJSON("u", h.state)
			case "ActiveConnection":
				active := h.activePath
				if active == "" {
					active = "/"
				}
				return generatedJSON("o", active)
			}
		}
		for _, p := range h.profiles {
			if p.ActivePath != object && object != fabricNMRoot+"/ActiveConnection/99" {
				continue
			}
			switch property {
			case "Devices":
				return generatedJSON("ao", []string{h.receipt.NM.DevicePath})
			case "Connection":
				return generatedJSON("o", p.Path)
			case "Uuid":
				return generatedJSON("s", p.UUID)
			case "State":
				return generatedJSON("u", 1)
			case "Master":
				return generatedJSON("o", "/")
			case "Default", "Default6":
				return generatedJSON("b", false)
			}
		}
		return nil, errors.New("unknown generated proof property")
	}
	if command == "busctl" && slices.Contains(args, "DeactivateConnection") {
		h.calls = append(h.calls, append([]string{command}, args...))
		want := []string{"--system", "call", h.owner, fabricNMRoot, fabricNMService, "DeactivateConnection", "o", h.activePath}
		if !slices.Equal(args, want) || len(h.saved) == 0 || !h.latestReceipt().NM.GeneratedPauseAttempted || h.latestReceipt().NM.GeneratedPausePath != h.activePath {
			h.t.Fatal("pause escaped the current attempt or preceded its durable intent")
		}
		h.pauses++
		if h.pauseErr {
			return nil, errors.New("synthetic private pause transport")
		}
		h.state, h.deviceUUID, h.activePath = 30, "", ""
		for i := range h.profiles {
			h.profiles[i].Active, h.profiles[i].Device, h.profiles[i].ActivePath = false, "", ""
		}
		for address := range h.native {
			if strings.HasPrefix(address, "fe80:") {
				delete(h.native, address)
			}
		}
		return []byte{}, nil
	}
	if command == "busctl" && slices.Contains(args, "ActivateConnection") {
		delete(h.native, "fe80::42/64")
	}
	return h.fabricNMTestHost.run(ctx, command, args...)
}

func (h *generatedNativeHost) io() fabricNMIO {
	value := h.fabricNMTestHost.io()
	value.run = h.run
	return value
}

func TestFabricGeneratedPendingNativeLifecycle(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnected", true: "exact-pending"}[pending], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, pending)
				original := h.profiles[0]
				regenerated := original
				regenerated.Fields = maps.Clone(original.Fields)
				regenerated.UUID, regenerated.Path = "00000000-0000-4000-8000-000000000003", fabricNMRoot+"/Settings/3"
				regenerated.Active, regenerated.Device, regenerated.ActivePath = false, "", ""
				regenerated.Fields["connection.uuid"], regenerated.Fields["connection.id"] = regenerated.UUID, "Regenerated default"
				h.retirePlaceholders, h.regenerateAt, h.regeneratedPlaceholder = true, "delete", &regenerated
				if err := fabricNMAdd(context.Background(), h.iface, h.receipt, h.io()); err != nil {
					t.Fatal(err)
				}
				if h.pauses != map[bool]int{false: 0, true: 1}[pending] || h.autoconnect || !h.latestReceipt().NM.PolicyEstablished {
					t.Fatal("wrong bounded pause/activation outcome")
				}
				if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
					t.Fatal(err)
				}
				if !h.autoconnect || !h.latestReceipt().NM.CleanupConfirmed || len(h.profiles) != 1 || h.profiles[0].UUID != regenerated.UUID {
					t.Fatal("policy restoration required the old generated UUID or lost regenerated state")
				}
				before := len(h.calls)
				if err := fabricNMRemove(context.Background(), h.iface, h.latestReceipt(), h.io()); err != nil {
					t.Fatal(err)
				}
				for _, call := range h.calls[before:] {
					if slices.Contains(call, "set-property") || slices.Contains(call, "Delete") || slices.Contains(call, "DeactivateConnection") {
						t.Fatal("completed cleanup reclaimed authority")
					}
				}
			})
		})
	}
}

func TestFabricGeneratedPendingRebindsOnlyFinalPreEffectGeneration(t *testing.T) {
	cycle := func(t *testing.T) (*generatedNativeHost, fabricInterface, fabricNativeReceipt) {
		h := generatedNativeTestHost(t, true)
		iface, receipt := h.iface, h.receipt
		current := *h.iface.GeneratedDefault
		current.ActivePath = fabricNMRoot + "/ActiveConnection/9"
		h.iface.GeneratedDefault = &current
		h.activePath = current.ActivePath
		h.profiles[0].ActivePath = current.ActivePath
		return h, iface, receipt
	}
	t.Run("approved stable cycle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h, reviewed, receipt := cycle(t)
			if err := fabricNMAdd(context.Background(), reviewed, receipt, h.io(), true); err != nil {
				t.Fatalf("%v: %v", err, errors.Unwrap(err))
			}
			retained := h.latestReceipt()
			if retained.Interface.GeneratedDefault == nil || retained.Interface.GeneratedDefault.ActivePath != fabricNMRoot+"/ActiveConnection/9" || h.pauses != 1 || h.policyChanges() == 0 {
				t.Fatal("final pre-effect generation was not rebound before the owned pause")
			}
		})
	})
	for _, mode := range []string{"missing approval", "stable profile changed"} {
		t.Run(mode, func(t *testing.T) {
			h, reviewed, receipt := cycle(t)
			approved := false
			if mode == "stable profile changed" {
				approved = true
				h.profiles[0].Fields["connection.id"] = "replacement profile"
				h.iface.GeneratedDefault.Name = "replacement profile"
				h.iface.GeneratedDefault.ProfileDigest = fabricNMProfileDigest(h.profiles[0].Fields)
			}
			err := fabricNMAdd(context.Background(), reviewed, receipt, h.io(), approved)
			var failure *fabricInspectError
			if !errors.As(err, &failure) || failure.Code != "network-manager-generation-changed" || len(h.saved) != 0 || h.pauses != 0 || h.policyChanges() != 0 {
				t.Fatalf("final pre-effect generation refusal was not typed and effect-free: %v", err)
			}
		})
	}
}

// NetworkManager replaces a failed generated DHCP attempt about every 45
// seconds, and each attempt carries its own link-local address. Once the
// selected pause is approved, the attempt pending at pause time is the one
// stopped and recorded.
func TestFabricGeneratedPendingPausesCurrentAttemptAtPauseTime(t *testing.T) {
	for _, mode := range []string{"replacement-attempt", "new-link-local"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, true)
				ops := h.io()
				injected := false
				ops.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
					data, err := h.run(ctx, command, args...)
					if !injected && slices.Contains(args, "set-property") && args[len(args)-1] == "false" {
						injected = true
						if mode == "replacement-attempt" {
							h.activePath = fabricNMRoot + "/ActiveConnection/8"
							h.profiles[0].ActivePath = h.activePath
						} else {
							added := h.native["fe80::42/64"]
							added.Local = "fe80::99"
							h.native["fe80::99/64"] = added
						}
					}
					return data, err
				}
				if err := fabricNMAdd(context.Background(), h.iface, h.receipt, ops); err != nil {
					t.Fatal(err)
				}
				retained := h.latestReceipt()
				wantPause := h.iface.GeneratedDefault.ActivePath
				if mode == "replacement-attempt" {
					wantPause = fabricNMRoot + "/ActiveConnection/8"
				}
				if h.pauses != 1 || h.autoconnect || !retained.NM.PolicyEstablished || retained.NM.GeneratedPausePath != wantPause {
					t.Fatalf("current attempt was not paused exactly once: pauses=%d receipt=%+v", h.pauses, retained.NM)
				}
				if err := fabricNMRemove(context.Background(), h.iface, retained, h.io()); err != nil || !h.autoconnect || !h.latestReceipt().NM.CleanupConfirmed {
					t.Fatalf("rollback after the current-attempt pause failed: %v", err)
				}
			})
		})
	}
}

// Unconfigured ports keep replacing their own DHCP attempts, so neither the
// proof nor the snapshot may treat another port's churn as a change here.
func TestFabricGeneratedPendingToleratesOtherAttemptChurn(t *testing.T) {
	for _, mode := range []string{"other-port-attempt", "straddled-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, true)
				ops := h.io()
				activeReads, deviceReads := 0, 0
				ops.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
					if mode == "other-port-attempt" && slices.Contains(args, "ActiveConnections") {
						activeReads++
						if activeReads%2 == 0 {
							paths := []string{fabricNMRoot + "/ActiveConnection/77"}
							if h.activePath != "" {
								paths = append(paths, h.activePath)
							}
							return generatedJSON("ao", paths)
						}
					}
					if mode == "straddled-snapshot" && command == "nmcli" && slices.Contains(args, "device") && slices.Contains(args, "show") {
						deviceReads++
						if deviceReads == 1 {
							saved := h.activePath
							h.activePath = fabricNMRoot + "/ActiveConnection/5"
							data, err := h.run(ctx, command, args...)
							h.activePath = saved
							return data, err
						}
					}
					return h.run(ctx, command, args...)
				}
				if err := fabricNMAdd(context.Background(), h.iface, h.receipt, ops); err != nil || h.pauses != 1 || !h.latestReceipt().NM.PolicyEstablished {
					t.Fatalf("unrelated attempt churn failed apply: %v pauses=%d", err, h.pauses)
				}
			})
		})
	}
}

func TestFabricGeneratedPendingRefusesChangedProofBeforePause(t *testing.T) {
	for _, mode := range []string{"applied-change", "overlap", "overlap-after", "wrong-attempt", "saved-profile", "after-list-duplicate", "proof-unavailable", "journal-failure"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, true)
				switch mode {
				case "applied-change":
					h.appliedChanged = true
				case "overlap":
					h.overlap = true
				case "wrong-attempt":
					h.activePath = fabricNMRoot + "/ActiveConnection/8"
					h.profiles[0].ActivePath = h.activePath
				case "saved-profile":
					h.profiles[0].Flags = 0
					h.profiles[0].Filename = "/etc/NetworkManager/system-connections/preserved.nmconnection"
				}
				ops := h.io()
				activeReads := 0
				ops.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
					if mode == "proof-unavailable" && slices.Contains(args, "GetAppliedConnection") {
						return nil, errors.New("unavailable")
					}
					if slices.Contains(args, "ActiveConnections") {
						activeReads++
						if mode == "after-list-duplicate" && activeReads == 2 {
							return generatedJSON("ao", []string{h.activePath, h.activePath})
						}
						if mode == "overlap-after" && activeReads == 2 {
							return generatedJSON("ao", []string{h.activePath, fabricNMRoot + "/ActiveConnection/99"})
						}
					}
					return h.run(ctx, command, args...)
				}
				save := ops.save
				ops.save = func(receipt fabricNativeReceipt) error {
					if mode == "journal-failure" && receipt.NM.GeneratedPauseAttempted {
						return errors.New("intent not saved")
					}
					return save(receipt)
				}
				if err := fabricNMAdd(context.Background(), h.iface, h.receipt, ops); err == nil || h.pauses != 0 {
					t.Fatalf("changed proof reached pause: error=%v pauses=%d", err, h.pauses)
				}
				for _, call := range h.calls {
					if slices.Contains(call, "ActivateConnection") || slices.Contains(call, "Delete") {
						t.Fatal("refused pause reached profile effects")
					}
				}
			})
		})
	}
}

func TestFabricGeneratedPendingFailureRetainsOnePauseAndPolicyRecovery(t *testing.T) {
	for _, mode := range []string{"lost-pause-reply", "cancel-after-pause"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, true)
				h.pauseErr = mode == "lost-pause-reply"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ops := h.io()
				ops.run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					data, err := h.run(ctx, command, args...)
					if mode == "cancel-after-pause" && slices.Contains(args, "DeactivateConnection") {
						cancel()
					}
					return data, err
				}
				if err := fabricNMAdd(ctx, h.iface, h.receipt, ops); err == nil || h.pauses != 1 || h.autoconnect {
					t.Fatalf("uncertain pause lost intent/policy: %v pauses=%d", err, h.pauses)
				}
				retained := h.latestReceipt()
				if !retained.NM.GeneratedPauseAttempted || retained.NM.CleanupConfirmed {
					t.Fatal("uncertain pause reported clean")
				}
				if err := fabricNMAdd(context.Background(), h.iface, retained, h.io()); err == nil || h.pauses != 1 {
					t.Fatal("uncertain pause was dispatched twice")
				}
				if mode == "lost-pause-reply" {
					if fabricNMRemove(context.Background(), h.iface, retained, h.io()) == nil || h.autoconnect {
						t.Fatal("unconfirmed pause restored policy before proof")
					}
					// Later authoritative state shows that the exact attempt settled.
					h.state, h.deviceUUID, h.activePath = 30, "", ""
					for i := range h.profiles {
						h.profiles[i].Active, h.profiles[i].Device, h.profiles[i].ActivePath = false, "", ""
					}
					delete(h.native, "fe80::42/64")
				}
				if err := fabricNMRemove(context.Background(), h.iface, retained, h.io()); err != nil || !h.autoconnect || !h.latestReceipt().NM.CleanupConfirmed || h.pauses != 1 {
					t.Fatalf("same-operation policy recovery failed: %v", err)
				}
			})
		})
	}
}

func TestFabricGeneratedProofRejectsMalformedAndDivergentFrames(t *testing.T) {
	for _, mode := range []string{"duplicate-key", "alternate-dict", "missing-section", "null-data", "zero-version", "unknown-field"} {
		t.Run(mode, func(t *testing.T) {
			data := `{"type":"a{sa{sv}}t","data":[{"connection":{},"ipv4":{},"ipv6":{}},17]}`
			switch mode {
			case "duplicate-key":
				data = strings.Replace(data, `"connection":{}`, `"connection":{},"connection":{}`, 1)
			case "alternate-dict":
				data = `{"type":"a{sa{sv}}t","data":[["connection",{},"ipv4",{},"ipv6",{}],17]}`
			case "missing-section":
				data = `{"type":"a{sa{sv}}t","data":[{},17]}`
			case "null-data":
				data = `{"type":"a{sa{sv}}t","data":null}`
			case "zero-version":
				data = strings.Replace(data, `,17]`, `,0]`, 1)
			case "unknown-field":
				data = strings.Replace(data, `"type":`, `"unknown":true,"type":`, 1)
			}
			if _, _, err := fabricNMSettingsReply([]byte(data), true); err == nil {
				t.Fatal("unsupported or incomplete method reply became proof")
			}
		})
	}
	for _, terminal := range []string{"established", "cleaned"} {
		h := generatedNativeTestHost(t, true)
		h.receipt.NM.GeneratedPauseAttempted = true
		h.receipt.NM.PolicyEstablished = terminal == "established"
		h.receipt.NM.CleanupConfirmed = terminal == "cleaned"
		if fabricNMReceiptValid(h.receipt) {
			t.Fatal("unresolved pause acquired a terminal receipt")
		}
	}
}

func TestFabricGeneratedPauseRechecksAfterDurableIntent(t *testing.T) {
	for _, mode := range []string{"usable", "replacement", "applied-changed", "settled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := generatedNativeTestHost(t, true)
				ops := h.io()
				save := ops.save
				injected := false
				ops.save = func(receipt fabricNativeReceipt) error {
					if err := save(receipt); err != nil {
						return err
					}
					if receipt.NM.GeneratedPauseAttempted && !injected {
						injected = true
						switch mode {
						case "usable":
							h.state = 100
							h.native["192.0.2.10/24"] = fabricNativeAddress{Family: "inet", Scope: "global", Local: "192.0.2.10", PrefixLen: 24, Valid: json.RawMessage(`4294967295`), Preferred: json.RawMessage(`4294967295`)}
						case "replacement":
							h.activePath = fabricNMRoot + "/ActiveConnection/9"
							h.profiles[0].ActivePath = h.activePath
						case "applied-changed":
							h.appliedChanged = true
						case "settled":
							h.state, h.deviceUUID, h.activePath = 30, "", ""
							h.profiles[0].Active, h.profiles[0].Device, h.profiles[0].ActivePath = false, "", ""
							delete(h.native, "fe80::42/64")
						}
					}
					return nil
				}
				err := fabricNMAdd(context.Background(), h.iface, h.receipt, ops)
				if !injected || h.pauses != 0 || (err == nil) != (mode == "settled") {
					t.Fatalf("stale durable-boundary dispatch: mode=%s error=%v pauses=%d", mode, err, h.pauses)
				}
				if !h.latestReceipt().NM.GeneratedPauseAttempted {
					t.Fatal("durable one-shot intent was lost")
				}
				if mode != "settled" && fabricNMAdd(context.Background(), h.iface, h.latestReceipt(), h.io()) == nil {
					t.Fatal("refused intent authorized another apply")
				}
			})
		})
	}
}
