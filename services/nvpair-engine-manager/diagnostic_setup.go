// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

const diagnosticSetupRecipe = "dgx-spark-arm64-nccl-2.30.7-tests-2.20.0-r1"

type diagnosticSetupMember struct {
	NodeID            string `json:"nodeId"`
	Principal         string `json:"principal"`
	GB10Observed      bool   `json:"gb10Observed"`
	ControlAdvertised bool   `json:"controlAdvertised"`
}

// Only the broker supplies observations, over the existing local stdio channel.
// This request is never accepted on the remote execution control endpoint.
type diagnosticSetupRequest struct {
	GroupID string                  `json:"groupId"`
	Members []diagnosticSetupMember `json:"members"`
}

type diagnosticSetupSource struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Revision string `json:"revision"`
}

type diagnosticSetupPrerequisite struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type diagnosticSetupStep struct {
	ID        string `json:"id"`
	Privilege string `json:"privilege"`
	Effect    string `json:"effect"`
	Rollback  string `json:"rollback"`
}

type diagnosticSetupReview struct {
	SchemaVersion     int                           `json:"schemaVersion"`
	ReviewID          string                        `json:"reviewId"`
	RecipeID          string                        `json:"recipeId"`
	GroupID           string                        `json:"groupId"`
	State             string                        `json:"state"`
	Executable        bool                          `json:"executable"`
	EffectsApplied    bool                          `json:"effectsApplied"`
	Target            diagnosticSetupTarget         `json:"target"`
	Package           diagnosticSetupPackage        `json:"package"`
	BuildJobs         int                           `json:"buildJobs"`
	ConcurrentNodes   int                           `json:"concurrentNodes"`
	RequiredDiskBytes *uint64                       `json:"requiredDiskBytes"`
	Members           []diagnosticSetupMember       `json:"members"`
	Sources           []diagnosticSetupSource       `json:"sources"`
	Prerequisites     []diagnosticSetupPrerequisite `json:"prerequisites"`
	Steps             []diagnosticSetupStep         `json:"steps"`
	Adaptations       []string                      `json:"adaptations"`
}

type diagnosticSetupTarget struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Device       string `json:"device"`
	MinimumCUDA  string `json:"minimumCuda"`
	SM           int    `json:"sm"`
}

type diagnosticSetupPackage struct {
	Name                string  `json:"name"`
	Architecture        string  `json:"architecture"`
	Version             *string `json:"version"`
	TransactionResolved bool    `json:"transactionResolved"`
}

func validateDiagnosticSetupRoster(request diagnosticSetupRequest) error {
	if !validDiagnosticParticipantCount(len(request.Members)) {
		return errors.New("DGX Spark setup review requires two or three currently selected participants")
	}
	identities := make([]string, 0, 2*len(request.Members))
	principals := map[string]bool{}
	for i, member := range request.Members {
		if !diagnosticToken.MatchString(member.NodeID) || !diagnosticToken.MatchString(member.Principal) || principals[member.Principal] || i > 0 && request.Members[i-1].NodeID >= member.NodeID {
			return errors.New("setup review participants have invalid or ambiguous identities")
		}
		principals[member.Principal] = true
		identities = append(identities, member.NodeID, member.Principal)
	}
	// Preserve the existing two-member identity encoding for retained records.
	binding, _ := json.Marshal(identities)
	digest := sha256.Sum256(binding)
	if request.GroupID != fmt.Sprintf("pair-recipe/nccl-%x", digest[:16]) {
		return errors.New("setup review does not match the selected participant roster")
	}
	return nil
}

func (d *diagnosticService) setupReview(request diagnosticSetupRequest) (diagnosticSetupReview, error) {
	if err := validateDiagnosticSetupRoster(request); err != nil {
		return diagnosticSetupReview{}, err
	}
	d.m.mesh.Refresh()
	if !d.m.mesh.Clustered() {
		return diagnosticSetupReview{}, errors.New("PAIR membership changed; refresh recipe inspection")
	}
	for _, member := range request.Members {
		if !d.m.mesh.HasPin(member.Principal) {
			return diagnosticSetupReview{}, errors.New("PAIR membership changed; refresh recipe inspection")
		}
	}
	// This is the review phase of the existing engine-manager ownership model.
	// No Install/Action, process, profile write, reservation or operation is made.
	review := diagnosticSetupReview{
		SchemaVersion: 1, RecipeID: diagnosticSetupRecipe, GroupID: request.GroupID,
		State: "blocked", Target: diagnosticSetupTarget{"linux", "arm64", "DGX Spark GB10", "13.0", 121},
		Package:   diagnosticSetupPackage{Name: "libopenmpi-dev", Architecture: "arm64"},
		BuildJobs: 2, ConcurrentNodes: 1,
		Members: append([]diagnosticSetupMember(nil), request.Members...),
		Sources: []diagnosticSetupSource{
			{"NVIDIA two-Spark NCCL recipe", "https://github.com/NVIDIA/dgx-spark-playbooks/blob/4663a75d67f129eb121b5ae9ddba21d55dee12bf/nvidia/nccl/README.md", "4663a75d67f129eb121b5ae9ddba21d55dee12bf"},
			{"NCCL v2.30.7-1", "https://github.com/NVIDIA/nccl/commit/73cf112295c33aee2b895f329f592f2a9b4b0f97", "73cf112295c33aee2b895f329f592f2a9b4b0f97"},
			{"nccl-tests v2.20.0 (PAIR source pin)", "https://github.com/NVIDIA/nccl-tests/commit/b4d5beebca8a76cf01335f724d154b9b9d394d96", "b4d5beebca8a76cf01335f724d154b9b9d394d96"},
		},
		Prerequisites: []diagnosticSetupPrerequisite{
			{"platform", "unknown", "Both participants must attest Linux ARM64, DGX Spark GB10, NVIDIA driver and CUDA toolkit >=13.0 with SM 121 compiler support. Discovery does not prove these facts."},
			{"packages", "unknown", "Resolve libopenmpi-dev and its ARM64 dependencies from each host's configured signed APT repositories. Package versions, downloads, dependency changes and repository access have not been inspected."},
			{"privilege", "blocked", "System MPI packages require separately authorized administrator access on each participant. Engine Manager is user-mode only; this review accepts no password and grants no elevation."},
			{"resources", "unknown", "Check writable PAIR-owned staging, free disk, native git/make/C++ tools and a CPU build window on each participant. Source/build byte requirements are not measured; proposed builds use at most two CPU jobs per node, sequential nodes."},
			{"artifacts", "unknown", "Verify the exact pinned source revisions and resulting NCCL/MPI/test binary and library identities. A source commit pin is not a verified downloadable-archive checksum or successful build."},
			{"validation", "blocked", "Existing SSH trust, exact GPUs/interfaces, idle availability and owned-process cleanup must be admitted separately before running the socket smoke test. NCCL 2.30.7 release notes include a possible two-rank CE-collective hang; runtime compatibility is unproven."},
		},
		Steps: []diagnosticSetupStep{
			{"system-mpi", "administrator", "On each participant, review the exact APT transaction for libopenmpi-dev and dependencies; refresh package metadata and install only after separate administrator approval.", "Record pre-existing packages and the exact transaction. Do not automatically remove shared packages; package rollback requires its own administrator review."},
			{"source", "user", "Acquire the pinned NVIDIA NCCL and nccl-tests sources into a new recipe-specific directory under that participant's PAIR engine-bin/diagnostic-tools root; record provenance and refuse pre-existing destination ownership conflicts.", "Remove only this operation's newly created staging directory after verifying its recorded ownership; retain pre-existing files and failed evidence."},
			{"build", "user", "Build NCCL src.build for compute_121/sm_121, then nccl-tests with MPI=1 against that private NCCL and the inspected system Open MPI/CUDA. Limit to two CPU jobs and one participant at a time.", "Cancel only owned compiler processes, confirm their exit, and remove only recorded new build outputs. Preserve the prior active tool receipt; never replace system CUDA/NCCL."},
			{"register", "user", "After verified build and library provenance, atomically register the owned artifact receipt with Engine Manager. Registration must not start a GPU test or mark communications passed.", "Restore the previous receipt if one existed; otherwise withdraw only the newly registered receipt. Keep source/build failure evidence."},
		},
		Adaptations: []string{
			"The vendor recipe pins NCCL but floats nccl-tests. PAIR pins nccl-tests v2.20.0 for reproducibility; NVIDIA does not establish this exact version pair as tested.",
			"PAIR proposes private managed build directories and bounded CPU concurrency instead of executing the vendor multi-host sudo/password helper.",
			"The vendor performance example uses all-gather/RoCE. PAIR's retained all-reduce socket smoke test, strict SSH trust and smaller buffers are separate adaptations; setup does not establish RDMA or performance acceptance.",
		},
	}
	if len(request.Members) == 3 {
		review.Prerequisites[0].Detail = "All three participants must attest Linux ARM64, DGX Spark GB10, NVIDIA driver and CUDA toolkit >=13.0 with SM 121 compiler support. Discovery does not prove these facts."
	}
	data, _ := json.Marshal(review)
	reviewDigest := sha256.Sum256(data)
	review.ReviewID = fmt.Sprintf("%x", reviewDigest)
	return review, nil
}

func (m *Manager) handleDiagnosticSetupReview(msg *Message) {
	var request diagnosticSetupRequest
	if err := strictDiagnosticJSON(msg.Params, &request); err != nil {
		m.codec.RespondError(msg.ID, -32602, "invalid diagnostic setup review request")
		return
	}
	result, err := m.exec.diagnostics.setupReview(request)
	m.respondOrErr(msg, result, err)
}
