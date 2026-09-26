// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHelperWireGoldenJSON(t *testing.T) {
	request := HelperRequest{
		SchemaVersion: SchemaVersion,
		OperationID:   strings.Repeat("ab", 16),
		Action:        HelperActionInspect,
	}
	raw, err := EncodeHelperRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	const requestGolden = `{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect"}`
	if string(raw) != requestGolden {
		t.Fatalf("request JSON = %s", raw)
	}
	decodedRequest, err := DecodeHelperRequest(raw)
	if err != nil || !reflect.DeepEqual(decodedRequest, request) {
		t.Fatalf("decoded request = %#v, error = %v", decodedRequest, err)
	}

	status := helperWireStatus(t, request.OperationID)
	response := HelperResponse{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Action:        request.Action,
		Accepted:      true,
		Status:        &status,
	}
	raw, err = EncodeHelperResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	const responseGolden = `{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect","accepted":true,"status":{"schemaVersion":1,"operationId":"abababababababababababababababab","phase":"inspect","binding":{"target":{"platform":"linux","architecture":"amd64"},"lane":"quick-connect","role":"headless","runtimeOwner":"headless","account":{"name":"pairuser","homePath":"/home/pairuser","authorizedKeysPath":"/home/pairuser/.ssh/authorized_keys"},"controllerKey":{"algorithm":"ssh-ed25519","material":"AAAAC3NzaC1lZDI1NTE5AAAAIBMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEy","fingerprintSha256":"0b66a4d4300d4b132e6b893e97f345fbc3a3bfdf13c94b707d47773b79d162f5"},"endpoint":{"address":"192.0.2.10","port":22},"product":{"id":"nvpair","version":"1.2.3","sha256":"1212121212121212121212121212121212121212121212121212121212121212","path":"/opt/nvpair/product"},"helper":{"id":"nvpair-host-helper","version":"1.2.3","sha256":"3434343434343434343434343434343434343434343434343434343434343434","path":"/usr/libexec/nvpair-host-helper"}},"observed":{"sshService":{"ownership":"absent","endpoint":null},"firewall":{"ownership":"absent","endpoint":null},"authorizedKey":{"ownership":"absent","identity":null},"helper":{"ownership":"absent","identity":null},"product":{"ownership":"absent","identity":null},"runtimeOwners":[]}}}`
	if string(raw) != responseGolden {
		t.Fatalf("response JSON = %s", raw)
	}
	decodedResponse, err := DecodeHelperResponse(raw)
	if err != nil || !reflect.DeepEqual(decodedResponse, response) {
		t.Fatalf("decoded response = %#v, error = %v", decodedResponse, err)
	}
}

func TestHelperWireRejectsForgedResultsAndFreeFormFields(t *testing.T) {
	operationID := strings.Repeat("ab", 16)
	tests := [][]byte{
		[]byte(`{"schemaVersion":1,"operationId":"` + operationID + `","action":"inspect","observations":{}}`),
		[]byte(`{"schemaVersion":1,"operationId":"` + operationID + `","action":"apply","receipt":{}}`),
		[]byte(`{"schemaVersion":1,"operationId":"` + operationID + `","action":"verify","command":"whoami"}`),
		[]byte(`{"schemaVersion":1,"operationId":"` + operationID + `","action":"rank-reconcile","rankReconcile":{"runId":"` + operationID + `","generation":1,"rank":0,"planDigest":"` + strings.Repeat("12", 32) + `","nodeId":"node-1"},"path":"/tmp/x"}`),
	}
	for _, raw := range tests {
		if _, err := DecodeHelperRequest(raw); err == nil {
			t.Fatalf("forged helper request accepted: %s", raw)
		}
	}

	status := helperWireStatus(t, operationID)
	status.OperationID = strings.Repeat("cd", 16)
	if _, err := EncodeHelperResponse(HelperResponse{
		SchemaVersion: SchemaVersion,
		OperationID:   operationID,
		Action:        HelperActionInspect,
		Accepted:      true,
		Status:        &status,
	}); err == nil {
		t.Fatal("mismatched target status accepted")
	}
}

func TestHelperFrameIsBounded(t *testing.T) {
	var framed bytes.Buffer
	if err := WriteHelperFrame(&framed, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	payload, err := ReadHelperFrame(&framed)
	if err != nil || string(payload) != "payload" {
		t.Fatalf("payload = %q, error = %v", payload, err)
	}
	if err := WriteHelperFrame(&framed, bytes.Repeat([]byte("x"), MaxHelperFrameBytes+1)); !errors.Is(err, ErrHelperFrameTooLarge) {
		t.Fatalf("oversized write error = %v", err)
	}
}

func helperWireStatus(t *testing.T, operationID string) Status {
	t.Helper()
	request := contractRequest(
		Target{
			Platform:     PlatformLinux,
			Architecture: ArchitectureAMD64,
		},
		LaneQuickConnect,
		RoleHeadless,
		RoleHeadless,
	)
	request.OperationID = operationID
	request.Binding.Product.Path = "/opt/nvpair/product"
	status := Status{
		SchemaVersion: SchemaVersion,
		OperationID:   operationID,
		Phase:         PhaseInspect,
		Binding:       request.Binding,
		Observed: Observations{
			SSHService:    EndpointObservation{Ownership: OwnershipAbsent},
			Firewall:      EndpointObservation{Ownership: OwnershipAbsent},
			AuthorizedKey: AuthorizedKeyObservation{Ownership: OwnershipAbsent},
			Helper:        ArtifactObservation{Ownership: OwnershipAbsent},
			Product:       ArtifactObservation{Ownership: OwnershipAbsent},
			RuntimeOwners: []RuntimeOwnerObservation{},
		},
	}
	if err := status.Validate(); err != nil {
		t.Fatal(err)
	}
	return status
}
