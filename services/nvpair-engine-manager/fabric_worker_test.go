// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
)

// Existing synthetic trust/account fixture plus in-memory SSH command seam.
// No SSH connection, privileged command, native query, listener, or worker runs.
func fabricWorkerFixture(t *testing.T) (*fabricService, cableLaunchPlan, fabricWorkerRequest) {
	t.Helper()
	f := newCableProductFixture(t)
	review := f.ready()
	f.s.mu.Lock()
	plans := f.s.reviews[review.ReviewID].plans
	f.s.mu.Unlock()
	if len(plans) != 2 {
		t.Fatal("synthetic exact access plans unavailable")
	}
	plan := plans[0]
	iface := fabricNativeTestInterface()
	second := iface
	second.Name, second.Index, second.MAC, second.Address, second.RDMADevices = "enP2p1s0f1np012", 43, "02:00:00:00:00:43", "172.31.240.5/30", []string{"mlx5_5"}
	request := fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "apply", OperationID: strings.Repeat("f", 32), Target: fabricTarget{
		NodeID: plan.NodeID, Principal: plan.Principal, SwitchID: iface.PhysicalPort.SwitchID, PortName: iface.PhysicalPort.PortName,
		Interfaces: []fabricInterface{iface, second},
	}}
	return f.s.m.exec.fabric, plan, request
}

func fabricWorkerFakeClient(s *fabricService, fn func(context.Context, string, io.Reader) ([]byte, error)) {
	s.m.onboarding.dial = func(_ context.Context, _ onboardingCandidate, _ onboardingAccess) (*onboardingSSH, error) {
		return &onboardingSSH{testRun: fn}, nil
	}
}

func TestFabricWorkerOwnPurposeAndSecretFraming(t *testing.T) {
	for _, method := range []string{"inspect", "apply", "rollback"} {
		t.Run(method, func(t *testing.T) {
			s, plan, request := fabricWorkerFixture(t)
			request.Method = method
			secret := "synthetic-admin-input"
			var framed *bytes.Reader
			calls := 0
			fabricWorkerFakeClient(s, func(ctx context.Context, command string, input io.Reader) ([]byte, error) {
				calls++
				if _, bounded := ctx.Deadline(); !bounded {
					t.Fatal("privileged worker lacks execution deadline")
				}
				expectedCommand := "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c " + onboardingQuote(fabricRootLaunchScript())
				if command != expectedCommand || strings.Contains(command, secret) || strings.Contains(command, request.OperationID) {
					t.Fatal("secret or request escaped fixed stdin-only launcher")
				}
				var ok bool
				framed, ok = input.(*bytes.Reader)
				if !ok {
					t.Fatal("worker input is not an owned bounded byte buffer")
				}
				data, err := io.ReadAll(input)
				if err != nil {
					t.Fatal(err)
				}
				lines := bytes.Split(data, []byte{'\n'})
				if len(lines) != 4 || string(lines[0]) != secret || len(lines[3]) != 0 || !bytes.HasPrefix(lines[1], []byte("PAIR-FABRIC-LAUNCH/1 ")) {
					t.Fatal("password/header/body/EOF framing changed")
				}
				var header map[string]json.RawMessage
				if json.Unmarshal(bytes.TrimPrefix(lines[1], []byte("PAIR-FABRIC-LAUNCH/1 ")), &header) != nil || len(header) != 8 || header["runtime"] == nil || header["receipt"] != nil {
					t.Fatal("exact current runtime identity was not preserved")
				}
				if bytes.Contains(lines[1], []byte(secret)) || bytes.Contains(lines[2], []byte(secret)) || header["argv"] != nil || header["environment"] != nil {
					t.Fatal("secret or arbitrary execution fields entered retained/public request")
				}
				var decoded fabricWorkerRequest
				if onboardingDecode(lines[2], &decoded) != nil || !reflect.DeepEqual(decoded, request) {
					t.Fatal("worker request changed its exact method, operation or targets")
				}
				return json.Marshal(fabricWorkerResult{Protocol: fabricWorkerProtocol, OperationID: request.OperationID, NodeID: plan.NodeID, Principal: plan.Principal, Method: method, CleanupConfirmed: method == "rollback"})
			})
			result, err := s.runWorker(context.Background(), plan, request)
			if err != nil || calls != 1 || result.CleanupConfirmed != (method == "rollback") {
				t.Fatalf("fixed worker exchange failed: %v", err)
			}
			if len(s.m.cables.runs) != 0 {
				t.Fatal("fabric request reused or approved a cable run")
			}
			if _, err := framed.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			cleared, _ := io.ReadAll(framed)
			if len(bytes.Trim(cleared, "\x00")) != 0 {
				t.Fatal("volatile input buffer retained elevation secret after launch")
			}
		})
	}
}

func TestFabricWorkerRejectsChangedResultWithoutExposingOutput(t *testing.T) {
	for _, mode := range []string{"operation", "node", "principal", "method", "unknown-field", "stderr-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, plan, request := fabricWorkerFixture(t)
			fabricWorkerFakeClient(s, func(context.Context, string, io.Reader) ([]byte, error) {
				result := fabricWorkerResult{Protocol: fabricWorkerProtocol, OperationID: request.OperationID, NodeID: plan.NodeID, Principal: plan.Principal, Method: request.Method}
				switch mode {
				case "operation":
					result.OperationID = strings.Repeat("e", 32)
				case "node":
					result.NodeID = "other-node"
				case "principal":
					result.Principal = "other-principal"
				case "method":
					result.Method = "rollback"
				case "unknown-field":
					return []byte(`{"untrusted":"synthetic-admin-input"}`), nil
				case "stderr-failure":
					return []byte("synthetic-admin-input"), errors.New("synthetic-admin-input")
				}
				return json.Marshal(result)
			})
			_, err := s.runWorker(context.Background(), plan, request)
			if err == nil || strings.Contains(err.Error(), "synthetic-admin-input") {
				t.Fatal("changed output accepted or exposed raw secret-bearing error")
			}
		})
	}
}

func TestFabricWorkerRejectsChangedAccessOrBinaryBeforePrivilegedCommand(t *testing.T) {
	for _, mode := range []string{"old-protocol", "missing-protocol", "target-node", "target-principal", "operation", "interfaces", "restored-access-generation", "hash", "certificate", "size"} {
		t.Run(mode, func(t *testing.T) {
			s, plan, request := fabricWorkerFixture(t)
			calls := 0
			fabricWorkerFakeClient(s, func(context.Context, string, io.Reader) ([]byte, error) {
				calls++
				return nil, errors.New("unexpected privileged command")
			})
			switch mode {
			case "old-protocol":
				request.Protocol = "pair-fabric-address/1"
			case "missing-protocol":
				request.Protocol = ""
			case "target-node":
				request.Target.NodeID = "other"
			case "target-principal":
				request.Target.Principal = "other"
			case "operation":
				request.OperationID = "not-an-operation"
			case "interfaces":
				request.Target.Interfaces = request.Target.Interfaces[:1]
			case "restored-access-generation":
				data, _ := json.Marshal(plan)
				var restored cableLaunchPlan
				if json.Unmarshal(data, &restored) != nil || restored.AccessGeneration != "" {
					t.Fatal("persistent plan unexpectedly contains volatile access generation")
				}
				plan = restored
			default:
				s.m.cables.inspect = func(_ context.Context, _ *onboardingSSH, p cableLaunchPlan, _ *clustertrust.Mesh) (cableLaunchPlan, error) {
					switch mode {
					case "hash":
						p.WorkerSHA256 = strings.Repeat("e", 64)
					case "certificate":
						p.CertificateSHA256 = strings.Repeat("e", 64)
					case "size":
						p.WorkerBytes++
					}
					return p, nil
				}
			}
			if _, err := s.runWorker(context.Background(), plan, request); err == nil || calls != 0 {
				t.Fatal("changed access, target or executable reached privileged launch")
			}
		})
	}
}

func TestFabricWorkerCapabilityIsPurposeSpecificAndUnprivileged(t *testing.T) {
	for _, mode := range []string{"valid", "old-fabric-worker", "old-cable-worker", "timed-policy", "wrong-bound", "unknown-field", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s, plan, _ := fabricWorkerFixture(t)
			calls := 0
			fabricWorkerFakeClient(s, func(_ context.Context, command string, input io.Reader) ([]byte, error) {
				calls++
				if command != onboardingQuote(plan.Runtime.WorkerPath)+" --fabric-address-capabilities-json" || input != nil || strings.Contains(command, "sudo") {
					t.Fatal("capability query elevated or carried access input")
				}
				switch mode {
				case "old-fabric-worker":
					return []byte(`{"protocol":"pair-fabric-address/1","persistence":"until-reboot","maxInterfaces":2}`), nil
				case "old-cable-worker":
					return []byte(`{"protocol":"pair-cable-worker/1","maxSeconds":10,"maxPorts":2}`), nil
				case "timed-policy":
					return []byte(`{"protocol":"pair-fabric-address/4","persistence":"operation-only","maxInterfaces":2}`), nil
				case "wrong-bound":
					return []byte(`{"protocol":"pair-fabric-address/4","persistence":"until-reboot","maxInterfaces":8}`), nil
				case "unknown-field":
					return []byte(`{"protocol":"pair-fabric-address/4","persistence":"until-reboot","maxInterfaces":2,"extra":true}`), nil
				case "missing":
					return nil, errors.New("unknown flag")
				}
				return []byte(`{"protocol":"pair-fabric-address/4","persistence":"until-reboot","maxInterfaces":2}`), nil
			})
			err := s.inspectWorkerCapability(context.Background(), plan)
			if (err == nil) != (mode == "valid") || calls != 1 {
				t.Fatalf("capability disposition incorrect: %v", err)
			}
		})
	}
}

func TestFabricWorkerRejectsInvalidInputBeforeLocalOrNativeIO(t *testing.T) {
	iface := fabricNativeTestInterface()
	valid := fabricWorkerRequest{Protocol: fabricWorkerProtocol, Method: "unsupported", OperationID: strings.Repeat("a", 32), Target: fabricTarget{NodeID: "node-a", Principal: "principal-a", Interfaces: []fabricInterface{iface, iface}}}
	data, _ := json.Marshal(valid)
	for _, input := range [][]byte{data, []byte(`{}`), []byte(`{"method":"apply","unexpected":true}`), bytes.Repeat([]byte{'x'}, (32<<10)+1)} {
		var output bytes.Buffer
		if err := runFabricAddressOnce(context.Background(), bytes.NewReader(input), &output, "node-a", 0, ""); err == nil || output.Len() != 0 {
			t.Fatal("invalid input reached local/native work or returned fabricated success")
		}
	}
}

func TestFabricWorkerLauncherAndStartupStayPurposeBound(t *testing.T) {
	script := fabricRootLaunchScript()
	for _, required := range []string{"prefix=b'PAIR-FABRIC-LAUNCH/1 '", "item=os.read(0,1)", "os.geteuid()!=0", "os.environ.get('SUDO_UID','-1')", "h['workerSha256']", "h['certificateSha256']", "os.memfd_create('nvpair-fabric-worker'", "fcntl.F_SEAL_WRITE|fcntl.F_SEAL_GROW|fcntl.F_SEAL_SHRINK|fcntl.F_SEAL_SEAL", "'--fabric-address-once'", "os.execve(executable,argv,"} {
		if !strings.Contains(script, required) {
			t.Fatalf("fixed launch trust/framing guard absent: %s", required)
		}
	}
	for _, forbidden := range []string{"PAIR-CABLE-LAUNCH/1 ", "'--cable-probe-once'", "sys.stdin.buffer", "subprocess", "os.system", "netplan apply"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("fabric launcher acquired a foreign/buffered action: %s", forbidden)
		}
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	capability, worker, service := strings.Index(text, "if *fabricAddressCapabilities {"), strings.Index(text, "if *fabricAddressOnce {"), strings.Index(text, `applog.Init("nvpair-engine-manager"`)
	if capability < 0 || worker < 0 || service < 0 || capability >= service || worker >= service || !strings.Contains(text[worker:service], "return") {
		t.Fatal("one-shot worker or capability starts normal services")
	}
}
