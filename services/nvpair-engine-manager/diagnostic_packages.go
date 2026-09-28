// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/zlib"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed diagnostic_mpi_packages.py
var diagnosticPackagesPython string

func (d *diagnosticService) closePackageAdmissionLocked() {
	d.packageAdmissionClosed = true
	for id, review := range d.mpiReviews {
		clear(review.PrivateKey)
		review.PrivateKey = nil
		delete(d.mpiReviews, id)
	}
	for _, cancel := range d.cancels {
		cancel()
	}
	for _, run := range d.packageRuns {
		if run.cancel != nil {
			run.cancel()
		}
	}
	for _, run := range d.runtimeRuns {
		if run.cancel != nil {
			run.cancel()
		}
	}
}
func (d *diagnosticService) closePackageAdmission() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closePackageAdmissionLocked()
}

type diagnosticPackageTarget struct {
	NodeID               string               `json:"nodeId"`
	Principal            string               `json:"principal"`
	Local                bool                 `json:"local"`
	Address              string               `json:"address"`
	Candidate            *onboardingCandidate `json:"candidate,omitempty"`
	State                string               `json:"state"`
	Reason               string               `json:"reason,omitempty"`
	Review               json.RawMessage      `json:"review,omitempty"`
	Receipt              json.RawMessage      `json:"receipt,omitempty"`
	CleanupConfirmed     bool                 `json:"cleanupConfirmed"`
	UnitMetadataRetained bool                 `json:"unitMetadataRetained,omitempty"`
}

type diagnosticPackageReview struct {
	FirstInstall            bool                      `json:"firstInstall,omitempty"`
	Purpose                 string                    `json:"purpose,omitempty"`
	DependencyUpgradeReview bool                      `json:"dependencyUpgradeReview,omitempty"`
	ReviewID                string                    `json:"reviewId"`
	GroupID                 string                    `json:"groupId"`
	ExpiresAt               int64                     `json:"expiresAt"`
	CanApprove              bool                      `json:"canApprove"`
	Targets                 []diagnosticPackageTarget `json:"targets"`
}

type diagnosticPackageBinding struct {
	Review        diagnosticPackageReview `json:"review"`
	RuntimeNodeID string                  `json:"runtimeNodeId,omitempty"`
	diagnosticParticipantBinding
}

// Package installation and runtime builds share the existing paired identity
// and volatile access owner. Neither carries credentials in its durable record.
type diagnosticParticipantBinding struct {
	Pair          diagnosticSetupRequest       `json:"pair"`
	Controller    string                       `json:"controller"`
	ControllerPin string                       `json:"controllerPin"`
	Pins          map[string]string            `json:"pins"`
	Targets       []diagnosticInspectionTarget `json:"targets"`
	Generations   map[string]string            `json:"-"`
}

func validDiagnosticParticipantBinding(binding diagnosticParticipantBinding) bool {
	count := len(binding.Targets)
	if !validDiagnosticParticipantCount(count) || len(binding.Pair.Members) != count || len(binding.Pins) != count || validateDiagnosticSetupRoster(binding.Pair) != nil || !diagnosticToken.MatchString(binding.Controller) || !diagnosticDigest.MatchString(binding.ControllerPin) {
		return false
	}
	for i, target := range binding.Targets {
		member := binding.Pair.Members[i]
		if target.NodeID != member.NodeID || target.Principal != member.Principal || !diagnosticDigest.MatchString(binding.Pins[target.Principal]) || net.ParseIP(target.Address) == nil || target.Local != (target.Principal == binding.Controller) || target.Local != (target.Candidate == nil) {
			return false
		}
		if !target.Local && (!onboardingID.MatchString(target.Candidate.CandidateID) || target.Candidate.Address != target.Address || target.Candidate.Port < 1 || target.Candidate.Port > 65535 || target.Candidate.HostKeySHA256 == "") {
			return false
		}
	}
	return true
}

type diagnosticPackageOperation struct {
	FirstInstall     bool                      `json:"firstInstall,omitempty"`
	Purpose          string                    `json:"purpose,omitempty"`
	OperationID      string                    `json:"operationId"`
	ReviewID         string                    `json:"reviewId"`
	GroupID          string                    `json:"groupId"`
	State            string                    `json:"state"`
	Stage            string                    `json:"stage"`
	Revision         uint64                    `json:"revision"`
	StartedAt        int64                     `json:"startedAt"`
	FinishedAt       int64                     `json:"finishedAt,omitempty"`
	Targets          []diagnosticPackageTarget `json:"targets"`
	CleanupConfirmed bool                      `json:"cleanupConfirmed"`
	RuntimeValidated bool                      `json:"runtimeValidated"`
}

type diagnosticPackageRun struct {
	DependencyUpgradeApprovals []diagnosticDependencyUpgradeApproval `json:"dependencyUpgradeApprovals,omitempty"`
	SchemaVersion              int                                   `json:"schemaVersion"`
	Owner                      string                                `json:"owner"`
	Public                     diagnosticPackageOperation            `json:"operation"`
	Binding                    diagnosticPackageBinding              `json:"binding"`
	cancel                     context.CancelFunc
	reconciling                bool
}

// Volatile approval frame only. Never write this type to a journal or log.
type diagnosticPackageApprove struct {
	FirstInstall               bool                                  `json:"firstInstall,omitempty"`
	Purpose                    string                                `json:"purpose,omitempty"`
	RuntimeNodeID              string                                `json:"runtimeNodeId,omitempty"`
	DependencyUpgradeApprovals []diagnosticDependencyUpgradeApproval `json:"dependencyUpgradeApprovals,omitempty"`
	ReviewID                   string                                `json:"reviewId"`
	Elevation                  []diagnosticPackageElevation          `json:"elevation,omitempty"`
}

type diagnosticPackageElevation struct {
	NodeID            string `json:"nodeId"`
	ElevationPassword string `json:"elevationPassword,omitempty"`
	NonInteractive    bool   `json:"nonInteractive"`
}

func packageElevation(entries []diagnosticPackageElevation, targets []diagnosticInspectionTarget) (map[string]string, map[string]bool, error) {
	passwords, consent := map[string]string{}, map[string]bool{}
	if len(entries) > len(targets) {
		return nil, nil, errors.New("too many administrator access selections")
	}
	for _, entry := range entries {
		known := false
		for _, target := range targets {
			known = known || target.NodeID == entry.NodeID
		}
		_, duplicate := consent[entry.NodeID]
		if !known || duplicate || len(entry.ElevationPassword) > 4096 || strings.ContainsAny(entry.ElevationPassword, "\x00\r\n") || entry.NonInteractive && entry.ElevationPassword != "" {
			clear(passwords)
			return nil, nil, errors.New("invalid administrator access selection")
		}
		passwords[entry.NodeID] = entry.ElevationPassword
		consent[entry.NodeID] = entry.NonInteractive || entry.ElevationPassword != ""
	}
	return passwords, consent, nil
}

type diagnosticInstalledMPIPackage struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

func packageFamilyVersion(family []diagnosticInstalledMPIPackage) string {
	if len(family) != 4 {
		return ""
	}
	names := map[string]bool{}
	version := family[0].Version
	if !strings.HasPrefix(version, "4.1.") {
		return ""
	}
	for _, pkg := range family {
		if names[pkg.Name] || pkg.Version != version || pkg.Status != "install ok installed" {
			return ""
		}
		architecture := "arm64"
		switch pkg.Name {
		case "libopenmpi-dev", "openmpi-bin", "libopenmpi3t64":
		case "openmpi-common":
			architecture = "all"
		default:
			return ""
		}
		if pkg.Architecture != architecture {
			return ""
		}
		names[pkg.Name] = true
	}
	return version
}

type diagnosticPackageResult struct {
	SchemaVersion        int                             `json:"schemaVersion"`
	Action               string                          `json:"action"`
	OperationID          string                          `json:"operationId"`
	PlanDigest           string                          `json:"planDigest"`
	State                string                          `json:"state"`
	EffectsApplied       bool                            `json:"effectsApplied"`
	EffectsUnknown       bool                            `json:"effectsUnknown,omitempty"`
	CleanupConfirmed     bool                            `json:"cleanupConfirmed"`
	UnitMetadataRetained bool                            `json:"unitMetadataRetained"`
	RuntimeValidated     bool                            `json:"runtimeValidated"`
	Identity             *diagnosticPackageIdentity      `json:"identity"`
	RuntimeBinding       *diagnosticPythonRuntimeBinding `json:"runtimeBinding,omitempty"`
	RuntimePrerequisites *diagnosticPythonPrerequisites  `json:"runtimePrerequisites,omitempty"`
	Plan                 json.RawMessage                 `json:"plan"`
	Receipt              json.RawMessage                 `json:"receipt"`
	ArchiveReceipt       json.RawMessage                 `json:"archiveReceipt"`
	InstalledFamily      []diagnosticInstalledMPIPackage `json:"installedFamily"`
	Errors               []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func diagnosticPackageProgram() string {
	// Both modules are pinned product code. No private input is embedded in argv.
	program := "import sys,types\n_module=types.ModuleType('diagnostic_tools_remote')\nexec(" + strconv.Quote(diagnosticInspectPython) + ",_module.__dict__)\nsys.modules['diagnostic_tools_remote']=_module\n" + diagnosticPackagesPython
	// Keep the existing native argument bound while adding one fixed recipe.
	// The payload is compiled-in product source, never caller-supplied program text.
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write([]byte(program))
	_ = writer.Close()
	return "import base64,zlib;exec(zlib.decompress(base64.b64decode(" + strconv.Quote(base64.StdEncoding.EncodeToString(compressed.Bytes())) + ")))"
}

func (d *diagnosticService) packageAccess(target diagnosticInspectionTarget, accepted map[string]string) (onboardingPrivateTarget, error) {
	if target.Local {
		return onboardingPrivateTarget{}, nil
	}
	if target.Candidate == nil {
		return onboardingPrivateTarget{}, errors.New("remote access target is missing")
	}
	s := d.m.onboarding
	s.mu.Lock()
	current := s.targets[target.Candidate.CandidateID]
	var value onboardingPrivateTarget
	if current != nil {
		value = *current
	}
	s.mu.Unlock()
	if current == nil || !value.candidate.AccessAvailable || value.accessGeneration == "" || !value.expiresAt.After(time.Now()) || value.changedKey || value.candidate.CandidateID != target.Candidate.CandidateID || value.candidate.Port != target.Candidate.Port || value.candidate.HostKeySHA256 == "" || value.candidate.Address != target.Address || value.candidate.HostKeySHA256 != target.Candidate.HostKeySHA256 {
		return value, errors.New("current device account access is unavailable or changed")
	}
	if !value.candidate.HostKeyTrusted && accepted[value.candidate.CandidateID] != value.candidate.HostKeySHA256 {
		return value, errors.New("the exact SSH fingerprint needs approval for this package review")
	}
	value.candidate.HostKeyTrusted = true
	return value, nil
}

func (d *diagnosticService) packageNative(ctx context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, request map[string]any) ([]byte, error) {
	if d.packageTestRun == nil && request["action"] == "review" && request["firstInstall"] == true && request["runtimePreparation"] == true {
		if err := d.stagePythonLocalArchives(ctx, target, access); err != nil {
			return nil, err
		}
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	defer clear(input)
	if d.packageTestRun != nil {
		return d.packageTestRun(ctx, target, access, input)
	}
	return d.participantProgram(ctx, target, access, diagnosticPackageProgram(), input)
}

// Only fixed embedded programs from typed setup handlers call this transport.
// Program text is never accepted from a client request or persisted job manifest.
func (d *diagnosticService) participantProgram(ctx context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, program string, input []byte) ([]byte, error) {
	command := "/usr/bin/python3 -I -c " + onboardingQuote(program)
	if len(command) >= 128<<10 {
		return nil, errors.New("embedded diagnostic program exceeds the native argument limit")
	}
	if target.Local {
		if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
			return nil, errors.New("local diagnostic setup requires its Linux ARM64 owner")
		}
		return diagnosticProcessInput(ctx, "/usr/bin/python3", []string{"-I", "-c", program}, []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + os.Getenv("HOME")}, bytes.NewReader(input), nil)
	}
	client, err := d.m.onboarding.dial(ctx, access.candidate, access.access)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBeforeRemoteCommand, err)
	}
	defer client.close()
	return client.run(ctx, command, bytes.NewReader(input))
}

func packageResult(raw []byte) (diagnosticPackageResult, error) {
	var result diagnosticPackageResult
	if len(raw) > 128<<10 || json.Unmarshal(raw, &result) != nil || result.SchemaVersion != 1 || result.RuntimeValidated {
		return result, errors.New("invalid package operation reply")
	}
	if result.UnitMetadataRetained || result.EffectsUnknown {
		result.CleanupConfirmed = false
	}
	if result.State != "succeeded" && (len(result.Receipt) == 0 || bytes.Equal(bytes.TrimSpace(result.Receipt), []byte("null"))) && len(result.ArchiveReceipt) != 0 && !bytes.Equal(bytes.TrimSpace(result.ArchiveReceipt), []byte("null")) {
		result.Receipt, _ = json.Marshal(map[string]any{"kind": "pair-mpi-package-failure", "operationId": result.OperationID, "planDigest": result.PlanDigest, "runtimeValidated": false, "installationSucceeded": false, "archiveReceipt": result.ArchiveReceipt})
	}
	return result, nil
}

func packageActionResult(raw []byte, action, operationID, planDigest string) (diagnosticPackageResult, error) {
	result, err := packageResult(raw)
	if err != nil {
		return result, err
	}
	if result.Action != action || operationID != "" && result.OperationID != operationID || planDigest != "" && result.PlanDigest != planDigest {
		return result, errors.New("package reply does not match the requested action, operation and approved plan")
	}
	return result, nil
}

type diagnosticApprovedPackagePlan struct {
	SchemaVersion  int                             `json:"schemaVersion"`
	Identity       *diagnosticPackageIdentity      `json:"identity"`
	RuntimeBinding *diagnosticPythonRuntimeBinding `json:"runtimeBinding,omitempty"`
	RecipeID       string                          `json:"recipeId"`
	PlanDigest     string                          `json:"planDigest"`
	RootPackage    struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"rootPackage"`
	Packages      []map[string]any `json:"packages"`
	ArchivePolicy struct {
		WorkerSHA256    string          `json:"workerSha256"`
		NeedrestartMode string          `json:"needrestartMode,omitempty"`
		ArchiveSource   json.RawMessage `json:"archiveSource,omitempty"`
	} `json:"archivePolicy"`
	Limits struct {
		MaxPackages           int  `json:"maxPackages"`
		Upgrades              bool `json:"upgrades"`
		Removals              bool `json:"removals"`
		Recommends            bool `json:"recommends"`
		MaxDependencyUpgrades int  `json:"maxDependencyUpgrades,omitempty"`
	} `json:"limits"`
}

func approvedPackagePlan(raw json.RawMessage) (diagnosticApprovedPackagePlan, error) {
	var plan diagnosticApprovedPackagePlan
	if json.Unmarshal(raw, &plan) != nil || !diagnosticDigest.MatchString(plan.PlanDigest) {
		return plan, errors.New("invalid fixed package plan")
	}
	if plan.RecipeID == diagnosticPythonPackageRecipe {
		if !validPythonPackagePlan(plan) {
			return plan, errors.New("invalid fixed Python-header package plan")
		}
		return plan, nil
	}
	if (plan.RecipeID != diagnosticPackageLegacyRecipe && plan.RecipeID != diagnosticPackageUpgradeRecipe) || plan.RootPackage.Name != "libopenmpi-dev" || len(plan.Packages) == 0 || len(plan.Packages) > 128 {
		return plan, errors.New("invalid fixed MPI package plan")
	}
	if plan.RecipeID == diagnosticPackageUpgradeRecipe && (!plan.Limits.Upgrades || plan.Limits.Removals || plan.Limits.Recommends || plan.Limits.MaxDependencyUpgrades != 14 || plan.ArchivePolicy.NeedrestartMode != "l" || !diagnosticDigest.MatchString(plan.ArchivePolicy.WorkerSHA256)) {
		return plan, errors.New("dependency upgrade policy or worker binding changed")
	}
	if _, err := diagnosticPackagePlanUpgrades(plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func validPackageReceipt(result diagnosticPackageResult, plan diagnosticApprovedPackagePlan, operationID string) bool {
	var receipt struct {
		RuntimeBinding       *diagnosticPythonRuntimeBinding `json:"runtimeBinding,omitempty"`
		RuntimePrerequisites *diagnosticPythonPrerequisites  `json:"runtimePrerequisites,omitempty"`
		RecipeID             string                          `json:"recipeId"`
		PlanDigest           string                          `json:"planDigest"`
		RuntimeValidated     *bool                           `json:"runtimeValidated"`
		Packages             []map[string]any                `json:"packages"`
		InstalledFamily      []diagnosticInstalledMPIPackage `json:"installedFamily"`
		ArchiveReceipt       *struct {
			SchemaVersion    int    `json:"schemaVersion"`
			OperationID      string `json:"operationId"`
			PlanDigest       string `json:"planDigest"`
			WorkerSHA256     string `json:"workerSha256"`
			State            string `json:"state"`
			Phase            string `json:"phase"`
			Verified         bool   `json:"verified"`
			DownloadExitCode *int   `json:"downloadExitCode"`
			InstallExitCode  *int   `json:"installExitCode"`
			Archives         []struct {
				Name         string `json:"name"`
				Version      string `json:"version"`
				Architecture string `json:"architecture"`
				SHA256       string `json:"sha256"`
				Size         uint64 `json:"size"`
			}
		} `json:"archiveReceipt"`
	}
	if json.Unmarshal(result.Receipt, &receipt) != nil || receipt.RecipeID != plan.RecipeID || receipt.PlanDigest != plan.PlanDigest || receipt.RuntimeValidated == nil || *receipt.RuntimeValidated || !reflect.DeepEqual(receipt.Packages, plan.Packages) {
		return false
	}
	if plan.RecipeID == diagnosticPythonPackageRecipe {
		if !validPythonPackagePlan(plan) || pythonPackageFamilyVersion(receipt.InstalledFamily) != diagnosticPythonPackageVersion || !reflect.DeepEqual(receipt.RuntimeBinding, plan.RuntimeBinding) || !validPythonPrerequisites(receipt.RuntimePrerequisites, plan.Identity, true) {
			return false
		}
	} else {
		if len(receipt.InstalledFamily) != 4 || packageFamilyVersion(receipt.InstalledFamily) != plan.RootPackage.Version {
			return false
		}
	}
	if !result.EffectsApplied {
		return true
	} // Verified already-present packages make no archive-byte claim.
	archive := receipt.ArchiveReceipt
	if archive == nil || archive.SchemaVersion != 1 || archive.OperationID != operationID || archive.PlanDigest != plan.PlanDigest || archive.WorkerSHA256 != plan.ArchivePolicy.WorkerSHA256 || !diagnosticDigest.MatchString(archive.WorkerSHA256) || archive.State != "succeeded" || archive.Phase != "complete" || !archive.Verified || archive.DownloadExitCode == nil || *archive.DownloadExitCode != 0 || archive.InstallExitCode == nil || *archive.InstallExitCode != 0 || len(archive.Archives) != len(plan.Packages) {
		return false
	}
	seen := map[string]bool{}
	for _, observed := range archive.Archives {
		if seen[observed.Name] {
			return false
		}
		seen[observed.Name] = true
		found := false
		for _, approved := range plan.Packages {
			if approved["name"] == observed.Name {
				size, ok := approved["size"].(float64)
				found = ok && size == float64(observed.Size) && approved["version"] == observed.Version && approved["architecture"] == observed.Architecture && approved["sha256"] == observed.SHA256
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func reviewedPackageVersion(result diagnosticPackageResult) string {
	if result.State == "already_installed" {
		if result.RuntimeBinding != nil {
			if (validPythonRuntimeBinding(result.RuntimeBinding, result.Identity) || validPythonSourceBinding(result.RuntimeBinding, result.Identity)) && validPythonPrerequisites(result.RuntimePrerequisites, result.Identity, true) {
				return pythonPackageFamilyVersion(result.InstalledFamily)
			}
			return ""
		}
		return packageFamilyVersion(result.InstalledFamily)
	}
	plan, err := approvedPackagePlan(result.Plan)
	if err == nil && plan.RecipeID == diagnosticPythonPackageRecipe {
		return plan.RootPackage.Version
	}
	if err != nil || !strings.HasPrefix(plan.RootPackage.Version, "4.1.") {
		return ""
	}
	return plan.RootPackage.Version
}

func packageReason(result diagnosticPackageResult) string {
	if len(result.Errors) == 0 {
		return "MPI package prerequisites are not ready for approval"
	}
	return diagnosticPublicMessage(result.Errors[0].Code + ": " + result.Errors[0].Message)
}

func (d *diagnosticService) reviewPackages(ctx context.Context, request diagnosticInspectionRequest, dependencyUpgradeReview ...bool) (diagnosticPackageReview, error) {
	upgradeReview := len(dependencyUpgradeReview) == 1 && dependencyUpgradeReview[0]
	return d.reviewPackageScope(ctx, request, upgradeReview, "", false)
}

func (d *diagnosticService) reviewPackageScope(ctx context.Context, request diagnosticInspectionRequest, upgradeReview bool, purpose string, firstInstall bool) (diagnosticPackageReview, error) {
	if firstInstall && (purpose != diagnosticPythonPackagePurpose || upgradeReview) {
		return diagnosticPackageReview{}, errors.New("first-install preparation requires the fixed Python-header purpose")
	}
	if purpose == diagnosticPythonPackagePurpose {
		d.mu.Lock()
		held := d.packageAdmissionClosed || d.packageActive != "" || d.packageRecoveryFailed || d.runtimeActive != "" || d.runtimeRecoveryFailed || d.ctx.Err() != nil
		d.mu.Unlock()
		if held {
			return diagnosticPackageReview{}, errors.New("existing package work or unresolved cleanup holds runtime repair")
		}
	}
	targets, err := d.packageInspectionTargets(request.diagnosticSetupRequest, purpose)
	if err != nil {
		return diagnosticPackageReview{}, err
	}
	accepted := map[string]string{}
	for _, key := range request.AcceptedHostKeys {
		if accepted[key.CandidateID] != "" {
			return diagnosticPackageReview{}, errors.New("duplicate fingerprint consent")
		}
		if purpose == diagnosticPythonPackagePurpose {
			candidate := targets.Targets[0].Candidate
			if candidate == nil || !onboardingID.MatchString(key.CandidateID) || key.CandidateID != candidate.CandidateID || key.SHA256 == "" || key.SHA256 != candidate.HostKeySHA256 {
				return diagnosticPackageReview{}, errors.New("fingerprint consent is not for the current runtime target")
			}
		}
		accepted[key.CandidateID] = key.SHA256
	}
	review := diagnosticPackageReview{FirstInstall: firstInstall, Purpose: purpose, DependencyUpgradeReview: upgradeReview, ReviewID: newOpID(), GroupID: request.GroupID, ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), CanApprove: true, Targets: []diagnosticPackageTarget{}}
	binding := diagnosticPackageBinding{Review: review, diagnosticParticipantBinding: diagnosticParticipantBinding{Pair: request.diagnosticSetupRequest, Controller: d.m.mesh.NodeUUID(), Pins: map[string]string{}, Targets: targets.Targets, Generations: map[string]string{}}}
	if purpose == diagnosticPythonPackagePurpose {
		binding.RuntimeNodeID = request.Members[0].NodeID
	}
	binding.ControllerPin, _ = d.m.mesh.PinSHA256(binding.Controller)
	versions := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(targets.Targets))*35*time.Second+5*time.Second)
	defer cancel()
	for _, target := range targets.Targets {
		row := diagnosticPackageTarget{NodeID: target.NodeID, Principal: target.Principal, Local: target.Local, Address: target.Address, Candidate: target.Candidate, State: "blocked", CleanupConfirmed: true}
		binding.Pins[target.Principal], _ = d.m.mesh.PinSHA256(target.Principal)
		access, readErr := d.packageAccess(target, accepted)
		if purpose == diagnosticPythonPackagePurpose && readErr != nil && target.Candidate != nil && !target.Candidate.AccessAvailable {
			row.State = "access-required"
		}
		if readErr == nil {
			binding.Generations[target.NodeID] = access.accessGeneration
			if !target.Local {
				candidate := access.candidate
				row.Candidate = &candidate
				for i := range binding.Targets {
					if binding.Targets[i].NodeID == target.NodeID {
						binding.Targets[i].Candidate = &candidate
					}
				}
			}
			readCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			nativeRequest := map[string]any{"action": "review", "nodeId": target.NodeID, "principal": target.Principal}
			if purpose == diagnosticPythonPackagePurpose {
				nativeRequest["runtimePreparation"] = true
				if firstInstall {
					nativeRequest["firstInstall"] = true
				}
			}
			if upgradeReview {
				nativeRequest["dependencyUpgradeReview"] = true
			}
			raw, callErr := d.packageNative(readCtx, target, access, nativeRequest)
			stop()
			readErr = callErr
			if readErr == nil {
				result, decodeErr := packageActionResult(raw, "review", "", "")
				readErr = decodeErr
				identityRequired := result.State == "reviewed" || result.State == "already_installed"
				if readErr == nil && (result.EffectsApplied || result.EffectsUnknown || identityRequired && (result.Identity == nil || result.Identity.NodeID != target.NodeID || result.Identity.Principal != target.Principal || result.Identity.UID <= 0)) {
					readErr = errors.New("package review did not verify the selected account identity")
				}
				if readErr == nil {
					if purpose == diagnosticPythonPackagePurpose && identityRequired && (!validPythonPreparationBinding(result.RuntimeBinding, result.Identity, firstInstall) || !validPythonPrerequisites(result.RuntimePrerequisites, result.Identity, result.State == "already_installed")) {
						readErr = errors.New("runtime review did not bind its actual managed interpreter and normal account")
					}
				}
				if readErr == nil {
					if result.State == "reviewed" {
						plan, err := approvedPackagePlan(result.Plan)
						if err != nil {
							readErr = err
						} else if (plan.RecipeID == diagnosticPackageUpgradeRecipe) != upgradeReview {
							readErr = errors.New("package review changed the selected dependency-upgrade policy")
						} else if (plan.RecipeID == diagnosticPythonPackageRecipe) != (purpose == diagnosticPythonPackagePurpose) || purpose == diagnosticPythonPackagePurpose && (!reflect.DeepEqual(plan.RuntimeBinding, result.RuntimeBinding) || !reflect.DeepEqual(plan.Identity, result.Identity)) {
							readErr = errors.New("package review changed its runtime purpose or interpreter binding")
						}
					}
				}
				if readErr == nil {
					if result.State == "reviewed" || result.State == "already_installed" {
						version := reviewedPackageVersion(result)
						if version == "" {
							readErr = errors.New("the selected MPI package family is incomplete or incompatible")
						} else {
							versions[version] = true
						}
					}
				}
				if readErr == nil {
					row.Review = append(json.RawMessage(nil), raw...)
					row.State = result.State
					if result.State != "reviewed" && result.State != "already_installed" {
						row.Reason = packageReason(result)
					}
				}
			}
		}
		if readErr != nil {
			row.Reason = diagnosticPublicMessage(readErr.Error())
		}
		if row.State != "reviewed" && row.State != "already_installed" {
			review.CanApprove = false
		}
		review.Targets = append(review.Targets, row)
	}
	if len(versions) > 1 {
		review.CanApprove = false
		for i := range review.Targets {
			review.Targets[i].State = "blocked"
			review.Targets[i].Reason = "All participants require the same OpenMPI package version; the reviewed versions differ"
		}
	}
	binding.Review = review
	if purpose == diagnosticPythonPackagePurpose && !review.CanApprove {
		if err := d.packagePinsCurrent(binding); err != nil {
			return review, err
		}
		current, err := d.pythonPreparationTargets(binding.RuntimeNodeID)
		if err != nil || len(current.Targets) != 1 || current.Targets[0].Address != binding.Targets[0].Address || current.Targets[0].Principal != binding.Targets[0].Principal {
			return review, errors.New("runtime target changed during prerequisite review")
		}
		return review, nil
	}
	if err := d.packageBindingCurrent(binding); err != nil {
		return review, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil {
		return review, errors.New("package admission is closed for manager shutdown")
	}
	if d.packageRecoveryFailed || d.packageActive != "" || d.runtimeActive != "" || d.runtimeRecoveryFailed {
		return review, errors.New("another package operation or unresolved cleanup holds this lane")
	}
	if len(d.packageReviews) >= 16 {
		for id := range d.packageReviews {
			delete(d.packageReviews, id)
			break
		}
	}
	d.packageReviews[review.ReviewID] = binding
	return review, nil
}

func (d *diagnosticService) packagePinsCurrent(binding diagnosticPackageBinding) error {
	if binding.Review.Purpose == "" {
		return d.participantPinsCurrent(binding.diagnosticParticipantBinding)
	}
	if !validPackageParticipantBinding(binding) {
		return errors.New("reviewed runtime package scope is incomplete or changed")
	}
	d.m.mesh.Refresh()
	pin, ok := d.m.mesh.PinSHA256(binding.Controller)
	if !ok || binding.Controller != d.m.mesh.NodeUUID() || pin != binding.ControllerPin {
		return errors.New("PAIR controller trust changed since runtime package review")
	}
	for _, target := range binding.Targets {
		pin, ok := d.m.mesh.PinSHA256(target.Principal)
		if !ok || pin != binding.Pins[target.Principal] {
			return errors.New("reviewed runtime participant trust changed")
		}
	}
	return nil
}

func (d *diagnosticService) participantPinsCurrent(binding diagnosticParticipantBinding) error {
	if !validDiagnosticParticipantBinding(binding) {
		return errors.New("original reviewed participant roster is incomplete or changed")
	}
	d.m.mesh.Refresh()
	pin, ok := d.m.mesh.PinSHA256(binding.Controller)
	if !ok || binding.Controller != d.m.mesh.NodeUUID() || pin != binding.ControllerPin {
		return errors.New("PAIR controller trust changed since package review")
	}
	for _, target := range binding.Targets {
		pin, ok = d.m.mesh.PinSHA256(target.Principal)
		if !ok || pin != binding.Pins[target.Principal] {
			return errors.New("reviewed participant trust changed")
		}
	}
	return nil
}

func (d *diagnosticService) packageBindingCurrent(binding diagnosticPackageBinding) error {
	if len(binding.Review.Targets) != len(binding.Targets) || binding.Review.GroupID != binding.Pair.GroupID {
		return errors.New("reviewed package participant roster changed")
	}
	for i, target := range binding.Targets {
		reviewed := binding.Review.Targets[i]
		if target.NodeID != reviewed.NodeID || target.Principal != reviewed.Principal || target.Address != reviewed.Address || target.Local != reviewed.Local {
			return errors.New("reviewed package participant identity changed")
		}
	}
	if binding.Review.Purpose == "" {
		return d.participantBindingCurrent(binding.diagnosticParticipantBinding)
	}
	if err := d.packagePinsCurrent(binding); err != nil {
		return err
	}
	current, err := d.pythonPreparationTargets(binding.RuntimeNodeID)
	if err != nil || len(current.Targets) != 1 {
		return errors.New("reviewed runtime target is unavailable")
	}
	target, before := current.Targets[0], binding.Targets[0]
	if target.NodeID != before.NodeID || target.Principal != before.Principal || target.Address != before.Address || target.Local != before.Local {
		return errors.New("reviewed runtime target changed")
	}
	if !target.Local {
		access, err := d.packageAccess(target, map[string]string{before.Candidate.CandidateID: before.Candidate.HostKeySHA256})
		if err != nil {
			return err
		}
		if access.accessGeneration != binding.Generations[target.NodeID] || access.candidate.HostKeySHA256 != before.Candidate.HostKeySHA256 {
			return errors.New("reviewed runtime account access changed")
		}
	}
	return nil
}

func (d *diagnosticService) participantBindingCurrent(binding diagnosticParticipantBinding) error {
	if err := d.participantPinsCurrent(binding); err != nil {
		return err
	}
	current, err := d.inspectionTargets(binding.Pair)
	if err != nil {
		return err
	}
	if len(current.Targets) != len(binding.Targets) {
		return errors.New("reviewed participant count changed")
	}
	for i, target := range current.Targets {
		before := binding.Targets[i]
		if target.Principal != before.Principal || target.NodeID != before.NodeID || target.Address != before.Address || target.Local != before.Local {
			return errors.New("reviewed participant or trust changed")
		}
		if !target.Local {
			access, err := d.packageAccess(target, map[string]string{before.Candidate.CandidateID: before.Candidate.HostKeySHA256})
			if err != nil {
				return err
			}
			if access.accessGeneration != binding.Generations[target.NodeID] || access.candidate.HostKeySHA256 != before.Candidate.HostKeySHA256 {
				return errors.New("reviewed device access changed")
			}
		}
	}
	return nil
}

func (d *diagnosticService) packageOperationPath(id string) string {
	return filepath.Join(d.m.exec.baseDir, "diagnostic-package-operations", id+".json")
}

func (d *diagnosticService) savePackageRun(run *diagnosticPackageRun) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil || len(data) > 1<<20 {
		return errors.New("package ownership record exceeds its durable size limit")
	}
	if err := os.MkdirAll(filepath.Dir(d.packageOperationPath(run.Public.OperationID)), 0700); err != nil {
		return err
	}
	return writeJSONAtomic(d.packageOperationPath(run.Public.OperationID), run)
}

func clonePackageOperation(op diagnosticPackageOperation) diagnosticPackageOperation {
	op.Targets = append([]diagnosticPackageTarget(nil), op.Targets...)
	return op
}

func (d *diagnosticService) packageSnapshot(run *diagnosticPackageRun) diagnosticPackageOperation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return clonePackageOperation(run.Public)
}

func (d *diagnosticService) approvePackages(ctx context.Context, request diagnosticPackageApprove) (diagnosticPackageOperation, error) {
	d.mu.Lock()
	binding, ok := d.packageReviews[request.ReviewID]
	for _, run := range d.packageRuns {
		if run.Public.ReviewID == request.ReviewID {
			if !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, run.Binding.Review) {
				d.mu.Unlock()
				return diagnosticPackageOperation{}, errors.New("approval purpose differs from the retained package review")
			}
			op := clonePackageOperation(run.Public)
			d.mu.Unlock()
			return op, nil
		}
	}
	d.mu.Unlock()
	if !ok || !binding.Review.CanApprove || time.Now().UnixMilli() > binding.Review.ExpiresAt {
		return diagnosticPackageOperation{}, errors.New("package review expired or has blocked participants")
	}
	if !validRuntimePackageReviewPlans(binding) {
		return diagnosticPackageOperation{}, errors.New("retained runtime package review no longer matches its fixed purpose and interpreter")
	}
	if !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, binding.Review) {
		return diagnosticPackageOperation{}, errors.New("approval purpose or runtime node changed")
	}
	if err := validateDiagnosticUpgradeApprovals(binding, request.DependencyUpgradeApprovals); err != nil {
		return diagnosticPackageOperation{}, err
	}
	if err := d.packageBindingCurrent(binding); err != nil {
		return diagnosticPackageOperation{}, err
	}
	passwords, consent, err := packageElevation(request.Elevation, binding.Targets)
	if err != nil {
		return diagnosticPackageOperation{}, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			for id := range passwords {
				delete(passwords, id)
			}
		}
	}()
	for i, target := range binding.Targets {
		if binding.Review.Targets[i].State == "already_installed" {
			continue
		}
		if !consent[target.NodeID] {
			return diagnosticPackageOperation{}, errors.New("explicit administrator access is required for each installing participant")
		}
	}
	d.m.exec.diagnosticMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			d.m.exec.diagnosticMu.Unlock()
		}
	}()
	fabricHeld := d.m.exec.fabric.held()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, existing := range d.packageRuns {
		if existing.Public.ReviewID == request.ReviewID {
			return clonePackageOperation(existing.Public), nil
		}
	}
	if fabricHeld {
		return diagnosticPackageOperation{}, errors.New("fabric setup or recovery holds package admission")
	}
	if d.packageAdmissionClosed || d.ctx.Err() != nil || ctx.Err() != nil {
		return diagnosticPackageOperation{}, errors.New("package admission is closed for manager shutdown or request cancellation")
	}
	if d.packageActive != "" || d.packageRecoveryFailed || d.runtimeActive != "" || d.runtimeRecoveryFailed {
		return diagnosticPackageOperation{}, errors.New("package operation or unconfirmed cleanup is already active")
	}
	if current, ok := d.packageReviews[request.ReviewID]; !ok || current.Review.ExpiresAt <= time.Now().UnixMilli() {
		return diagnosticPackageOperation{}, errors.New("package review expired or was closed before approval admission")
	}
	run := &diagnosticPackageRun{DependencyUpgradeApprovals: append([]diagnosticDependencyUpgradeApproval(nil), request.DependencyUpgradeApprovals...), SchemaVersion: 1, Owner: "pair-mpi-package-controller-v1", Public: diagnosticPackageOperation{FirstInstall: binding.Review.FirstInstall, Purpose: binding.Review.Purpose, OperationID: newOpID(), ReviewID: request.ReviewID, GroupID: binding.Review.GroupID, State: "running", Stage: "preparing", Revision: 1, StartedAt: time.Now().UnixMilli(), Targets: append([]diagnosticPackageTarget(nil), binding.Review.Targets...)}, Binding: binding}
	if err := d.savePackageRun(run); err != nil {
		return diagnosticPackageOperation{}, err
	}
	opCtx, cancel := context.WithTimeout(d.ctx, time.Duration(len(binding.Targets))*4*time.Minute)
	run.cancel = cancel
	d.packageRuns[run.Public.OperationID] = run
	d.packageActive = run.Public.OperationID
	d.m.exec.diagnosticMu.Unlock()
	admissionLocked = false
	handedOff = true
	go d.executePackages(opCtx, run, passwords)
	return clonePackageOperation(run.Public), nil
}

func (d *diagnosticService) updatePackageRun(run *diagnosticPackageRun, change func(*diagnosticPackageOperation)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	change(&run.Public)
	run.Public.Revision++
	if err := d.savePackageRun(run); err != nil {
		d.packageRecoveryFailed = true
		return err
	}
	return nil
}

func (d *diagnosticService) executePackages(ctx context.Context, run *diagnosticPackageRun, passwords map[string]string) {
	defer func() {
		for id := range passwords {
			delete(passwords, id)
		}
	}()
	failed := false
	for i, target := range run.Binding.Targets {
		d.mu.Lock()
		held := d.packageRecoveryFailed
		d.mu.Unlock()
		if held {
			failed = true
			break
		}
		row := run.Binding.Review.Targets[i]
		if err := ctx.Err(); err != nil {
			failed = true
			break
		}
		if err := d.packageBindingCurrent(run.Binding); err != nil {
			_ = d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
				op.Targets[i].State = "failed"
				op.Targets[i].Reason = diagnosticPublicMessage(err.Error())
			})
			failed = true
			break
		}
		accepted := map[string]string{}
		if target.Candidate != nil {
			accepted[target.Candidate.CandidateID] = target.Candidate.HostKeySHA256
		}
		access, err := d.packageAccess(target, accepted)
		if err != nil {
			failed = true
			_ = d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
				op.Targets[i].State = "failed"
				op.Targets[i].Reason = err.Error()
			})
			break
		}
		if row.State == "already_installed" {
			original, originalErr := packageResult(row.Review)
			readCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			raw, err := d.packageNativeBound(readCtx, target, access, run.Binding, map[string]any{"action": "review", "nodeId": target.NodeID, "principal": target.Principal})
			stop()
			fresh, decodeErr := packageActionResult(raw, "review", "", "")
			if err != nil || decodeErr != nil || originalErr != nil || fresh.State != "already_installed" || fresh.EffectsApplied || fresh.EffectsUnknown || fresh.Identity == nil || !reflect.DeepEqual(fresh.Identity, original.Identity) || reviewedPackageVersion(fresh) == "" || reviewedPackageVersion(fresh) != reviewedPackageVersion(original) || run.Binding.Review.Purpose == diagnosticPythonPackagePurpose && !samePythonRuntimeReview(original, fresh) {
				failed = true
				_ = d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
					op.Targets[i].State = "failed"
					op.Targets[i].Reason = "Existing MPI packages changed; review again"
				})
				break
			}
			if d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
				op.Targets[i].State = "already_installed"
				op.Targets[i].Review = raw
				op.Targets[i].CleanupConfirmed = true
			}) != nil {
				failed = true
				break
			}
			continue
		}
		observed, err := packageResult(row.Review)
		if err != nil || observed.Identity == nil || len(observed.Plan) == 0 {
			failed = true
			break
		}
		plan, err := approvedPackagePlan(observed.Plan)
		if err != nil {
			failed = true
			break
		}
		request := map[string]any{"action": "provision", "operationId": run.Public.OperationID, "nodeId": target.NodeID, "principal": target.Principal, "expectedUID": observed.Identity.UID, "expectedHome": observed.Identity.Home, "approvedPlan": observed.Plan}
		if upgrades, _ := diagnosticPackagePlanUpgrades(plan); upgrades {
			if validateDiagnosticUpgradeApprovals(run.Binding, run.DependencyUpgradeApprovals) != nil {
				failed = true
				break
			}
			request["dependencyUpgradesApproved"] = true
		}
		if password := passwords[target.NodeID]; password != "" {
			request["elevationPassword"] = password
		}
		if err := d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
			op.Stage = "installing"
			op.Targets[i].State = "installing"
			op.Targets[i].CleanupConfirmed = false
		}); err != nil {
			failed = true
			break
		}
		targetCtx, stop := context.WithTimeout(ctx, 220*time.Second)
		raw, callErr := d.packageNativeBound(targetCtx, target, access, run.Binding, request)
		stop()
		delete(request, "elevationPassword")
		result, decodeErr := packageActionResult(raw, "provision", run.Public.OperationID, plan.PlanDigest)
		if decodeErr == nil && result.State == "succeeded" && !validPackageReceipt(result, plan, run.Public.OperationID) {
			decodeErr = errors.New("installed MPI package receipt did not match approved and measured artifacts")
		}
		// Retain measured acquisition evidence when a same-plan retry observes
		// packages that this operation already installed successfully.
		if callErr == nil && decodeErr == nil && result.State == "succeeded" && !result.EffectsApplied {
			prior := d.packageSnapshot(run).Targets[i].Receipt
			if validPackageReceipt(diagnosticPackageResult{Receipt: prior, EffectsApplied: true}, plan, run.Public.OperationID) {
				result.Receipt = prior
			}
		}
		if callErr != nil || decodeErr != nil {
			// A lost response is not proof that a privileged transaction never ran.
			cleanupCtx, cleanupStop := context.WithTimeout(context.Background(), 40*time.Second)
			cleanup := map[string]any{"action": "cancel", "operationId": run.Public.OperationID, "nodeId": target.NodeID, "principal": target.Principal, "expectedPlanDigest": plan.PlanDigest}
			if password := passwords[target.NodeID]; password != "" {
				cleanup["elevationPassword"] = password
			}
			raw, callErr = d.packageNativeBound(cleanupCtx, target, access, run.Binding, cleanup)
			cleanupStop()
			delete(cleanup, "elevationPassword")
			result, decodeErr = packageActionResult(raw, "cancel", run.Public.OperationID, plan.PlanDigest)
			// Cleanup cannot rescue a rejected installation receipt into success.
			if result.State == "succeeded" {
				result.State = "cancelled"
			}
		}
		if callErr != nil || decodeErr != nil {
			result.State = "cleanup_unconfirmed"
			result.CleanupConfirmed = false
		}
		if result.State != "succeeded" || !result.CleanupConfirmed {
			failed = true
		}
		if d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
			op.Targets[i].State = result.State
			op.Targets[i].Receipt = append(json.RawMessage(nil), result.Receipt...)
			op.Targets[i].CleanupConfirmed = result.CleanupConfirmed
			op.Targets[i].UnitMetadataRetained = result.UnitMetadataRetained
			if result.State != "succeeded" {
				op.Targets[i].Reason = packageReason(result)
			}
		}) != nil {
			failed = true
			break
		}
		delete(passwords, target.NodeID)
		if failed {
			break
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clean := true
	cancelled := ctx.Err() != nil
	for _, target := range run.Public.Targets {
		clean = clean && target.CleanupConfirmed && !target.UnitMetadataRetained
		cancelled = cancelled || target.State == "cancelled"
	}
	run.Public.State = "completed"
	run.Public.Stage = "packages-installed"
	if failed {
		run.Public.State = "failed"
		run.Public.Stage = "package-failed"
	}
	if cancelled {
		run.Public.State = "cancelled"
		run.Public.Stage = "cancelled"
	}
	if !clean {
		run.Public.Stage = "cleanup-unconfirmed"
	}
	run.Public.CleanupConfirmed = clean
	run.Public.FinishedAt = time.Now().UnixMilli()
	run.Public.Revision++
	if run.cancel != nil {
		run.cancel()
		run.cancel = nil
	}
	if clean {
		d.packageActive = ""
	}
	if d.savePackageRun(run) != nil {
		d.packageRecoveryFailed = true
	}
}

func (d *diagnosticService) loadPackageRuns() {
	directory, err := os.Open(filepath.Join(d.m.exec.baseDir, "diagnostic-package-operations"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		d.packageRecoveryFailed = true
		return
	}
	defer directory.Close()
	// Page history so valid completed jobs never become a recovery error merely
	// because the user has run the bounded recipe more than 64 times.
	for {
		entries, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			d.packageRecoveryFailed = true
			return
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if entry.IsDir() || !onboardingID.MatchString(id) {
				continue
			}
			raw, err := readOnboardingFile(d.packageOperationPath(id), 1<<20)
			var run diagnosticPackageRun
			if err != nil || json.Unmarshal(raw, &run) != nil || !validPackageRun(run, id) {
				d.packageRecoveryFailed = true
				continue
			}
			if run.Public.State == "running" || run.Public.State == "cancelling" {
				wasCancelled := run.Public.State == "cancelling"
				run.Public.State = "interrupted"
				run.Public.Stage = "reauthorize-and-reconcile"
				if wasCancelled {
					run.Public.State = "cancelled"
					run.Public.Stage = "cancelled-cleanup-required"
				}
				run.Public.CleanupConfirmed = true
				for _, target := range run.Public.Targets {
					run.Public.CleanupConfirmed = run.Public.CleanupConfirmed && target.CleanupConfirmed && !target.UnitMetadataRetained
				}
				run.Public.Revision++
			}
			d.packageRuns[id] = &run
			if !run.Public.CleanupConfirmed {
				d.packageActive = id
			}
			for i, target := range run.Binding.Targets {
				if target.Local || target.Candidate == nil {
					continue
				}
				candidate := *target.Candidate
				candidate.AccessAvailable = false
				candidate.HostKeyTrusted = false
				candidate.Reason = "Reauthorize access for the retained package operation"
				run.Public.Targets[i].Candidate = &candidate
				if d.m.onboarding.targets[candidate.CandidateID] == nil {
					d.m.onboarding.targets[candidate.CandidateID] = &onboardingPrivateTarget{candidate: candidate, lifetime: "session"}
				}
			}
			if d.savePackageRun(&run) != nil {
				d.packageRecoveryFailed = true
			}
		}
		if readErr == io.EOF {
			break
		}
	}
}

func validPackageRun(run diagnosticPackageRun, id string) bool {
	if !validRuntimePackageReviewPlans(run.Binding) {
		return false
	}
	if !diagnosticPackageRunUpgradeApprovalsValid(run) {
		return false
	}
	count := len(run.Binding.Targets)
	if run.SchemaVersion != 1 || run.Owner != "pair-mpi-package-controller-v1" || run.Public.OperationID != id || !onboardingID.MatchString(run.Public.ReviewID) || run.Public.ReviewID != run.Binding.Review.ReviewID || run.Public.Purpose != run.Binding.Review.Purpose || run.Public.FirstInstall != run.Binding.Review.FirstInstall || run.Public.GroupID != run.Binding.Pair.GroupID || run.Public.GroupID != run.Binding.Review.GroupID || run.Public.RuntimeValidated || !validPackageParticipantBinding(run.Binding) || len(run.Public.Targets) != count || len(run.Binding.Review.Targets) != count {
		return false
	}
	seen := map[string]bool{}
	for i, target := range run.Binding.Targets {
		row, reviewed := run.Public.Targets[i], run.Binding.Review.Targets[i]
		if seen[target.NodeID] || target.NodeID != row.NodeID || target.Principal != row.Principal || target.Address != row.Address || target.Local != row.Local || target.NodeID != reviewed.NodeID || target.Principal != reviewed.Principal || target.Address != reviewed.Address || target.Local != reviewed.Local {
			return false
		}
		seen[target.NodeID] = true
		if target.Local != (target.Candidate == nil) || !target.Local && (!onboardingID.MatchString(target.Candidate.CandidateID) || target.Candidate.Address != target.Address || target.Candidate.HostKeySHA256 == "") {
			return false
		}
		if run.Public.CleanupConfirmed && (!run.Public.Targets[i].CleanupConfirmed || run.Public.Targets[i].UnitMetadataRetained) {
			return false
		}
	}
	return true
}

type diagnosticPackageAction struct {
	FirstInstall         bool                         `json:"firstInstall,omitempty"`
	Purpose              string                       `json:"purpose,omitempty"`
	RuntimeNodeID        string                       `json:"runtimeNodeId,omitempty"`
	OperationID          string                       `json:"operationId"`
	ReviewID             string                       `json:"reviewId,omitempty"`
	GroupID              string                       `json:"groupId,omitempty"`
	Elevation            []diagnosticPackageElevation `json:"elevation,omitempty"`
	CloseUnstartedReview bool                         `json:"closeUnstartedReview,omitempty"`
}

func (d *diagnosticService) retryPackages(ctx context.Context, request diagnosticPackageAction) (diagnosticPackageOperation, error) {
	if err := validatePackageActionScope(request); err != nil {
		return diagnosticPackageOperation{}, err
	}
	if !onboardingID.MatchString(request.OperationID) {
		return diagnosticPackageOperation{}, errors.New("valid retained package operation ID required")
	}
	d.m.exec.diagnosticMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			d.m.exec.diagnosticMu.Unlock()
		}
	}()
	if d.m.exec.fabric.held() {
		return diagnosticPackageOperation{}, errors.New("fabric setup or recovery holds package retry admission")
	}
	d.mu.Lock()
	run := d.packageRuns[request.OperationID]
	if run != nil && !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, run.Binding.Review) {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("retry purpose differs from the retained package operation")
	}
	if d.packageAdmissionClosed || d.ctx.Err() != nil || d.runtimeActive != "" || d.runtimeRecoveryFailed {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("package retry admission is closed or another setup owns the lane")
	}
	if run == nil || run.cancel != nil || run.reconciling || run.Public.State != "failed" && run.Public.State != "interrupted" || d.packageRecoveryFailed {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("retained package operation is not retryable")
	}
	if !diagnosticPackageRunUpgradeApprovalsValid(*run) {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("retained dependency-upgrade approval is missing or changed")
	}
	if d.packageActive != "" && d.packageActive != request.OperationID {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("another package operation owns the lane")
	}
	run.reconciling = true
	d.packageActive = run.Public.OperationID
	ctx, retryCancel := context.WithCancel(ctx)
	run.cancel = retryCancel
	d.mu.Unlock()
	d.m.exec.diagnosticMu.Unlock()
	admissionLocked = false
	started := false
	defer func() {
		if !started {
			d.mu.Lock()
			run.reconciling = false
			run.cancel = nil
			if ctx.Err() != nil {
				run.Public.State = "cancelled"
				run.Public.Stage = "cancelled"
				run.Public.FinishedAt = time.Now().UnixMilli()
				run.Public.CleanupConfirmed = true
				for _, target := range run.Public.Targets {
					run.Public.CleanupConfirmed = run.Public.CleanupConfirmed && target.CleanupConfirmed && !target.UnitMetadataRetained
				}
				if !run.Public.CleanupConfirmed {
					run.Public.Stage = "cleanup-unconfirmed"
				}
				run.Public.Revision++
				if d.savePackageRun(run) != nil {
					d.packageRecoveryFailed = true
				}
			}
			if run.Public.CleanupConfirmed && d.packageActive == run.Public.OperationID {
				d.packageActive = ""
			}
			d.mu.Unlock()
		}
		retryCancel()
	}()
	passwords, consent, err := packageElevation(request.Elevation, run.Binding.Targets)
	if err != nil {
		return d.packageSnapshot(run), err
	}
	defer func() {
		if !started {
			for id := range passwords {
				delete(passwords, id)
			}
		}
	}()
	if err := d.packagePinsCurrent(run.Binding); err != nil {
		return d.packageSnapshot(run), err
	}
	bindings := map[string]string{}
	for i, target := range run.Binding.Targets {
		if run.Binding.Review.Targets[i].State != "already_installed" && run.Public.Targets[i].State != "succeeded" && !consent[target.NodeID] {
			return d.packageSnapshot(run), errors.New("explicit administrator access is required for the remaining package work")
		}
		accepted := map[string]string{}
		if target.Candidate != nil {
			accepted[target.Candidate.CandidateID] = target.Candidate.HostKeySHA256
		}
		access, err := d.packageAccess(target, accepted)
		if err != nil {
			return d.packageSnapshot(run), err
		}
		bindings[target.NodeID] = access.accessGeneration
		if !run.Public.Targets[i].CleanupConfirmed {
			observed, err := packageResult(run.Binding.Review.Targets[i].Review)
			if err != nil {
				return d.packageSnapshot(run), err
			}
			plan, err := approvedPackagePlan(observed.Plan)
			if err != nil {
				return d.packageSnapshot(run), err
			}
			request := map[string]any{"action": "reconcile", "operationId": run.Public.OperationID, "nodeId": target.NodeID, "principal": target.Principal, "expectedPlanDigest": plan.PlanDigest}
			if password := passwords[target.NodeID]; password != "" {
				request["elevationPassword"] = password
			}
			readCtx, stop := context.WithTimeout(ctx, 40*time.Second)
			raw, err := d.packageNativeBound(readCtx, target, access, run.Binding, request)
			stop()
			delete(request, "elevationPassword")
			result, decodeErr := packageActionResult(raw, "reconcile", run.Public.OperationID, plan.PlanDigest)
			if err != nil || decodeErr != nil || !result.CleanupConfirmed {
				return d.packageSnapshot(run), errors.New("the original package job is still running or its cleanup is unconfirmed")
			}
			if d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
				op.Targets[i].CleanupConfirmed = true
				op.Targets[i].UnitMetadataRetained = false
				op.Targets[i].Receipt = result.Receipt
				if result.State == "cancelled" {
					op.Targets[i].State = "cancelled"
					op.State = "cancelled"
					op.Stage = "closed-before-retry"
					op.FinishedAt = time.Now().UnixMilli()
					op.CleanupConfirmed = true
					for _, target := range op.Targets {
						op.CleanupConfirmed = op.CleanupConfirmed && target.CleanupConfirmed && !target.UnitMetadataRetained
					}
				}
			}) != nil {
				return d.packageSnapshot(run), errors.New("recovery checkpoint unavailable")
			}
			if result.State == "cancelled" {
				return d.packageSnapshot(run), errors.New("the original package operation was closed during reconciliation; obtain a fresh review")
			}
		}
	}
	// Fresh access may resume the same account/SSH identities, never replace the
	// original package plan, controller/peer trust pins or operation identifier.
	resumedBinding := run.Binding
	resumedBinding.Generations = bindings
	if err := d.packageBindingCurrent(resumedBinding); err != nil {
		return d.packageSnapshot(run), err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.packageAdmissionClosed || d.ctx.Err() != nil {
		return clonePackageOperation(run.Public), errors.New("package retry admission closed before publication")
	}
	if ctx.Err() != nil {
		return clonePackageOperation(run.Public), ctx.Err()
	}
	run.Binding.Generations = bindings
	run.Public.State = "running"
	run.Public.Stage = "resuming"
	run.Public.FinishedAt = 0
	run.Public.CleanupConfirmed = false
	run.Public.Revision++
	if err := d.savePackageRun(run); err != nil {
		d.packageRecoveryFailed = true
		return diagnosticPackageOperation{}, err
	}
	opCtx, cancel := context.WithTimeout(d.ctx, time.Duration(len(run.Binding.Targets))*4*time.Minute)
	run.cancel = cancel
	run.reconciling = false
	d.packageActive = run.Public.OperationID
	started = true
	go d.executePackages(opCtx, run, passwords)
	return clonePackageOperation(run.Public), nil
}

type diagnosticPackageStatus struct {
	Operation        *diagnosticPackageOperation `json:"operation"`
	RecoveryRequired bool                        `json:"recoveryRequired"`
	ReviewClosed     bool                        `json:"reviewClosed,omitempty"`
}

func (d *diagnosticService) packageStatus(request diagnosticPackageAction) (diagnosticPackageStatus, error) {
	if err := validatePackageActionScope(request); err != nil {
		return diagnosticPackageStatus{}, err
	}
	if len(request.Elevation) != 0 {
		return diagnosticPackageStatus{}, errors.New("status does not accept administrator access")
	}
	if request.CloseUnstartedReview && (!onboardingID.MatchString(request.ReviewID) || request.OperationID != "" || request.GroupID != "") {
		return diagnosticPackageStatus{}, errors.New("closing an unstarted approval requires only its review identifier")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var selected *diagnosticPackageRun
	for _, run := range d.packageRuns {
		if request.OperationID != "" && run.Public.OperationID != request.OperationID || request.ReviewID != "" && run.Public.ReviewID != request.ReviewID || request.GroupID != "" && run.Public.GroupID != request.GroupID {
			continue
		}
		if selected == nil || selected.Public.CleanupConfirmed && !run.Public.CleanupConfirmed || selected.Public.CleanupConfirmed == run.Public.CleanupConfirmed && selected.Public.StartedAt < run.Public.StartedAt {
			selected = run
		}
	}
	result := diagnosticPackageStatus{RecoveryRequired: d.packageRecoveryFailed}
	if selected != nil {
		if (request.OperationID != "" || request.ReviewID != "" || request.GroupID != "" || request.Purpose != "") && !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, selected.Binding.Review) {
			return diagnosticPackageStatus{}, errors.New("status purpose differs from the retained package operation")
		}
		op := clonePackageOperation(selected.Public)
		result.Operation = &op
	} else if request.CloseUnstartedReview && !result.RecoveryRequired {
		if review, ok := d.packageReviews[request.ReviewID]; ok && !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, review.Review) {
			return diagnosticPackageStatus{}, errors.New("review closure purpose or runtime node changed")
		}
		// Admission rechecks this map under the same mutex. Deleting the review
		// closes a lost or rejected approval without racing a later job launch.
		delete(d.packageReviews, request.ReviewID)
		result.ReviewClosed = true
	}
	return result, nil
}

func (d *diagnosticService) packageOperation(ctx context.Context, method string, request diagnosticPackageAction) (diagnosticPackageOperation, error) {
	if err := validatePackageActionScope(request); err != nil {
		return diagnosticPackageOperation{}, err
	}
	if !onboardingID.MatchString(request.OperationID) {
		return diagnosticPackageOperation{}, errors.New("valid package operation ID required")
	}
	d.mu.Lock()
	run := d.packageRuns[request.OperationID]
	if run == nil {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("package operation is unknown")
	}
	if !packageScopeMatches(request.Purpose, request.RuntimeNodeID, request.FirstInstall, run.Binding.Review) {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("cancellation purpose differs from the retained package operation")
	}
	if method == "engine:diagnostic-package-status" {
		op := clonePackageOperation(run.Public)
		d.mu.Unlock()
		return op, nil
	}
	if run.cancel != nil {
		run.Public.State = "cancelling"
		run.Public.Stage = "cancelling"
		run.Public.Revision++
		err := d.savePackageRun(run)
		if err != nil {
			d.packageRecoveryFailed = true
		}
		run.cancel()
		op := clonePackageOperation(run.Public)
		d.mu.Unlock()
		return op, err
	}
	if run.Public.CleanupConfirmed {
		op := clonePackageOperation(run.Public)
		d.mu.Unlock()
		return op, nil
	}
	if run.reconciling {
		op := clonePackageOperation(run.Public)
		d.mu.Unlock()
		return op, nil
	}
	if run.Binding.Controller != d.m.mesh.NodeUUID() {
		d.mu.Unlock()
		return diagnosticPackageOperation{}, errors.New("cleanup belongs to a different PAIR controller")
	}
	run.reconciling = true
	d.mu.Unlock()
	defer func() { d.mu.Lock(); run.reconciling = false; d.mu.Unlock() }()
	passwords, consent, err := packageElevation(request.Elevation, run.Binding.Targets)
	if err != nil {
		return d.packageSnapshot(run), err
	}
	defer func() {
		for id := range passwords {
			delete(passwords, id)
		}
	}()
	for i, target := range run.Binding.Targets {
		if run.Public.Targets[i].CleanupConfirmed {
			continue
		}
		if err := d.packagePinsCurrent(run.Binding); err != nil {
			return d.packageSnapshot(run), err
		}
		if !consent[target.NodeID] {
			return d.packageSnapshot(run), errors.New("explicit administrator access is required to reconcile the retained package job")
		}
		accepted := map[string]string{}
		if target.Candidate != nil {
			accepted[target.Candidate.CandidateID] = target.Candidate.HostKeySHA256
		}
		access, err := d.packageAccess(target, accepted)
		if err != nil {
			return d.packageSnapshot(run), err
		}
		original, err := packageResult(run.Binding.Review.Targets[i].Review)
		if err != nil {
			return d.packageSnapshot(run), err
		}
		plan, err := approvedPackagePlan(original.Plan)
		if err != nil {
			return d.packageSnapshot(run), err
		}
		cleanup := map[string]any{"action": "cancel", "operationId": run.Public.OperationID, "nodeId": target.NodeID, "principal": target.Principal, "expectedPlanDigest": plan.PlanDigest}
		if password := passwords[target.NodeID]; password != "" {
			cleanup["elevationPassword"] = password
		}
		cleanupCtx, stop := context.WithTimeout(ctx, 40*time.Second)
		raw, err := d.packageNativeBound(cleanupCtx, target, access, run.Binding, cleanup)
		stop()
		delete(cleanup, "elevationPassword")
		result, decodeErr := packageActionResult(raw, "cancel", run.Public.OperationID, plan.PlanDigest)
		if err != nil || decodeErr != nil {
			return d.packageSnapshot(run), errors.New("owned package cleanup is still unconfirmed")
		}
		if d.updatePackageRun(run, func(op *diagnosticPackageOperation) {
			op.Targets[i].CleanupConfirmed = result.CleanupConfirmed
			op.Targets[i].State = result.State
			op.Targets[i].Receipt = result.Receipt
			op.Targets[i].UnitMetadataRetained = result.UnitMetadataRetained
		}) != nil {
			return d.packageSnapshot(run), errors.New("cleanup checkpoint could not be retained")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clean := true
	for _, target := range run.Public.Targets {
		clean = clean && target.CleanupConfirmed && !target.UnitMetadataRetained
	}
	run.Public.CleanupConfirmed = clean
	if clean {
		run.Public.State = "cancelled"
		run.Public.Stage = "cleanup-confirmed"
		d.packageActive = ""
	}
	run.Public.Revision++
	if d.savePackageRun(run) != nil {
		d.packageRecoveryFailed = true
	}
	return clonePackageOperation(run.Public), nil
}

func (m *Manager) handleDiagnosticPackages(ctx context.Context, msg *Message) {
	defer clear(msg.Params)
	var result any
	var err error
	switch msg.Method {
	case "engine:vllm-python-prepare-review":
		var request diagnosticPythonPreparationRequest
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid fixed runtime prerequisite review")
		} else {
			result, err = m.exec.diagnostics.reviewVLLMPythonPreparation(ctx, request)
		}
	case "engine:diagnostic-package-review":
		var request struct {
			diagnosticInspectionRequest
			DependencyUpgradeReview *bool `json:"dependencyUpgradeReview,omitempty"`
		}
		if onboardingDecode(msg.Params, &request) != nil || request.DependencyUpgradeReview != nil && !*request.DependencyUpgradeReview {
			err = errors.New("invalid package review request")
		} else {
			result, err = m.exec.diagnostics.reviewPackages(ctx, request.diagnosticInspectionRequest, request.DependencyUpgradeReview != nil)
		}
	case "engine:diagnostic-package-approve":
		var request diagnosticPackageApprove
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid package approval request")
		} else {
			result, err = m.exec.diagnostics.approvePackages(ctx, request)
		}
	case "engine:diagnostic-package-status", "engine:diagnostic-package-cancel", "engine:diagnostic-package-retry":
		var request diagnosticPackageAction
		if onboardingDecode(msg.Params, &request) != nil {
			err = errors.New("invalid package operation request")
		} else if msg.Method == "engine:diagnostic-package-status" {
			result, err = m.exec.diagnostics.packageStatus(request)
		} else if request.CloseUnstartedReview {
			err = errors.New("review closure is only supported by package status reconciliation")
		} else if msg.Method == "engine:diagnostic-package-retry" {
			result, err = m.exec.diagnostics.retryPackages(ctx, request)
		} else {
			result, err = m.exec.diagnostics.packageOperation(ctx, msg.Method, request)
		}
	default:
		err = errors.New("unsupported package operation")
	}
	m.respondOrErr(msg, result, err)
}
