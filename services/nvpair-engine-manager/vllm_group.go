// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The desktop drives this lifecycle through Engine Manager; Linux attaches the
// fixed system-unit participant owner. Source admission is not live acceptance:
// model construction, kernels, transport and cleanup still require runtime proof.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

var errVLLMGroupNativeUnavailable = errors.New("distributed vLLM activation requires the supported Linux system owner, administrator approval and the reviewed per-rank Socket or Qwen RoCE containment policy")

const (
	vllmGroupQualificationBudget = 3 * time.Minute
	// Missing-rank reconciliation can first close one exact retained predecessor
	// and then fence the requested operation. Each root Stop can take about 12s.
	vllmGroupParticipantCleanupBudget = 30 * time.Second
	// Up to three participants stop serially. Leave room to join the existing
	// cleanup and observe its final state without shortening the last peer's cap.
	vllmGroupCleanupBudget = 3*vllmGroupParticipantCleanupBudget + 5*time.Second
)

type vllmGroupMember struct {
	Resources                  *vllmResourceSettings  `json:"resources,omitempty"`
	Placement                  *vllmGroupPlacement    `json:"placement,omitempty"`
	NodeID                     string                 `json:"nodeId"`
	PinSHA256                  string                 `json:"pinSha256"`
	GPUUUID                    string                 `json:"gpuUuid"`
	ModelDigest                string                 `json:"modelDigest"`
	RuntimeDigest              string                 `json:"runtimeDigest"`
	RuntimeCompatibilitySHA256 string                 `json:"runtimeCompatibilitySha256"`
	Fabric                     *vllmGroupMemberFabric `json:"fabric,omitempty"`
}

type vllmGroupLimits struct {
	RuntimeSeconds int   `json:"runtimeSeconds"`
	MemoryMaxBytes int64 `json:"memoryMaxBytes"`
	TasksMax       int   `json:"tasksMax"`
}

func vllmGroupLimitsForModel(model string) vllmGroupLimits {
	if isQwen38ProfileModel(model) {
		// This is nominally 16 GiB below a 128 GiB Spark before other host
		// consumers; live free-memory and construction still gate Start.
		return vllmGroupLimits{RuntimeSeconds: 3600, MemoryMaxBytes: 112 << 30, TasksMax: 512}
	}
	return vllmGroupLimits{RuntimeSeconds: 600, MemoryMaxBytes: 16 << 30, TasksMax: 512}
}

type vllmGroupPlan struct {
	Topology     vllmGroupTopology      `json:"topology"`
	Transport    *vllmGroupTransport    `json:"transport,omitempty"`
	DirectSocket *vllmGroupDirectSocket `json:"directSocket,omitempty"`
	Limits       vllmGroupLimits        `json:"limits"`
	Coordinator  string                 `json:"coordinator"`
	Model        string                 `json:"model"`
	Runtime      string                 `json:"runtime"`
	Members      []vllmGroupMember      `json:"members"`
}

// These fields and their order match the only pre-limits producer. Keeping a
// separate wire type lets retained-file admission distinguish an omitted field
// from explicit zeroes without weakening current plan validation.
type legacyVLLMGroupTopology struct {
	TensorParallel   int    `json:"tensorParallel"`
	PipelineParallel int    `json:"pipelineParallel"`
	DataParallel     int    `json:"dataParallel"`
	ConfigSHA256     string `json:"configSha256"`
}

type legacyVLLMGroupMember struct {
	Resources                  *vllmResourceSettings `json:"resources,omitempty"`
	Placement                  *vllmGroupPlacement   `json:"placement,omitempty"`
	NodeID                     string                `json:"nodeId"`
	PinSHA256                  string                `json:"pinSha256"`
	GPUUUID                    string                `json:"gpuUuid"`
	ModelDigest                string                `json:"modelDigest"`
	RuntimeDigest              string                `json:"runtimeDigest"`
	RuntimeCompatibilitySHA256 string                `json:"runtimeCompatibilitySha256"`
}

type legacyVLLMGroupPlan struct {
	Topology    legacyVLLMGroupTopology `json:"topology"`
	Coordinator string                  `json:"coordinator"`
	Model       string                  `json:"model"`
	Runtime     string                  `json:"runtime"`
	Members     []legacyVLLMGroupMember `json:"members"`
}

// Generation 55 predates peer-subnet routing fields but otherwise uses the
// current Qwen plan. This wire type preserves the exact old field order for a
// cleanup-only digest check; it is never used to create a fresh plan.
type historicalQwenGroupTransport struct {
	Mode                  string `json:"mode"`
	OperationID           string `json:"operationId"`
	QualificationSHA256   string `json:"qualificationSha256"`
	NetGDRLevel           int    `json:"netGdrLevel"`
	NetGDRC2C             int    `json:"netGdrC2c"`
	NetGDRRead            int    `json:"netGdrRead"`
	NetPlugin             string `json:"netPlugin"`
	EnvPlugin             string `json:"envPlugin"`
	GINPlugin             string `json:"ginPlugin"`
	SocketPayloadFallback bool   `json:"socketPayloadFallback"`
}

type historicalQwenGroupPlan struct {
	Topology     vllmGroupTopology             `json:"topology"`
	Transport    *historicalQwenGroupTransport `json:"transport,omitempty"`
	DirectSocket *vllmGroupDirectSocket        `json:"directSocket,omitempty"`
	Limits       vllmGroupLimits               `json:"limits"`
	Coordinator  string                        `json:"coordinator"`
	Model        string                        `json:"model"`
	Runtime      string                        `json:"runtime"`
	Members      []vllmGroupMember             `json:"members"`
}

type vllmGroupReview struct {
	ReviewID          string        `json:"reviewId"`
	PlanDigest        string        `json:"planDigest"`
	Plan              vllmGroupPlan `json:"plan"`
	ExpiresAt         int64         `json:"expiresAt"`
	ActivationEnabled bool          `json:"activationEnabled"`
	Reason            string        `json:"reason"`
}

type vllmGroupRank struct {
	StartFailure     *vllmRankStartFailure `json:"startFailure,omitempty"`
	CleanupFailure   *vllmRankStartFailure `json:"cleanupFailure,omitempty"`
	NodeID           string                `json:"nodeId"`
	Attempted        bool                  `json:"attempted"`
	Started          bool                  `json:"started"`
	CleanupConfirmed bool                  `json:"cleanupConfirmed"`
}

type vllmGroupRun struct {
	RunID            string          `json:"runId"`
	Generation       uint64          `json:"generation"`
	PlanDigest       string          `json:"planDigest"`
	Plan             vllmGroupPlan   `json:"plan"`
	State            string          `json:"state"`
	Ranks            []vllmGroupRank `json:"ranks"`
	CleanupConfirmed bool            `json:"cleanupConfirmed"`
	Failure          string          `json:"failure,omitempty"`
	legacyDigest     bool
}

type retainedVLLMGroupRun struct {
	RunID            string          `json:"runId"`
	Generation       uint64          `json:"generation"`
	PlanDigest       string          `json:"planDigest"`
	Plan             json.RawMessage `json:"plan"`
	State            string          `json:"state"`
	Ranks            []vllmGroupRank `json:"ranks"`
	CleanupConfirmed bool            `json:"cleanupConfirmed"`
	Failure          string          `json:"failure,omitempty"`
}

func legacyVLLMGroupWire(r vllmGroupRun) (retainedVLLMGroupRun, error) {
	legacy := legacyVLLMGroupPlan{
		Topology: legacyVLLMGroupTopology{
			TensorParallel: r.Plan.Topology.TensorParallel, PipelineParallel: r.Plan.Topology.PipelineParallel,
			DataParallel: r.Plan.Topology.DataParallel, ConfigSHA256: r.Plan.Topology.ConfigSHA256,
		},
		Coordinator: r.Plan.Coordinator, Model: r.Plan.Model, Runtime: r.Plan.Runtime,
	}
	for _, member := range r.Plan.Members {
		legacy.Members = append(legacy.Members, legacyVLLMGroupMember{
			Resources: member.Resources, Placement: member.Placement, NodeID: member.NodeID,
			PinSHA256: member.PinSHA256, GPUUUID: member.GPUUUID, ModelDigest: member.ModelDigest,
			RuntimeDigest: member.RuntimeDigest, RuntimeCompatibilitySHA256: member.RuntimeCompatibilitySHA256,
		})
	}
	plan, err := json.Marshal(legacy)
	if err != nil {
		return retainedVLLMGroupRun{}, err
	}
	return retainedVLLMGroupRun{
		RunID: r.RunID, Generation: r.Generation, PlanDigest: r.PlanDigest, Plan: plan,
		State: r.State, Ranks: r.Ranks, CleanupConfirmed: r.CleanupConfirmed, Failure: r.Failure,
	}, nil
}

func legacyVLLMGroupPlanDigest(p vllmGroupPlan) (string, error) {
	if p.Runtime != "0.28.0" || isQwen38ProfileModel(p.Model) || p.Limits != vllmGroupLimitsForModel(p.Model) || !validLegacyVLLMGroupMemberState(p) {
		return "", errors.New("plan is not an exact cleanup-compatible legacy serving group")
	}
	validationPlan := cloneVLLMGroupPlan(p)
	validationPlan.Runtime = vllmManagedVersion
	if _, err := vllmGroupPlanDigest(validationPlan); err != nil {
		return "", err
	}
	wire, err := legacyVLLMGroupWire(vllmGroupRun{Plan: p})
	if err != nil {
		return "", err
	}
	var legacy legacyVLLMGroupPlan
	if strictDiagnosticJSON(wire.Plan, &legacy) != nil {
		return "", errors.New("legacy serving-group plan is unavailable")
	}
	normalized := vllmGroupPlan{
		Topology: vllmGroupTopology{TensorParallel: legacy.Topology.TensorParallel, PipelineParallel: legacy.Topology.PipelineParallel, DataParallel: legacy.Topology.DataParallel, ConfigSHA256: legacy.Topology.ConfigSHA256},
		Limits:   vllmGroupLimitsForModel(legacy.Model), Coordinator: legacy.Coordinator, Model: legacy.Model, Runtime: legacy.Runtime,
	}
	for _, member := range legacy.Members {
		normalized.Members = append(normalized.Members, vllmGroupMember{Resources: member.Resources, Placement: member.Placement, NodeID: member.NodeID, PinSHA256: member.PinSHA256, GPUUUID: member.GPUUUID, ModelDigest: member.ModelDigest, RuntimeDigest: member.RuntimeDigest, RuntimeCompatibilitySHA256: member.RuntimeCompatibilitySHA256})
	}
	if !reflect.DeepEqual(p, normalized) {
		return "", errors.New("legacy serving-group plan contains fields outside its historical digest")
	}
	sum := sha256.Sum256(wire.Plan)
	return hex.EncodeToString(sum[:]), nil
}

func historicalQwenGroupPlanDigest(p vllmGroupPlan) (string, error) {
	if !isQwen38ProfileModel(p.Model) || p.Runtime != vllmQwen38Runtime || p.Transport == nil ||
		p.Transport.SubnetAwareRouting != nil || p.Transport.SubnetPrefixLength != nil || p.Transport.MergeNICs != nil {
		return "", errors.New("plan is not an exact historical Qwen cleanup plan")
	}
	validation := cloneVLLMGroupPlan(p)
	routing, prefix, merge := false, 0, true
	validation.Transport.SubnetAwareRouting, validation.Transport.SubnetPrefixLength, validation.Transport.MergeNICs = &routing, &prefix, &merge
	if _, err := vllmGroupPlanDigest(validation); err != nil {
		return "", err
	}
	transport := p.Transport
	wire := historicalQwenGroupPlan{
		Topology: p.Topology,
		Transport: &historicalQwenGroupTransport{
			Mode: transport.Mode, OperationID: transport.OperationID, QualificationSHA256: transport.QualificationSHA256,
			NetGDRLevel: transport.NetGDRLevel, NetGDRC2C: transport.NetGDRC2C, NetGDRRead: transport.NetGDRRead,
			NetPlugin: transport.NetPlugin, EnvPlugin: transport.EnvPlugin, GINPlugin: transport.GINPlugin,
			SocketPayloadFallback: transport.SocketPayloadFallback,
		},
		DirectSocket: p.DirectSocket, Limits: p.Limits, Coordinator: p.Coordinator, Model: p.Model, Runtime: p.Runtime, Members: p.Members,
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Every participant action is bound to the entire immutable operation, its
// generation and rank. A future transport must additionally authenticate pins;
// a caller-supplied digest is not live hardware, model or trust attestation.
type vllmGroupBinding struct {
	RunID      string
	Generation uint64
	PlanDigest string
	Plan       vllmGroupPlan
	Rank       int
}

type vllmGroupCall func(context.Context, vllmGroupBinding, string) error

// ponytail: one group belongs to the existing per-engine owner, not a second
// registry. All entrypoint lock ordering is engine.opMu -> group.mu -> engine.mu.
type vllmServingGroup struct {
	engine       *engineState
	path         string
	mu           sync.Mutex
	review       *vllmGroupReview
	run          vllmGroupRun
	held         bool
	cancel       context.CancelFunc
	ctx          context.Context
	done         chan struct{}
	call         vllmGroupCall // nil until a managed native rank owner is admitted
	live         func(context.Context, vllmGroupRun) error
	route        func(context.Context, vllmGroupRun) error // nil until the broker route gate is attached
	fabric       vllmGroupFabricLeaser
	routing      bool // collectively ready; awaiting the local proxy route before public ready
	shuttingDown bool
	elevation    *vllmGroupElevation
}

type vllmGroupFabricLeaser interface {
	acquireConsumerLease(context.Context, string, string, fabricConsumerLease, *fabricConsumerLease) error
	releaseConsumerLease(operationID string, lease fabricConsumerLease) error
}

func vllmGroupFabricBinding(plan vllmGroupPlan) (operationID, qualification string, ok bool) {
	if plan.Transport != nil {
		return plan.Transport.OperationID, plan.Transport.QualificationSHA256, true
	}
	if plan.DirectSocket != nil {
		return plan.DirectSocket.OperationID, plan.DirectSocket.QualificationSHA256, true
	}
	return "", "", false
}

func cloneVLLMGroupPlan(p vllmGroupPlan) vllmGroupPlan {
	if p.Transport != nil {
		value := *p.Transport
		if value.SubnetAwareRouting != nil {
			field := *value.SubnetAwareRouting
			value.SubnetAwareRouting = &field
		}
		if value.SubnetPrefixLength != nil {
			field := *value.SubnetPrefixLength
			value.SubnetPrefixLength = &field
		}
		if value.MergeNICs != nil {
			field := *value.MergeNICs
			value.MergeNICs = &field
		}
		p.Transport = &value
	}
	p.DirectSocket = cloneVLLMGroupDirectSocket(p.DirectSocket)
	if p.Topology.MTP != nil {
		value := *p.Topology.MTP
		p.Topology.MTP = &value
	}
	if p.Topology.DFlash != nil {
		value := *p.Topology.DFlash
		p.Topology.DFlash = &value
	}
	if p.Topology.FlashInferAutotune != nil {
		value := *p.Topology.FlashInferAutotune
		p.Topology.FlashInferAutotune = &value
	}
	p.Members = append([]vllmGroupMember(nil), p.Members...)
	for i := range p.Members {
		if original := p.Members[i].Placement; original != nil {
			value := *original
			p.Members[i].Placement = &value
		}
		if original := p.Members[i].Resources; original != nil {
			value := *original
			if original.GPUMemoryUtilization != nil {
				n := *original.GPUMemoryUtilization
				value.GPUMemoryUtilization = &n
			}
			if original.MaxModelLen != nil {
				n := *original.MaxModelLen
				value.MaxModelLen = &n
			}
			if original.GPUUUID != nil {
				n := *original.GPUUUID
				value.GPUUUID = &n
			}
			if original.TensorParallelSize != nil {
				n := *original.TensorParallelSize
				value.TensorParallelSize = &n
			}
			p.Members[i].Resources = &value
		}
		if original := p.Members[i].Fabric; original != nil {
			p.Members[i].Fabric = &vllmGroupMemberFabric{Lanes: append([]vllmGroupRDMALane(nil), original.Lanes...)}
		}
	}
	return p
}

func cloneVLLMGroupRun(r vllmGroupRun) vllmGroupRun {
	r.Plan = cloneVLLMGroupPlan(r.Plan)
	r.Ranks = append([]vllmGroupRank(nil), r.Ranks...)
	for i := range r.Ranks {
		if r.Ranks[i].StartFailure != nil {
			f := cloneVLLMRankStartFailure(*r.Ranks[i].StartFailure)
			r.Ranks[i].StartFailure = &f
		}
		if r.Ranks[i].CleanupFailure != nil {
			f := cloneVLLMRankStartFailure(*r.Ranks[i].CleanupFailure)
			r.Ranks[i].CleanupFailure = &f
		}
	}
	return r
}

func vllmGroupPlanDigest(p vllmGroupPlan) (string, error) {
	if len(p.Members) < 2 || len(p.Members) > 3 || p.Coordinator != p.Members[0].NodeID || !remoteModelID(p.Model) {
		return "", errors.New("select two or three ordered members, coordinator first and one exact model")
	}
	var err error
	if isQwen38ProfileModel(p.Model) {
		if p.Runtime != vllmQwen38Runtime {
			return "", errors.New("Qwen3.8 requires the exact pinned vLLM runtime")
		}
		err = validateQwen38GroupTopology(p.Topology, len(p.Members))
		if err == nil {
			err = validateVLLMGroupTransport(p)
		}
	} else {
		if p.Runtime != vllmManagedVersion && p.Runtime != "0.28.0" {
			return "", errors.New("ordinary serving groups require the managed vLLM runtime")
		}
		err = validateVLLMGroupTopology(p.Topology, len(p.Members))
		if err == nil && p.Transport != nil {
			err = errors.New("ordinary serving groups cannot inherit the Qwen3.8 RoCE transport contract")
		}
	}
	if err == nil {
		err = validateVLLMGroupDirectSocket(p)
	}
	if err != nil {
		return "", err
	}
	if p.Limits != vllmGroupLimitsForModel(p.Model) {
		return "", errors.New("serving-group runtime, host-memory or task limit differs from the reviewed model profile")
	}
	nodes, pins, devices := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, member := range p.Members {
		if !cableIdentifier(member.NodeID, 128) || !onboardingSHA.MatchString(member.PinSHA256) || !onboardingSHA.MatchString(member.ModelDigest) || !onboardingSHA.MatchString(member.RuntimeDigest) || !onboardingSHA.MatchString(member.RuntimeCompatibilitySHA256) || !vllmGPUUUIDPattern.MatchString(member.GPUUUID) {
			return "", errors.New("each member needs a node identity, pin, complete model/runtime digests and one GPU UUID")
		}
		device := strings.ToLower(member.GPUUUID)
		if nodes[member.NodeID] || pins[member.PinSHA256] || devices[device] || member.ModelDigest != p.Members[0].ModelDigest || member.RuntimeCompatibilitySHA256 != p.Members[0].RuntimeCompatibilitySHA256 {
			return "", errors.New("members must be distinct and use the same model, complete dependency archive set and Python ABI")
		}
		nodes[member.NodeID], pins[member.PinSHA256], devices[device] = true, true, true
		if !isQwen38ProfileModel(p.Model) && member.Fabric != nil {
			return "", errors.New("ordinary serving-group member cannot carry Qwen3.8 fabric bindings")
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeLegacyVLLMGroupRun(data json.RawMessage) (vllmGroupRun, bool) {
	var retained retainedVLLMGroupRun
	if strictDiagnosticJSON(data, &retained) != nil {
		return vllmGroupRun{}, false
	}
	switch retained.State {
	case "starting", "ready", "stopping", "stopped", "failed", "cleanup-required":
	default:
		return vllmGroupRun{}, false
	}
	if !retained.CleanupConfirmed && (retained.State != "cleanup-required" || retained.Failure == "") {
		return vllmGroupRun{}, false
	}
	if retained.CleanupConfirmed && retained.State != "stopped" && retained.State != "failed" {
		return vllmGroupRun{}, false
	}
	var legacy legacyVLLMGroupPlan
	if strictDiagnosticJSON(retained.Plan, &legacy) != nil {
		return vllmGroupRun{}, false
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		return vllmGroupRun{}, false
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != retained.PlanDigest {
		return vllmGroupRun{}, false
	}
	plan := vllmGroupPlan{
		Topology: vllmGroupTopology{
			TensorParallel:   legacy.Topology.TensorParallel,
			PipelineParallel: legacy.Topology.PipelineParallel,
			DataParallel:     legacy.Topology.DataParallel,
			ConfigSHA256:     legacy.Topology.ConfigSHA256,
		},
		Limits:      vllmGroupLimitsForModel(legacy.Model),
		Coordinator: legacy.Coordinator,
		Model:       legacy.Model,
		Runtime:     legacy.Runtime,
	}
	for _, member := range legacy.Members {
		plan.Members = append(plan.Members, vllmGroupMember{
			Resources: member.Resources, Placement: member.Placement, NodeID: member.NodeID,
			PinSHA256: member.PinSHA256, GPUUUID: member.GPUUUID, ModelDigest: member.ModelDigest,
			RuntimeDigest: member.RuntimeDigest, RuntimeCompatibilitySHA256: member.RuntimeCompatibilitySHA256,
		})
	}
	if legacy.Runtime != "0.28.0" || isQwen38ProfileModel(legacy.Model) {
		return vllmGroupRun{}, false
	}
	validationPlan := cloneVLLMGroupPlan(plan)
	validationPlan.Runtime = vllmManagedVersion
	if _, err := vllmGroupPlanDigest(validationPlan); err != nil || !validLegacyVLLMGroupMemberState(plan) {
		return vllmGroupRun{}, false
	}
	run := vllmGroupRun{
		RunID: retained.RunID, Generation: retained.Generation, PlanDigest: retained.PlanDigest,
		Plan: plan, State: retained.State, Ranks: retained.Ranks,
		CleanupConfirmed: retained.CleanupConfirmed, Failure: retained.Failure, legacyDigest: true,
	}
	if !onboardingID.MatchString(run.RunID) || run.Generation == 0 || len(run.Ranks) != len(run.Plan.Members) {
		return vllmGroupRun{}, false
	}
	for i, rank := range run.Ranks {
		if (rank.StartFailure != nil && !validVLLMRankStartFailure(rank.StartFailure)) ||
			(rank.CleanupFailure != nil && (!validVLLMRankStartFailure(rank.CleanupFailure) || !rank.Attempted || rank.CleanupConfirmed)) ||
			rank.NodeID != run.Plan.Members[i].NodeID || rank.Started && !rank.Attempted || rank.CleanupConfirmed && !rank.Attempted || retained.CleanupConfirmed && rank.Attempted && !rank.CleanupConfirmed {
			return vllmGroupRun{}, false
		}
	}
	return run, true
}

func validLegacyVLLMGroupMemberState(plan vllmGroupPlan) bool {
	addresses := make(map[string]bool, len(plan.Members))
	for _, member := range plan.Members {
		if member.Resources == nil || member.Placement == nil {
			return false
		}
		raw, err := json.Marshal(member.Resources)
		if err != nil {
			return false
		}
		resources, err := decodeVLLMResourceSettings(raw)
		if err != nil || resources.validateParallelism() != nil || resources.TensorParallelSize != nil && *resources.TensorParallelSize != 1 || resources.GPUUUID != nil && !strings.EqualFold(*resources.GPUUUID, member.GPUUUID) {
			return false
		}
		placement := member.Placement
		address := net.ParseIP(placement.Address)
		if !path.IsAbs(placement.ModelPath) || address == nil || address.To4() == nil || !address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || address.String() != placement.Address || addresses[placement.Address] || placement.APIPort < 1024 || placement.APIPort > 65535 || placement.MasterPort != vllmGroupMasterPort || placement.APIPort == placement.MasterPort {
			return false
		}
		addresses[placement.Address] = true
	}
	return true
}

func parseRetainedVLLMGroupRun(data json.RawMessage) (vllmGroupRun, bool) {
	var run vllmGroupRun
	if strictDiagnosticJSON(data, &run) == nil && validVLLMGroupRun(run) {
		return run, true
	}
	if legacy, ok := normalizeLegacyVLLMGroupRun(data); ok {
		return legacy, true
	}
	var historical vllmGroupRun
	if strictDiagnosticJSON(data, &historical) != nil || !historical.CleanupConfirmed || historical.State != "stopped" && historical.State != "failed" {
		return vllmGroupRun{}, false
	}
	digest, err := historicalQwenGroupPlanDigest(historical.Plan)
	if err != nil || digest != historical.PlanDigest {
		return vllmGroupRun{}, false
	}
	historical.legacyDigest = true
	if !validVLLMGroupRun(historical) {
		return vllmGroupRun{}, false
	}
	return historical, true
}

// Caller holds engine.opMu. No public request or normal engine lifecycle calls
// this yet; attaching here ensures future admission uses the same engine owner.
func newVLLMServingGroup(st *engineState, path string) (*vllmServingGroup, error) {
	st.mu.Lock()
	if st.vllmGroup != nil {
		st.mu.Unlock()
		return nil, errors.New("vLLM group owner already exists")
	}
	g := &vllmServingGroup{engine: st, path: path}
	g.mu.Lock()
	defer g.mu.Unlock()
	st.vllmGroup = g
	st.mu.Unlock()
	if err := validateVLLMOwnedPath(filepath.Dir(path), path); err != nil {
		g.held = true
		return g, err
	}
	var data json.RawMessage
	err := readVLLMJSON(filepath.Dir(path), path, 32<<10, &data)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	var valid bool
	if err == nil {
		g.run, valid = parseRetainedVLLMGroupRun(data)
	}
	if err != nil || !valid {
		g.held = true
		return g, errors.New("retained serving-group ownership is invalid; preserve it for recovery")
	}
	if !g.run.CleanupConfirmed {
		g.held = true
		g.run.State = "cleanup-required"
		if g.run.Failure == "" {
			g.run.Failure = "prior owner ended; reconcile every attempted rank before another start"
		}
	}
	return g, nil
}

func validVLLMGroupRun(r vllmGroupRun) bool {
	digest, err := vllmGroupPlanDigest(r.Plan)
	if r.legacyDigest {
		err, digest = nil, r.PlanDigest
	}
	if err != nil || digest != r.PlanDigest || !onboardingID.MatchString(r.RunID) || r.Generation == 0 || len(r.Ranks) != len(r.Plan.Members) {
		return false
	}
	switch r.State {
	case "starting", "ready", "stopping", "stopped", "failed", "cleanup-required":
	default:
		return false
	}
	clean := true
	for i, rank := range r.Ranks {
		if (rank.StartFailure != nil && !validVLLMRankStartFailure(rank.StartFailure)) ||
			(rank.CleanupFailure != nil && (!validVLLMRankStartFailure(rank.CleanupFailure) || !rank.Attempted || rank.CleanupConfirmed)) ||
			rank.NodeID != r.Plan.Members[i].NodeID || rank.Started && !rank.Attempted || rank.CleanupConfirmed && !rank.Attempted {
			return false
		}
		clean = clean && (!rank.Attempted || rank.CleanupConfirmed)
	}
	return !r.CleanupConfirmed || clean && (r.State == "stopped" || r.State == "failed")
}

func (g *vllmServingGroup) reserved() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

func (g *vllmServingGroup) status() vllmGroupRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneVLLMGroupRun(g.run)
}

func (g *vllmServingGroup) reviewPlan(plan vllmGroupPlan) (vllmGroupReview, error) {
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		return vllmGroupReview{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held || g.shuttingDown {
		return vllmGroupReview{}, errors.New("the current serving-group reservation must be reconciled first")
	}
	r := vllmGroupReview{ReviewID: newOpID(), PlanDigest: digest, Plan: cloneVLLMGroupPlan(plan), ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), Reason: errVLLMGroupNativeUnavailable.Error()}
	if g.call != nil && g.live != nil {
		r.ActivationEnabled = true
		r.Reason = ""
	}
	g.review = &r
	result := r
	result.Plan = cloneVLLMGroupPlan(r.Plan)
	return result, nil
}

func (g *vllmServingGroup) saveLocked() error {
	if err := validateVLLMOwnedPath(filepath.Dir(g.path), g.path); err != nil {
		g.held = true
		return err
	}
	value := any(g.run)
	if g.run.legacyDigest {
		legacy, err := legacyVLLMGroupWire(g.run)
		if err != nil {
			g.held = true
			return err
		}
		value = legacy
	}
	if err := writeVLLMJSON(filepath.Dir(g.path), g.path, value); err != nil {
		g.held = true
		return fmt.Errorf("serving-group ownership could not be retained: %w", err)
	}
	return nil
}

func (g *vllmServingGroup) start(ctx context.Context, reviewID string) (vllmGroupRun, error) {
	return g.startWithElevation(ctx, reviewID, nil)
}

func (g *vllmServingGroup) startWithElevation(ctx context.Context, reviewID string, auth *vllmGroupElevation) (vllmGroupRun, error) {
	g.engine.opMu.Lock()
	g.mu.Lock()
	var launchedRun vllmGroupRun
	var launchedDone chan struct{}
	defer func() {
		g.mu.Unlock()
		g.engine.opMu.Unlock()
		if launchedDone != nil {
			go g.execute(ctx, launchedRun, launchedDone)
		}
	}()
	if g.call == nil {
		return cloneVLLMGroupRun(g.run), errVLLMGroupNativeUnavailable
	}
	if g.held || g.shuttingDown || g.review == nil || g.review.ReviewID != reviewID || time.Now().UnixMilli() >= g.review.ExpiresAt || ctx.Err() != nil {
		return cloneVLLMGroupRun(g.run), errors.New("current unexpired review and unreserved engine are required")
	}
	g.engine.mu.Lock()
	busy := g.engine.running || g.engine.proc != nil || g.engine.stopPending != 0
	g.engine.mu.Unlock()
	if busy || g.run.Generation == ^uint64(0) {
		return cloneVLLMGroupRun(g.run), errors.New("engine is occupied or generation exhausted")
	}
	previousRun := cloneVLLMGroupRun(g.run)
	previousOperation, _, previousFabric := vllmGroupFabricBinding(previousRun.Plan)
	if previousRun.RunID != "" && previousRun.CleanupConfirmed && previousFabric {
		if g.fabric == nil {
			return cloneVLLMGroupRun(g.run), errors.New("the fabric owner is unavailable")
		}
		if err := g.fabric.releaseConsumerLease(previousOperation, vllmGroupFabricLease(previousRun)); err != nil {
			return cloneVLLMGroupRun(g.run), fmt.Errorf("prior fabric lease could not be released: %w", err)
		}
	}
	next := vllmGroupRun{RunID: newOpID(), Generation: g.run.Generation + 1, PlanDigest: g.review.PlanDigest, Plan: cloneVLLMGroupPlan(g.review.Plan), State: "starting"}
	g.run = next
	for _, member := range g.run.Plan.Members {
		g.run.Ranks = append(g.run.Ranks, vllmGroupRank{NodeID: member.NodeID})
	}
	g.held = true
	// Retain the new generation before the independent fabric owner retains its
	// lease. A crash after lease acquisition can then be reconciled from this
	// exact run instead of stranding an ownerless fabric hold.
	if err := g.saveLocked(); err != nil {
		g.run.State = "cleanup-required"
		return cloneVLLMGroupRun(g.run), err
	}
	if operation, qualification, needsFabric := vllmGroupFabricBinding(next.Plan); needsFabric {
		if g.fabric == nil {
			g.closeUnlaunchedRunLocked("the fabric owner is unavailable")
			return cloneVLLMGroupRun(g.run), errors.New("the fabric owner is unavailable")
		}
		// The prior generation is terminal with every rank cleaned (the owner is
		// not held), so only its lease may be superseded.
		var replaces *fabricConsumerLease
		if previousRun.RunID != "" {
			previous := vllmGroupFabricLease(previousRun)
			replaces = &previous
		}
		if err := g.fabric.acquireConsumerLease(ctx, operation, qualification, vllmGroupFabricLease(g.run), replaces); err != nil {
			g.closeUnlaunchedRunLocked("the reviewed fabric could not be leased")
			return cloneVLLMGroupRun(g.run), err
		}
	}
	g.elevation = auth
	g.review = nil // one approval is consumed by one generation
	ctx, g.cancel = context.WithCancel(ctx)
	g.ctx = ctx
	g.done = make(chan struct{})
	run, done := cloneVLLMGroupRun(g.run), g.done
	launchedRun, launchedDone = run, done
	return run, nil
}

func (g *vllmServingGroup) closeUnlaunchedRunLocked(reason string) {
	g.run.State = "failed"
	g.run.Failure = reason
	g.run.CleanupConfirmed = true
	g.held = false
	if err := g.saveLocked(); err != nil {
		g.run.CleanupConfirmed = false
		g.held = true
	}
}

func (g *vllmServingGroup) binding(run vllmGroupRun, rank int) vllmGroupBinding {
	return vllmGroupBinding{RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest, Plan: cloneVLLMGroupPlan(run.Plan), Rank: rank}
}

func (g *vllmServingGroup) execute(ctx context.Context, run vllmGroupRun, done chan struct{}) {
	defer close(done)
	g.mu.Lock()
	auth := g.elevation
	g.mu.Unlock()
	defer auth.close()
	var failure error
	for i := range run.Ranks {
		if failure = ctx.Err(); failure != nil {
			break
		}
		g.mu.Lock()
		g.run.Ranks[i].Attempted = true // retain ownership before even prepare may have effects
		failure = g.saveLocked()
		g.mu.Unlock()
		if failure != nil {
			break
		}
		if failure = g.call(ctx, g.binding(run, i), "prepare"); failure != nil {
			detail := rankStartError("admission", "unclassified", failure, 0)
			g.mu.Lock()
			g.run.Ranks[i].StartFailure = &detail.Failure
			_ = g.saveLocked()
			g.mu.Unlock()
			break
		}
	}
	// Participant start must acknowledge launch, not wait for cross-rank HTTP
	// readiness. All ranks launch before the coordinator's collective readiness.
	for i := range run.Ranks {
		if failure != nil {
			break
		}
		if failure = ctx.Err(); failure != nil {
			break
		}
		if failure = g.call(ctx, g.binding(run, i), "start"); failure != nil {
			detail := rankStartError("peer-return", "unclassified", failure, 0)
			g.mu.Lock()
			g.run.Ranks[i].StartFailure = &detail.Failure
			_ = g.saveLocked()
			g.mu.Unlock()
			break
		}
		g.mu.Lock()
		g.run.Ranks[i].Started = true
		failure = g.saveLocked()
		g.mu.Unlock()
	}
	if failure == nil {
		failure = g.call(ctx, g.binding(run, 0), "ready")
	}
	if failure == nil && g.route != nil {
		// The broker may route the coordinator from here, but ready is published
		// only once the local proxy lists it among the model's inference owners.
		g.mu.Lock()
		g.routing = true
		g.mu.Unlock()
		failure = g.route(ctx, run)
	}
	g.mu.Lock()
	if failure == nil {
		failure = ctx.Err()
	}
	if failure == nil {
		g.routing = false
		g.run.State = "ready"
		failure = g.saveLocked()
		if failure != nil {
			g.run.State = "cleanup-required"
		}
	}
	g.mu.Unlock()
	if failure == nil {
		failure = g.monitor(ctx, run)
	}
	g.cleanup(run, failure)
}

func (g *vllmServingGroup) monitor(ctx context.Context, run vllmGroupRun) error {
	if g.live == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := g.live(ctx, run); err != nil {
				g.mu.Lock()
				g.run.State = "stopping" // withdraw before cleanup I/O
				g.mu.Unlock()
				return err
			}
		}
	}
}

func (g *vllmServingGroup) cleanup(run vllmGroupRun, cause error) {
	g.cleanupWithHeldOwner(run, cause, nil, time.Time{})
}

// heldOwner is internal synchronous ownership, not a request/JSON option.
func (g *vllmServingGroup) cleanupWithHeldOwner(run vllmGroupRun, cause error, heldOwner *engineState, deadline time.Time) {
	limit := time.Now().Add(vllmGroupCleanupBudget)
	if deadline.IsZero() || deadline.After(limit) {
		deadline = limit
	}
	// Cleanup survives caller cancellation, but Stop's fallback reconciliation
	// must not renew the already-admitted outer deadline.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if heldOwner != nil {
		var revoke func()
		ctx, revoke = vllmHeldEngineContext(ctx, heldOwner)
		defer revoke()
	}
	g.mu.Lock()
	g.run.State = "stopping" // withdraw eligibility before remote cleanup
	g.routing = false
	g.run.CleanupConfirmed = false
	if cause != nil && !errors.Is(cause, context.Canceled) {
		g.run.Failure = "a participant action failed; all owned ranks require cleanup"
	}
	_ = g.saveLocked()
	g.mu.Unlock()
	clean := true
	for i := len(run.Ranks) - 1; i >= 0; i-- {
		g.mu.Lock()
		attempted, confirmed := g.run.Ranks[i].Attempted, g.run.Ranks[i].CleanupConfirmed
		g.mu.Unlock()
		// Confirmed cleanup belongs to this retained generation; retry only the
		// unresolved ranks, matching Reconcile's administrator input scope.
		if !attempted || confirmed {
			continue
		}
		err := errVLLMGroupNativeUnavailable
		if g.call != nil {
			err = g.call(ctx, g.binding(run, i), "stop")
		}
		g.mu.Lock()
		g.run.Ranks[i].CleanupConfirmed = err == nil
		if err == nil {
			g.run.Ranks[i].CleanupFailure = nil
		} else {
			detail := rankStartError("peer-return", "unclassified", err, 0)
			g.run.Ranks[i].CleanupFailure = &detail.Failure
		}
		clean = clean && err == nil
		if g.saveLocked() != nil {
			clean = false
		}
		g.mu.Unlock()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.run.CleanupConfirmed = clean
	g.run.State = "cleanup-required"
	if clean {
		g.run.State = "stopped"
		if g.run.Failure != "" {
			g.run.State = "failed"
		}
	}
	if err := g.saveLocked(); err != nil {
		g.run.CleanupConfirmed, clean = false, false
		g.run.State = "cleanup-required"
	}
	g.held = !clean
	_ = g.releaseSettledFabricLeaseLocked()
}

// A generation whose every rank confirmed cleanup releases its fabric lease. A
// release that cannot be retained is retried on the next owner lookup,
// including the first one after a restart.
func (g *vllmServingGroup) releaseSettledFabricLeaseLocked() error {
	operation, _, needsFabric := vllmGroupFabricBinding(g.run.Plan)
	if g.held || !g.run.CleanupConfirmed || !needsFabric || g.fabric == nil {
		return nil
	}
	return g.fabric.releaseConsumerLease(operation, vllmGroupFabricLease(g.run))
}

func (g *vllmServingGroup) releaseSettledFabricLease() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.releaseSettledFabricLeaseLocked()
}

func (g *vllmServingGroup) stop(ctx context.Context, runID string, generation uint64) error {
	g.mu.Lock()
	if g.run.RunID != runID || g.run.Generation != generation {
		g.mu.Unlock()
		return errors.New("serving-group stop belongs to a different operation generation")
	}
	if !g.held && g.run.CleanupConfirmed {
		g.mu.Unlock()
		return nil
	}
	if g.cancel != nil {
		g.run.State = "stopping"
		g.run.CleanupConfirmed = false
		_ = g.saveLocked()
		g.cancel() // cancellation never waits behind the engine lifecycle lock
	}
	done := g.done
	g.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.reserved() {
		return errors.New("owned rank cleanup remains unconfirmed; reconcile this same operation")
	}
	return nil
}

func (g *vllmServingGroup) reconcile(ctx context.Context, runID string, generation uint64) error {
	g.engine.opMu.Lock()
	defer g.engine.opMu.Unlock()
	return g.reconcileLocked(ctx, runID, generation)
}

// Caller holds engine.opMu, including normal Stop and shutdown.
func (g *vllmServingGroup) reconcileLocked(ctx context.Context, runID string, generation uint64) error {
	g.mu.Lock()
	if g.run.RunID != runID || g.run.Generation != generation || !validVLLMGroupRun(g.run) || ctx.Err() != nil {
		g.mu.Unlock()
		return errors.New("exact retained operation is required for cleanup reconciliation")
	}
	if !g.held && g.run.CleanupConfirmed {
		g.mu.Unlock()
		return nil
	}
	if g.done != nil {
		select {
		case <-g.done:
		default:
			g.mu.Unlock()
			return errors.New("cancel and join the active operation before reconciliation")
		}
	}
	run := cloneVLLMGroupRun(g.run)
	g.mu.Unlock()
	deadline, _ := ctx.Deadline()
	g.cleanupWithHeldOwner(run, nil, g.engine, deadline)
	if g.reserved() {
		return errors.New("cleanup remains unconfirmed; no replacement generation was admitted")
	}
	return nil
}

// Eligibility only, NOT broker advertisement. Public integration must also
// require authenticated live participant and coordinator HTTP/model readiness.
func (g *vllmServingGroup) coordinatorEligible(nodeID, runID string, generation uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx == nil || g.ctx.Err() != nil || g.run.State != "ready" || g.run.RunID != runID || g.run.Generation != generation || g.run.Plan.Coordinator != nodeID {
		return false
	}
	for _, rank := range g.run.Ranks {
		if !rank.Started || rank.CleanupConfirmed {
			return false
		}
	}
	return true
}

// All callers hold the existing engine lifecycle lock. Stop/reconciliation must
// remain available; ordinary runtime/model/resource/port mutations must not.
func rejectVLLMGroupOwnerMutation(st *engineState, effect string) error {
	if (st.vllmGroup != nil && st.vllmGroup.reserved()) || (st.vllmRank != nil && st.vllmRank.reserved()) {
		return fmt.Errorf("a vLLM serving-group reservation prevents %s", effect)
	}
	return nil
}

// Interrupt before waiting on opMu, just like ordinary pending engine startup.
func requestVLLMGroupStop(st *engineState, shutdown bool) {
	st.mu.Lock()
	g := st.vllmGroup
	st.mu.Unlock()
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shuttingDown = g.shuttingDown || shutdown
	if g.cancel != nil && g.held {
		g.cancel()
		g.run.State = "stopping"
		g.run.CleanupConfirmed = false
		_ = g.saveLocked()
	}
}

// Ordinary engine Stop owns the whole group, not only the local rank. A missing
// native recovery adapter must leave a durable hold, never imply ranks are gone.
func stopVLLMGroupLocked(st *engineState) error {
	g := st.vllmGroup
	if g == nil || !g.reserved() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vllmGroupCleanupBudget)
	defer cancel()
	run := g.status()
	if err := g.stop(ctx, run.RunID, run.Generation); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return g.reconcileLocked(ctx, run.RunID, run.Generation)
}
