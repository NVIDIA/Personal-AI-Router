// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const vllmGroupMasterPort = 29500

type vllmGroupSelection struct {
	NodeIDs     []string `json:"nodeIds"`
	Model       string   `json:"model"`
	Parallelism string   `json:"parallelism,omitempty"`
}
type vllmGroupPlacement struct {
	ModelPath  string `json:"modelPath"`
	Address    string `json:"address"`
	APIPort    int    `json:"apiPort"`
	MasterPort int    `json:"masterPort"`
}
type vllmGroupAddress struct {
	IP     string `json:"ip"`
	Prefix string `json:"prefix"`
}
type vllmQwen38GroupFacts struct {
	RecipeSHA256                string `json:"recipeSha256"`
	BundleReceiptSHA256         string `json:"bundleReceiptSha256"`
	SnapshotReceiptSHA256       string `json:"snapshotReceiptSha256"`
	ExpertParallelReceiptSHA256 string `json:"expertParallelReceiptSha256"`
	EPLBReceiptSHA256           string `json:"eplbReceiptSha256,omitempty"`
	RDMAReceiptSHA256           string `json:"rdmaReceiptSha256"`
	LaunchReceiptSHA256         string `json:"launchReceiptSha256"`
	ProviderClosureSHA256       string `json:"providerClosureSha256,omitempty"`
	ModelDigest                 string `json:"modelDigest,omitempty"`
	RuntimeCompatibilitySHA256  string `json:"runtimeCompatibilitySha256,omitempty"`
	ExpertParallelBindingSHA256 string `json:"expertParallelBindingSha256,omitempty"`
	EPLBBindingSHA256           string `json:"eplbBindingSha256,omitempty"`
	PreparedSchema2             bool   `json:"preparedSchema2,omitempty"`
	Layout                      string `json:"layout"`
	Nodes                       int    `json:"nodes"`
}
type vllmGroupFacts struct {
	controlRoute               *vllmGroupControlRoute // local transport evidence; never accepted from JSON
	fabricTransport            *vllmGroupTransport    // existing fabric owner evidence; never accepted from peer JSON
	fabric                     *vllmGroupMemberFabric // existing fabric owner evidence; never accepted from peer JSON
	directSocket               *vllmGroupDirectSocket // existing fabric owner evidence; never accepted from peer JSON
	ringSocket                 *vllmGroupRingSocket   // existing fabric owner evidence; never accepted from peer JSON
	NodeID                     string                 `json:"nodeId"`
	Model                      string                 `json:"model"`
	ModelDigest                string                 `json:"modelDigest"`
	RuntimeDigest              string                 `json:"runtimeDigest"`
	RuntimeVersion             string                 `json:"runtimeVersion"`
	RuntimeCompatibilitySHA256 string                 `json:"runtimeCompatibilitySha256"`
	GPUUUID                    string                 `json:"gpuUuid"`
	ModelPath                  string                 `json:"modelPath"`
	APIPort                    int                    `json:"apiPort"`
	Resources                  vllmResourceSettings   `json:"resources"`
	Addresses                  []vllmGroupAddress     `json:"addresses"`
	Config                     []byte                 `json:"config"`
	Qwen38                     *vllmQwen38GroupFacts  `json:"qwen38,omitempty"`
}

func qwen38ReviewBinding(kind string, nodes int, layout vllmQwen38Layout, modelDigest, compatibilitySHA256, providerClosureSHA256 string) string {
	raw, _ := json.Marshal(struct {
		Kind, RecipeSHA256, ModelDigest, RuntimeCompatibilitySHA256, ProviderClosureSHA256, Layout string
		Nodes                                                                                      int
	}{kind, vllmQwen38RecipeSHA256, modelDigest, compatibilitySHA256, providerClosureSHA256, layout.Label, nodes})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func qwen38GroupFacts(st *engineState, dir string, receipt vllmRuntimeReceipt, modelDigest, compatibilitySHA256 string, nodes int) (*vllmQwen38GroupFacts, error) {
	if receipt.Version != vllmQwen38Runtime || receipt.Architecture != "arm64" || receipt.RecipeID != vllmQwen38RecipeID || receipt.RecipeSHA256 != vllmQwen38RecipeSHA256 {
		return nil, errors.New("Qwen3.8 group requires the exact pinned ARM64 runtime")
	}
	if receipt.Schema == vllmEnvironmentReceiptSchema {
		bundle, launch, err := preparedQwen38Receipts(st, dir, receipt)
		layout, layoutErr := qwen38Layout(nodes)
		if err != nil || layoutErr != nil || launch.RuntimeCompatibilitySHA256 != compatibilitySHA256 || !onboardingSHA.MatchString(modelDigest) || !onboardingSHA.MatchString(compatibilitySHA256) {
			return nil, errors.New("Qwen3.8 prepared runtime, exact model or launch capability changed during review")
		}
		facts := &vllmQwen38GroupFacts{RecipeSHA256: vllmQwen38RecipeSHA256, BundleReceiptSHA256: receipt.BundleReceiptSHA256, SnapshotReceiptSHA256: modelDigest, LaunchReceiptSHA256: bundle.LaunchReceiptSHA256, ProviderClosureSHA256: bundle.Provider.ObservedClosureSHA256, ModelDigest: modelDigest, RuntimeCompatibilitySHA256: compatibilitySHA256, PreparedSchema2: true, Layout: layout.Label, Nodes: nodes}
		facts.ExpertParallelBindingSHA256 = qwen38ReviewBinding("expert-parallel", nodes, layout, modelDigest, compatibilitySHA256, facts.ProviderClosureSHA256)
		if layout.EPLB {
			facts.EPLBBindingSHA256 = qwen38ReviewBinding("eplb", nodes, layout, modelDigest, compatibilitySHA256, facts.ProviderClosureSHA256)
		}
		return facts, nil
	}
	bundle, err := installedQwen38BundleReceipt(st, dir, receipt)
	if err != nil || bundle.Phase != "" {
		return nil, errors.New("Qwen3.8 group requires a fully admitted runtime bundle, not a stage-only environment")
	}
	m := bundle.Member
	return &vllmQwen38GroupFacts{RecipeSHA256: bundle.RecipeSHA256, BundleReceiptSHA256: receipt.BundleReceiptSHA256, SnapshotReceiptSHA256: m.SnapshotReceiptSHA256, ExpertParallelReceiptSHA256: m.ExpertParallelReceiptSHA256, EPLBReceiptSHA256: m.EPLBReceiptSHA256, RDMAReceiptSHA256: m.RDMAReceiptSHA256, LaunchReceiptSHA256: m.LaunchReceiptSHA256, ProviderClosureSHA256: m.ProviderClosureSHA256, Layout: m.Layout, Nodes: m.Nodes}, nil
}

func validateCurrentQwen38Provider(facts *vllmQwen38GroupFacts, current vllmQwen38ProviderObservation) error {
	if facts == nil || !current.Qualified || current.ProfileID == "" ||
		current.ExpectedClosureSHA256 != current.ObservedClosureSHA256 ||
		!knownQwen38ProviderClosure(current.ObservedClosureSHA256) ||
		!slices.Contains(current.AllowedClosureSHA256, current.ObservedClosureSHA256) ||
		len(current.Mismatches) != 0 {
		return errors.New("Qwen3.8 current provider qualification is unavailable")
	}
	if facts.ProviderClosureSHA256 != current.ObservedClosureSHA256 {
		return errors.New("Qwen3.8 provider closure changed after runtime preparation")
	}
	return nil
}

func (e *Executor) currentQwen38GroupFacts(ctx context.Context, st *engineState, dir string, receipt vllmRuntimeReceipt, modelDigest, compatibilitySHA256 string, nodes int) (*vllmQwen38GroupFacts, error) {
	facts, err := qwen38GroupFacts(st, dir, receipt, modelDigest, compatibilitySHA256, nodes)
	if err != nil {
		return nil, err
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		return nil, err
	}
	if err := validateCurrentQwen38Provider(facts, e.inspectQwen38Providers(ctx, recipe)); err != nil {
		return nil, err
	}
	return facts, nil
}

func validateQwen38GroupFacts(value *vllmQwen38GroupFacts, nodes int) error {
	layout, err := qwen38Layout(nodes)
	if err != nil || value == nil || value.RecipeSHA256 != vllmQwen38RecipeSHA256 || value.Layout != layout.Label || value.Nodes != nodes {
		return errors.New("Qwen3.8 member runtime does not match the reviewed fixed group layout")
	}
	if value.PreparedSchema2 {
		if !knownQwen38ProviderClosure(value.ProviderClosureSHA256) || !onboardingSHA.MatchString(value.BundleReceiptSHA256) || !onboardingSHA.MatchString(value.SnapshotReceiptSHA256) || value.SnapshotReceiptSHA256 != value.ModelDigest || !onboardingSHA.MatchString(value.LaunchReceiptSHA256) || !onboardingSHA.MatchString(value.RuntimeCompatibilitySHA256) || value.ExpertParallelBindingSHA256 != qwen38ReviewBinding("expert-parallel", nodes, layout, value.ModelDigest, value.RuntimeCompatibilitySHA256, value.ProviderClosureSHA256) || value.ExpertParallelReceiptSHA256 != "" || value.RDMAReceiptSHA256 != "" {
			return errors.New("Qwen3.8 prepared member lacks exact model, provider, EP binding or launch capability")
		}
		if layout.EPLB && value.EPLBBindingSHA256 != qwen38ReviewBinding("eplb", nodes, layout, value.ModelDigest, value.RuntimeCompatibilitySHA256, value.ProviderClosureSHA256) || !layout.EPLB && value.EPLBBindingSHA256 != "" || value.EPLBReceiptSHA256 != "" {
			return errors.New("Qwen3.8 prepared member EPLB binding differs from the fixed layout")
		}
		return nil
	}
	for _, digest := range []string{value.BundleReceiptSHA256, value.SnapshotReceiptSHA256, value.ExpertParallelReceiptSHA256, value.RDMAReceiptSHA256, value.LaunchReceiptSHA256} {
		if !onboardingSHA.MatchString(digest) {
			return errors.New("Qwen3.8 member lacks exact snapshot, EP, RDMA or launch admission")
		}
	}
	if !knownQwen38ProviderClosure(value.ProviderClosureSHA256) {
		return errors.New("Qwen3.8 member provider closure is not in the closed catalog")
	}
	if layout.EPLB && !onboardingSHA.MatchString(value.EPLBReceiptSHA256) || !layout.EPLB && value.EPLBReceiptSHA256 != "" {
		return errors.New("Qwen3.8 member EPLB admission differs from the fixed layout")
	}
	return nil
}

func validateVLLMGroupSelection(s vllmGroupSelection) error {
	if s.Parallelism != "" && s.Parallelism != "tensor" && s.Parallelism != "pipeline" {
		return errors.New("parallelism must be tensor or pipeline when selected")
	}
	if len(s.NodeIDs) < 2 || len(s.NodeIDs) > 3 || !remoteModelID(s.Model) {
		return errors.New("select two or three paired nodes and one exact model")
	}
	if isQwen38ProfileModel(s.Model) {
		if s.Parallelism != "" {
			return errors.New("Qwen3.8 uses one fixed reviewed layout; do not select tensor or pipeline mode")
		}
		if _, err := qwen38Layout(len(s.NodeIDs)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, node := range s.NodeIDs {
		if !cableIdentifier(node, 128) || seen[node] {
			return errors.New("select distinct current node IDs")
		}
		seen[node] = true
	}
	return nil
}

// Same content identity as the native rank owner; paths/timestamps are not model
// or runtime content and must not make equivalent participant runtimes differ.
func vllmPlanRuntimeDigest(r vllmRuntimeReceipt) string {
	return vllmRankRuntimeDigest(r)
}

func (e *Executor) inspectVLLMGroupFacts(ctx context.Context, modelID string, nodes int) (vllmGroupFacts, error) {
	var f vllmGroupFacts
	st, err := e.state("vllm")
	if err != nil {
		return f, err
	}
	if !lockVLLMGroupOwner(ctx, st) {
		return f, errors.Join(errors.New("vLLM owner is busy; review after the current operation"), ctx.Err())
	}
	defer st.opMu.Unlock()
	if err = rejectVLLMGroupOwnerMutation(st, "group review"); err != nil {
		return f, err
	}
	st.mu.Lock()
	busy := st.running || st.proc != nil || st.stopPending != 0 || st.adopted
	port := st.port
	st.mu.Unlock()
	if busy || e.shuttingDown.Load() {
		return f, errors.New("stop the selected engine before reviewing a serving group")
	}
	model, dir, err := verifyVLLMModel(ctx, st, modelID)
	if err != nil {
		return f, err
	}
	if isQwen38ProfileModel(model.ID) {
		if err = validateQwen38LocalProfile(model, dir); err != nil {
			return f, err
		}
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return f, err
	}
	if record.Active == "" || record.Removing {
		return f, errors.New("selected node has no available managed runtime")
	}
	python, _, receipt, err := validateVLLMRuntimeBinaries(st, record.Active)
	if err != nil {
		return f, err
	}
	if err = validateVLLMGroupRunnableReceipt(receipt, ""); err != nil {
		return f, err
	}
	pythonFacts, err := e.probeVLLMPython(ctx, python)
	if err != nil {
		return f, err
	}
	if reason := vllmPythonBuildPrerequisiteReason(pythonFacts); reason != "" {
		return f, errors.New(reason)
	}
	compatibility, err := e.captureVLLMRuntimeLock(ctx, st)
	if err != nil {
		return f, err
	}
	if compatibility.SourceRuntimeDigest != vllmPlanRuntimeDigest(receipt) {
		return f, errors.New("managed runtime changed during group compatibility inspection")
	}
	var qwen *vllmQwen38GroupFacts
	if isQwen38ProfileModel(model.ID) {
		qwen, err = e.currentQwen38GroupFacts(ctx, st, receipt.Environment, receipt, model.Digest, compatibility.CompatibilitySHA256, nodes)
		if err != nil {
			return f, err
		}
	}
	settings, err := readVLLMResourceSettings(st)
	if err != nil {
		return f, err
	}
	if settings.TensorParallelSize != nil && *settings.TensorParallelSize != 1 {
		return f, errors.New("group review requires one local GPU per selected node")
	}
	inventory, err := e.readVLLMPairInventory(ctx)
	if err != nil {
		return f, err
	}
	if inventory.HostUUID != e.vllmNodeID {
		return f, errors.New("current PAIR node identity changed")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	out, err := e.runVLLMCommand(probeCtx, e.vllmNvidiaSMIPath(), []string{"--query-gpu=uuid", "--format=csv,noheader,nounits"}, vllmPythonProbeEnv())
	cancel()
	if err != nil {
		return f, err
	}
	devices, err := vllmGPUSelection(strings.Join(strings.Fields(string(out)), ","))
	if err != nil {
		return f, err
	}
	gpu := ""
	if settings.GPUUUID != nil {
		gpu = *settings.GPUUUID
		if !slices.Contains(devices, gpu) {
			return f, errors.New("saved GPU is not currently available")
		}
	} else if len(devices) == 1 {
		gpu = devices[0]
	} else {
		return f, errors.New("choose one GPU in normal runtime settings before reviewing this node")
	}
	config, err := readVLLMGroupModelConfig(model, dir)
	if err != nil {
		return f, err
	}
	addresses, err := vllmGroupLocalAddresses()
	if err != nil {
		return f, err
	}
	f = vllmGroupFacts{NodeID: e.vllmNodeID, Model: model.ID, ModelDigest: model.Digest, RuntimeDigest: vllmPlanRuntimeDigest(receipt), RuntimeVersion: receipt.Version, RuntimeCompatibilitySHA256: compatibility.CompatibilitySHA256, GPUUUID: gpu, ModelPath: dir, APIPort: port, Resources: settings, Addresses: addresses, Config: config, Qwen38: qwen}
	return f, nil
}

func readVLLMGroupModelConfig(model vllmModel, dir string) ([]byte, error) {
	path := filepath.Join(dir, "config.json")
	if err := validateVLLMOwnedPath(dir, path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("model configuration exceeds its bounded review size")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("model configuration read is unavailable or oversized")
	}
	sum := sha256.Sum256(data)
	for _, entry := range model.Files {
		if entry.Path == "config.json" && entry.SHA256 == hex.EncodeToString(sum[:]) && entry.Size == int64(len(data)) {
			return data, nil
		}
	}
	return nil, errors.New("model configuration changed during review")
}

func vllmGroupLocalAddresses() ([]vllmGroupAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []vllmGroupAddress
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, raw := range addresses {
			ip, prefix, err := net.ParseCIDR(raw.String())
			if err != nil || ip.To4() == nil || !ip.IsPrivate() {
				continue
			}
			result = append(result, vllmGroupAddress{IP: ip.String(), Prefix: prefix.String()})
		}
	}
	if len(result) == 0 || len(result) > 32 {
		return nil, errors.New("bounded current private interface addresses are unavailable")
	}
	slices.SortFunc(result, func(a, b vllmGroupAddress) int { return strings.Compare(a.IP, b.IP) })
	return result, nil
}

// Bind the observed same-subnet addresses, not an assertion of isolation. The
// native admission owner must still qualify/contain the internal vLLM transport.
func assembleVLLMGroupPlan(s vllmGroupSelection, facts []vllmGroupFacts, pins []string) (vllmGroupPlan, error) {
	var plan vllmGroupPlan
	if err := validateVLLMGroupSelection(s); err != nil {
		return plan, err
	}
	if len(facts) != len(s.NodeIDs) || len(pins) != len(facts) {
		return plan, errors.New("every selected member needs current authenticated facts")
	}
	qwen := isQwen38ProfileModel(s.Model)
	qwenProviderClosure := ""
	runtimeVersion := facts[0].RuntimeVersion
	if qwen {
		runtimeVersion = vllmQwen38Runtime
	} else if runtimeVersion != vllmManagedVersion && runtimeVersion != "0.28.0" {
		return plan, errors.New("ordinary serving group requires one exact admitted current or retained vLLM runtime")
	}
	plan = vllmGroupPlan{Coordinator: s.NodeIDs[0], Model: s.Model, Runtime: runtimeVersion, Limits: vllmGroupLimitsForModel(s.Model), Topology: vllmGroupTopology{TensorParallel: len(facts), PipelineParallel: 1, DataParallel: 1}}
	// Three nodes are not implicitly a TP3 request, and two ordinary nodes
	// default to TP2 only over a bound direct socket lane. Three ordinary nodes
	// carry a bound ring socket in either mode. Model validation gates every
	// explicit mode before its fabric requirement.
	direct := !qwen && len(facts) == 2 && facts[0].directSocket != nil
	if !qwen && (s.Parallelism == "pipeline" || s.Parallelism == "" && (len(facts) == 3 || !direct)) {
		plan.Topology.TensorParallel = 1
		plan.Topology.PipelineParallel = len(facts)
	}
	sum := sha256.Sum256(facts[0].Config)
	configSHA256 := hex.EncodeToString(sum[:])
	if qwen {
		plan.Runtime = vllmQwen38Runtime
		topology, topologyErr := qwen38GroupTopology(len(facts), configSHA256)
		if topologyErr != nil {
			return vllmGroupPlan{}, topologyErr
		}
		plan.Topology = topology
		if facts[0].fabricTransport == nil {
			return vllmGroupPlan{}, errors.New("Qwen3.8 group requires the existing fabric owner's current qualification")
		}
		transport := *facts[0].fabricTransport
		plan.Transport = &transport
	} else {
		plan.Topology.ConfigSHA256 = configSHA256
		if len(facts) == 2 && plan.Topology.TensorParallel == 2 {
			if !direct {
				return vllmGroupPlan{}, errVLLMGroupTensorNeedsDirectFabric
			}
			plan.DirectSocket = cloneVLLMGroupDirectSocket(facts[0].directSocket)
		}
		if len(facts) == 3 {
			plan.RingSocket = cloneVLLMGroupRingSocket(facts[0].ringSocket)
		}
	}
	common := map[string]bool{}
	for _, a := range facts[0].Addresses {
		common[a.Prefix] = true
	}
	for _, f := range facts[1:] {
		current := map[string]bool{}
		for _, a := range f.Addresses {
			current[a.Prefix] = true
		}
		for prefix := range common {
			if !current[prefix] {
				delete(common, prefix)
			}
		}
	}
	if len(common) != 1 {
		return plan, errors.New("a unique observed common private subnet is required; ambiguous routing needs explicit native placement")
	}
	prefix := ""
	for value := range common {
		prefix = value
	}
	used := map[string]bool{}
	for rank, f := range facts {
		if f.NodeID != s.NodeIDs[rank] || f.Model != s.Model || f.ModelPath == "" || f.APIPort < 1024 || f.APIPort > 65535 || f.APIPort == vllmGroupMasterPort {
			return plan, errors.New("selected node, exact model path or managed API port changed")
		}
		if _, err := vllmGroupLayerPartition(plan.Topology, len(facts), f.Config); err != nil {
			return plan, err
		}
		if qwen {
			if f.RuntimeVersion != vllmQwen38Runtime {
				return plan, errors.New("Qwen3.8 member runtime version differs from the fixed profile")
			}
			if err := validateQwen38GroupFacts(f.Qwen38, len(facts)); err != nil {
				return plan, err
			}
			if qwenProviderClosure == "" {
				qwenProviderClosure = f.Qwen38.ProviderClosureSHA256
			} else if f.Qwen38.ProviderClosureSHA256 != qwenProviderClosure {
				return plan, errors.New("Qwen3.8 members have different qualified provider closures; update the nodes to one exact provider profile")
			}
		} else if f.Qwen38 != nil || f.RuntimeVersion != plan.Runtime {
			return plan, errors.New("ordinary serving group requires the same exact admitted vLLM runtime on every member")
		}
		raw, _ := json.Marshal(f.Resources)
		if _, err := decodeVLLMResourceSettings(raw); err != nil {
			return plan, err
		}
		if err := f.Resources.validateParallelism(); err != nil {
			return plan, err
		}
		if f.Resources.TensorParallelSize != nil && *f.Resources.TensorParallelSize != 1 {
			return plan, errors.New("each group participant must own exactly one local GPU")
		}
		if f.Resources.GPUUUID != nil && !strings.EqualFold(*f.Resources.GPUUUID, f.GPUUUID) {
			return plan, errors.New("saved GPU resource does not match observed member GPU")
		}
		address := ""
		for _, a := range f.Addresses {
			ip, network, err := net.ParseCIDR(a.Prefix)
			host := net.ParseIP(a.IP)
			if err != nil || ip.To4() == nil || host == nil || !host.IsPrivate() || !network.Contains(host) {
				return plan, errors.New("participant address is not a current private prefix member")
			}
			if a.Prefix == prefix {
				if address != "" && address != a.IP {
					return plan, errors.New("ambiguous participant address in common subnet")
				}
				address = a.IP
			}
		}
		if address == "" || used[address] {
			return plan, errors.New("selected participants need distinct current addresses")
		}
		used[address] = true
		resources := f.Resources
		member := vllmGroupMember{NodeID: f.NodeID, PinSHA256: pins[rank], GPUUUID: f.GPUUUID, ModelDigest: f.ModelDigest, RuntimeDigest: f.RuntimeDigest, RuntimeCompatibilitySHA256: f.RuntimeCompatibilitySHA256, Resources: &resources, Placement: &vllmGroupPlacement{ModelPath: f.ModelPath, Address: address, APIPort: f.APIPort, MasterPort: vllmGroupMasterPort}}
		if qwen {
			if f.fabric == nil {
				return plan, errors.New("Qwen3.8 member lacks the existing fabric owner's qualified endpoints")
			}
			member.Fabric = &vllmGroupMemberFabric{Lanes: append([]vllmGroupRDMALane(nil), f.fabric.Lanes...)}
		}
		plan.Members = append(plan.Members, member)
	}
	if !qwen && plan.Topology.TensorParallel == 3 && plan.RingSocket == nil {
		return vllmGroupPlan{}, errVLLMGroupTensorNeedsRingFabric
	}
	if _, err := vllmGroupPlanDigest(plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func (m *Manager) buildVLLMGroupPlan(ctx context.Context, s vllmGroupSelection) (vllmGroupPlan, error) {
	var empty vllmGroupPlan
	if err := validateVLLMGroupSelection(s); err != nil {
		return empty, err
	}
	if m.cableLocal == nil || m.exec == nil || s.NodeIDs[0] != m.cableLocal.nodeID {
		return empty, errors.New("the first selected node must be this local coordinator")
	}
	ctx, cancel := context.WithTimeout(ctx, vllmGroupReviewBudget(s.Model))
	defer cancel()
	principals, pins, err := m.vllmGroupIdentities(s)
	if err != nil {
		return empty, err
	}
	// Each member proves its own retained model content, which takes minutes for
	// large models, so members inspect concurrently. The first refusal cancels
	// the rest and is the one reported.
	facts := make([]vllmGroupFacts, len(s.NodeIDs))
	membersCtx, cancelMembers := context.WithCancel(ctx)
	defer cancelMembers()
	var failure error
	var failureMu sync.Mutex
	var members sync.WaitGroup
	for rank, node := range s.NodeIDs {
		members.Go(func() {
			memberErr := withVLLMGroupFactsBudget(membersCtx, s.Model, func(memberCtx context.Context) error {
				if rank == 0 {
					var inspectErr error
					facts[rank], inspectErr = m.exec.inspectVLLMGroupFacts(memberCtx, s.Model, len(s.NodeIDs))
					return inspectErr
				}
				peer, ok := m.peers.lookup(node)
				if !ok || peer.clusterUUID != principals[rank] {
					return errors.New("selected paired identity changed")
				}
				client, inspectErr := m.remoteClient(memberCtx, peer)
				if inspectErr != nil {
					return inspectErr
				}
				facts[rank], inspectErr = client.vllmGroupFacts(memberCtx, s)
				if inspectErr != nil {
					return inspectErr
				}
				current, known := m.peers.lookup(node)
				if !known || current.clusterUUID != principals[rank] || facts[rank].controlRoute == nil || !slices.Contains(current.addresses, facts[rank].controlRoute.Peer) {
					return errors.New("authenticated participant control address changed during review")
				}
				return nil
			})
			if memberErr != nil {
				failureMu.Lock()
				if failure == nil {
					failure = fmt.Errorf("group member %s: %w", node, memberErr)
					cancelMembers()
				}
				failureMu.Unlock()
			}
		})
	}
	members.Wait()
	if failure != nil {
		return empty, failure
	}
	for rank := 1; rank < len(facts); rank++ {
		if err := bindVLLMGroupControlRoute(&facts[0], &facts[rank]); err != nil {
			return empty, fmt.Errorf("group member %s: %w", s.NodeIDs[rank], err)
		}
	}
	after, afterPins, err := m.vllmGroupIdentities(s)
	if err != nil || !slices.Equal(principals, after) || !slices.Equal(pins, afterPins) {
		return empty, errors.New("paired membership changed during review")
	}
	if isQwen38ProfileModel(s.Model) {
		operationID, qualification, endpoints, fabricErr := requalifyVLLMGroupFabric(ctx, m.exec.fabric, s.NodeIDs)
		if fabricErr != nil {
			return empty, fabricErr
		}
		if err := bindQwen38FabricFacts(s, facts, principals, operationID, qualification, endpoints); err != nil {
			return empty, err
		}
	} else if len(s.NodeIDs) == 2 && s.Parallelism != "pipeline" {
		operationID, qualification, endpoints, fabricErr := m.vllmGroupDirectFabric(ctx, s.NodeIDs)
		switch {
		case fabricErr == nil:
			if err := bindVLLMGroupDirectSocketFacts(s, facts, principals, operationID, qualification, endpoints); err != nil {
				return empty, err
			}
		case errors.Is(fabricErr, errNoFabric) && s.Parallelism == "":
			// Genuine absence: the default falls back to PP2 on the management network.
		case errors.Is(fabricErr, errNoFabric):
			return empty, errVLLMGroupTensorNeedsDirectFabric
		default:
			return empty, fabricErr
		}
	} else if len(s.NodeIDs) == 3 {
		if err := resolveVLLMGroupRingSocket(ctx, s, facts, principals, m.vllmGroupRingFabric); err != nil {
			return empty, err
		}
	}
	return assembleVLLMGroupPlan(s, facts, pins)
}

func withVLLMGroupFactsBudget(ctx context.Context, model string, inspect func(context.Context) error) error {
	memberCtx, cancel := context.WithTimeout(ctx, vllmGroupFactsBudget(model))
	defer cancel()
	return inspect(memberCtx)
}

func vllmGroupReviewBudget(model string) time.Duration {
	if isQwen38ProfileModel(model) {
		// Concurrent member content inspections, each bounded by its own facts
		// budget, plus fresh fabric/identity work. This finite ceiling is not
		// measured hashing-time evidence.
		return 35 * time.Minute
	}
	return 2 * time.Minute
}
