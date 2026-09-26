// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"nvpair-shared/hostbootstrap"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSetupCandidateLanesStaySeparate(t *testing.T) {
	ready := setupCandidate{
		CandidateID:    "a",
		Port:           22,
		BootstrapState: "ssh-ready",
	}
	bootstrap := setupCandidate{
		CandidateID:    "b",
		Port:           22,
		BootstrapState: "bootstrap-required",
	}
	if setupCandidateLane(ready) != "ssh-ready" || setupCandidateLane(bootstrap) != "bootstrap-required" {
		t.Fatal("enrollment lanes were mixed")
	}
	partition := partitionSetupCandidates([]setupCandidate{bootstrap, ready})
	if len(partition.SSHReady) != 1 ||
		partition.SSHReady[0].CandidateID != "a" ||
		len(partition.BootstrapRequired) != 1 ||
		partition.BootstrapRequired[0].CandidateID != "b" {
		t.Fatalf("partition=%#v", partition)
	}
}

func TestSetupBootstrapRPCParityWithDesktop(t *testing.T) {
	request := setupBootstrapTestRequest()
	observed := setupBootstrapAbsentObservations()
	status := hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       request.Binding,
		Observed:      observed,
	}
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	verified := setupBootstrapExactObservations(request)
	receipt, err := hostbootstrap.NewReceipt(plan, verified)
	if err != nil {
		t.Fatal(err)
	}
	reference := setupBootstrapTargetReference{
		CandidateID:   strings.Repeat("c", 32),
		AccessID:      strings.Repeat("d", 32),
		HostKeySHA256: "SHA256:fixture-host-key",
	}

	for _, test := range []struct {
		method string
		build  func(*rpc.Client) tea.Cmd
		result any
	}{
		{
			method: "engine:onboarding-bootstrap-catalog",
			build:  setupBootstrapCatalogCmd,
			result: setupBootstrapCatalog{
				SchemaVersion: 1,
				Targets:       []setupBootstrapCatalogTarget{},
			},
		},
		{
			method: "engine:onboarding-bootstrap-controller-keys",
			build:  setupBootstrapControllerKeysCmd,
			result: setupBootstrapControllerKeys{
				SchemaVersion: 1,
				Keys:          []hostbootstrap.PublicKeyIdentity{request.Binding.ControllerKey},
			},
		},
		{
			method: "engine:onboarding-scopes",
			build:  setupScopesCmd,
			result: map[string]any{"scopes": []setupDiscoveryScope{}},
		},
		{
			method: "engine:onboarding-discover",
			build: func(client *rpc.Client) tea.Cmd {
				return setupDiscoverCmd(client, strings.Repeat("e", 32))
			},
			result: map[string]any{"candidates": []setupCandidate{}},
		},
		{
			method: "engine:onboarding-import-artifact",
			build: func(client *rpc.Client) tea.Cmd {
				return setupImportArtifactCmd(client, "/fixture/nvpair-product.zip")
			},
			result: setupArtifact{
				ArtifactID: "fixture",
				Version:    "1.2.3",
				Platform:   "linux",
				Arch:       "arm64",
				SHA256:     strings.Repeat("a", 64),
				Provenance: "engineering",
			},
		},
		{
			method: "engine:onboarding-bootstrap-inspect",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapInspectCmd(client, reference, request)
			},
			result: status,
		},
		{
			method: "engine:onboarding-bootstrap-review",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapReviewCmd(client, reference, request)
			},
			result: plan,
		},
		{
			method: "engine:onboarding-bootstrap-apply",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapApplyCmd(client, reference, plan)
			},
			result: status,
		},
		{
			method: "engine:onboarding-bootstrap-status",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapStatusCmd(client, reference, request.OperationID)
			},
			result: status,
		},
		{
			method: "engine:onboarding-bootstrap-recover",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapRecoverCmd(client, reference, request.OperationID)
			},
			result: status,
		},
		{
			method: "engine:onboarding-bootstrap-verify",
			build: func(client *rpc.Client) tea.Cmd {
				return setupBootstrapVerifyCmd(client, reference, plan)
			},
			result: receipt,
		},
	} {
		t.Run(test.method, func(t *testing.T) {
			rpcRequest, _ := runTUICommand(t, test.build, test.result)
			assertSetupMethod(t, rpcRequest, test.method)
			raw := strings.ToLower(string(rpcRequest.Params))
			if strings.Contains(raw, "private") ||
				strings.Contains(raw, `"receipt"`) ||
				strings.Contains(raw, `"observations"`) {
				t.Fatalf("request carried prohibited controller state: %s", rpcRequest.Params)
			}
		})
	}
}

func TestSetupBootstrapFourStepStateUsesBackendMetadata(t *testing.T) {
	requestFixture := setupBootstrapTestRequest()
	candidate := setupCandidate{
		CandidateID:           strings.Repeat("c", 32),
		Label:                 "fixture",
		Address:               requestFixture.Binding.Endpoint.Address,
		Port:                  requestFixture.Binding.Endpoint.Port,
		AccessID:              strings.Repeat("d", 32),
		AccessAvailable:       true,
		HostKeySHA256:         "SHA256:fixture-host-key",
		HostKeyTrusted:        true,
		BootstrapSource:       "mdns+ssh-banner",
		BootstrapState:        "ssh-ready",
		BootstrapPlatform:     string(requestFixture.Binding.Target.Platform),
		BootstrapArchitecture: string(requestFixture.Binding.Target.Architecture),
	}
	view := newSetupView(nil)
	view.candidates = []setupCandidate{candidate}
	view.selected[candidate.CandidateID] = true
	view.accessAccounts[candidate.CandidateID] =
		requestFixture.Binding.Account.Name
	view.bootstrapKeys = setupBootstrapControllerKeys{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Keys: []hostbootstrap.PublicKeyIdentity{
			requestFixture.Binding.ControllerKey,
		},
	}
	view.bootstrapCatalog = setupBootstrapCatalog{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Targets: []setupBootstrapCatalogTarget{{
			Target: requestFixture.Binding.Target,
			Roles: []hostbootstrap.Role{
				hostbootstrap.RoleDesktop,
				hostbootstrap.RoleHeadless,
			},
			Bootstrap: setupBootstrapCatalogArtifact{
				Identity: hostbootstrap.ArtifactIdentity{
					ID:      "nvpair-host-bootstrap",
					Version: "1.2.3",
					SHA256:  strings.Repeat("3", 64),
					Path:    "/usr/libexec/nvpair-host-bootstrap",
				},
				FileName:   "nvpair-host-bootstrap",
				Size:       1024,
				Provenance: "engineering",
			},
			Helper: setupBootstrapCatalogArtifact{
				Identity:   requestFixture.Binding.Helper,
				FileName:   "nvpair-host-helper",
				Size:       1024,
				Provenance: "engineering",
			},
			Product: setupBootstrapCatalogArtifact{
				Identity:   requestFixture.Binding.Product,
				FileName:   "nvpair-product.zip",
				Size:       2048,
				Provenance: "engineering",
			},
		}},
	}

	if command := view.bootstrapAction(); command == nil ||
		view.bootstrapRequest == nil ||
		view.pending != "bootstrap inspection" {
		t.Fatalf(
			"prepare/inspect state command=%v request=%#v pending=%q",
			command != nil,
			view.bootstrapRequest,
			view.pending,
		)
	}
	if view.bootstrapRequest.Binding.Target !=
		requestFixture.Binding.Target ||
		view.bootstrapRequest.Binding.Lane !=
			hostbootstrap.LaneQuickConnect ||
		view.bootstrapRequest.Binding.Role != hostbootstrap.RoleAuto ||
		view.bootstrapRequest.Binding.Account !=
			requestFixture.Binding.Account {
		t.Fatalf("request=%#v", view.bootstrapRequest)
	}

	view.pending = ""
	view.bootstrapStatus = &hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   view.bootstrapRequest.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       view.bootstrapRequest.Binding,
		Observed:      setupBootstrapAbsentObservations(),
	}
	if command := view.bootstrapAction(); command == nil ||
		view.pending != "bootstrap review" {
		t.Fatalf("review transition command=%v pending=%q", command != nil, view.pending)
	}

	plan, err := hostbootstrap.Reconcile(
		*view.bootstrapRequest,
		setupBootstrapAbsentObservations(),
	)
	if err != nil {
		t.Fatal(err)
	}
	view.pending = ""
	view.bootstrapPlan = &plan
	view.bootstrapStatus = nil
	if command := view.bootstrapAction(); command == nil ||
		view.pending != "bootstrap apply" {
		t.Fatalf("apply transition command=%v pending=%q", command != nil, view.pending)
	}

	view.pending = ""
	view.bootstrapStatus = &hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   plan.OperationID,
		Phase:         hostbootstrap.PhaseApply,
		Decision:      plan.Decision,
		Binding:       plan.Binding,
		Observed:      plan.Observed,
	}
	if command := view.bootstrapAction(); command == nil ||
		view.pending != "bootstrap verification" {
		t.Fatalf("verify transition command=%v pending=%q", command != nil, view.pending)
	}
}

func setupBootstrapTestRequest() hostbootstrap.Request {
	algorithm := []byte(hostbootstrap.PublicKeyAlgorithmED25519)
	blob := make([]byte, 4+len(algorithm)+4+32)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(algorithm)))
	copy(blob[4:], algorithm)
	offset := 4 + len(algorithm)
	binary.BigEndian.PutUint32(blob[offset:offset+4], 32)
	for index := offset + 4; index < len(blob); index++ {
		blob[index] = byte(index)
	}
	sum := sha256.Sum256(blob)
	return hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("a", 32),
		Binding: hostbootstrap.Binding{
			Target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureARM64,
			},
			Lane:         hostbootstrap.LaneQuickConnect,
			Role:         hostbootstrap.RoleAuto,
			RuntimeOwner: hostbootstrap.RoleHeadless,
			Account: hostbootstrap.AccountIdentity{
				Name:               "pairuser",
				HomePath:           "/home/pairuser",
				AuthorizedKeysPath: "/home/pairuser/.ssh/authorized_keys",
			},
			ControllerKey: hostbootstrap.PublicKeyIdentity{
				Algorithm:         hostbootstrap.PublicKeyAlgorithmED25519,
				Material:          base64.StdEncoding.EncodeToString(blob),
				FingerprintSHA256: hex.EncodeToString(sum[:]),
			},
			Endpoint: hostbootstrap.SSHEndpoint{
				Address: "192.0.2.10",
				Port:    22,
			},
			Product: hostbootstrap.ArtifactIdentity{
				ID:      "nvpair",
				Version: "1.2.3",
				SHA256:  strings.Repeat("1", 64),
				Path:    "/opt/nvpair/product",
			},
			Helper: hostbootstrap.ArtifactIdentity{
				ID:      "nvpair-host-helper",
				Version: "1.2.3",
				SHA256:  strings.Repeat("2", 64),
				Path:    "/usr/libexec/nvpair-host-helper",
			},
		},
	}
}

func setupBootstrapAbsentObservations() hostbootstrap.Observations {
	return hostbootstrap.Observations{
		SSHService:    hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Firewall:      hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Helper:        hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Product:       hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{},
	}
}

func setupBootstrapExactObservations(
	request hostbootstrap.Request,
) hostbootstrap.Observations {
	endpoint := request.Binding.Endpoint
	helper := request.Binding.Helper
	product := request.Binding.Product
	return hostbootstrap.Observations{
		SSHService: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		},
		Firewall: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity: &hostbootstrap.AuthorizedKeyIdentity{
				AccountName:       request.Binding.Account.Name,
				Path:              request.Binding.Account.AuthorizedKeysPath,
				FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
			},
		},
		Helper: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &helper,
		},
		Product: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &product,
		},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{{
			Ownership: hostbootstrap.OwnershipOwned,
			Owner:     request.Binding.RuntimeOwner,
		}},
	}
}
