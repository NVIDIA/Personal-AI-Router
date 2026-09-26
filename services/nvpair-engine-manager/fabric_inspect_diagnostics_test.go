// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// These tests call only the existing in-memory NetworkManager query seam.
// No native interface, process, configuration file, or network is inspected.
func inspectTestNMQuery(stage, command string, args []string) bool {
	switch stage {
	case "bus":
		return command == "busctl" && slices.Contains(args, "GetId")
	case "owner":
		return command == "busctl" && slices.Contains(args, "GetNameOwner")
	case "device":
		return command == "nmcli" && slices.Contains(args, "device")
	case "profiles":
		return command == "nmcli" && slices.Contains(args, "UUID,TYPE,DBUS-PATH,ACTIVE,DEVICE,ACTIVE-PATH,FILENAME")
	case "profile-flags":
		return command == "busctl" && slices.Contains(args, fabricNMSettings) && slices.Contains(args, "Flags")
	}
	return false
}

func inspectTestNMCode(t *testing.T, err error, want string) {
	t.Helper()
	var tagged *fabricInspectError
	if !errors.As(err, &tagged) || tagged.Code != want || !validFabricFailureCode(tagged.Code) {
		t.Fatalf("wrong first inspection category: got=%v want=%s", err, want)
	}
	if strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("untrusted query text escaped through the inspection error")
	}
	data, marshalErr := json.Marshal(struct {
		Code string `json:"code"`
	}{tagged.Code})
	if marshalErr != nil || strings.Contains(string(data), "synthetic-private") {
		t.Fatal("public inspection category contains private query text")
	}
}

func TestFabricInspectNMFirstQueryCategory(t *testing.T) {
	for _, stage := range []string{"bus", "owner", "device", "profiles", "profile-flags"} {
		t.Run(stage, func(t *testing.T) {
			h := fabricNMTestNew(t)
			if stage == "profile-flags" {
				h.profiles = []fabricNMProfile{h.ownedProfile()}
			}
			injected := errors.New("synthetic-private-query-error")
			failed := false
			run := func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if failed {
					t.Fatal("queries continued after the first failure")
				}
				if inspectTestNMQuery(stage, command, args) {
					failed = true
					return nil, injected
				}
				return h.run(ctx, command, args...)
			}
			_, err := fabricNMRead(context.Background(), run, []fabricInterface{h.iface})
			code := map[string]string{"bus": "network-manager-bus-unavailable", "owner": "network-manager-owner-unavailable", "device": "network-manager-device-unavailable", "profiles": "network-manager-profiles-unavailable", "profile-flags": "network-manager-profiles-unavailable"}[stage]
			inspectTestNMCode(t, err, code)
			if !failed || !errors.Is(err, injected) {
				t.Fatal("the original query cause was replaced by a lower helper")
			}
			if creates, activates, deletes := h.mutations(); creates != 0 || activates != 0 || deletes != 0 || h.policyChanges() != 0 || len(h.saved) != 0 {
				t.Fatal("inspection changed mutation or rollback policy")
			}
		})
	}
}

func TestFabricInspectNMFirstTypedAndContextCauseSurvive(t *testing.T) {
	for _, mode := range []string{"typed", "cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			h := fabricNMTestNew(t)
			var cause error
			want := "route-query-failed"
			switch mode {
			case "typed":
				cause = fabricInspectionError(want, errors.New("synthetic-private-first-cause"))
			case "cancelled":
				cause = fmt.Errorf("synthetic-private-context: %w", context.Canceled)
				want = "cancelled"
			case "deadline":
				cause = fmt.Errorf("synthetic-private-context: %w", context.DeadlineExceeded)
				want = "deadline-exceeded"
			}
			calls := 0
			_, err := fabricNMRead(context.Background(), func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, cause }, []fabricInterface{h.iface})
			inspectTestNMCode(t, err, want)
			if calls != 1 || !errors.Is(err, cause) {
				t.Fatal("query failure was retried or replaced")
			}
			if mode == "typed" && err != cause {
				t.Fatal("an outer stage replaced the already-typed first cause")
			}
			if outer := fabricInspectionError("network-manager-query-failed", err); outer != err {
				t.Fatal("native outer inspection replaced the first cause")
			}
		})
	}
}

func TestFabricInspectNMDataAndGenerationFailures(t *testing.T) {
	for _, mode := range []string{"bus-data", "owner-data", "device-data", "profiles-data", "profile-flags-data", "owner-generation", "bus-generation", "final-owner-query", "final-bus-query"} {
		t.Run(mode, func(t *testing.T) {
			h := fabricNMTestNew(t)
			if mode == "profile-flags-data" {
				h.profiles = []fabricNMProfile{h.ownedProfile()}
			}
			ownerReads, busReads := 0, 0
			hit := false
			run := func(ctx context.Context, command string, args ...string) ([]byte, error) {
				if hit {
					t.Fatal("query continued after decisive invalid data or generation")
				}
				if inspectTestNMQuery("owner", command, args) {
					ownerReads++
				}
				if inspectTestNMQuery("bus", command, args) {
					busReads++
				}
				stage, data := strings.CutSuffix(mode, "-data")
				if data && inspectTestNMQuery(stage, command, args) {
					hit = true
					return []byte("synthetic-private-invalid-data\n"), nil
				}
				if ownerReads == 2 && inspectTestNMQuery("owner", command, args) {
					if mode == "owner-generation" {
						hit = true
						return []byte("s \":1.99\"\n"), nil
					}
					if mode == "final-owner-query" {
						hit = true
						return nil, errors.New("synthetic-private-final-owner")
					}
				}
				if busReads == 2 && inspectTestNMQuery("bus", command, args) {
					if mode == "bus-generation" {
						hit = true
						return []byte("s \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\n"), nil
					}
					if mode == "final-bus-query" {
						hit = true
						return nil, errors.New("synthetic-private-final-bus")
					}
				}
				return h.run(ctx, command, args...)
			}
			_, err := fabricNMRead(context.Background(), run, []fabricInterface{h.iface})
			want := "network-manager-generation-changed"
			switch mode {
			case "bus-data", "final-bus-query":
				want = "network-manager-bus-unavailable"
			case "owner-data", "final-owner-query":
				want = "network-manager-owner-unavailable"
			case "device-data":
				want = "network-manager-device-unavailable"
			case "profiles-data", "profile-flags-data":
				want = "network-manager-profiles-unavailable"
			}
			inspectTestNMCode(t, err, want)
			if !hit {
				t.Fatal("fixture did not reach the intended inspection boundary")
			}
		})
	}
}

func TestFabricInspectNMSuccessKeepsFactsAndBlockers(t *testing.T) {
	h := fabricNMTestNew(t)
	snapshot, err := fabricNMRead(context.Background(), h.run, []fabricInterface{h.iface})
	if err != nil || snapshot.BusID != h.busID || snapshot.Owner != h.owner || len(snapshot.Devices) != 1 || len(snapshot.Profiles) != 0 {
		t.Fatalf("successful inventory changed: %+v %v", snapshot, err)
	}
	if blockers := fabricNMPreflight(snapshot, []fabricInterface{h.iface}, ""); len(blockers) != 0 {
		t.Fatal("inspection tags introduced a blocker")
	}
	h.state = 100
	h.deviceUUID = h.receipt.NM.UUID
	h.activePath = fabricNMRoot + "/ActiveConnection/9"
	snapshot, err = fabricNMRead(context.Background(), h.run, []fabricInterface{h.iface})
	if err != nil || len(fabricNMPreflight(snapshot, []fabricInterface{h.iface}, "")) == 0 {
		t.Fatal("successful policy blockers became execution errors or disappeared")
	}
	if creates, activates, deletes := h.mutations(); creates != 0 || activates != 0 || deletes != 0 || h.policyChanges() != 0 || len(h.saved) != 0 {
		t.Fatal("inspection-only queries changed native policy")
	}
}
