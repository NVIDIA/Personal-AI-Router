// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path"
	"reflect"
	"strings"
	"time"
)

const diagnosticPythonPackagePurpose = "vllm-python-headers"
const diagnosticPythonPackageRecipe = "dgx-spark-ubuntu24.04-arm64-python312-headers-v1"
const diagnosticPythonPackagePrefix = "pair-runtime/vllm-"
const diagnosticPythonPackageVersion = "3.12.3-1ubuntu0.16"

var diagnosticPythonPackages = map[string]string{
	"python3.12-dev":    diagnosticPythonPackageVersion,
	"libpython3.12-dev": diagnosticPythonPackageVersion,
	"libexpat1-dev":     "2.6.1-2ubuntu0.4",
}

const diagnosticPythonSnapshotWorkerSHA256 = "5becaf628649e8066037ea4cdd9268b0ae9984ec5767505354f03d802bf50baf"
const diagnosticPythonLocalWorkerSHA256 = "7741e1a738c05a3b2d9222fff4bc4b1a967c16a4bf9144c3baa70008b396746f"

var diagnosticPythonLocalDependencies = map[string]string{
	"python3.12": "3.12.3-1ubuntu0.16", "python3.12-minimal": "3.12.3-1ubuntu0.16",
	"libpython3.12-stdlib": "3.12.3-1ubuntu0.16", "libpython3.12t64": "3.12.3-1ubuntu0.16",
	"libexpat1": "2.6.1-2ubuntu0.4", "zlib1g-dev": "1:1.3.dfsg-3.1ubuntu2.2", "libc6-dev": "2.39-0ubuntu8.8",
}

var diagnosticPythonSnapshotArtifacts = map[string]struct {
	URL, SHA256 string
	Size        uint64
}{
	"libexpat1-dev":     {"https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/e/expat/libexpat1-dev_2.6.1-2ubuntu0.4_arm64.deb", "38b2c2c024947d0dea10bcd21333ea9f942aeb423da62c3b67c17ac92ed57004", 128426},
	"libpython3.12-dev": {"https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/p/python3.12/libpython3.12-dev_3.12.3-1ubuntu0.16_arm64.deb", "644c40641fc40da4f8205fb96b60b404677e28402d8b2b55d437697cae1d7f96", 5540320},
	"python3.12-dev":    {"https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/p/python3.12/python3.12-dev_3.12.3-1ubuntu0.16_arm64.deb", "424a323ebfacc1454c805cb3486525b73dfb684d21f548ccfb96ba0eaf3a96a8", 497934},
}

func validPythonArchivePolicy(plan diagnosticApprovedPackagePlan) bool {
	if len(plan.ArchivePolicy.ArchiveSource) == 0 {
		return plan.ArchivePolicy.WorkerSHA256 != diagnosticPythonLocalWorkerSHA256 // Local provenance is mandatory; retained legacy policy stays native-verified.
	}
	var source struct {
		Kind, Snapshot string
		URLs           map[string]string `json:"urls"`
		Subdir         string            `json:"subdir"`
		Dependencies   map[string]string `json:"dependencies"`
	}
	decoder := json.NewDecoder(bytes.NewReader(plan.ArchivePolicy.ArchiveSource))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&source) != nil {
		return false
	}
	if source.Kind == "verified-local-archive" {
		return source.Snapshot == "20260901T000000Z" && source.Subdir == ".cache/nvpair/python312-headers-20260901" && len(source.URLs) == 0 && reflect.DeepEqual(source.Dependencies, diagnosticPythonLocalDependencies) && plan.ArchivePolicy.WorkerSHA256 == diagnosticPythonLocalWorkerSHA256 && validPythonSourceBinding(plan.RuntimeBinding, plan.Identity)
	}
	if source.Kind != "ubuntu-snapshot" || source.Snapshot != "20260901T000000Z" || len(source.URLs) != 3 || source.Subdir != "" || len(source.Dependencies) != 0 || plan.ArchivePolicy.WorkerSHA256 != diagnosticPythonSnapshotWorkerSHA256 {
		return false
	}
	for name, artifact := range diagnosticPythonSnapshotArtifacts {
		if source.URLs[name] != artifact.URL {
			return false
		}
	}
	return true
}

type diagnosticPackageIdentity struct {
	NodeID    string `json:"nodeId"`
	Principal string `json:"principal"`
	UID       int    `json:"uid"`
	Home      string `json:"home"`
}

type diagnosticPythonRuntimeBinding struct {
	SchemaVersion          int    `json:"schemaVersion"`
	InstallRoot            string `json:"installRoot,omitempty"`
	EnvironmentID          string `json:"environmentId,omitempty"`
	PythonPath             string `json:"pythonPath,omitempty"`
	PythonSHA256           string `json:"pythonSHA256,omitempty"`
	RuntimeRecordSHA256    string `json:"runtimeRecordSHA256,omitempty"`
	RuntimeReceiptSHA256   string `json:"runtimeReceiptSHA256,omitempty"`
	PipReportSHA256        string `json:"pipReportSHA256,omitempty"`
	PyvenvConfigSHA256     string `json:"pyvenvConfigSHA256,omitempty"`
	SourceExecutable       string `json:"sourceExecutable"`
	SourceExecutableSHA256 string `json:"sourceExecutableSHA256"`
	SourcePackageVersion   string `json:"sourcePackageVersion"`
	PythonVersion          string `json:"pythonVersion"`
	IncludePath            string `json:"includePath"`
	ConfigHeaderPath       string `json:"configHeaderPath"`
}

type diagnosticPythonPrerequisites struct {
	CheckedAsUID           int  `json:"checkedAsUID"`
	SourceBindingUnchanged bool `json:"sourceBindingUnchanged"`
	Compiler               bool `json:"compiler"`
	PythonHeaders          bool `json:"pythonHeaders"`
	ConfigHeader           bool `json:"configHeader"`
}

type diagnosticPythonPreparationRequest struct {
	FirstInstall     bool   `json:"firstInstall,omitempty"`
	NodeID           string `json:"nodeId"`
	AcceptedHostKeys []struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	} `json:"acceptedHostKeys,omitempty"`
}

func validPythonRuntimeBinding(binding *diagnosticPythonRuntimeBinding, identity *diagnosticPackageIdentity) bool {
	if binding == nil || identity == nil || identity.UID <= 0 || !strings.HasPrefix(identity.Home, "/") || identity.Home == "/" || !vllmEnvironmentID.MatchString(binding.EnvironmentID) {
		return false
	}
	expected := path.Join(identity.Home, ".config/Nvidia Corporation/Personal AI Router/engine-bin/vllm/environments", binding.EnvironmentID, "bin/python")
	if binding.SchemaVersion != 1 || binding.InstallRoot != "" || binding.PythonPath != expected || binding.SourceExecutable != "/usr/bin/python3.12" || binding.SourcePackageVersion != diagnosticPythonPackageVersion || binding.PythonVersion != "3.12.3" || binding.IncludePath != "/usr/include/python3.12" || binding.ConfigHeaderPath != "/usr/include/python3.12/pyconfig.h" || binding.PythonSHA256 != binding.SourceExecutableSHA256 {
		return false
	}
	for _, value := range []string{binding.PythonSHA256, binding.RuntimeRecordSHA256, binding.RuntimeReceiptSHA256, binding.PipReportSHA256, binding.PyvenvConfigSHA256, binding.SourceExecutableSHA256} {
		if !diagnosticDigest.MatchString(value) {
			return false
		}
	}
	return true
}

// Schema 2 binds an absent installation and its fixed system source. It never
// supplies an environment or runtime receipt, and schema 1 stays repair-only.
func validPythonSourceBinding(binding *diagnosticPythonRuntimeBinding, identity *diagnosticPackageIdentity) bool {
	return binding != nil && identity != nil && identity.UID > 0 && strings.HasPrefix(identity.Home, "/") && identity.Home != "/" &&
		binding.SchemaVersion == 2 && binding.InstallRoot == path.Join(identity.Home, ".config/Nvidia Corporation/Personal AI Router/engine-bin/vllm") &&
		binding.EnvironmentID == "" && binding.PythonPath == "" && binding.PythonSHA256 == "" && binding.RuntimeRecordSHA256 == "" && binding.RuntimeReceiptSHA256 == "" && binding.PipReportSHA256 == "" && binding.PyvenvConfigSHA256 == "" &&
		binding.SourceExecutable == "/usr/bin/python3.12" && diagnosticDigest.MatchString(binding.SourceExecutableSHA256) && binding.SourcePackageVersion == diagnosticPythonPackageVersion &&
		binding.PythonVersion == "3.12.3" && binding.IncludePath == "/usr/include/python3.12" && binding.ConfigHeaderPath == "/usr/include/python3.12/pyconfig.h"
}

func validPythonPreparationBinding(binding *diagnosticPythonRuntimeBinding, identity *diagnosticPackageIdentity, firstInstall bool) bool {
	if firstInstall {
		return validPythonSourceBinding(binding, identity)
	}
	return validPythonRuntimeBinding(binding, identity)
}

func validPythonPrerequisites(facts *diagnosticPythonPrerequisites, identity *diagnosticPackageIdentity, ready bool) bool {
	return facts != nil && identity != nil && identity.UID > 0 && facts.CheckedAsUID == identity.UID && facts.SourceBindingUnchanged && facts.Compiler && (!ready || facts.PythonHeaders && facts.ConfigHeader)
}

func pythonPackageFamilyVersion(family []diagnosticInstalledMPIPackage) string {
	if len(family) != len(diagnosticPythonPackages) {
		return ""
	}
	seen := map[string]bool{}
	for _, row := range family {
		version, ok := diagnosticPythonPackages[row.Name]
		if !ok || seen[row.Name] || row.Status != "install ok installed" || row.Architecture != "arm64" || row.Version != version {
			return ""
		}
		seen[row.Name] = true
	}
	return diagnosticPythonPackageVersion
}

func validPythonPackagePlan(plan diagnosticApprovedPackagePlan) bool {
	if plan.SchemaVersion != 1 || plan.RecipeID != diagnosticPythonPackageRecipe || plan.RootPackage.Name != "python3.12-dev" || plan.RootPackage.Version != diagnosticPythonPackageVersion || len(plan.Packages) < 1 || len(plan.Packages) > 3 || plan.Limits.Upgrades || plan.Limits.Removals || plan.Limits.Recommends || plan.Limits.MaxPackages != 3 || plan.Limits.MaxDependencyUpgrades != 0 || plan.ArchivePolicy.NeedrestartMode != "" || !diagnosticDigest.MatchString(plan.ArchivePolicy.WorkerSHA256) || !(validPythonRuntimeBinding(plan.RuntimeBinding, plan.Identity) || validPythonSourceBinding(plan.RuntimeBinding, plan.Identity)) {
		return false
	}
	if !validPythonArchivePolicy(plan) {
		return false
	}
	seen := map[string]bool{}
	localArchive := plan.ArchivePolicy.WorkerSHA256 == diagnosticPythonLocalWorkerSHA256
	var download, installed float64
	for _, item := range plan.Packages {
		name, nameOK := item["name"].(string)
		version, versionOK := item["version"].(string)
		architecture, architectureOK := item["architecture"].(string)
		expected, allowed := diagnosticPythonPackages[name]
		_, upgrade := item["installedVersion"]
		sha, shaOK := item["sha256"].(string)
		size, sizeOK := item["size"].(float64)
		installedSize, installedOK := item["installedSize"].(float64)
		origin, originOK := item["origin"].(map[string]any)
		if !nameOK || !versionOK || !architectureOK || !allowed || upgrade || seen[name] || expected != version || architecture != "arm64" || len(item) != 7 || !shaOK || !diagnosticDigest.MatchString(sha) || !sizeOK || size <= 0 || size > 256<<20 || size != float64(uint64(size)) || !installedOK || installedSize <= 0 || installedSize > 1<<30 || installedSize != float64(uint64(installedSize)) || !originOK {
			return false
		}
		if localArchive {
			kib := map[string]uint64{"libexpat1-dev": 712, "libpython3.12-dev": 27505, "python3.12-dev": 502}
			if len(origin) != 3 || origin["kind"] != "verified_local_archive" || origin["source"] != "ubuntu-snapshot-20260901T000000Z" || origin["trusted"] != false || installedSize != float64(kib[name]*1024) {
				return false
			}
		} else {
			if len(origin) != 6 || origin["trusted"] != true || origin["label"] != "Ubuntu" || origin["origin"] != "Ubuntu" || (origin["site"] != "ports.ubuntu.com" && origin["site"] != "archive.ubuntu.com" && origin["site"] != "security.ubuntu.com") || (origin["archive"] != "noble" && origin["archive"] != "noble-updates" && origin["archive"] != "noble-security") {
				return false
			}
			if origin["component"] != "main" && origin["component"] != "universe" && origin["component"] != "restricted" && origin["component"] != "multiverse" {
				return false
			}
		}
		if len(plan.ArchivePolicy.ArchiveSource) != 0 {
			artifact := diagnosticPythonSnapshotArtifacts[name]
			if sha != artifact.SHA256 || size != float64(artifact.Size) {
				return false
			}
		}
		download += size
		installed += installedSize
		seen[name] = true
	}
	return download <= 256<<20 && installed <= 1<<30
}

func packageScopeMatches(purpose, nodeID string, firstInstall bool, review diagnosticPackageReview) bool {
	if firstInstall != review.FirstInstall {
		return false
	}
	if review.Purpose == "" {
		return purpose == "" && nodeID == "" && !firstInstall
	}
	return review.Purpose == diagnosticPythonPackagePurpose && purpose == review.Purpose && diagnosticToken.MatchString(nodeID) && len(review.Targets) == 1 && review.Targets[0].NodeID == nodeID && review.GroupID == diagnosticPythonPackagePrefix+nodeID
}

func validPackageParticipantBinding(binding diagnosticPackageBinding) bool {
	if binding.Review.Purpose == "" {
		return !binding.Review.FirstInstall && binding.RuntimeNodeID == "" && validDiagnosticParticipantBinding(binding.diagnosticParticipantBinding)
	}
	if binding.Review.Purpose != diagnosticPythonPackagePurpose || !diagnosticToken.MatchString(binding.RuntimeNodeID) || binding.Pair.GroupID != diagnosticPythonPackagePrefix+binding.RuntimeNodeID || len(binding.Pair.Members) != 1 || len(binding.Targets) != 1 || len(binding.Pins) != 1 || !diagnosticToken.MatchString(binding.Controller) || !diagnosticDigest.MatchString(binding.ControllerPin) {
		return false
	}
	member, target := binding.Pair.Members[0], binding.Targets[0]
	return member.NodeID == binding.RuntimeNodeID && member.Principal == member.NodeID && target.NodeID == member.NodeID && target.Principal == member.Principal && diagnosticDigest.MatchString(binding.Pins[target.Principal]) && net.ParseIP(target.Address) != nil && target.Local == (target.Principal == binding.Controller) && target.Local == (target.Candidate == nil) && (target.Local || onboardingID.MatchString(target.Candidate.CandidateID) && target.Candidate.Address == target.Address && target.Candidate.Port == 22 && target.Candidate.HostKeySHA256 != "")
}

func validRuntimePackageReviewPlans(binding diagnosticPackageBinding) bool {
	if binding.Review.Purpose == "" {
		return !binding.Review.FirstInstall
	}
	if binding.Review.Purpose != diagnosticPythonPackagePurpose || binding.Review.DependencyUpgradeReview || len(binding.Review.Targets) != 1 {
		return false
	}
	row := binding.Review.Targets[0]
	result, err := packageResult(row.Review)
	if err != nil || result.Identity == nil || result.Identity.NodeID != binding.RuntimeNodeID || result.Identity.Principal != binding.RuntimeNodeID || !validPythonPreparationBinding(result.RuntimeBinding, result.Identity, binding.Review.FirstInstall) || !validPythonPrerequisites(result.RuntimePrerequisites, result.Identity, row.State == "already_installed") {
		return false
	}
	if row.State == "already_installed" {
		return pythonPackageFamilyVersion(result.InstalledFamily) != ""
	}
	plan, err := approvedPackagePlan(result.Plan)
	return row.State == "reviewed" && err == nil && plan.RecipeID == diagnosticPythonPackageRecipe && reflect.DeepEqual(plan.RuntimeBinding, result.RuntimeBinding) && reflect.DeepEqual(plan.Identity, result.Identity)
}

// This is one runtime-repair target, not a synthetic MPI group. The existing
// peer directory and pin owner resolve identity; accepted SSH keys never do.
func (d *diagnosticService) pythonPreparationTargets(nodeID string) (diagnosticInspection, error) {
	result := diagnosticInspection{GroupID: diagnosticPythonPackagePrefix + nodeID, Targets: []diagnosticInspectionTarget{}}
	if !diagnosticToken.MatchString(nodeID) {
		return result, errors.New("a current paired runtime node is required")
	}
	d.m.mesh.Refresh()
	peer, ok := d.m.peers.lookup(nodeID)
	if !ok || peer.clusterUUID != nodeID || peer.port <= 0 || len(peer.addresses) == 0 || net.ParseIP(peer.addresses[0]) == nil {
		return result, errors.New("runtime repair requires a current paired control target")
	}
	if pin, ok := d.m.mesh.PinSHA256(peer.clusterUUID); !ok || !diagnosticDigest.MatchString(pin) {
		return result, errors.New("runtime repair target is not pinned by PAIR")
	}
	d.m.onboarding.expireAccess(time.Now())
	target := diagnosticInspectionTarget{NodeID: nodeID, Principal: peer.clusterUUID, Address: peer.addresses[0], Local: nodeID == d.m.mesh.NodeUUID(), State: "not-inspected"}
	if !target.Local {
		candidate, err := d.m.onboarding.addTarget(onboardingAddTargetRequest{Address: peer.addresses[0], Port: 22, Label: nodeID})
		if err != nil {
			return result, err
		}
		target.Candidate, target.State = &candidate, "access-required"
	}
	result.Targets = append(result.Targets, target)
	return result, nil
}

func (d *diagnosticService) packageInspectionTargets(request diagnosticSetupRequest, purpose string) (diagnosticInspection, error) {
	if purpose == "" {
		return d.inspectionTargets(request)
	}
	if purpose != diagnosticPythonPackagePurpose || len(request.Members) != 1 || request.Members[0].NodeID != request.Members[0].Principal || request.GroupID != diagnosticPythonPackagePrefix+request.Members[0].NodeID {
		return diagnosticInspection{}, errors.New("runtime package scope changed")
	}
	return d.pythonPreparationTargets(request.Members[0].NodeID)
}

func (d *diagnosticService) reviewVLLMPythonPreparation(ctx context.Context, request diagnosticPythonPreparationRequest) (diagnosticPackageReview, error) {
	if !diagnosticToken.MatchString(request.NodeID) || len(request.AcceptedHostKeys) > 1 {
		return diagnosticPackageReview{}, errors.New("invalid runtime prerequisite review target")
	}
	internal := diagnosticInspectionRequest{diagnosticSetupRequest: diagnosticSetupRequest{GroupID: diagnosticPythonPackagePrefix + request.NodeID, Members: []diagnosticSetupMember{{NodeID: request.NodeID, Principal: request.NodeID}}}, AcceptedHostKeys: request.AcceptedHostKeys}
	return d.reviewPackageScope(ctx, internal, false, diagnosticPythonPackagePurpose, request.FirstInstall)
}

func (d *diagnosticService) packageNativeBound(ctx context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget, binding diagnosticPackageBinding, request map[string]any) ([]byte, error) {
	if binding.Review.Purpose == diagnosticPythonPackagePurpose {
		request["runtimePreparation"] = true
		if binding.Review.FirstInstall {
			request["firstInstall"] = true
		}
	}
	return d.packageNative(ctx, target, access, request)
}

func samePythonRuntimeReview(before, after diagnosticPackageResult) bool {
	return (validPythonRuntimeBinding(after.RuntimeBinding, after.Identity) || validPythonSourceBinding(after.RuntimeBinding, after.Identity)) && validPythonPrerequisites(after.RuntimePrerequisites, after.Identity, true) && reflect.DeepEqual(before.RuntimeBinding, after.RuntimeBinding)
}

func validatePackageActionScope(request diagnosticPackageAction) error {
	if request.Purpose == "" {
		if request.FirstInstall || request.RuntimeNodeID != "" || strings.HasPrefix(request.GroupID, diagnosticPythonPackagePrefix) {
			return errors.New("runtime package status requires its explicit purpose and node")
		}
		return nil
	}
	if request.Purpose != diagnosticPythonPackagePurpose || !diagnosticToken.MatchString(request.RuntimeNodeID) || request.GroupID != "" && request.GroupID != diagnosticPythonPackagePrefix+request.RuntimeNodeID {
		return errors.New("invalid runtime package purpose or node")
	}
	if request.OperationID == "" && request.ReviewID == "" && request.GroupID == "" {
		return errors.New("runtime package scope requires its retained operation, review or group selector")
	}
	return nil
}
