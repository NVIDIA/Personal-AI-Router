// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/agent"
	"nvpair-shared/discovery"
	"nvpair-shared/hostbootstrap"
)

func TestBootstrapBackendUsesFixedHelperAndRejectsForgedObservations(t *testing.T) {
	request := bootstrapTestRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	status := bootstrapTestStatus(request, bootstrapAbsentObservations())
	service, commands, helperRequests := bootstrapServiceFixture(
		t,
		request,
		func(_ hostbootstrap.HelperRequest) hostbootstrap.HelperResponse {
			return bootstrapHelperStatusResponse(
				hostbootstrap.HelperActionInspect,
				status,
			)
		},
	)
	raw := bootstrapRequestParams(t, request)
	plan, err := service.bootstrapReview(context.Background(), raw)
	if err != nil || plan.Decision != hostbootstrap.DecisionApply {
		t.Fatalf("plan=%#v error=%v", plan, err)
	}
	if !reflect.DeepEqual(
		*commands,
		[]string{"/usr/libexec/nvpair-host-helper request"},
	) {
		t.Fatalf("commands=%v", *commands)
	}
	if len(*helperRequests) != 1 ||
		(*helperRequests)[0].Action != hostbootstrap.HelperActionInspect {
		t.Fatalf("helper requests=%#v", *helperRequests)
	}
	encoded, err := hostbootstrap.EncodeHelperRequest((*helperRequests)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range [][]byte{
		[]byte(`"request"`),
		[]byte(`"plan"`),
		[]byte(`"observed"`),
		[]byte(`"receipt"`),
	} {
		if bytes.Contains(encoded, prohibited) {
			t.Fatalf("helper request carried controller state: %s", encoded)
		}
	}

	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("test request is invalid")
	}
	envelope["observations"] = json.RawMessage(`{}`)
	forged, _ := json.Marshal(envelope)
	if _, err := service.bootstrapReview(
		context.Background(),
		forged,
	); err == nil {
		t.Fatal("controller observations were accepted")
	}

	forgedStatus := status
	forgedStatus.Binding.Endpoint.Address = "192.0.2.99"
	forgedStatus.Observed = bootstrapAbsentObservations()
	*commands = nil
	*helperRequests = nil
	service.dial = bootstrapDialFixture(
		t,
		commands,
		helperRequests,
		func(_ hostbootstrap.HelperRequest) hostbootstrap.HelperResponse {
			return bootstrapHelperStatusResponse(
				hostbootstrap.HelperActionInspect,
				forgedStatus,
			)
		},
	)
	if _, err := service.bootstrapInspect(
		context.Background(),
		raw,
	); err == nil {
		t.Fatal("forged target observations were accepted")
	}
}

func TestBootstrapBackendBindsHostKeyAccessAndAccount(t *testing.T) {
	request := bootstrapTestRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	service, commands, _ := bootstrapServiceFixture(
		t,
		request,
		func(helperRequest hostbootstrap.HelperRequest) hostbootstrap.HelperResponse {
			return bootstrapHelperStatusResponse(
				helperRequest.Action,
				bootstrapTestStatus(
					request,
					bootstrapAbsentObservations(),
				),
			)
		},
	)
	var values map[string]json.RawMessage
	raw := bootstrapRequestParams(t, request)
	if json.Unmarshal(raw, &values) != nil {
		t.Fatal("test request is invalid")
	}
	for _, test := range []struct {
		name  string
		field string
		value string
	}{
		{name: "candidate", field: "candidateId", value: strings.Repeat("c", 32)},
		{name: "access", field: "accessId", value: strings.Repeat("d", 32)},
		{name: "host-key", field: "hostKeySha256", value: "SHA256:changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := make(map[string]json.RawMessage, len(values))
			for field, value := range values {
				mutated[field] = value
			}
			mutated[test.field], _ = json.Marshal(test.value)
			params, _ := json.Marshal(mutated)
			if _, err := service.bootstrapInspect(
				context.Background(),
				params,
			); err == nil {
				t.Fatal("changed target binding was accepted")
			}
		})
	}
	changedAccount := request
	changedAccount.Binding.Account.Name = "other"
	if _, err := service.bootstrapInspect(
		context.Background(),
		bootstrapRequestParams(t, changedAccount),
	); err == nil {
		t.Fatal("request account was not bound to volatile access")
	}
	if len(*commands) != 0 {
		t.Fatalf("invalid binding reached SSH: %v", *commands)
	}
}

func TestBootstrapBackendApplyStatusRecoveryAndVerifyUseTypedTargetResults(t *testing.T) {
	request := bootstrapTestRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	observed := bootstrapAbsentObservations()
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	exact := bootstrapExactObservations(request)
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	service, _, helperRequests := bootstrapServiceFixture(
		t,
		request,
		func(helperRequest hostbootstrap.HelperRequest) hostbootstrap.HelperResponse {
			switch helperRequest.Action {
			case hostbootstrap.HelperActionInspect:
				status := bootstrapTestStatus(request, observed)
				return bootstrapHelperStatusResponse(
					helperRequest.Action,
					status,
				)
			case hostbootstrap.HelperActionApply:
				status := hostbootstrap.Status{
					SchemaVersion: hostbootstrap.SchemaVersion,
					OperationID:   request.OperationID,
					Phase:         hostbootstrap.PhaseApply,
					Decision:      plan.Decision,
					Binding:       request.Binding,
					Observed:      plan.Observed,
				}
				return bootstrapHelperStatusResponse(
					helperRequest.Action,
					status,
				)
			case hostbootstrap.HelperActionVerify:
				return hostbootstrap.HelperResponse{
					SchemaVersion: hostbootstrap.SchemaVersion,
					OperationID:   request.OperationID,
					Action:        helperRequest.Action,
					Accepted:      true,
					Receipt:       &receipt,
				}
			default:
				t.Fatalf("unexpected helper action %q", helperRequest.Action)
				return hostbootstrap.HelperResponse{}
			}
		},
	)
	if _, err := service.bootstrapApply(
		context.Background(),
		bootstrapPlanParams(t, plan),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.bootstrapStatus(
		context.Background(),
		bootstrapOperationParams(t, request.OperationID),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.bootstrapRecover(
		context.Background(),
		bootstrapOperationParams(t, request.OperationID),
	); err != nil {
		t.Fatal(err)
	}
	gotReceipt, err := service.bootstrapVerify(
		context.Background(),
		bootstrapPlanParams(t, plan),
	)
	if err != nil || !reflect.DeepEqual(gotReceipt, receipt) {
		t.Fatalf("receipt=%#v error=%v", gotReceipt, err)
	}
	var actions []hostbootstrap.HelperAction
	for _, helperRequest := range *helperRequests {
		actions = append(actions, helperRequest.Action)
	}
	if !reflect.DeepEqual(actions, []hostbootstrap.HelperAction{
		hostbootstrap.HelperActionApply,
		hostbootstrap.HelperActionInspect,
		hostbootstrap.HelperActionApply,
		hostbootstrap.HelperActionVerify,
	}) {
		t.Fatalf("actions=%v", actions)
	}
}

func TestBootstrapCatalogHasExactlySixCanonicalTargets(t *testing.T) {
	raw, err := json.Marshal(bootstrapCatalogTestFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte(`"integrity"`),
		[]byte(`"combination"`),
		[]byte(`"signature"`),
	} {
		if !bytes.Contains(raw, required) {
			t.Fatalf("catalog omits final packaging metadata %s", required)
		}
	}
	canonical := append([]byte(nil), raw...)
	catalog, err := parseBootstrapCatalog(raw)
	if err != nil || len(catalog.Targets) != 6 {
		t.Fatalf("catalog targets=%d error=%v", len(catalog.Targets), err)
	}
	distinctVersions := bootstrapCatalogTestFixture()
	for index := range distinctVersions.Targets {
		distinctVersions.Targets[index].Bootstrap.Identity.Version =
			"0.1.0"
		distinctVersions.Targets[index].Helper.Identity.Version =
			"0.2.0"
		distinctVersions.Targets[index].Product.Identity.Version =
			"1.2.3"
	}
	raw, _ = json.Marshal(distinctVersions)
	if _, err := parseBootstrapCatalog(raw); err != nil {
		t.Fatalf("distinct component versions rejected: %v", err)
	}
	short := catalog
	short.Targets = short.Targets[:5]
	raw, _ = json.Marshal(short)
	if _, err := parseBootstrapCatalog(raw); err == nil {
		t.Fatal("short catalog accepted")
	}
	duplicate := catalog
	duplicate.Targets[5] = duplicate.Targets[0]
	raw, _ = json.Marshal(duplicate)
	if _, err := parseBootstrapCatalog(raw); err == nil {
		t.Fatal("duplicate target accepted")
	}
	for _, malformed := range [][]byte{
		bytes.Replace(
			canonical,
			[]byte(`"schemaVersion":1`),
			[]byte(`"schemaVersion":1,"extra":true`),
			1,
		),
		bytes.Replace(
			canonical,
			[]byte(`"schemaVersion":1`),
			[]byte(`"SchemaVersion":1`),
			1,
		),
		bytes.Replace(
			canonical,
			[]byte(`"schemaVersion":1`),
			[]byte(`"schemaVersion":1,"schemaVersion":1`),
			1,
		),
	} {
		if _, err := parseBootstrapCatalog(malformed); err == nil {
			t.Fatalf("noncanonical catalog accepted: %s", malformed)
		}
	}
	if _, _, err := onboardingPlatform("Fedora", "amd64"); err == nil {
		t.Fatal("Fedora was guessed as a supported bootstrap target")
	}
}

func TestBootstrapCatalogUsesPackagedCanonicalResourcePath(
	t *testing.T,
) {
	executable := filepath.Join(
		t.TempDir(),
		"resources",
		"cli-bin",
		"nvpair-engine-manager.exe",
	)
	got, err := bootstrapCatalogResourcePath(executable)
	want := filepath.Join(
		filepath.Dir(filepath.Dir(executable)),
		bootstrapCatalogResourceDirectory,
		bootstrapCatalogFile,
	)
	if err != nil || got != want {
		t.Fatalf("catalog path=%q want=%q error=%v", got, want, err)
	}
}

func TestBootstrapCatalogRPCReturnsOnlySignedPublicMetadata(t *testing.T) {
	catalog := bootstrapCatalogTestFixture()
	service := &onboardingService{
		loadBootstrapCatalog: func() (bootstrapCatalog, error) {
			return catalog, nil
		},
	}
	result, err := service.bootstrapCatalogMetadata()
	if err != nil || !reflect.DeepEqual(result, catalog) {
		t.Fatalf("catalog=%#v error=%v", result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range [][]byte{
		[]byte(`private`),
		[]byte(`secret`),
		[]byte(`material`),
	} {
		if bytes.Contains(bytes.ToLower(raw), prohibited) {
			t.Fatalf("catalog leaked non-artifact data: %s", raw)
		}
	}
}

func TestBootstrapControllerKeysReadAgentAndConfiguredPublicFilesOnly(t *testing.T) {
	public := bootstrapTestPublicKey()
	blob, err := base64.StdEncoding.DecodeString(public.Material)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join("fixture", "home")
	config := filepath.Join(home, ".ssh", "config")
	publicPath := filepath.Join(home, ".ssh", "id_fixture.pub")
	reads := []string{}
	source := bootstrapControllerKeySource{
		homeDir: func() (string, error) { return home, nil },
		agentKeys: func(context.Context) ([]*agent.Key, error) {
			return []*agent.Key{{
				Format:  string(public.Algorithm),
				Blob:    blob,
				Comment: "agent fixture",
			}}, nil
		},
		readFile: func(path string) ([]byte, error) {
			reads = append(reads, path)
			switch path {
			case config:
				return []byte("Host *\n  IdentityFile ~/.ssh/id_fixture\n"), nil
			case publicPath:
				return []byte(
					string(public.Algorithm) + " " +
						public.Material + " configured fixture\n",
				), nil
			default:
				return nil, os.ErrNotExist
			}
		},
	}
	keys, err := readBootstrapControllerKeys(
		context.Background(),
		source,
	)
	if err != nil || len(keys) != 1 || keys[0] != public {
		t.Fatalf("keys=%#v error=%v", keys, err)
	}
	for _, path := range reads {
		if path == filepath.Join(home, ".ssh", "id_fixture") {
			t.Fatalf("private key path was read: %v", reads)
		}
	}
}

func TestManualCandidateDoesNotInferBootstrapStateFromSSHPort(t *testing.T) {
	service := &onboardingService{
		targets: map[string]*onboardingPrivateTarget{},
	}
	candidate, err := service.addTarget(onboardingAddTargetRequest{
		Address: "192.0.2.10",
		Port:    22,
		Label:   "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.BootstrapSource != "manual" ||
		candidate.BootstrapState != "bootstrap-required" {
		t.Fatalf("candidate=%#v", candidate)
	}
}

func TestPairedServingNodeIsExcludedFromSetupCandidates(t *testing.T) {
	_, mesh, _ := remoteActionPin(t)
	var output bytes.Buffer
	manager := NewManager(
		NewCodec(&output),
		newTestExecutor(t, testEngineManifest(fakeEngineBin)),
		mesh,
	)
	manager.peers.peers["paired-node"] = ecPeer{
		nodeID:      "paired-node",
		addresses:   []string{"192.0.2.20"},
		port:        14323,
		clusterUUID: "fixture-peer",
	}
	manager.onboarding.discover = func(context.Context) []discovery.Node {
		return []discovery.Node{{
			Host:      "paired.local.",
			Port:      22,
			Addresses: []string{"192.0.2.20"},
		}}
	}
	result, err := manager.onboarding.candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	values, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("candidates result=%#v", result)
	}
	candidates, ok := values["candidates"].([]onboardingCandidate)
	if !ok || len(candidates) != 0 {
		t.Fatalf("paired node entered setup candidates: %#v", values)
	}
}

func TestBootstrapHelperCommandsAreFixedPerPlatform(t *testing.T) {
	tests := []struct {
		platform hostbootstrap.Platform
		command  string
	}{
		{
			platform: hostbootstrap.PlatformWindows,
			command:  `"C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe" request`,
		},
		{
			platform: hostbootstrap.PlatformDarwin,
			command:  "/Library/PrivilegedHelperTools/nvpair-host-helper request",
		},
		{
			platform: hostbootstrap.PlatformLinux,
			command:  "/usr/libexec/nvpair-host-helper request",
		},
	}
	for _, test := range tests {
		command, err := fixedBootstrapHelperCommand(test.platform)
		if err != nil || command != test.command {
			t.Fatalf(
				"platform=%s command=%q error=%v",
				test.platform,
				command,
				err,
			)
		}
	}
}

func bootstrapServiceFixture(
	t *testing.T,
	request hostbootstrap.Request,
	response func(hostbootstrap.HelperRequest) hostbootstrap.HelperResponse,
) (*onboardingService, *[]string, *[]hostbootstrap.HelperRequest) {
	t.Helper()
	candidateID := strings.Repeat("a", 32)
	accessID := strings.Repeat("b", 32)
	hostKey := "SHA256:reviewed-host-key"
	commands := &[]string{}
	helperRequests := &[]hostbootstrap.HelperRequest{}
	service := &onboardingService{
		targets: map[string]*onboardingPrivateTarget{
			candidateID: {
				candidate: onboardingCandidate{
					CandidateID:     candidateID,
					Label:           "target",
					Address:         request.Binding.Endpoint.Address,
					Port:            request.Binding.Endpoint.Port,
					AccessID:        accessID,
					AccessAvailable: true,
					HostKeySHA256:   hostKey,
					HostKeyTrusted:  true,
					BootstrapSource: "manual",
					BootstrapState:  "ssh-ready",
				},
				access: onboardingAccess{
					user:     request.Binding.Account.Name,
					password: "volatile-only",
				},
				expiresAt: time.Now().Add(time.Minute),
			},
		},
	}
	service.loadBootstrapCatalog = func() (bootstrapCatalog, error) {
		return bootstrapCatalogTestFixture(), nil
	}
	service.dial = bootstrapDialFixture(
		t,
		commands,
		helperRequests,
		response,
	)
	return service, commands, helperRequests
}

func bootstrapDialFixture(
	t *testing.T,
	commands *[]string,
	helperRequests *[]hostbootstrap.HelperRequest,
	response func(hostbootstrap.HelperRequest) hostbootstrap.HelperResponse,
) func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
	t.Helper()
	return func(
		_ context.Context,
		_ onboardingCandidate,
		_ onboardingAccess,
	) (*onboardingSSH, error) {
		return &onboardingSSH{testRun: func(
			_ context.Context,
			command string,
			input io.Reader,
		) ([]byte, error) {
			*commands = append(*commands, command)
			raw, err := io.ReadAll(input)
			if err != nil {
				return nil, err
			}
			helperRequest, err := hostbootstrap.DecodeHelperRequest(raw)
			if err != nil {
				return nil, err
			}
			*helperRequests = append(*helperRequests, helperRequest)
			return hostbootstrap.EncodeHelperResponse(response(helperRequest))
		}}, nil
	}
}

func bootstrapRequestParams(
	t *testing.T,
	request hostbootstrap.Request,
) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		CandidateID string                `json:"candidateId"`
		AccessID    string                `json:"accessId"`
		HostKey     string                `json:"hostKeySha256"`
		Request     hostbootstrap.Request `json:"request"`
	}{
		CandidateID: strings.Repeat("a", 32),
		AccessID:    strings.Repeat("b", 32),
		HostKey:     "SHA256:reviewed-host-key",
		Request:     request,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bootstrapPlanParams(
	t *testing.T,
	plan hostbootstrap.Plan,
) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		CandidateID string             `json:"candidateId"`
		AccessID    string             `json:"accessId"`
		HostKey     string             `json:"hostKeySha256"`
		Plan        hostbootstrap.Plan `json:"plan"`
	}{
		CandidateID: strings.Repeat("a", 32),
		AccessID:    strings.Repeat("b", 32),
		HostKey:     "SHA256:reviewed-host-key",
		Plan:        plan,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bootstrapOperationParams(
	t *testing.T,
	operationID string,
) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		CandidateID string `json:"candidateId"`
		AccessID    string `json:"accessId"`
		HostKey     string `json:"hostKeySha256"`
		OperationID string `json:"operationId"`
	}{
		CandidateID: strings.Repeat("a", 32),
		AccessID:    strings.Repeat("b", 32),
		HostKey:     "SHA256:reviewed-host-key",
		OperationID: operationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bootstrapHelperStatusResponse(
	action hostbootstrap.HelperAction,
	status hostbootstrap.Status,
) hostbootstrap.HelperResponse {
	return hostbootstrap.HelperResponse{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   status.OperationID,
		Action:        action,
		Accepted:      true,
		Status:        &status,
	}
}

func bootstrapTestRequest(target hostbootstrap.Target) hostbootstrap.Request {
	account := hostbootstrap.AccountIdentity{Name: "pairuser"}
	product := hostbootstrap.ArtifactIdentity{
		ID:      "nvpair",
		Version: "1.2.3",
		SHA256:  strings.Repeat("12", 32),
	}
	helper := hostbootstrap.ArtifactIdentity{
		ID:      "nvpair-host-helper",
		Version: "1.2.3",
		SHA256:  strings.Repeat("34", 32),
	}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		account.HomePath = `C:\Users\pairuser`
		account.AuthorizedKeysPath =
			`C:\Users\pairuser\.ssh\authorized_keys`
		product.Path =
			`C:\Program Files\NVIDIA Corporation\PAIR\product`
		helper.Path =
			`C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
	case hostbootstrap.PlatformDarwin:
		account.HomePath = "/Users/pairuser"
		account.AuthorizedKeysPath =
			"/Users/pairuser/.ssh/authorized_keys"
		product.Path = "/Applications/NVPAIR.app"
		helper.Path =
			"/Library/PrivilegedHelperTools/nvpair-host-helper"
	case hostbootstrap.PlatformLinux:
		account.HomePath = "/home/pairuser"
		account.AuthorizedKeysPath =
			"/home/pairuser/.ssh/authorized_keys"
		product.Path = "/opt/nvpair/product"
		helper.Path = "/usr/libexec/nvpair-host-helper"
	}
	return hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("c", 32),
		Binding: hostbootstrap.Binding{
			Target:        target,
			Lane:          hostbootstrap.LaneQuickConnect,
			Role:          hostbootstrap.RoleHeadless,
			RuntimeOwner:  hostbootstrap.RoleHeadless,
			Account:       account,
			ControllerKey: bootstrapTestPublicKey(),
			Endpoint: hostbootstrap.SSHEndpoint{
				Address: "192.0.2.10",
				Port:    22,
			},
			Product: product,
			Helper:  helper,
		},
	}
}

func bootstrapTestPublicKey() hostbootstrap.PublicKeyIdentity {
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
	return hostbootstrap.PublicKeyIdentity{
		Algorithm:         hostbootstrap.PublicKeyAlgorithmED25519,
		Material:          base64.StdEncoding.EncodeToString(blob),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}
}

func bootstrapAbsentObservations() hostbootstrap.Observations {
	return hostbootstrap.Observations{
		SSHService: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		},
		Firewall: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		},
		Helper: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		},
		Product: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{},
	}
}

func bootstrapExactObservations(
	request hostbootstrap.Request,
) hostbootstrap.Observations {
	endpoint := request.Binding.Endpoint
	key := hostbootstrap.AuthorizedKeyIdentity{
		AccountName:       request.Binding.Account.Name,
		Path:              request.Binding.Account.AuthorizedKeysPath,
		FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
	}
	helper := request.Binding.Helper
	product := request.Binding.Product
	firewall := hostbootstrap.EndpointObservation{
		Ownership: hostbootstrap.OwnershipOwned,
		Endpoint:  &endpoint,
	}
	if request.Binding.Target.Platform == hostbootstrap.PlatformDarwin {
		firewall = hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipNotApplicable,
		}
	}
	return hostbootstrap.Observations{
		SSHService: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		},
		Firewall: firewall,
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &key,
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

func bootstrapTestStatus(
	request hostbootstrap.Request,
	observed hostbootstrap.Observations,
) hostbootstrap.Status {
	return hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       request.Binding,
		Observed:      observed,
	}
}

func bootstrapCatalogTestFixture() bootstrapCatalog {
	catalog := bootstrapCatalog{
		SchemaVersion: hostbootstrap.SchemaVersion,
		Integrity: bootstrapCatalogIntegrity{
			ChecksumAlgorithm: "sha256",
			ChecksumFile:      bootstrapCatalogChecksumFile,
			Signature: bootstrapCatalogSignature{
				Status:       "unsigned",
				Kind:         "none",
				ChecksumFile: bootstrapCatalogChecksumFile,
			},
		},
		Targets: []bootstrapCatalogTarget{},
	}
	for _, target := range hostbootstrap.SupportedTargets() {
		request := bootstrapTestRequest(target)
		bootstrapPath := "/usr/libexec/nvpair-host-bootstrap"
		bootstrapFile := "nvpair-host-bootstrap"
		helperFile := "nvpair-host-helper"
		if target.Platform == hostbootstrap.PlatformWindows {
			bootstrapPath =
				`C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-bootstrap.exe`
			bootstrapFile = "nvpair-host-bootstrap.exe"
			helperFile = "nvpair-host-helper.exe"
		} else if target.Platform == hostbootstrap.PlatformDarwin {
			bootstrapPath =
				"/Library/PrivilegedHelperTools/nvpair-host-bootstrap"
		}
		catalog.Targets = append(catalog.Targets, bootstrapCatalogTarget{
			Target: target,
			Roles: []hostbootstrap.Role{
				hostbootstrap.RoleDesktop,
				hostbootstrap.RoleHeadless,
			},
			Bootstrap: bootstrapCatalogArtifact{
				Identity: hostbootstrap.ArtifactIdentity{
					ID:      "nvpair-host-bootstrap",
					Version: "1.2.3",
					SHA256:  strings.Repeat("56", 32),
					Path:    bootstrapPath,
				},
				FileName:   bootstrapFile,
				Size:       1024,
				Provenance: "engineering",
				Signature: bootstrapCatalogTestSignature(
					strings.Repeat("56", 32),
					1024,
				),
			},
			Helper: bootstrapCatalogArtifact{
				Identity:   request.Binding.Helper,
				FileName:   helperFile,
				Size:       1024,
				Provenance: "engineering",
				Signature: bootstrapCatalogTestSignature(
					request.Binding.Helper.SHA256,
					1024,
				),
			},
			Product: bootstrapCatalogArtifact{
				Identity:   request.Binding.Product,
				FileName:   "nvpair-product.zip",
				Size:       4096,
				Provenance: "engineering",
				Signature: bootstrapCatalogTestSignature(
					strings.Repeat("78", 32),
					8192,
				),
			},
			Combination: bootstrapCatalogCombination{
				FileName: "nvpair-bootstrap-" +
					string(target.Platform) + "-" +
					string(target.Architecture) + ".zip",
				Size:       16384,
				SHA256:     strings.Repeat("90", 32),
				Provenance: "engineering",
				Signature: bootstrapCatalogTestSignature(
					strings.Repeat("90", 32),
					16384,
				),
			},
		})
	}
	return catalog
}

func bootstrapCatalogTestSignature(
	digest string,
	size int64,
) bootstrapCatalogSignature {
	return bootstrapCatalogSignature{
		Status:        "unsigned",
		Kind:          "none",
		ContentSHA256: digest,
		ContentSize:   size,
	}
}
