// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const vllmSystemRankOwner = "pair-vllm-rank-system-v1"

type vllmSystemRankPolicy struct {
	EffectivePrograms struct {
		Ingress []uint32 `json:"ingress"`
		Egress  []uint32 `json:"egress"`
	} `json:"effectivePrograms"`
	IngressAllowed           bool     `json:"ingressAllowed"`
	ForbiddenIngressReceived bool     `json:"forbiddenIngressReceived"`
	EgressDenied             bool     `json:"egressDenied"`
	Transport                string   `json:"transport"`
	RDMADisabled             bool     `json:"rdmaDisabled"`
	SocketPayloadFallback    bool     `json:"socketPayloadFallback"`
	SocketInterface          string   `json:"socketInterface,omitempty"`
	HCAs                     []string `json:"hcas,omitempty"`
	NetGDRLevel              *int     `json:"netGdrLevel,omitempty"`
	NetGDRC2C                *int     `json:"netGdrC2c,omitempty"`
	NetGDRRead               *int     `json:"netGdrRead,omitempty"`
	NetPlugin                string   `json:"netPlugin,omitempty"`
	EnvPlugin                string   `json:"envPlugin,omitempty"`
	GINPlugin                string   `json:"ginPlugin,omitempty"`
	SubnetAwareRouting       *int     `json:"subnetAwareRouting,omitempty"`
	SubnetPrefixLength       *int     `json:"subnetPrefixLength,omitempty"`
	MergeNICs                *int     `json:"mergeNICs,omitempty"`
}

type vllmSystemRankResult struct {
	stdoutBytes      int                     // Process output length only; never serialized as a native field.
	State            string                  `json:"state"`
	Unit             string                  `json:"unit,omitempty"`
	PlanHash         string                  `json:"planHash,omitempty"`
	Supervisor       *vllmRankSystemIdentity `json:"supervisor,omitempty"`
	EffectsApplied   bool                    `json:"effectsApplied,omitempty"`
	CleanupConfirmed bool                    `json:"cleanupConfirmed"`
	MainPID          int                     `json:"mainPid,omitempty"`
	OwnedPIDs        []int                   `json:"ownedPids,omitempty"`
	Policy           *vllmSystemRankPolicy   `json:"policy,omitempty"`
	BPF              map[string][]uint32     `json:"bpf,omitempty"`
	Transport        string                  `json:"transport,omitempty"`
	Tombstone        bool                    `json:"tombstone,omitempty"`
	StartFenced      bool                    `json:"startFenced,omitempty"`
}

func vllmSystemPlanHash(plan vllmRankSystemPlan) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(plan); err != nil {
		return "", err
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	// The first fixed native worker accepts ASCII model/runtime filename forms.
	// Do not silently disagree with Python's ensure_ascii canonical plan hash.
	for _, value := range data {
		if value >= 128 {
			return "", errors.New("native rank plan needs the supported ASCII path form")
		}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func buildVLLMSystemRankPlan(b vllmGroupBinding, receipt vllmRuntimeReceipt) (vllmRankSystemPlan, error) {
	if b.Rank < 0 || b.Rank >= len(b.Plan.Members) || receipt.Environment == "" || vllmRankRuntimeDigest(receipt) != b.Plan.Members[b.Rank].RuntimeDigest {
		return vllmRankSystemPlan{}, errors.New("native system rank runtime differs from the reviewed content")
	}
	return buildVLLMSystemRankPlanAt(b, receipt.Environment)
}

func buildVLLMSystemClosurePlan(st *engineState, b vllmGroupBinding) (vllmRankSystemPlan, error) {
	runtimeDir, err := filepath.Abs(st.installDir)
	if err != nil || runtimeDir == "" {
		return vllmRankSystemPlan{}, errors.New("native rank closure root is unavailable")
	}
	return buildVLLMSystemRankPlanAt(b, runtimeDir)
}

func buildVLLMSystemRankPlanAt(b vllmGroupBinding, runtimeDir string) (vllmRankSystemPlan, error) {
	var result vllmRankSystemPlan
	if runtime.GOOS != "linux" || os.Getuid() <= 0 || b.Rank < 0 || b.Rank >= len(b.Plan.Members) || !filepath.IsAbs(runtimeDir) {
		return result, errors.New("native system rank requires a normal Linux account and owned runtime root")
	}
	member := b.Plan.Members[b.Rank]
	if member.Resources == nil || member.Placement == nil || b.Plan.Members[0].Placement == nil {
		return result, errors.New("native rank has no reviewed resources or placement")
	}
	manager := vllmRankSystemIdentity{PID: os.Getpid(), StartTicks: vllmSystemProcessTicks(os.Getpid()), UID: os.Getuid()}
	if manager.StartTicks == "" {
		return result, errors.New("current engine-manager process identity is unavailable")
	}
	result = vllmRankSystemPlan{Owner: vllmSystemRankOwner, RunID: b.RunID, Generation: b.Generation,
		Rank: b.Rank, PlanDigest: b.PlanDigest, NodeID: member.NodeID, Model: b.Plan.Model,
		ModelDigest: member.ModelDigest, RuntimeDigest: member.RuntimeDigest, ConfigSHA256: b.Plan.Topology.ConfigSHA256,
		UID: manager.UID, Manager: manager, RuntimeDir: runtimeDir, ModelPath: member.Placement.ModelPath,
		Resources: vllmRankSystemResources{Memory: member.Resources.GPUMemoryUtilization, Length: member.Resources.MaxModelLen, GPU: member.Resources.GPUUUID, TP: member.Resources.TensorParallelSize},
		GPUUUID:   member.GPUUUID, LocalAddress: member.Placement.Address, CoordinatorAddress: b.Plan.Members[0].Placement.Address,
		APIPort: member.Placement.APIPort, MasterPort: member.Placement.MasterPort,
		Topology: vllmRankSystemTopology{TP: b.Plan.Topology.TensorParallel, PP: b.Plan.Topology.PipelineParallel, DP: b.Plan.Topology.DataParallel, EP: b.Plan.Topology.ExpertParallel, EPLB: b.Plan.Topology.EPLB, RedundantExperts: b.Plan.Topology.RedundantExperts, ContextLength: b.Plan.Topology.ContextLength, MaxSequences: b.Plan.Topology.MaxSequences, KVCacheMemoryBytes: b.Plan.Topology.KVCacheMemoryBytes, MTP: b.Plan.Topology.MTP, DFlash: b.Plan.Topology.DFlash, FlashInferAutotune: b.Plan.Topology.FlashInferAutotune},
		Limits:   vllmRankSystemLimits{RuntimeSeconds: b.Plan.Limits.RuntimeSeconds, MemoryMaxBytes: b.Plan.Limits.MemoryMaxBytes, TasksMax: b.Plan.Limits.TasksMax}}
	if isQwen38ProfileModel(b.Plan.Model) {
		if err := validateVLLMGroupTransport(b.Plan); err != nil || member.Fabric == nil {
			return vllmRankSystemPlan{}, errors.New("native Qwen3.8 rank lacks the exact qualified RoCE plan")
		}
		transport := *b.Plan.Transport
		result.Transport = &transport
		result.RDMALanes = append([]vllmGroupRDMALane(nil), member.Fabric.Lanes...)
	}
	if ds := b.Plan.DirectSocket; ds != nil {
		if err := validateVLLMGroupDirectSocket(b.Plan); err != nil {
			return vllmRankSystemPlan{}, errors.New("native rank lacks the exact qualified direct socket plan")
		}
		lane := ds.Lanes[b.Rank]
		result.DirectSocket = &vllmRankDirectSocket{Mode: ds.Mode, OperationID: ds.OperationID, QualificationSHA256: ds.QualificationSHA256,
			InterfaceName: lane.InterfaceName, InterfaceIndex: lane.InterfaceIndex, MAC: lane.MAC, LocalAddress: lane.LocalAddress,
			PeerAddress: lane.PeerAddress, PeerNodeID: ds.Lanes[1-b.Rank].NodeID}
	}
	devices, err := vllmRankDevicePaths(member.GPUUUID, result.RDMALanes)
	if err != nil {
		return vllmRankSystemPlan{}, err
	}
	result.Devices = devices
	for _, selected := range b.Plan.Members {
		if selected.Placement == nil {
			return vllmRankSystemPlan{}, errors.New("every native peer needs a reviewed address")
		}
		result.Peers = append(result.Peers, selected.Placement.Address)
	}
	_, err = vllmSystemPlanHash(result)
	return result, err
}

// Reuse the normal administrator choice and fixed sudo/stdin pattern. With a
// cached/NOPASSWD route sudo may leave the password line unread; the fixed loader
// selects only the final JSON line. No secret enters argv, environment or output.
func vllmSystemRankInvocation(action string, plan vllmRankSystemPlan, supervisor *vllmRankSystemIdentity, elevation *diagnosticPackageElevation) ([]string, []byte, error) {
	argv, body, err := fixedVLLMSystemRankCommand(action, plan, supervisor)
	if err != nil {
		return nil, nil, err
	}
	if action == "normal-stop" || action == "normal-status" {
		return argv, body, nil
	}
	if elevation == nil || elevation.NodeID != plan.NodeID {
		return nil, nil, errors.New("administrator access for this rank is required in PAIR")
	}
	passwords, consent, err := packageElevation([]diagnosticPackageElevation{*elevation}, []diagnosticInspectionTarget{{NodeID: plan.NodeID}})
	clear(passwords)
	if err != nil || !consent[plan.NodeID] {
		return nil, nil, errors.New("invalid rank administrator selection")
	}
	const original = "raw=sys.stdin.buffer.read(65537)"
	const framed = "lines=sys.stdin.buffer.read(69634).splitlines()\nif len(lines) not in (1,2): raise SystemExit('bounded fixed input required')\nraw=lines[-1]"
	if len(argv) != 5 || strings.Count(argv[4], original) != 1 {
		return nil, nil, errors.New("fixed rank loader changed")
	}
	argv[4] = strings.Replace(argv[4], original, framed, 1)
	input := make([]byte, 0, len(body)+len(elevation.ElevationPassword)+2)
	command := []string{"/usr/bin/sudo", "-n", "--"}
	if !elevation.NonInteractive {
		command = []string{"/usr/bin/sudo", "-S", "-p", "", "--"}
		input = append(input, elevation.ElevationPassword...)
		input = append(input, '\n')
	}
	input = append(input, body...)
	input = append(input, '\n')
	return append(command, argv...), input, nil
}

func runVLLMSystemRank(ctx context.Context, action string, plan vllmRankSystemPlan, supervisor *vllmRankSystemIdentity, elevation *diagnosticPackageElevation) (vllmSystemRankResult, error) {
	var result vllmSystemRankResult
	if runtime.GOOS != "linux" {
		return result, errVLLMGroupNativeUnavailable
	}
	argv, input, err := vllmSystemRankInvocation(action, plan, supervisor, elevation)
	if err != nil {
		return result, rankStartError("admission", "invocation_invalid", err, 0)
	}
	defer clear(input)
	ctx, cancel := context.WithTimeout(ctx, vllmSystemRankActionBudget(action, plan.Model))
	defer cancel()
	raw, err := vllmSystemProcessInput(ctx, argv, input)
	if err != nil {
		return result, rankStartError("helper", "process_failed", err, len(raw))
	}
	if strictDiagnosticJSON(raw, &result) != nil {
		return result, rankStartError("native-return", "invalid_json", nil, len(raw))
	}
	result.stdoutBytes = len(raw)
	expected, err := vllmSystemPlanHash(plan)
	if err != nil {
		return result, err
	}
	unit := fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", plan.RunID, plan.Generation, plan.Rank)
	if (action != "normal-stop" || result.State == "closed") && (result.PlanHash != expected || result.Unit != unit) {
		return vllmSystemRankResult{}, rankStartError("native-return", "binding_mismatch", nil, len(raw))
	}
	return result, nil
}

func vllmSystemRankActionBudget(action, model string) time.Duration {
	if action == "start" && isQwen38ProfileModel(model) {
		return 10 * time.Minute
	}
	return 90 * time.Second
}
