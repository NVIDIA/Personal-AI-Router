// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const diagnosticPreset = "nccl-smoke"

// These profiles are provisioned by the operator on each participant. They are
// not engine manifests, PAIR membership, or writable through the desktop API.
type diagnosticTool struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type diagnosticMember struct {
	NodeID           string                  `json:"nodeId"`
	Principal        string                  `json:"principal"`
	ClusterPinSHA256 string                  `json:"clusterPinSha256,omitempty"`
	Host             string                  `json:"host"`
	User             string                  `json:"user"`
	GPU              string                  `json:"gpu"`
	Interface        string                  `json:"interface"`
	Manager          diagnosticTool          `json:"manager"`
	NCCL             diagnosticTool          `json:"nccl"`
	SMI              diagnosticTool          `json:"smi"`
	Runtime          diagnosticMemberRuntime `json:"runtime,omitzero"`
	Fabric           diagnosticMemberFabric  `json:"fabric,omitzero"`
}

type diagnosticProfile struct {
	Transport    string             `json:"transport"`
	GroupID      string             `json:"groupId"`
	Label        string             `json:"label"`
	OwnerNodeID  string             `json:"ownerNodeId"`
	Members      []diagnosticMember `json:"members"`
	MPI          diagnosticTool     `json:"mpi"`
	SSH          diagnosticTool     `json:"ssh"`
	KnownHosts   diagnosticTool     `json:"knownHosts"`
	IdentityFile string             `json:"identityFile"`
	// This is operator acknowledgement of an externally maintained test window,
	// not a claim that PAIR can exclude arbitrary foreign GPU users.
	DedicatedTestWindow bool                       `json:"dedicatedTestWindow"`
	Bootstrap           diagnosticBootstrapProfile `json:"bootstrap,omitzero"`
	Fabric              diagnosticMPIFabric        `json:"fabric,omitzero"`
}

type diagnosticGroup struct {
	ActiveOperationID string   `json:"activeOperationId,omitempty"`
	GroupID           string   `json:"groupId"`
	Label             string   `json:"label"`
	OwnerNodeID       string   `json:"ownerNodeId"`
	MemberNodeIDs     []string `json:"memberNodeIds"`
	Preset            string   `json:"preset"`
	Available         bool     `json:"available"`
	Reason            string   `json:"reason,omitempty"`
}

type diagnosticSample struct {
	Bytes         uint64  `json:"bytes"`
	AlgorithmGBps float64 `json:"algorithmGBps"`
	BusGBps       float64 `json:"busGBps"`
	LatencyUs     float64 `json:"latencyUs"`
	Wrong         uint64  `json:"wrong"`
}

type diagnosticOperation struct {
	profileDigest    string
	OperationID      string             `json:"operationId"`
	GroupID          string             `json:"groupId"`
	OwnerNodeID      string             `json:"ownerNodeId"`
	Preset           string             `json:"preset"`
	RecipeID         string             `json:"recipeId,omitempty"`
	State            string             `json:"state"`
	StartedAt        int64              `json:"startedAt"`
	FinishedAt       int64              `json:"finishedAt,omitempty"`
	Message          string             `json:"message"`
	CleanupConfirmed bool               `json:"cleanupConfirmed"`
	MemberNodeIDs    []string           `json:"memberNodeIds"`
	Samples          []diagnosticSample `json:"samples,omitempty"`
}

var diagnosticToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var diagnosticPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
var diagnosticDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func loadDiagnosticProfiles(base string) ([]diagnosticProfile, error) {
	if base == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(base, "diagnostic-groups.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := diagnosticProfileOwnership(f); err != nil {
		return nil, err
	}
	var body struct {
		Groups []diagnosticProfile `json:"groups"`
	}
	dec := json.NewDecoder(io.LimitReader(f, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("diagnostic profile: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("diagnostic profile has trailing or oversized data")
	}
	if len(body.Groups) > 16 {
		return nil, errors.New("at most 16 diagnostic groups are supported")
	}
	seen := map[string]bool{}
	for _, p := range body.Groups {
		if !diagnosticToken.MatchString(p.GroupID) || seen[p.GroupID] {
			return nil, errors.New("invalid or duplicate diagnostic group ID")
		}
		seen[p.GroupID] = true
	}
	return body.Groups, nil
}

func (p diagnosticProfile) member(id string) (diagnosticMember, bool) {
	for _, member := range p.Members {
		if member.NodeID == id {
			return member, true
		}
	}
	return diagnosticMember{}, false
}

func (p diagnosticProfile) descriptor() diagnosticGroup {
	g := diagnosticGroup{GroupID: p.GroupID, Label: p.Label, OwnerNodeID: p.OwnerNodeID, Preset: diagnosticPreset, MemberNodeIDs: []string{}}
	for _, member := range p.Members {
		g.MemberNodeIDs = append(g.MemberNodeIDs, member.NodeID)
	}
	return g
}

func (p diagnosticProfile) validate() error {
	if p.Bootstrap != (diagnosticBootstrapProfile{}) {
		return validateDiagnosticBootstrapProfile(p)
	}
	if p.Fabric != (diagnosticMPIFabric{}) {
		return errors.New("fabric NCCL bindings require an operation bootstrap profile")
	}
	if p.Transport != "socket" {
		return errors.New("this fixed NCCL smoke recipe requires explicit socket transport; RDMA recipes are not admitted")
	}
	if !p.DedicatedTestWindow {
		return errors.New("operator must reserve a dedicated test window; PAIR does not exclude foreign GPU users")
	}
	if len(p.Members) < 2 || len(p.Members) > 4 {
		return errors.New("the fixed NCCL smoke recipe supports 2 to 4 participants")
	}
	seen := map[string]bool{}
	principals := map[string]bool{}
	for _, m := range p.Members {
		if m.Runtime != (diagnosticMemberRuntime{}) || m.Fabric != (diagnosticMemberFabric{}) {
			return errors.New("managed runtime bindings require an operation bootstrap profile")
		}
		if !diagnosticToken.MatchString(m.NodeID) || !diagnosticToken.MatchString(m.Principal) || seen[m.NodeID] || principals[m.Principal] {
			return errors.New("diagnostic members require distinct pinned node identities")
		}
		seen[m.NodeID], principals[m.Principal] = true, true
		if net.ParseIP(m.Host).To4() == nil || !diagnosticToken.MatchString(m.User) || !strings.HasPrefix(m.GPU, "GPU-") || !diagnosticToken.MatchString(m.GPU) || !diagnosticToken.MatchString(m.Interface) {
			return errors.New("invalid pinned host, user, GPU, or interface")
		}
		for _, tool := range []diagnosticTool{m.Manager, m.NCCL, m.SMI} {
			if err := tool.validate(); err != nil {
				return err
			}
		}
	}
	if !seen[p.OwnerNodeID] {
		return errors.New("diagnostic coordinator must be an explicit participant")
	}
	for _, tool := range []diagnosticTool{p.MPI, p.SSH, p.KnownHosts} {
		if err := tool.validate(); err != nil {
			return err
		}
	}
	if !diagnosticPath.MatchString(p.IdentityFile) {
		return errors.New("an existing SSH identity file must be explicitly configured")
	}
	return nil
}

func (t diagnosticTool) validate() error {
	if !diagnosticPath.MatchString(t.Path) || !diagnosticDigest.MatchString(t.SHA256) {
		return errors.New("diagnostic tools and SSH known-hosts require an absolute safe path and SHA-256 pin")
	}
	return nil
}

func (t diagnosticTool) verify() error {
	if err := t.validate(); err != nil {
		return err
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return fmt.Errorf("required diagnostic file unavailable: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<20 {
		return errors.New("diagnostic file is not a bounded regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != t.SHA256 {
		return fmt.Errorf("diagnostic file identity changed: %s", t.Path)
	}
	return nil
}
