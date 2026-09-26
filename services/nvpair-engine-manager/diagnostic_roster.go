// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// PAIR supports only these reviewed participant counts; this is not an N-node
// admission contract.
func validDiagnosticParticipantCount(n int) bool { return n == 2 || n == 3 }

func validDiagnosticMPIMemberIDs(ids []string) bool {
	if !validDiagnosticParticipantCount(len(ids)) {
		return false
	}
	for i, id := range ids {
		if !diagnosticToken.MatchString(id) || i > 0 && ids[i-1] >= id {
			return false
		}
	}
	return true
}

func diagnosticMPIMemberDigest(ids []string) string {
	if !validDiagnosticMPIMemberIDs(ids) {
		return ""
	}
	raw, _ := json.Marshal(ids)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func diagnosticMPIProfileMemberIDs(p diagnosticProfile) []string {
	ids := make([]string, len(p.Members))
	for i, member := range p.Members {
		ids[i] = member.NodeID
	}
	return ids
}

func diagnosticMPIRecoveryMemberIDs(members []diagnosticMPIRecoveryMember) []string {
	ids := make([]string, len(members))
	for i, member := range members {
		ids[i] = member.NodeID
	}
	return ids
}

// Select only from one complete, validated registration. The original record
// and build binding remain untouched; no target can be supplied by the client.
func selectDiagnosticMPIRecord(record diagnosticManagedRecord, binding diagnosticParticipantBinding, ids []string) (diagnosticManagedRecord, diagnosticParticipantBinding, error) {
	if !validDiagnosticParticipantBinding(binding) || len(record.Targets) != len(binding.Targets) || record.GroupID != binding.Pair.GroupID {
		return record, binding, errors.New("registered MPI roster does not match its original build")
	}
	all := make([]string, len(record.Targets))
	for i, target := range record.Targets {
		bound := binding.Targets[i]
		if target.NodeID != bound.NodeID || target.Principal != bound.Principal || target.Address != bound.Address || target.Local != bound.Local || target.ClusterPinSHA256 != binding.Pins[target.Principal] {
			return record, binding, errors.New("registered MPI target changed its original participant binding")
		}
		all[i] = target.NodeID
	}
	if ids == nil {
		ids = all
	}
	if !validDiagnosticMPIMemberIDs(ids) {
		return record, binding, errors.New("MPI selection requires two or three sorted distinct registered nodes")
	}
	selected := record
	selected.Targets = make([]diagnosticManagedTarget, 0, len(ids))
	view := binding
	view.Pair.Members = make([]diagnosticSetupMember, 0, len(ids))
	view.Targets = make([]diagnosticInspectionTarget, 0, len(ids))
	view.Pins = make(map[string]string, len(ids))
	view.Generations = make(map[string]string, len(ids))
	identities := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		i, ok := slices.BinarySearch(all, id)
		if !ok {
			return record, binding, errors.New("MPI participant is absent from the selected registered build")
		}
		target := binding.Targets[i]
		selected.Targets = append(selected.Targets, record.Targets[i])
		view.Targets = append(view.Targets, target)
		view.Pair.Members = append(view.Pair.Members, binding.Pair.Members[i])
		view.Pins[target.Principal] = binding.Pins[target.Principal]
		view.Generations[id] = binding.Generations[id]
		identities = append(identities, id, target.Principal)
	}
	// Discovery checks use the selected roster's identity; the selected record
	// retains the original build GroupID and OperationID for artifact ownership.
	raw, _ := json.Marshal(identities)
	digest := sha256.Sum256(raw)
	view.Pair.GroupID = fmt.Sprintf("pair-recipe/nccl-%x", digest[:16])
	return selected, view, nil
}
