// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func contractPublicKey() PublicKeyIdentity {
	algorithm := []byte(PublicKeyAlgorithmED25519)
	blob := make([]byte, 4+len(algorithm)+4+32)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(algorithm)))
	copy(blob[4:], algorithm)
	keyOffset := 4 + len(algorithm)
	binary.BigEndian.PutUint32(blob[keyOffset:keyOffset+4], 32)
	for index := keyOffset + 4; index < len(blob); index++ {
		blob[index] = byte(index)
	}
	sum := sha256.Sum256(blob)
	return PublicKeyIdentity{
		Algorithm:         PublicKeyAlgorithmED25519,
		Material:          base64.StdEncoding.EncodeToString(blob),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}
}

func contractRequest(target Target, lane Lane, role Role, owner Role) Request {
	account := AccountIdentity{Name: "pairuser"}
	product := ArtifactIdentity{
		ID:      "nvpair",
		Version: "1.2.3",
		SHA256:  strings.Repeat("12", 32),
	}
	helper := ArtifactIdentity{
		ID:      "nvpair-host-helper",
		Version: "1.2.3",
		SHA256:  strings.Repeat("34", 32),
	}
	switch target.Platform {
	case PlatformWindows:
		account.HomePath = `C:\Users\pairuser`
		account.AuthorizedKeysPath = `C:\Users\pairuser\.ssh\authorized_keys`
		product.Path = `C:\Program Files\NVIDIA Corporation\PAIR\nvpair.exe`
		helper.Path = `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
	case PlatformDarwin:
		account.HomePath = "/Users/pairuser"
		account.AuthorizedKeysPath = "/Users/pairuser/.ssh/authorized_keys"
		product.Path = "/Applications/NVPAIR.app"
		helper.Path = "/Library/PrivilegedHelperTools/nvpair-host-helper"
	case PlatformLinux:
		account.HomePath = "/home/pairuser"
		account.AuthorizedKeysPath = "/home/pairuser/.ssh/authorized_keys"
		product.Path = "/opt/nvpair/nvpair"
		helper.Path = "/usr/libexec/nvpair-host-helper"
	}
	return Request{
		SchemaVersion: SchemaVersion,
		OperationID:   strings.Repeat("ab", 16),
		Binding: Binding{
			Target:        target,
			Lane:          lane,
			Role:          role,
			RuntimeOwner:  owner,
			Account:       account,
			ControllerKey: contractPublicKey(),
			Endpoint:      SSHEndpoint{Address: "192.0.2.10", Port: 22},
			Product:       product,
			Helper:        helper,
		},
	}
}

func absentObservations() Observations {
	return Observations{
		SSHService:    EndpointObservation{Ownership: OwnershipAbsent},
		Firewall:      EndpointObservation{Ownership: OwnershipAbsent},
		AuthorizedKey: AuthorizedKeyObservation{Ownership: OwnershipAbsent},
		Product:       ArtifactObservation{Ownership: OwnershipAbsent},
		Helper:        ArtifactObservation{Ownership: OwnershipAbsent},
		RuntimeOwners: []RuntimeOwnerObservation{},
	}
}

func exactObservations(request Request) Observations {
	endpoint := request.Binding.Endpoint
	product := request.Binding.Product
	helper := request.Binding.Helper
	observed := Observations{
		SSHService: EndpointObservation{
			Ownership: OwnershipOwned,
			Endpoint:  &endpoint,
		},
		Firewall: EndpointObservation{
			Ownership: OwnershipOwned,
			Endpoint:  &endpoint,
		},
		AuthorizedKey: AuthorizedKeyObservation{
			Ownership: OwnershipOwned,
			Identity: &AuthorizedKeyIdentity{
				AccountName:      request.Binding.Account.Name,
				Path:             request.Binding.Account.AuthorizedKeysPath,
				FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
			},
		},
		Product: ArtifactObservation{
			Ownership: OwnershipOwned,
			Identity:  &product,
		},
		Helper: ArtifactObservation{
			Ownership: OwnershipOwned,
			Identity:  &helper,
		},
		RuntimeOwners: []RuntimeOwnerObservation{{
			Ownership: OwnershipOwned,
			Owner:     request.Binding.RuntimeOwner,
		}},
	}
	if request.Binding.Target.Platform == PlatformDarwin {
		observed.Firewall = EndpointObservation{Ownership: OwnershipNotApplicable}
	}
	return observed
}

func TestClosedTargetMatrix(t *testing.T) {
	want := []Target{
		{Platform: PlatformWindows, Architecture: ArchitectureAMD64},
		{Platform: PlatformWindows, Architecture: ArchitectureARM64},
		{Platform: PlatformDarwin, Architecture: ArchitectureAMD64},
		{Platform: PlatformDarwin, Architecture: ArchitectureARM64},
		{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		{Platform: PlatformLinux, Architecture: ArchitectureARM64},
	}
	if got := SupportedTargets(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedTargets() = %#v, want %#v", got, want)
	}
	for _, target := range want {
		request := contractRequest(target, LaneQuickConnect, RoleAuto, RoleDesktop)
		if err := request.Validate(); err != nil {
			t.Fatalf("target %+v: %v", target, err)
		}
		observed := absentObservations()
		if target.Platform == PlatformDarwin {
			observed.Firewall = EndpointObservation{Ownership: OwnershipNotApplicable}
		}
		plan, err := Reconcile(request, observed)
		if err != nil || plan.Decision != DecisionApply {
			t.Fatalf("target %+v plan = %+v, %v", target, plan, err)
		}
	}
	unsupported := contractRequest(Target{Platform: Platform("freebsd"), Architecture: ArchitectureAMD64}, LaneQuickConnect, RoleAuto, RoleDesktop)
	if err := unsupported.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestReconcileSupportsBothLanesAndAllDecisions(t *testing.T) {
	for _, lane := range []Lane{LaneQuickConnect, LaneZeroTouch} {
		request := contractRequest(
			Target{Platform: PlatformLinux, Architecture: ArchitectureARM64},
			lane,
			RoleHeadless,
			RoleHeadless,
		)
		observed := absentObservations()
		plan, err := Reconcile(request, observed)
		if err != nil {
			t.Fatalf("lane %q: %v", lane, err)
		}
		if plan.Phase != PhaseReview || plan.Decision != DecisionApply || len(plan.Actions) != 6 {
			t.Fatalf("lane %q first plan = %+v", lane, plan)
		}
		if err := plan.Validate(); err != nil {
			t.Fatalf("lane %q plan validation: %v", lane, err)
		}

		exact := exactObservations(request)
		plan, err = Reconcile(request, exact)
		if err != nil || plan.Decision != DecisionNoOp || len(plan.Actions) != 0 {
			t.Fatalf("lane %q no-op plan = %+v, %v", lane, plan, err)
		}

		repair := exactObservations(request)
		repair.Helper.Identity.SHA256 = strings.Repeat("56", 32)
		plan, err = Reconcile(request, repair)
		if err != nil || plan.Decision != DecisionRepairOwned || !reflect.DeepEqual(plan.Actions, []ResourceKind{ResourceHelper}) {
			t.Fatalf("lane %q repair plan = %+v, %v", lane, plan, err)
		}

		foreign := absentObservations()
		endpoint := request.Binding.Endpoint
		foreign.Firewall = EndpointObservation{Ownership: OwnershipForeign, Endpoint: &endpoint}
		plan, err = Reconcile(request, foreign)
		if err != nil || plan.Phase != PhaseReview || plan.Decision != DecisionRefuseForeign || len(plan.Actions) != 0 {
			t.Fatalf("lane %q refusal plan = %+v, %v", lane, plan, err)
		}
	}
}

func TestRequestRequiresOneConsistentRuntimeOwner(t *testing.T) {
	target := Target{Platform: PlatformDarwin, Architecture: ArchitectureARM64}
	request := contractRequest(target, LaneQuickConnect, RoleDesktop, RoleHeadless)
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("role/owner mismatch error = %v", err)
	}
	request.Binding.Role = RoleAuto
	request.Binding.RuntimeOwner = ""
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty runtime owner error = %v", err)
	}

	request = contractRequest(target, LaneQuickConnect, RoleAuto, RoleDesktop)
	observed := exactObservations(request)
	observed.RuntimeOwners = append(observed.RuntimeOwners, RuntimeOwnerObservation{
		Ownership: OwnershipOwned,
		Owner:     RoleHeadless,
	})
	if _, err := Reconcile(request, observed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dual-owner error = %v", err)
	}
	observed.RuntimeOwners[1].Owner = RoleDesktop
	if _, err := Reconcile(request, observed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate-owner error = %v", err)
	}
}

func TestTransitionValidatorRequiresReviewAndTerminalPhases(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformWindows, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleDesktop,
		RoleDesktop,
	)
	absent := absentObservations()
	exact := exactObservations(request)
	inspect := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         PhaseInspect,
		Binding:       request.Binding,
		Observed:      absent,
	}
	review := inspect
	review.Phase = PhaseReview
	review.Decision = DecisionApply
	apply := review
	apply.Phase = PhaseApply
	verify := apply
	verify.Phase = PhaseVerify
	verify.Observed = exact
	complete := verify
	complete.Phase = PhaseComplete

	for _, transition := range [][2]Status{{inspect, review}, {review, apply}, {apply, verify}, {verify, complete}} {
		if err := ValidateTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("%s -> %s: %v", transition[0].Phase, transition[1].Phase, err)
		}
	}
	if err := ValidateTransition(inspect, apply); !errors.Is(err, ErrTransition) {
		t.Fatalf("applied before review: %v", err)
	}
	if err := ValidateTransition(inspect, verify); !errors.Is(err, ErrTransition) {
		t.Fatalf("skipped review: %v", err)
	}
	if err := ValidateTransition(review, verify); !errors.Is(err, ErrTransition) {
		t.Fatalf("skipped apply for mutating decision: %v", err)
	}

	blocked := inspect
	blocked.Phase = PhaseBlocked
	blocked.Decision = DecisionRefuseForeign
	blocked.Observed.Firewall.Ownership = OwnershipForeign
	blocked.Observed.Firewall.Endpoint = &request.Binding.Endpoint
	if err := ValidateTransition(inspect, blocked); !errors.Is(err, ErrTransition) {
		t.Fatalf("inspect skipped review: %v", err)
	}
	foreignInspect := blocked
	foreignInspect.Phase = PhaseInspect
	foreignInspect.Decision = ""
	foreignReview := blocked
	foreignReview.Phase = PhaseReview
	if err := ValidateTransition(foreignInspect, foreignReview); err != nil {
		t.Fatalf("foreign inspect -> review: %v", err)
	}
	if err := ValidateTransition(foreignReview, blocked); err != nil {
		t.Fatalf("foreign review -> blocked: %v", err)
	}
	if err := ValidateTransition(blocked, inspect); !errors.Is(err, ErrTransition) {
		t.Fatalf("left blocked: %v", err)
	}
	if err := ValidateTransition(complete, inspect); !errors.Is(err, ErrTransition) {
		t.Fatalf("left complete: %v", err)
	}
	mutatedComplete := complete
	mutatedComplete.Binding.Endpoint.Port = 2222
	if err := ValidateTransition(complete, mutatedComplete); !errors.Is(err, ErrTransition) {
		t.Fatalf("mutated complete status: %v", err)
	}
}

func TestNoOpTransitionsFromReviewDirectlyToVerify(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneZeroTouch,
		RoleHeadless,
		RoleHeadless,
	)
	review := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         PhaseReview,
		Decision:      DecisionNoOp,
		Binding:       request.Binding,
		Observed:      exactObservations(request),
	}
	verify := review
	verify.Phase = PhaseVerify
	if err := ValidateTransition(review, verify); err != nil {
		t.Fatal(err)
	}
	apply := review
	apply.Phase = PhaseApply
	if err := ValidateTransition(review, apply); !errors.Is(err, ErrTransition) {
		t.Fatalf("no-op entered apply: %v", err)
	}
}

func TestReceiptAndStatusBindVerifiedContract(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformDarwin, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleAuto,
		RoleDesktop,
	)
	observed := exactObservations(request)
	plan, err := Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewReceipt(plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Phase != PhaseComplete || receipt.Binding != request.Binding || receipt.OperationID != request.OperationID {
		t.Fatalf("receipt lost binding: %+v", receipt)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	status := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         PhaseComplete,
		Decision:      DecisionNoOp,
		Binding:       request.Binding,
		Observed:      observed,
	}
	if err := status.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsUnknownDuplicateAndProhibitedFields(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformWindows, Architecture: ArchitectureARM64},
		LaneQuickConnect,
		RoleAuto,
		RoleHeadless,
	)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeRequest(raw); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	unknown := append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"extra":true}`)...)
	if _, err = DecodeRequest(unknown); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown field error = %v", err)
	}
	nestedUnknown := bytes.Replace(raw, []byte(`"port":22`), []byte(`"port":22,"zone":"lan"`), 1)
	if _, err = DecodeRequest(nestedUnknown); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nested unknown field error = %v", err)
	}
	duplicate := bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1)
	if _, err = DecodeRequest(duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate field error = %v", err)
	}
	invalidUTF8 := bytes.Replace(raw, []byte("NVIDIA Corporation"), append([]byte{0xff}, []byte("VIDIA Corporation")...), 1)
	if _, err = DecodeRequest(invalidUTF8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	for _, field := range []string{
		`"password":"not-allowed"`,
		`"token":"not-allowed"`,
		`"privateKey":"-----BEGIN OPENSSH PRIVATE KEY-----"`,
		`"command":"rm -rf /"`,
		`"argv":["sh","-c","id"]`,
	} {
		payload := append(append([]byte{}, raw[:len(raw)-1]...), []byte(","+field+"}")...)
		if _, err = DecodeRequest(payload); !errors.Is(err, ErrProhibited) {
			t.Fatalf("%s error = %v", field, err)
		}
	}
	if _, err = DecodeRequest(bytes.Repeat([]byte(" "), maxPayloadBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize payload error = %v", err)
	}
}

func TestStrictDecodersCoverPlanReceiptAndStatus(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureARM64},
		LaneZeroTouch,
		RoleHeadless,
		RoleHeadless,
	)
	observed := exactObservations(request)
	plan, err := Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewReceipt(plan, observed)
	if err != nil {
		t.Fatal(err)
	}
	status := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         PhaseComplete,
		Decision:      DecisionNoOp,
		Binding:       request.Binding,
		Observed:      observed,
	}
	cases := []struct {
		name   string
		value  interface{}
		decode func([]byte) error
	}{
		{name: "plan", value: plan, decode: func(raw []byte) error {
			_, decodeErr := DecodePlan(raw)
			return decodeErr
		}},
		{name: "receipt", value: receipt, decode: func(raw []byte) error {
			_, decodeErr := DecodeReceipt(raw)
			return decodeErr
		}},
		{name: "status", value: status, decode: func(raw []byte) error {
			_, decodeErr := DecodeStatus(raw)
			return decodeErr
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw, marshalErr := json.Marshal(testCase.value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if decodeErr := testCase.decode(raw); decodeErr != nil {
				t.Fatalf("valid payload: %v", decodeErr)
			}
			unknown := append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"extra":true}`)...)
			if decodeErr := testCase.decode(unknown); !errors.Is(decodeErr, ErrInvalid) {
				t.Fatalf("unknown field error = %v", decodeErr)
			}
		})
	}
}

func TestValidationRejectsMalformedIdentitiesAndNoncanonicalValues(t *testing.T) {
	target := Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64}
	valid := contractRequest(target, LaneQuickConnect, RoleHeadless, RoleHeadless)
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "operation id", mutate: func(request *Request) {
			request.OperationID = strings.Repeat("AB", 16)
		}},
		{name: "address", mutate: func(request *Request) {
			request.Binding.Endpoint.Address = "EXAMPLE.COM"
		}},
		{name: "port zero", mutate: func(request *Request) {
			request.Binding.Endpoint.Port = 0
		}},
		{name: "port too high", mutate: func(request *Request) {
			request.Binding.Endpoint.Port = 65536
		}},
		{name: "path", mutate: func(request *Request) {
			request.Binding.Account.HomePath = "/home/pairuser/../pairuser"
		}},
		{name: "control character path", mutate: func(request *Request) {
			request.Binding.Product.Path = "/opt/nvpair/\u007f"
		}},
		{name: "empty artifact identity", mutate: func(request *Request) {
			request.Binding.Product.ID = ""
		}},
		{name: "duplicate artifact identity", mutate: func(request *Request) {
			request.Binding.Helper.ID = request.Binding.Product.ID
		}},
		{name: "duplicate artifact path", mutate: func(request *Request) {
			request.Binding.Helper.Path = request.Binding.Product.Path
		}},
		{name: "artifact digest", mutate: func(request *Request) {
			request.Binding.Product.SHA256 = strings.Repeat("AA", 32)
		}},
		{name: "public key material", mutate: func(request *Request) {
			request.Binding.ControllerKey.Material = "not-base64"
		}},
		{name: "public key fingerprint", mutate: func(request *Request) {
			request.Binding.ControllerKey.FingerprintSHA256 = strings.Repeat("00", 32)
		}},
		{name: "oversize account", mutate: func(request *Request) {
			request.Binding.Account.Name = strings.Repeat("a", maxAccountNameBytes+1)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := valid
			testCase.mutate(&request)
			if err := request.Validate(); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPlanRejectsDuplicateAndUnknownActions(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformWindows, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleDesktop,
		RoleDesktop,
	)
	plan, err := Reconcile(request, absentObservations())
	if err != nil {
		t.Fatal(err)
	}
	plan.Actions = append(plan.Actions, plan.Actions[0])
	if err = plan.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate action error = %v", err)
	}
	plan, err = Reconcile(request, absentObservations())
	if err != nil {
		t.Fatal(err)
	}
	plan.Actions[0] = ResourceKind("shell-command")
	if err = plan.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown action error = %v", err)
	}
}

func TestClosedEnumsRejectUnknownValues(t *testing.T) {
	target := Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64}
	request := contractRequest(target, LaneQuickConnect, RoleHeadless, RoleHeadless)

	invalidLane := request
	invalidLane.Binding.Lane = Lane("interactive")
	if err := invalidLane.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("lane error = %v", err)
	}
	invalidRole := request
	invalidRole.Binding.Role = Role("server")
	if err := invalidRole.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("role error = %v", err)
	}
	invalidArchitecture := request
	invalidArchitecture.Binding.Target.Architecture = Architecture("riscv64")
	if err := invalidArchitecture.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("architecture error = %v", err)
	}
	invalidAlgorithm := request
	invalidAlgorithm.Binding.ControllerKey.Algorithm = PublicKeyAlgorithm("ssh-dss")
	if err := invalidAlgorithm.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("public-key algorithm error = %v", err)
	}

	observed := absentObservations()
	observed.SSHService.Ownership = Ownership("shared")
	if _, err := Reconcile(request, observed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ownership error = %v", err)
	}

	status := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         Phase("pending"),
		Binding:       request.Binding,
		Observed:      absentObservations(),
	}
	if err := status.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("phase error = %v", err)
	}
	status.Phase = PhaseReview
	status.Decision = Decision("skip")
	if err := status.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("decision error = %v", err)
	}
}
