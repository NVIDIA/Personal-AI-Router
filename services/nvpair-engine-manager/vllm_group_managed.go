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
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The paired owner resolves this private placement, never request-supplied URLs,
// shell commands or environment variables. Each node admits its own verified
// model path and a dedicated internal network before construction. Native integration
// remains responsible for establishing that isolation, not a boolean in JSON.
type vllmRankPlacement struct {
	LocalNode          string
	LocalAddress       string
	CoordinatorAddress string
	ModelPath          string
	MasterPort         int
	APIPort            int
}

type vllmRankReceipt struct {
	StartFailure         *vllmRankStartFailure   `json:"startFailure,omitempty"`
	SystemPlan           *vllmRankSystemPlan     `json:"systemPlan,omitempty"`
	SystemPolicy         *vllmSystemRankPolicy   `json:"systemPolicy,omitempty"`
	Supervisor           *vllmRankSystemIdentity `json:"supervisor,omitempty"`
	SystemEffectsApplied bool                    `json:"systemEffectsApplied,omitempty"`
	StartFenced          bool                    `json:"startFenced,omitempty"`
	RunID                string                  `json:"runId"`
	Generation           uint64                  `json:"generation"`
	PlanDigest           string                  `json:"planDigest"`
	Rank                 int                     `json:"rank"`
	State                string                  `json:"state"`
	CleanupConfirmed     bool                    `json:"cleanupConfirmed"`
}

// One native process per engine, reusing the existing process group, private
// environment and bounded stop. No participant endpoint is advertised.
type vllmManagedRank struct {
	mu                  sync.Mutex
	e                   *Executor
	st                  *engineState
	binding             vllmGroupBinding
	placement           vllmRankPlacement
	receipt             vllmRankReceipt
	path                string
	proc                *managedProc
	startup             *vllmStartupDiagnostics
	qualificationCancel context.CancelFunc
	qualificationDone   chan struct{}
	qualificationErr    error
}

func vllmRankRuntimeDigest(r vllmRuntimeReceipt) string {
	// Exclude installation paths/timestamps; retain executable/dependency content.
	identity := []string{r.Version, r.Architecture, r.WheelSHA256, r.PythonSHA256, r.PipReportSHA256}
	if r.Schema == vllmEnvironmentReceiptSchema {
		identity = []string{r.Version, r.Architecture, r.RecipeID, r.UVSHA256, r.PythonSHA256, r.CLISHA256, r.PipReportSHA256}
	} else if r.RecipeID != "" {
		identity = append(identity, r.RecipeID, r.RecipeSHA256, r.RecipeReceiptSHA256, r.BundleReceiptSHA256, r.NodeID)
	}
	raw, _ := json.Marshal(identity)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func validateVLLMGroupRunnableReceipt(receipt vllmRuntimeReceipt, expectedDigest string) error {
	current := admittedManagedVLLMReceipt(receipt) && receipt.Version == vllmManagedVersion
	retained := receipt.Schema == vllmLegacyReceiptSchema && receipt.RecipeID == "" &&
		receipt.Version == "0.28.0" && recognizedManagedVLLMReceipt(nil, receipt)
	qwen := admittedRetainedQwen38Receipt(receipt)
	if !current && !retained && !qwen {
		return errors.New("serving-group activation requires an exact admitted current or retained managed vLLM runtime")
	}
	if expectedDigest != "" && vllmRankRuntimeDigest(receipt) != expectedDigest {
		return errors.New("reviewed managed runtime content changed")
	}
	return nil
}

func (e *Executor) validateVLLMReviewedRuntime(ctx context.Context, st *engineState, receipt vllmRuntimeReceipt, member vllmGroupMember) error {
	if err := validateVLLMGroupRunnableReceipt(receipt, member.RuntimeDigest); err != nil {
		return err
	}
	lock, err := e.captureVLLMRuntimeLock(ctx, st)
	if err != nil {
		return err
	}
	if lock.SourceRuntimeDigest != member.RuntimeDigest ||
		lock.CompatibilitySHA256 != member.RuntimeCompatibilitySHA256 {
		return errors.New("reviewed managed runtime ABI or dependency compatibility changed")
	}
	return nil
}

func validateVLLMRankBinding(b vllmGroupBinding, p vllmRankPlacement) error {
	digest, err := vllmGroupPlanDigest(b.Plan)
	if err != nil || digest != b.PlanDigest || !onboardingID.MatchString(b.RunID) || b.Generation == 0 || b.Rank < 0 || b.Rank >= len(b.Plan.Members) {
		return errors.New("managed rank requires an exact supported reviewed operation")
	}
	if b.Plan.Members[b.Rank].NodeID != p.LocalNode || !filepath.IsAbs(p.ModelPath) || p.MasterPort < 1024 || p.MasterPort > 65535 || p.APIPort < 1024 || p.APIPort > 65535 || p.MasterPort == p.APIPort {
		return errors.New("managed rank placement does not match its node, local model or ports")
	}
	for _, value := range []string{p.LocalAddress, p.CoordinatorAddress} {
		ip := net.ParseIP(value)
		if ip == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
			return errors.New("managed rank needs admitted literal private network addresses")
		}
	}
	if b.Rank == 0 && p.LocalAddress != p.CoordinatorAddress {
		return errors.New("coordinator placement does not identify the local node")
	}
	return nil
}

func vllmRankArgs(cli string, b vllmGroupBinding, p vllmRankPlacement, s vllmResourceSettings) ([]string, error) {
	if err := validateVLLMRankBinding(b, p); err != nil {
		return nil, err
	}
	if b.Plan.RingSocket != nil {
		return nil, errors.New("the qualified ring socket requires the contained native owner")
	}
	if len(b.Plan.Members) != 2 || b.Plan.Topology.TensorParallel != 2 || b.Plan.Topology.PipelineParallel != 1 {
		return nil, errors.New("direct managed-process compiler supports TP2/PP1 only; use the contained native owner for other modes")
	}
	if isQwen38ProfileModel(b.Plan.Model) {
		return nil, errors.New("Qwen3.8 requires the contained native RoCE owner")
	}
	if b.Plan.DirectSocket != nil {
		return nil, errors.New("the qualified direct socket requires the contained native owner")
	}
	if _, err := decodeVLLMResourceSettings(mustVLLMRankJSON(s)); err != nil {
		return nil, err
	}
	if s.TensorParallelSize != nil && *s.TensorParallelSize != 1 {
		return nil, errors.New("group rank requires one locally selected GPU")
	}
	if s.GPUUUID != nil && !strings.EqualFold(*s.GPUUUID, b.Plan.Members[b.Rank].GPUUUID) {
		return nil, errors.New("saved GPU selection differs from reviewed group GPU")
	}
	args := []string{cli, "serve", p.ModelPath, "--served-model-name", b.Plan.Model,
		"--distributed-executor-backend", "mp", "--data-parallel-backend", "mp",
		"--data-parallel-size", "1", "--pipeline-parallel-size", "1", "--tensor-parallel-size", "2",
		"--nnodes", "2", "--node-rank", strconv.Itoa(b.Rank), "--master-addr", p.CoordinatorAddress, "--master-port", strconv.Itoa(p.MasterPort)}
	if b.Plan.Runtime == vllmManagedVersion {
		timeout := strconv.Itoa(vllmGroupDistributedTimeoutSeconds)
		args = append(args, "--distributed-timeout-seconds", timeout, "--cpu-distributed-timeout-seconds", timeout)
	}
	if b.Rank == 0 {
		args = append(args, "--host", "127.0.0.1", "--port", strconv.Itoa(p.APIPort))
	} else {
		args = append(args, "--headless")
	}
	return appendVLLMResourceArgs(args, s), nil
}
func mustVLLMRankJSON(s vllmResourceSettings) []byte { data, _ := json.Marshal(s); return data }

func validateVLLMRankReviewedSettings(st *engineState, b vllmGroupBinding, p vllmRankPlacement) error {
	member := b.Plan.Members[b.Rank]
	coordinator := b.Plan.Members[0].Placement
	if member.Resources == nil || member.Placement == nil || coordinator == nil {
		return rankStartError("admission", "placement_rank_mismatch", nil, 0)
	}
	if member.Placement.ModelPath != p.ModelPath || member.Placement.Address != p.LocalAddress ||
		member.Placement.APIPort != p.APIPort || member.Placement.MasterPort != p.MasterPort ||
		coordinator.Address != p.CoordinatorAddress || coordinator.MasterPort != p.MasterPort {
		return rankStartError("admission", "placement_rank_mismatch", nil, 0)
	}
	settings, err := readVLLMResourceSettings(st)
	if err != nil || !reflect.DeepEqual(settings, *member.Resources) {
		return rankStartError("admission", "saved_resource_settings_changed", err, 0)
	}
	return nil
}

func admitRetainedVLLMRank(st *engineState, path string) error {
	var old vllmRankReceipt
	err := readVLLMJSON(st.installDir, path, vllmRankReceiptLimit, &old)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !old.CleanupConfirmed || old.State != "stopped" {
		return rankStartError("admission", "retained_rank_owner_changed", err, 0)
	}
	return nil
}

// Called under the existing engine opMu after paired identity + placement
// admission. Journaling happens before launch; a retained unfinished receipt
// blocks replacement instead of guessing that the old rank vanished.
func (e *Executor) prepareVLLMRank(ctx context.Context, st *engineState, b vllmGroupBinding, p vllmRankPlacement) (*vllmManagedRank, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("managed distributed ranks require native Linux qualification")
	}
	if err := validateVLLMRankBinding(b, p); err != nil {
		return nil, rankStartError("admission", "operation_binding_invalid", err, 0)
	}
	if err := rejectVLLMClosedRank(st, b); err != nil {
		return nil, rankStartError("admission", "operation_binding_invalid", err, 0)
	}
	if err := validateVLLMRankReviewedSettings(st, b, p); err != nil {
		return nil, err
	}
	st.mu.Lock()
	busy := st.running || st.proc != nil || st.adopted || st.stopPending != 0
	st.mu.Unlock()
	if busy || (st.vllmRank != nil && st.vllmRank.reserved()) {
		return nil, rankStartError("admission", "prepare_owner_busy", nil, 0)
	}
	model, dir, err := verifyVLLMModel(ctx, st, b.Plan.Model)
	if err != nil || model.Digest != b.Plan.Members[b.Rank].ModelDigest || dir != p.ModelPath {
		return nil, rankStartError("admission", "model_content_changed", err, 0)
	}
	if isQwen38ProfileModel(model.ID) {
		if err = validateQwen38LocalProfile(model, dir); err != nil {
			return nil, rankStartError("admission", "model_manifest_changed", err, 0)
		}
	}
	config, err := readVLLMGroupModelConfig(model, dir)
	if err != nil {
		return nil, rankStartError("admission", "model_config_changed", err, 0)
	}
	if _, err := vllmGroupLayerPartition(b.Plan.Topology, len(b.Plan.Members), config); err != nil {
		return nil, rankStartError("admission", "model_config_changed", err, 0)
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return nil, rankStartError("admission", "runtime_content_changed", err, 0)
	}
	_, _, receipt, err := validateVLLMRuntimeBinaries(st, record.Active)
	if err != nil {
		return nil, rankStartError("admission", "runtime_content_changed", err, 0)
	}
	if err = e.validateVLLMReviewedRuntime(ctx, st, receipt, b.Plan.Members[b.Rank]); err != nil {
		return nil, rankStartError("admission", "runtime_binding_changed", err, 0)
	}
	if isQwen38ProfileModel(model.ID) {
		facts, factsErr := e.currentQwen38GroupFacts(ctx, st, receipt.Environment, receipt, model.Digest, b.Plan.Members[b.Rank].RuntimeCompatibilitySHA256, len(b.Plan.Members))
		if factsErr != nil || validateQwen38GroupFacts(facts, len(b.Plan.Members)) != nil {
			return nil, rankStartError("admission", "managed_runtime_receipt_changed", factsErr, 0)
		}
	}
	r := &vllmManagedRank{e: e, st: st, binding: b, placement: p, path: filepath.Join(st.installDir, "serving-rank.json")}
	r.binding.Plan = cloneVLLMGroupPlan(b.Plan)
	if err := admitRetainedVLLMRank(st, r.path); err != nil {
		return nil, err
	}
	r.receipt = vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Rank: b.Rank, State: "prepared"}
	if err := r.save(); err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.vllmRank = r
	st.mu.Unlock()
	return r, nil
}

func (r *vllmManagedRank) save() error {
	raw, err := json.MarshalIndent(r.receipt, "", "  ")
	if err != nil || len(raw)+1 > 16<<10 {
		return errors.New("rank receipt exceeds its bound")
	}
	return writeVLLMJSON(r.st.installDir, r.path, r.receipt)
}
func (r *vllmManagedRank) matches(b vllmGroupBinding) bool {
	digest, err := vllmGroupPlanDigest(b.Plan)
	digestMatches := err == nil && digest == b.PlanDigest
	if !digestMatches {
		legacyDigest, legacyErr := legacyVLLMGroupPlanDigest(b.Plan)
		digestMatches = legacyErr == nil && legacyDigest == b.PlanDigest
	}
	return digestMatches && b.RunID == r.binding.RunID && b.Generation == r.binding.Generation && b.PlanDigest == r.binding.PlanDigest && b.Rank == r.binding.Rank &&
		b.RunID == r.receipt.RunID && b.Generation == r.receipt.Generation && b.PlanDigest == r.receipt.PlanDigest && b.Rank == r.receipt.Rank
}

func (r *vllmManagedRank) start(ctx context.Context, b vllmGroupBinding) error {
	r.mu.Lock()
	publish := false
	defer func() {
		r.mu.Unlock()
		if publish {
			r.e.emitState("vllm")
		}
	}()
	if !r.matches(b) || r.receipt.State != "prepared" || ctx.Err() != nil {
		return errors.New("rank start requires this exact prepared generation")
	}
	e, st := r.e, r.st
	if err := validateVLLMRankReviewedSettings(st, b, r.placement); err != nil {
		return err
	}
	model, dir, err := verifyVLLMModel(ctx, st, b.Plan.Model)
	if err != nil || dir != r.placement.ModelPath || model.Digest != b.Plan.Members[b.Rank].ModelDigest {
		return errors.New("model changed after preparation")
	}
	if isQwen38ProfileModel(model.ID) {
		if err = validateQwen38LocalProfile(model, dir); err != nil {
			return err
		}
	}
	config, err := readVLLMGroupModelConfig(model, dir)
	if err != nil {
		return err
	}
	if _, err := vllmGroupLayerPartition(b.Plan.Topology, len(b.Plan.Members), config); err != nil {
		return err
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return err
	}
	python, cli, receipt, err := validateVLLMRuntimeBinaries(st, record.Active)
	if err == nil {
		err = e.validateVLLMReviewedRuntime(ctx, st, receipt, b.Plan.Members[b.Rank])
	}
	if err != nil {
		return errors.New("runtime content or compatibility changed after preparation")
	}
	if isQwen38ProfileModel(model.ID) {
		facts, factsErr := e.currentQwen38GroupFacts(ctx, st, receipt.Environment, receipt, model.Digest, b.Plan.Members[b.Rank].RuntimeCompatibilitySHA256, len(b.Plan.Members))
		if factsErr != nil || validateQwen38GroupFacts(facts, len(b.Plan.Members)) != nil {
			return errors.New("Qwen3.8 runtime admission changed after preparation")
		}
	}
	facts, err := e.probeVLLMPython(ctx, python)
	if err != nil {
		return err
	}
	if reason := vllmPythonBuildPrerequisiteReason(facts); reason != "" {
		return errors.New(reason)
	}
	settings, err := readVLLMResourceSettings(st)
	if err != nil {
		return err
	}
	args, err := vllmRankArgs(cli, b, r.placement, settings)
	if err != nil {
		return err
	}
	env, cleanup, err := e.vllmChildEnv(st, receipt.Environment)
	if err != nil {
		return err
	}
	env = e.vllmServingEnv(env)
	inventory, err := e.runVLLMCommand(ctx, e.vllmNvidiaSMIPath(), []string{"--query-gpu=uuid", "--format=csv,noheader,nounits"}, env)
	found := false
	for _, line := range strings.Split(string(inventory), "\n") {
		found = found || strings.EqualFold(strings.TrimSpace(line), b.Plan.Members[b.Rank].GPUUUID)
	}
	if err != nil || !found {
		cleanup()
		return errors.New("reviewed GPU is not available")
	}
	env = append(env, "CUDA_VISIBLE_DEVICES="+b.Plan.Members[b.Rank].GPUUUID, "VLLM_HOST_IP="+r.placement.LocalAddress)
	if err := ctx.Err(); err != nil {
		cleanup()
		return err
	}
	r.receipt.State = "starting"
	if err := r.save(); err != nil {
		cleanup()
		return err
	}
	startup := newVLLMStartupDiagnostics()
	startup.runtimeDir = filepath.Dir(filepath.Dir(python))
	envMap := make(map[string]string, len(env))
	for _, value := range env {
		key, item, ok := strings.Cut(value, "=")
		if ok {
			envMap[key] = item
		}
	}
	proc, err := startManagedProcWithEnv(python, args, envMap, false, startup.onLine)
	if err != nil {
		cleanup()
		startup.stop()
		r.receipt.State = "failed"
		_ = r.save()
		return err
	}
	r.proc, r.startup = proc, startup
	st.mu.Lock()
	st.proc = proc
	st.running = true
	st.healthy = false
	st.stopping = false
	st.adopted = false
	st.port = r.placement.APIPort
	st.servingModel = b.Plan.Model
	st.version = b.Plan.Runtime
	st.gen++
	st.mu.Unlock()
	publish = true
	r.receipt.State = "started"
	if err := r.save(); err != nil {
		return err
	} // exact owner retained for normal stop
	// Parent group owns readiness and failure cleanup; no standalone auto-restart.
	return nil
}

// Only this sentinel translates to C's successful, bound State="started"
// pending receipt. Real readiness/qualification failures must remain errors.
var errVLLMRankReadinessPending = errors.New("owned vLLM rank readiness is pending")

func (r *vllmManagedRank) ready(ctx context.Context, b vllmGroupBinding) error {
	state, err := r.readiness(ctx, b)
	if err != nil {
		return err
	}
	if state != "ready" {
		return errVLLMRankReadinessPending
	}
	return nil
}

// Readiness is a bounded observation. Generation qualification runs at most
// once for this immutable owner, outside the short peer HTTP request lifetime.
// Its context covers bounded first-inference warmup and is canceled by normal rank Stop.
func (r *vllmManagedRank) readiness(ctx context.Context, b vllmGroupBinding) (string, error) {
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return "", err
	}
	if !r.matches(b) || r.proc == nil || (r.receipt.State != "started" && r.receipt.State != "ready") {
		err := r.qualificationErr
		if err == nil {
			err = errors.New("rank readiness has no matching launched owner")
		}
		r.mu.Unlock()
		return "", err
	}
	proc := r.proc
	select {
	case <-proc.done:
		r.mu.Unlock()
		return "", errors.New("owned rank exited")
	default:
	}
	if b.Rank != 0 {
		r.mu.Unlock()
		return "", errors.New("participant liveness is not collective model readiness")
	}
	if r.qualificationDone != nil && r.receipt.State == "started" {
		r.mu.Unlock()
		return "started", nil
	}
	wasReady := r.receipt.State == "ready"
	r.mu.Unlock()

	// Existing proof checks the owned listener, runtime version, health and exact
	// model. It performs no inference and already bounds its HTTP work to 2s.
	healthy := r.e.probeVLLM(ctx, r.placement.APIPort)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.proc != proc || !r.matches(b) || (r.receipt.State != "started" && r.receipt.State != "ready") {
		return "", errors.New("rank owner changed during readiness observation")
	}
	select {
	case <-proc.done:
		return "", errors.New("owned rank exited")
	default:
	}
	if !healthy {
		if wasReady || r.receipt.State == "ready" {
			return "", errors.New("qualified coordinator lost owned model health")
		}
		return "started", nil
	}
	if r.receipt.State == "ready" {
		return "ready", nil
	}
	if r.qualificationDone == nil {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), vllmGroupQualificationBudget)
		done := make(chan struct{})
		r.qualificationCancel, r.qualificationDone = cancel, done
		go r.qualifyGeneration(work, cancel, done, proc, b)
	}
	return "started", nil
}

func (r *vllmManagedRank) qualifyGeneration(ctx context.Context, cancel context.CancelFunc, done chan struct{}, proc *managedProc, b vllmGroupBinding) {
	defer close(done)
	defer cancel()
	err := r.e.verifyVLLMGeneration(ctx, r.st, r.placement.APIPort)
	if err == nil && !r.e.probeVLLM(ctx, r.placement.APIPort) {
		err = errors.New("coordinator lost owned model health during qualification")
	}
	r.mu.Lock()
	publish := false
	defer func() {
		r.mu.Unlock()
		if publish {
			r.e.emitState("vllm")
		}
	}()
	// Stop withdraws the state and cancels this context before process cleanup.
	// A late HTTP completion cannot republish readiness after that withdrawal.
	if r.proc != proc || !r.matches(b) || r.receipt.State != "started" {
		return
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	select {
	case <-proc.done:
		err = errors.New("owned rank exited during qualification")
	default:
	}
	if err != nil {
		r.qualificationErr = err
		r.receipt.State = "failed"
		_ = r.save()
		return
	}
	r.receipt.State = "ready"
	if err := r.save(); err != nil {
		r.qualificationErr = err
		r.receipt.State = "failed"
		return
	}
	if r.startup != nil {
		r.startup.stop()
	}
	r.st.mu.Lock()
	r.st.healthy = true
	r.st.mu.Unlock()
	publish = true
}

func (r *vllmManagedRank) stop(b vllmGroupBinding) error {
	if r.hasSystemOwner() {
		r.mu.Lock()
		plan := r.receipt.SystemPlan
		matches := b.RunID == plan.RunID && b.Generation == plan.Generation && b.PlanDigest == plan.PlanDigest && b.Rank == plan.Rank
		r.mu.Unlock()
		if !matches {
			return errors.New("stop belongs to another native rank generation")
		}
		return r.stopSystem(context.Background(), nil)
	}
	r.mu.Lock()
	publish := false
	defer func() {
		r.mu.Unlock()
		if publish {
			r.e.emitState("vllm")
		}
	}()
	if !r.matches(b) {
		return errors.New("stop belongs to another rank generation")
	}
	if r.receipt.CleanupConfirmed {
		return nil
	}
	r.receipt.State = "stopping"
	if r.qualificationCancel != nil {
		r.qualificationCancel()
	}
	journalErr := r.save() // Failed retention must never suppress owned-process Stop.
	r.st.mu.Lock()
	r.st.healthy = false
	r.st.stopping = true
	r.st.mu.Unlock()
	publish = true
	if r.proc != nil {
		if err := stopVLLMProcess(r.proc, 5*time.Second); err != nil {
			return errors.Join(journalErr, err)
		}
	}
	if r.startup != nil {
		r.startup.stop()
	}
	r.receipt.State = "stopped"
	r.receipt.CleanupConfirmed = true
	if err := r.save(); err != nil {
		r.receipt.CleanupConfirmed = false
		return errors.Join(journalErr, err)
	}
	r.st.mu.Lock()
	r.st.running = false
	r.st.proc = nil
	r.st.mu.Unlock()
	r.proc = nil
	return nil
}

func (r *vllmManagedRank) call(ctx context.Context, b vllmGroupBinding, action string) error {
	switch action {
	case "status":
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.matches(b) || r.proc == nil || (r.receipt.State != "started" && r.receipt.State != "ready") {
			return errors.New("rank has no matching live process owner")
		}
		select {
		case <-r.proc.done:
			return errors.New("owned rank exited")
		default:
			return nil
		}
	case "start":
		return r.start(ctx, b)
	case "ready":
		return r.ready(ctx, b)
	case "stop":
		return r.stop(b)
	default:
		return fmt.Errorf("unsupported managed rank action %q", action)
	}
}

func (r *vllmManagedRank) reserved() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.receipt.CleanupConfirmed
}
