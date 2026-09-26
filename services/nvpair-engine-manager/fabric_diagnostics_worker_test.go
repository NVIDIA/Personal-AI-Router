// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestFabricInspectProviderRetainsBoundFailureCode(t *testing.T) {
	s, plan, request := fabricWorkerFixture(t)
	request.Method = "inspect"
	fabricWorkerFakeClient(s, func(context.Context, string, io.Reader) ([]byte, error) {
		return json.Marshal(map[string]any{
			"protocol": "pair-fabric-address/4", "operationId": request.OperationID,
			"nodeId": plan.NodeID, "principal": plan.Principal, "method": "inspect",
			"facts": fabricNativeFacts{}, "cleanupConfirmed": false,
			"failureCode": "network-manager-device-unavailable",
		})
	})
	result, err := s.runWorker(context.Background(), plan, request)
	if err == nil || err.Error() != "network-manager-device-unavailable" {
		t.Fatalf("bound inspection failure was lost: error=%v result=%+v", err, result)
	}
	if result.Facts.Digest != "" || len(result.Facts.Routes) != 0 || len(result.Facts.Blockers) != 0 || result.CleanupConfirmed {
		t.Fatal("inspection failure was presented as usable facts or cleanup")
	}
}

func fabricDiagnosticWorkerFixture() (fabricWorkerRequest, fabricInventory) {
	first := fabricNativeTestInterface()
	first.Addresses = []string{}
	second := first
	second.Name, second.Index, second.MAC, second.Address, second.RDMADevices = "enP2p1s0f1np012", 43, "02:00:00:00:00:43", "172.31.240.5/30", []string{"mlx5_5"}
	request := fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: strings.Repeat("a", 32), Target: fabricTarget{
		NodeID: "node-a", Principal: "principal-a", SwitchID: first.PhysicalPort.SwitchID, PortName: first.PhysicalPort.PortName, Interfaces: []fabricInterface{first, second},
	}}
	positive := true
	inventory := fabricInventory{NodeID: "node-a", Principal: "principal-a", Routes: []string{}}
	for _, iface := range request.Target.Interfaces {
		inventory.Interfaces = append(inventory.Interfaces, fabricObservedInterface{fabricInterface: iface, Up: true, Physical: &positive, Carrier: &positive, SpeedMbps: 200000})
	}
	return request, inventory
}

func fabricDiagnosticIO(request fabricWorkerRequest, inventory fabricInventory, calls *[]string) fabricWorkerIO {
	return fabricWorkerIO{
		identity: func(context.Context) (string, error) {
			*calls = append(*calls, "identity")
			return request.Target.Principal, nil
		},
		inventory: func(context.Context) (fabricInventory, error) {
			*calls = append(*calls, "inventory")
			return inventory, nil
		},
		inspect: func(context.Context, []fabricInterface, bool) (fabricNativeFacts, error) {
			*calls = append(*calls, "inspect")
			return fabricNativeFacts{Digest: strings.Repeat("a", 64), Routes: []string{}, Blockers: []string{}}, nil
		},
		add: func(_ context.Context, iface fabricInterface, operation string, seconds int, _ bool) error {
			*calls = append(*calls, "add:"+iface.Name)
			return nil
		},
		remove: func(_ context.Context, iface fabricInterface, operation string) error {
			*calls = append(*calls, "remove:"+iface.Name)
			return nil
		},
	}
}

func fabricDiagnosticRequestBytes(t *testing.T, request fabricWorkerRequest) []byte {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestFabricInspectWorkerForwardsReviewedPauseApproval(t *testing.T) {
	request, inventory := fabricDiagnosticWorkerFixture()
	request.SelectedPortPauseApproved = true
	var calls []string
	io := fabricDiagnosticIO(request, inventory, &calls)
	approved := false
	io.inspect = func(_ context.Context, _ []fabricInterface, value bool) (fabricNativeFacts, error) {
		approved = value
		return fabricNativeFacts{Digest: strings.Repeat("a", 64), Routes: []string{}, Blockers: []string{}}, nil
	}
	var output bytes.Buffer
	if err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), &output, request.Target.NodeID, io); err != nil || !approved {
		t.Fatalf("reviewed selected-port approval did not reach native inspection: %v", err)
	}
}

func TestFabricInspectWorkerFailureBindingAndPartialDiscard(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"inventory-error", "inventory-unavailable"}, {"inventory-tagged", "route-query-failed"},
		{"inventory-principal", "identity-mismatch"}, {"inventory-node", "identity-mismatch"},
		{"facts-changed", "facts-changed"}, {"native-unknown", "native-inspect-failed"},
		{"native-tagged", "network-manager-query-failed"}, {"cancel-cause", "cancelled"}, {"deadline-cause", "deadline-exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, inventory := fabricDiagnosticWorkerFixture()
			calls := []string{}
			ops := fabricDiagnosticIO(request, inventory, &calls)
			originalInventory := ops.inventory
			ops.inventory = func(ctx context.Context) (fabricInventory, error) {
				facts, _ := originalInventory(ctx)
				switch tc.name {
				case "inventory-error":
					return fabricInventory{Principal: "partial-unbound-principal"}, errors.New("synthetic-private-inventory-detail")
				case "inventory-tagged":
					return fabricInventory{}, fabricInspectionError("route-query-failed", errors.New("synthetic-private-route-detail"))
				case "inventory-principal":
					facts.Principal = "changed-principal"
				case "inventory-node":
					facts.NodeID = "changed-node"
				case "facts-changed":
					facts.Interfaces[0].MTU++
				}
				return facts, nil
			}
			ops.inspect = func(context.Context, []fabricInterface, bool) (fabricNativeFacts, error) {
				calls = append(calls, "inspect")
				var cause error = errors.New("synthetic-private-native-detail")
				switch tc.name {
				case "native-tagged":
					cause = fabricInspectionError("network-manager-query-failed", cause)
				case "cancel-cause":
					cause = context.Canceled
				case "deadline-cause":
					cause = context.DeadlineExceeded
				}
				return fabricNativeFacts{Digest: "partial-private-digest", Routes: []string{"private-route"}, Blockers: []string{"private-blocker"}}, cause
			}
			var output bytes.Buffer
			if err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), &output, request.Target.NodeID, ops); err != nil {
				t.Fatal(err)
			}
			var result fabricWorkerResult
			fields, err := decodeFabricWorkerObject(output.Bytes(), &result)
			if err != nil || len(fields) != 8 || result.Protocol != fabricWorkerProtocol || result.OperationID != request.OperationID || result.NodeID != request.Target.NodeID || result.Principal != request.Target.Principal || result.Method != "inspect" || result.FailureCode != tc.code || !reflect.DeepEqual(result.Facts, fabricNativeFacts{}) || result.CleanupConfirmed {
				t.Fatalf("wrong bounded failure: %+v %v", result, err)
			}
			if bytes.Count(output.Bytes(), []byte{'\n'}) != 1 || strings.Contains(output.String(), "private") || strings.Contains(strings.Join(calls, ","), "add:") || strings.Contains(strings.Join(calls, ","), "remove:") {
				t.Fatal("failure leaked detail, emitted extra result or mutated addresses")
			}
			if strings.HasPrefix(tc.name, "inventory") || tc.name == "facts-changed" {
				if !reflect.DeepEqual(calls, []string{"identity", "inventory"}) {
					t.Fatalf("rejected facts reached native inspection: %v", calls)
				}
			}
		})
	}
}

func TestFabricInspectWorkerInvalidOrUnboundInputIsOpaque(t *testing.T) {
	for _, mode := range []string{"old-protocol", "missing-protocol", "null-protocol", "duplicate-protocol", "invalid-operation", "other-node", "blank-principal", "wrong-count", "method", "unknown-field", "truncated", "principal-mismatch", "identity-error"} {
		t.Run(mode, func(t *testing.T) {
			request, inventory := fabricDiagnosticWorkerFixture()
			switch mode {
			case "old-protocol":
				request.Protocol = "pair-fabric-address/1"
			case "invalid-operation":
				request.OperationID = "invalid"
			case "other-node":
				request.Target.NodeID = "unbound-node"
			case "blank-principal":
				request.Target.Principal = ""
			case "wrong-count":
				request.Target.Interfaces = request.Target.Interfaces[:1]
			case "method":
				request.Method = "arbitrary"
			}
			calls := []string{}
			ops := fabricDiagnosticIO(request, inventory, &calls)
			if mode == "principal-mismatch" || mode == "identity-error" {
				ops.identity = func(context.Context) (string, error) {
					calls = append(calls, "identity")
					if mode == "identity-error" {
						return "", errors.New("synthetic-private-identity")
					}
					return "other-actual-principal", nil
				}
			}
			raw := fabricDiagnosticRequestBytes(t, request)
			switch mode {
			case "missing-protocol":
				raw = []byte(strings.Replace(string(raw), `"protocol":"pair-fabric-address/4",`, "", 1))
			case "null-protocol":
				raw = []byte(strings.Replace(string(raw), `"protocol":"pair-fabric-address/4"`, `"protocol":null`, 1))
			case "duplicate-protocol":
				raw = []byte(strings.Replace(string(raw), "{", `{"protocol":"pair-fabric-address/4",`, 1))
			case "unknown-field":
				raw = []byte(strings.Replace(string(raw), "{", `{"private-extra":"synthetic-private",`, 1))
			case "truncated":
				raw = raw[:len(raw)/2]
			}
			var output bytes.Buffer
			if err := runFabricAddressWorker(context.Background(), bytes.NewReader(raw), &output, "node-a", ops); err == nil || output.Len() != 0 {
				t.Fatalf("unbound request emitted evidence: %v %q", err, output.Bytes())
			}
			want := []string{}
			if mode == "principal-mismatch" || mode == "identity-error" {
				want = []string{"identity"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("unbound request reached work: %v", calls)
			}
		})
	}
}

func TestFabricInspectProviderRejectsFailureMixedWithSuccess(t *testing.T) {
	for _, mode := range []string{"old-protocol", "missing-protocol", "null-code", "empty-code", "unknown-code", "duplicate-code", "missing-facts", "missing-cleanup", "digest", "route", "blocker", "cleanup", "rollback", "transport", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			s, plan, request := fabricWorkerFixture(t)
			request.Method = "inspect"
			if mode == "apply" || mode == "rollback" {
				request.Method = mode
			}
			fabricWorkerFakeClient(s, func(context.Context, string, io.Reader) ([]byte, error) {
				facts := map[string]any{"digest": "", "routes": []string{}, "blockers": []string{}}
				wire := map[string]any{"protocol": fabricWorkerProtocol, "operationId": request.OperationID, "nodeId": plan.NodeID, "principal": plan.Principal, "method": request.Method, "facts": facts, "cleanupConfirmed": false, "failureCode": "inventory-unavailable"}
				switch mode {
				case "old-protocol":
					wire["protocol"] = "pair-fabric-address/1"
				case "missing-protocol":
					delete(wire, "protocol")
				case "null-code":
					wire["failureCode"] = nil
				case "empty-code":
					wire["failureCode"] = ""
				case "unknown-code":
					wire["failureCode"] = "synthetic-private-code"
				case "missing-facts":
					delete(wire, "facts")
				case "missing-cleanup":
					delete(wire, "cleanupConfirmed")
				case "digest":
					facts["digest"] = strings.Repeat("a", 64)
				case "route":
					facts["routes"] = []string{"192.0.2.0/24"}
				case "blocker":
					facts["blockers"] = []string{"private-blocker"}
				case "cleanup":
					wire["cleanupConfirmed"] = true
				}
				raw, _ := json.Marshal(wire)
				if mode == "transport" {
					return raw, errors.New("synthetic-private-transport")
				}
				if mode == "duplicate-code" {
					raw = []byte(strings.Replace(string(raw), "{", `{"failureCode":"inventory-unavailable",`, 1))
				}
				if mode == "truncated" {
					raw = raw[:len(raw)/2]
				}
				return raw, nil
			})
			result, err := s.runWorker(context.Background(), plan, request)
			var typed *fabricInspectError
			want := "result-invalid"
			if mode == "transport" {
				want = "transport-unavailable"
			}
			if !errors.As(err, &typed) || typed.Code != want || !reflect.DeepEqual(result, fabricWorkerResult{}) || strings.Contains(err.Error(), "private") {
				t.Fatalf("malformed failure admitted: %+v %v", result, err)
			}
		})
	}
}

func TestFabricInspectionErrorPreservesFirstCause(t *testing.T) {
	if fabricInspectionError("inventory-unavailable", nil) != nil {
		t.Fatal("nil error became failure")
	}
	private := errors.New("synthetic-private-original")
	first := fabricInspectionError("network-manager-device-unavailable", private)
	wrapped := fabricInspectionError("inventory-unavailable", errors.Join(first, context.Canceled))
	if wrapped != first || !errors.Is(wrapped, private) || wrapped.Error() != "network-manager-device-unavailable" {
		t.Fatal("outer stage replaced first typed cause")
	}
	for _, tc := range []struct {
		cause error
		code  string
	}{{private, "native-inspect-failed"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "deadline-exceeded"}} {
		got := fabricInspectionError("unknown-private-code", tc.cause)
		if got.Error() != tc.code || !errors.Is(got, tc.cause) {
			t.Fatalf("invalid fixed fallback/cause: %v", got)
		}
	}
}

func TestFabricInspectWorkerOutputIsBoundedAndCancellationAware(t *testing.T) {
	for _, mode := range []string{"cancelled", "blocked", "short"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request, inventory := fabricDiagnosticWorkerFixture()
				calls := []string{}
				ops := fabricDiagnosticIO(request, inventory, &calls)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ops.inspect = func(context.Context, []fabricInterface, bool) (fabricNativeFacts, error) {
					if mode == "cancelled" {
						cancel()
					}
					return fabricNativeFacts{}, errors.New("synthetic-private-native")
				}
				var output bytes.Buffer
				var writer io.Writer = &output
				if mode == "blocked" {
					reader, pipe := io.Pipe()
					defer reader.Close()
					defer pipe.Close()
					writer = pipe
				}
				if mode == "short" {
					writer = fabricDiagnosticShortWriter{}
				}
				began := time.Now()
				if err := runFabricAddressWorker(ctx, bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), writer, request.Target.NodeID, ops); err == nil || output.Len() != 0 || time.Since(began) > 2*time.Second {
					t.Fatalf("incomplete report became success or exceeded budget: %v", err)
				}
			})
		})
	}
}

type fabricDiagnosticShortWriter struct{}

func (fabricDiagnosticShortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestFabricInspectLargeSuccessPreservesExistingReplyEnvelope(t *testing.T) {
	request, inventory := fabricDiagnosticWorkerFixture()
	calls := []string{}
	ops := fabricDiagnosticIO(request, inventory, &calls)
	facts := fabricNativeFacts{Digest: strings.Repeat("a", 64), Blockers: []string{}}
	for i := 0; i < 2048; i++ {
		facts.Routes = append(facts.Routes, fmt.Sprintf("10.240.%d.%d/32", i/256, i%256))
	}
	ops.inspect = func(context.Context, []fabricInterface, bool) (fabricNativeFacts, error) { return facts, nil }
	var output bytes.Buffer
	if err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), &output, request.Target.NodeID, ops); err != nil {
		t.Fatalf("previously bounded successful fact reply became an opaque failure: %v", err)
	}
	if output.Len() <= 32<<10 || output.Len() > 64<<10 {
		t.Fatal("fixture must exercise the existing larger successful reply envelope")
	}
	var decoded fabricWorkerResult
	if _, err := decodeFabricWorkerObject(output.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded.Facts, facts) || decoded.FailureCode != "" {
		t.Fatalf("larger successful facts were lost: %v", err)
	}
	s, plan, providerRequest := fabricWorkerFixture(t)
	providerRequest.Method = "inspect"
	decoded.OperationID, decoded.NodeID, decoded.Principal = providerRequest.OperationID, plan.NodeID, plan.Principal
	fabricWorkerFakeClient(s, func(context.Context, string, io.Reader) ([]byte, error) { return json.Marshal(decoded) })
	result, err := s.runWorker(context.Background(), plan, providerRequest)
	if err != nil || !reflect.DeepEqual(result.Facts, facts) || result.FailureCode != "" {
		t.Fatalf("provider narrowed successful reply envelope: %v", err)
	}
}

func TestFabricWorkerAcceptedResultLimitStaysSeparateFromTransport(t *testing.T) {
	result := fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64)}}
	for i := 0; i < 4096; i++ {
		result.Facts.Routes = append(result.Facts.Routes, fmt.Sprintf("10.240.%d.%d/32", i/256, i%256))
	}
	var output bytes.Buffer
	if err := writeFabricWorkerResult(context.Background(), &output, result); err != nil || output.Len() <= 64<<10 || output.Len() > 128<<10 {
		t.Fatalf("existing bounded transport envelope changed: %v", err)
	}
	var decoded fabricWorkerResult
	if _, err := decodeFabricWorkerObject(output.Bytes(), &decoded); err == nil {
		t.Fatal("provider widened the existing 64-KiB accepted-result limit")
	}
}

func TestFabricWorkerSuccessfulOutputRemainsBounded(t *testing.T) {
	for _, mode := range []string{"oversized", "short", "blocked", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				result := fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64)}}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var output bytes.Buffer
				var writer io.Writer = &output
				switch mode {
				case "oversized":
					result.Facts.Routes = []string{strings.Repeat("x", 128<<10)}
				case "short":
					writer = fabricDiagnosticShortWriter{}
				case "blocked":
					reader, pipe := io.Pipe()
					defer reader.Close()
					defer pipe.Close()
					writer = pipe
				case "cancelled":
					cancel()
				}
				began := time.Now()
				err := writeFabricWorkerResult(ctx, writer, result)
				if err == nil || output.Len() != 0 || time.Since(began) > 5*time.Second {
					t.Fatalf("success writer accepted partial/unbounded output: %v", err)
				}
				if mode == "blocked" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(began) != 5*time.Second) {
					t.Fatalf("blocked success output did not use caller deadline: elapsed=%v error=%v", time.Since(began), err)
				}
				if mode == "oversized" {
					raw, _ := json.Marshal(result)
					var decoded fabricWorkerResult
					if _, err := decodeFabricWorkerObject(raw, &decoded); err == nil {
						t.Fatal("provider accepted output beyond the existing envelope")
					}
				}
			})
		})
	}
}

type fabricDiagnosticDelayedWriter struct {
	output *bytes.Buffer
	delay  time.Duration
	done   chan struct{}
}

func (w fabricDiagnosticDelayedWriter) Write(data []byte) (int, error) {
	defer close(w.done)
	time.Sleep(w.delay)
	return w.output.Write(data)
}

func TestFabricWorkerSuccessDeliveryUsesExistingOperationBudget(t *testing.T) {
	for _, method := range []string{"inspect", "apply", "rollback"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request, inventory := fabricDiagnosticWorkerFixture()
				request.Method = method
				calls := []string{}
				ops := fabricDiagnosticIO(request, inventory, &calls)
				var output bytes.Buffer
				writer := fabricDiagnosticDelayedWriter{output: &output, delay: 3 * time.Second, done: make(chan struct{})}
				began := time.Now()
				err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), writer, request.Target.NodeID, ops)
				elapsed := time.Since(began)
				<-writer.done // Join the fake writer even when the old timeout wins.
				if err != nil {
					t.Fatalf("successful reply within existing operation budget was rejected after %v: %v", elapsed, err)
				}
				var result fabricWorkerResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.Protocol != fabricWorkerProtocol || result.Method != method || result.FailureCode != "" || result.CleanupConfirmed != (method == "rollback") || time.Since(began) != 3*time.Second {
					t.Fatal("delayed complete success changed its result or delivery budget")
				}
			})
		})
	}
}

func TestFabricWorkerAddressMethodsKeepOrderAndFailureSemantics(t *testing.T) {
	for _, mode := range []string{"apply", "apply-fails", "apply-generation-changed", "rollback", "rollback-fails"} {
		t.Run(mode, func(t *testing.T) {
			request, inventory := fabricDiagnosticWorkerFixture()
			request.Method = strings.Split(mode, "-")[0]
			calls := []string{}
			ops := fabricDiagnosticIO(request, inventory, &calls)
			if mode == "apply-fails" {
				ops.add = func(context.Context, fabricInterface, string, int, bool) error {
					calls = append(calls, "add-fails")
					return errors.New("synthetic-private-add")
				}
			}
			if mode == "apply-generation-changed" {
				ops.add = func(context.Context, fabricInterface, string, int, bool) error {
					calls = append(calls, "add-generation-changed")
					return fabricInspectionError("network-manager-generation-changed", errors.New("synthetic-private-generation"))
				}
			}
			if mode == "rollback-fails" {
				ops.remove = func(_ context.Context, iface fabricInterface, _ string) error {
					calls = append(calls, "remove:"+iface.Name)
					return errors.New("synthetic-private-remove")
				}
			}
			var output bytes.Buffer
			err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), &output, request.Target.NodeID, ops)
			var result fabricWorkerResult
			if mode == "apply-fails" {
				if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || result.FailureCode != "native-inspect-failed" || result.CleanupConfirmed || len(calls) != 3 || strings.Contains(output.String(), "private") {
					t.Fatalf("apply failure lost its fixed code, leaked detail or continued: %+v %v calls=%v", result, err, calls)
				}
				return
			}
			if mode == "apply-generation-changed" {
				if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || result.FailureCode != "network-manager-generation-changed" || len(calls) != 3 {
					t.Fatalf("typed pre-effect apply refusal was lost: %+v %v calls=%v", result, err, calls)
				}
				return
			}
			if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || result.Protocol != fabricWorkerProtocol || result.FailureCode != "" || result.CleanupConfirmed != (mode == "rollback") {
				t.Fatalf("address method result changed: %+v %v", result, err)
			}
			prefix := "add:"
			indices := []int{0, 1}
			if request.Method == "rollback" {
				prefix = "remove:"
				indices = []int{1, 0}
			}
			want := []string{"identity", "inventory", prefix + request.Target.Interfaces[indices[0]].Name, prefix + request.Target.Interfaces[indices[1]].Name}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("address order changed: %v", calls)
			}
		})
	}
}
