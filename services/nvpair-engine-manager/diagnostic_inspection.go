// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"
)

//go:embed diagnostic_tools_remote.py
var diagnosticInspectPython string

type diagnosticInspectionRequest struct {
	diagnosticSetupRequest
	AcceptedHostKeys []struct {
		CandidateID string `json:"candidateId"`
		SHA256      string `json:"sha256"`
	} `json:"acceptedHostKeys,omitempty"`
}

type diagnosticInspectionTarget struct {
	NodeID    string               `json:"nodeId"`
	Principal string               `json:"principal"`
	Address   string               `json:"address"`
	Local     bool                 `json:"local"`
	Candidate *onboardingCandidate `json:"candidate,omitempty"`
	State     string               `json:"state"`
	Reason    string               `json:"reason,omitempty"`
	Facts     json.RawMessage      `json:"facts,omitempty"`
}

type diagnosticInspection struct {
	GroupID      string                       `json:"groupId"`
	Targets      []diagnosticInspectionTarget `json:"targets"`
	ObservedAt   int64                        `json:"observedAt"`
	CanProvision bool                         `json:"canProvision"`
}

// This uses onboarding's existing volatile candidate/access owner. It does not
// call onboarding inspection/approval, install PAIR, or change cluster membership.
func (d *diagnosticService) inspectionTargets(request diagnosticSetupRequest) (diagnosticInspection, error) {
	result := diagnosticInspection{GroupID: request.GroupID, Targets: []diagnosticInspectionTarget{}}
	if _, err := d.setupReview(request); err != nil {
		return result, err
	}
	s := d.m.onboarding
	s.expireAccess(time.Now())
	for _, member := range request.Members {
		peer, ok := d.m.peers.lookup(member.NodeID)
		if !ok || peer.clusterUUID != member.Principal || len(peer.addresses) == 0 || net.ParseIP(peer.addresses[0]) == nil {
			return result, errors.New("selected participant has no current pinned PAIR control address")
		}
		if member.Principal == d.m.mesh.NodeUUID() {
			result.Targets = append(result.Targets, diagnosticInspectionTarget{NodeID: member.NodeID, Principal: member.Principal, Address: peer.addresses[0], Local: true, State: "not-inspected"})
			continue
		}
		candidate, err := s.addTarget(onboardingAddTargetRequest{Address: peer.addresses[0], Port: 22, Label: member.NodeID})
		if err != nil {
			return result, err
		}
		result.Targets = append(result.Targets, diagnosticInspectionTarget{NodeID: member.NodeID, Principal: member.Principal, Address: peer.addresses[0], Candidate: &candidate, State: "access-required"})
	}
	return result, nil
}

func (d *diagnosticService) inspectParticipants(ctx context.Context, request diagnosticInspectionRequest) (diagnosticInspection, error) {
	if err := ctx.Err(); err != nil {
		return diagnosticInspection{}, err
	}
	result, err := d.inspectionTargets(request.diagnosticSetupRequest)
	if err != nil {
		return result, err
	}
	if len(request.AcceptedHostKeys) > len(result.Targets) {
		return result, errors.New("SSH fingerprint consent exceeds the selected participants")
	}
	accepted := map[string]string{}
	for _, key := range request.AcceptedHostKeys {
		if _, duplicate := accepted[key.CandidateID]; duplicate || !onboardingID.MatchString(key.CandidateID) || key.SHA256 == "" {
			return result, errors.New("invalid or duplicate SSH fingerprint acceptance")
		}
		known := false
		for _, target := range result.Targets {
			known = known || (target.Candidate != nil && target.Candidate.CandidateID == key.CandidateID)
		}
		if !known {
			return result, errors.New("SSH fingerprint is not for a selected participant")
		}
		accepted[key.CandidateID] = key.SHA256
	}
	s := d.m.onboarding
	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(result.Targets))*35*time.Second+5*time.Second)
	defer cancel()
	for i := range result.Targets {
		row := &result.Targets[i]
		input, _ := json.Marshal(map[string]any{"action": "inspect", "nodeId": row.NodeID, "principal": row.Principal, "peerAddress": result.Targets[(i+1)%len(result.Targets)].Address})
		if row.Local {
			readCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			output, readErr := diagnosticInspectLocal(readCtx, input)
			applyDiagnosticInspection(row, output, readErr)
			stop()
			continue
		}
		s.mu.Lock()
		current := s.targets[row.Candidate.CandidateID]
		var target onboardingPrivateTarget
		if current != nil {
			target = *current
		}
		s.mu.Unlock()
		if current == nil || !target.candidate.AccessAvailable || target.expiresAt.IsZero() || !time.Now().Before(target.expiresAt) {
			row.Reason = "Supply device account access through PAIR before inspection"
			continue
		}
		candidate := target.candidate
		row.Candidate = &candidate
		if target.changedKey || target.candidate.HostKeySHA256 == "" || (!target.candidate.HostKeyTrusted && accepted[target.candidate.CandidateID] != target.candidate.HostKeySHA256) {
			row.State, row.Reason = "blocked", "Existing SSH trust rejects this device, or its exact observed fingerprint has not been accepted"
			continue
		}
		target.candidate.HostKeyTrusted = true // Only this read's explicit fingerprint consent.
		readCtx, stop := context.WithTimeout(ctx, 35*time.Second)
		client, readErr := s.dial(readCtx, target.candidate, target.access)
		if readErr == nil {
			var output []byte
			output, readErr = client.run(readCtx, "/usr/bin/python3 -I -c "+onboardingQuote(diagnosticInspectPython), bytes.NewReader(input))
			client.close()
			if readErr == nil {
				readErr = readCtx.Err()
			}
			applyDiagnosticInspection(row, output, readErr)
		}
		stop()
		s.mu.Lock()
		latest := s.targets[target.candidate.CandidateID]
		unchanged := latest != nil && latest.accessGeneration == target.accessGeneration && latest.candidate.HostKeySHA256 == target.candidate.HostKeySHA256 && latest.expiresAt.After(time.Now())
		s.mu.Unlock()
		if !unchanged {
			readErr = errors.New("device access changed during inspection; inspect again")
		}
		if readErr != nil {
			row.State, row.Reason, row.Facts = "blocked", diagnosticPublicMessage(readErr.Error()), nil
		}
	}
	if _, err := d.setupReview(request.diagnosticSetupRequest); err != nil {
		return diagnosticInspection{}, err
	}
	result.ObservedAt = time.Now().UnixMilli()
	return result, nil
}

func diagnosticInspectLocal(ctx context.Context, input []byte) ([]byte, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		return nil, errors.New("local participant inspection requires its native Linux ARM64 owner; no SSH-to-self is requested")
	}
	// Fixed product program and validated nonsecret selectors; no user command
	// or environment. Existing diagnostic process ownership bounds cancellation.
	script := diagnosticInspectionProgram(input, diagnosticInspectPython)
	return diagnosticProcess(ctx, "/usr/bin/python3", []string{"-I", "-c", script}, []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + os.Getenv("HOME")}, nil)
}

func diagnosticInspectionProgram(input []byte, source string) string {
	// The production helper reads stdin.buffer, including on the local-owner path.
	return fmt.Sprintf("import io,sys\nsys.stdin=io.TextIOWrapper(io.BytesIO(%s.encode('utf-8')))\n%s", strconv.Quote(string(input)), source)
}

func applyDiagnosticInspection(row *diagnosticInspectionTarget, output []byte, err error) {
	if err == nil {
		var facts struct {
			SchemaVersion  int    `json:"schemaVersion"`
			Action         string `json:"action"`
			State          string `json:"state"`
			EffectsApplied *bool  `json:"effectsApplied"`
			Executable     *bool  `json:"executable"`
			Errors         []struct {
				Code  string `json:"code"`
				Phase string `json:"phase"`
			} `json:"errors"`
			Identity struct {
				NodeID    string `json:"nodeId"`
				Principal string `json:"principal"`
			} `json:"identity"`
		}
		if len(output) > 128<<10 || json.Unmarshal(output, &facts) != nil || facts.SchemaVersion != 1 || facts.Action != "inspect" || facts.Identity.NodeID != row.NodeID || facts.Identity.Principal != row.Principal || facts.EffectsApplied == nil || *facts.EffectsApplied || facts.Executable == nil || *facts.Executable {
			err = errors.New("native inspection identity or read-only response did not match the selected PAIR participant")
			for i, detail := range facts.Errors {
				if i >= 8 {
					break
				}
				if diagnosticToken.MatchString(detail.Code) && diagnosticToken.MatchString(detail.Phase) {
					err = fmt.Errorf("%w; %s/%s", err, detail.Phase, detail.Code)
				}
			}
		} else if facts.State != "inspected" && facts.State != "blocked" {
			err = errors.New("native inspection returned an unsupported state")
		} else {
			row.State = facts.State
			row.Facts = append(json.RawMessage(nil), output...)
		}
	}
	if err != nil {
		row.State, row.Reason, row.Facts = "blocked", diagnosticPublicMessage(err.Error()), nil
	}
}

func (m *Manager) handleDiagnosticInspection(ctx context.Context, msg *Message) {
	var request diagnosticInspectionRequest
	if onboardingDecode(msg.Params, &request) != nil {
		m.codec.RespondError(msg.ID, -32602, "invalid diagnostic inspection request")
		return
	}
	var result diagnosticInspection
	var err error
	if msg.Method == "engine:diagnostic-targets" {
		result, err = m.exec.diagnostics.inspectionTargets(request.diagnosticSetupRequest)
	} else {
		result, err = m.exec.diagnostics.inspectParticipants(ctx, request)
	}
	m.respondOrErr(msg, result, err)
}
