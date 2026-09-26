// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"bytes"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeRejectsCaseVariantAliasesAndSemanticDuplicates(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "top-level case alias",
			raw:  bytes.Replace(raw, []byte(`"schemaVersion"`), []byte(`"SchemaVersion"`), 1),
		},
		{
			name: "nested case alias",
			raw:  bytes.Replace(raw, []byte(`"address"`), []byte(`"Address"`), 1),
		},
		{
			name: "case-insensitive semantic duplicate",
			raw: bytes.Replace(
				raw,
				[]byte(`"schemaVersion":1`),
				[]byte(`"schemaVersion":1,"SCHEMAVERSION":1`),
				1,
			),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, decodeErr := DecodeRequest(testCase.raw); !errors.Is(decodeErr, ErrInvalid) {
				t.Fatalf("error = %v", decodeErr)
			}
		})
	}
}

func TestPublicKeyValidationRejectsMalformedRSAAndECDSA(t *testing.T) {
	offCurvePoint := make([]byte, 65)
	offCurvePoint[0] = 4
	tests := []struct {
		name string
		key  PublicKeyIdentity
	}{
		{
			name: "negative RSA exponent",
			key:  reviewPublicKey(PublicKeyAlgorithmRSA, []byte{0x80}, []byte{0x01, 0x01}),
		},
		{
			name: "noncanonical RSA exponent",
			key:  reviewPublicKey(PublicKeyAlgorithmRSA, []byte{0x00, 0x03}, []byte{0x01, 0x01}),
		},
		{
			name: "negative RSA modulus",
			key:  reviewPublicKey(PublicKeyAlgorithmRSA, []byte{0x03}, []byte{0x80, 0x01}),
		},
		{
			name: "ECDSA point outside P-256",
			key: reviewPublicKey(
				PublicKeyAlgorithmECDSA,
				[]byte("nistp256"),
				offCurvePoint,
			),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.key.validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPublicKeyValidationAcceptsCanonicalRSAAndECDSA(t *testing.T) {
	rsaKey := reviewPublicKey(
		PublicKeyAlgorithmRSA,
		[]byte{0x01, 0x00, 0x01},
		[]byte{0x00, 0x80, 0x01},
	)
	if err := rsaKey.validate(); err != nil {
		t.Fatalf("RSA: %v", err)
	}
	curve := elliptic.P256()
	ecdsaKey := reviewPublicKey(
		PublicKeyAlgorithmECDSA,
		[]byte("nistp256"),
		elliptic.Marshal(curve, curve.Params().Gx, curve.Params().Gy),
	)
	if err := ecdsaKey.validate(); err != nil {
		t.Fatalf("ECDSA: %v", err)
	}
}

func TestTypedValidationRejectsInvalidUTF8Path(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	request.Binding.Product.Path = string([]byte{'/', 'o', 'p', 't', '/', 0xff})
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestSSHEndpointRejectsHostnameAndAcceptsCanonicalIPLiteral(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	request.Binding.Endpoint.Address = "target.example.test"
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hostname error = %v", err)
	}
	request.Binding.Endpoint.Address = "2001:db8::10"
	if err := request.Validate(); err != nil {
		t.Fatalf("canonical IPv6 error = %v", err)
	}
	request.Binding.Endpoint.Address = "2001:0db8::10"
	if err := request.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncanonical IPv6 error = %v", err)
	}
}

func TestBlockedTransitionRequiresImmutableForeignRefusalReview(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	absent := absentObservations()
	exact := exactObservations(request)
	repair := absentObservations()
	endpoint := request.Binding.Endpoint
	repair.SSHService = EndpointObservation{Ownership: OwnershipOwned, Endpoint: &endpoint}
	foreign := absentObservations()
	foreign.Firewall = EndpointObservation{Ownership: OwnershipForeign, Endpoint: &endpoint}
	blocked := reviewStatus(request, foreign, PhaseBlocked, DecisionRefuseForeign)
	predecessors := []struct {
		name   string
		status Status
	}{
		{name: "review apply", status: reviewStatus(request, absent, PhaseReview, DecisionApply)},
		{name: "apply", status: reviewStatus(request, absent, PhaseApply, DecisionApply)},
		{name: "verify apply", status: reviewStatus(request, exact, PhaseVerify, DecisionApply)},
		{name: "review no-op", status: reviewStatus(request, exact, PhaseReview, DecisionNoOp)},
		{name: "verify no-op", status: reviewStatus(request, exact, PhaseVerify, DecisionNoOp)},
		{name: "review repair", status: reviewStatus(request, repair, PhaseReview, DecisionRepairOwned)},
		{name: "apply repair", status: reviewStatus(request, repair, PhaseApply, DecisionRepairOwned)},
	}
	for _, testCase := range predecessors {
		t.Run(testCase.name, func(t *testing.T) {
			if err := ValidateTransition(testCase.status, blocked); !errors.Is(err, ErrTransition) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	foreignReview := reviewStatus(request, foreign, PhaseReview, DecisionRefuseForeign)
	if err := ValidateTransition(foreignReview, blocked); err != nil {
		t.Fatalf("valid refusal review: %v", err)
	}
	mutated := blocked
	changedEndpoint := request.Binding.Endpoint
	changedEndpoint.Port = 2222
	mutated.Observed.Firewall.Endpoint = &changedEndpoint
	if err := ValidateTransition(foreignReview, mutated); !errors.Is(err, ErrTransition) {
		t.Fatalf("mutated observations: %v", err)
	}
}

func TestStrictDecodeRejectsNullWrongScalarsAndLoneSurrogates(t *testing.T) {
	request := contractRequest(
		Target{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	inspectStatus := reviewStatus(request, absentObservations(), PhaseInspect, "")
	statusRaw, err := json.Marshal(inspectStatus)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		raw    []byte
		decode func([]byte) error
	}{
		{
			name: "decision null",
			raw:  append(append([]byte{}, statusRaw[:len(statusRaw)-1]...), []byte(`,"decision":null}`)...),
			decode: func(raw []byte) error {
				_, decodeErr := DecodeStatus(raw)
				return decodeErr
			},
		},
		{
			name: "nested port null",
			raw:  bytes.Replace(requestRaw, []byte(`"port":22`), []byte(`"port":null`), 1),
			decode: func(raw []byte) error {
				_, decodeErr := DecodeRequest(raw)
				return decodeErr
			},
		},
		{
			name: "nested address number",
			raw: bytes.Replace(
				requestRaw,
				[]byte(`"address":"192.0.2.10"`),
				[]byte(`"address":22`),
				1,
			),
			decode: func(raw []byte) error {
				_, decodeErr := DecodeRequest(raw)
				return decodeErr
			},
		},
		{
			name: "lone high surrogate path",
			raw: bytes.Replace(
				requestRaw,
				[]byte(`"path":"/opt/nvpair/nvpair"`),
				[]byte(`"path":"/opt/\ud800"`),
				1,
			),
			decode: func(raw []byte) error {
				_, decodeErr := DecodeRequest(raw)
				return decodeErr
			},
		},
		{
			name: "lone low surrogate path",
			raw: bytes.Replace(
				requestRaw,
				[]byte(`"path":"/opt/nvpair/nvpair"`),
				[]byte(`"path":"/opt/\udc00"`),
				1,
			),
			decode: func(raw []byte) error {
				_, decodeErr := DecodeRequest(raw)
				return decodeErr
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if decodeErr := testCase.decode(testCase.raw); !errors.Is(decodeErr, ErrInvalid) {
				t.Fatalf("error = %v", decodeErr)
			}
		})
	}
	validPair := bytes.Replace(
		requestRaw,
		[]byte(`"path":"/opt/nvpair/nvpair"`),
		[]byte(`"path":"/opt/\ud83d\ude80"`),
		1,
	)
	if _, decodeErr := DecodeRequest(validPair); decodeErr != nil {
		t.Fatalf("valid surrogate pair: %v", decodeErr)
	}
}

func reviewStatus(request Request, observed Observations, phase Phase, decision Decision) Status {
	return Status{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         phase,
		Decision:      decision,
		Binding:       request.Binding,
		Observed:      observed,
	}
}

func reviewPublicKey(algorithm PublicKeyAlgorithm, fields ...[]byte) PublicKeyIdentity {
	blob := reviewSSHString(nil, []byte(algorithm))
	for _, field := range fields {
		blob = reviewSSHString(blob, field)
	}
	sum := sha256.Sum256(blob)
	return PublicKeyIdentity{
		Algorithm:         algorithm,
		Material:          base64.StdEncoding.EncodeToString(blob),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}
}

func reviewSSHString(destination, value []byte) []byte {
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(value)))
	destination = append(destination, size...)
	return append(destination, value...)
}
